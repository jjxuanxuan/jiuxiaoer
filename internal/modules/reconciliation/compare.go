package reconciliation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func (s *Service) compare(ctx context.Context, tx *gorm.DB, run Run, billDate time.Time, billType string) (uint64, map[string]uint64, error) {
	return s.compareDate(ctx, tx, run, billDate, billType)
}

func (s *Service) compareDate(ctx context.Context, tx *gorm.DB, run Run, billDateTime time.Time, billType string) (uint64, map[string]uint64, error) {
	stats := map[string]uint64{}
	var total uint64
	add := func(kind string, observation *Observation, local, wechat any, businessNo, tradeNo, refundNo string) error {
		localJSON, err := jsonValue(local)
		if err != nil {
			return err
		}
		wechatJSON, err := jsonValue(wechat)
		if err != nil {
			return err
		}
		line := "local"
		if observation != nil {
			line = fmt.Sprintf("%d", observation.LineNo)
		}
		row := Discrepancy{
			ID: s.ids.Next(), RunID: run.ID, BillDate: billDateTime, BillType: billType, DiscrepancyType: kind,
			BusinessNo: stringPtr(businessNo), ProviderTradeNo: stringPtr(tradeNo), ProviderRefundNo: stringPtr(refundNo),
			LocalValue: localJSON, WeChatValue: wechatJSON, Status: "open",
			DedupeKey: digestKey(kind, businessNo, tradeNo, refundNo, line),
		}
		if err := tx.WithContext(ctx).Create(&row).Error; err != nil {
			return err
		}
		total++
		stats[kind]++
		return nil
	}

	var cursor uint64
	for {
		var observations []Observation
		if err := tx.WithContext(ctx).Where("run_id=? AND id>?", run.ID, cursor).Order("id").Limit(500).Find(&observations).Error; err != nil {
			return 0, nil, err
		}
		if len(observations) == 0 {
			break
		}
		for i := range observations {
			observation := &observations[i]
			if err := s.compareObservation(ctx, tx, observation, add); err != nil {
				return 0, nil, err
			}
			cursor = observation.ID
		}
	}

	if billType == BillTypeTradeAll {
		start, end := billDateTime, billDateTime.AddDate(0, 0, 1)
		var paymentCursor uint64
		for {
			var payments []localPayment
			err := tx.WithContext(ctx).Table("payments AS p").
				Select("p.id,p.payment_no,p.status,p.amount,p.currency,p.provider_trade_no,p.provider_status,p.paid_at").
				Where("p.id>? AND p.provider='wechat' AND p.status='succeeded' AND p.paid_at>=? AND p.paid_at<? AND p.deleted_at IS NULL", paymentCursor, start, end).
				Where("NOT EXISTS (SELECT 1 FROM wechat_bill_observations o WHERE o.run_id=? AND o.entry_kind='payment' AND (o.business_no=p.payment_no OR (p.provider_trade_no IS NOT NULL AND o.provider_trade_no=p.provider_trade_no)))", run.ID).
				Order("p.id").Limit(500).Scan(&payments).Error
			if err != nil {
				return 0, nil, err
			}
			if len(payments) == 0 {
				break
			}
			for _, payment := range payments {
				tradeNo := deref(payment.ProviderTradeNo)
				local := paymentValue(payment)
				if err := add(DiscrepancyMissingWeChat, nil, local, nil, payment.PaymentNo, tradeNo, ""); err != nil {
					return 0, nil, err
				}
				paymentCursor = payment.ID
			}
		}

		var refundCursor uint64
		for {
			var refunds []localRefund
			err := tx.WithContext(ctx).Table("refunds AS r").
				Select("r.id,r.payment_id,r.refund_no,r.status,r.currency,r.provider_refund_id,r.provider_status,r.amount,r.requested_at,r.provider_accepted_at").
				Where("r.id>? AND r.provider='wechat' AND r.provider_refund_id IS NOT NULL AND r.status IN ? AND COALESCE(r.provider_accepted_at,r.requested_at)>=? AND COALESCE(r.provider_accepted_at,r.requested_at)<? AND r.deleted_at IS NULL", refundCursor, []string{"pending", "succeeded", "failed", "exception"}, start, end).
				Where("NOT EXISTS (SELECT 1 FROM wechat_bill_observations o WHERE o.run_id=? AND o.entry_kind='refund' AND (o.business_no=r.refund_no OR o.provider_refund_no=r.provider_refund_id))", run.ID).
				Order("r.id").Limit(500).Scan(&refunds).Error
			if err != nil {
				return 0, nil, err
			}
			if len(refunds) == 0 {
				break
			}
			for _, refund := range refunds {
				providerRefundNo := deref(refund.ProviderRefundID)
				if err := add(DiscrepancyMissingWeChat, nil, refundValue(refund), nil, refund.RefundNo, "", providerRefundNo); err != nil {
					return 0, nil, err
				}
				refundCursor = refund.ID
			}
		}
	}
	return total, stats, nil
}

type discrepancyAdder func(string, *Observation, any, any, string, string, string) error

func (s *Service) compareObservation(ctx context.Context, tx *gorm.DB, observation *Observation, add discrepancyAdder) error {
	businessNo, tradeNo, refundNo := deref(observation.BusinessNo), deref(observation.ProviderTradeNo), deref(observation.ProviderRefundNo)
	wechat := observationValue(*observation)
	switch observation.EntryKind {
	case "payment":
		payment, err := findPayment(ctx, tx, businessNo, tradeNo)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return add(DiscrepancyMissingLocal, observation, nil, wechat, businessNo, tradeNo, "")
		}
		if err != nil {
			return err
		}
		local := paymentValue(payment)
		if (observation.Amount != nil && payment.Amount != *observation.Amount) || !strings.EqualFold(payment.Currency, deref(observation.Currency)) {
			if err := add(DiscrepancyAmountMismatch, observation, local, wechat, businessNo, tradeNo, ""); err != nil {
				return err
			}
		}
		if payment.Status != "succeeded" {
			if err := add(DiscrepancyStatusMismatch, observation, local, wechat, businessNo, tradeNo, ""); err != nil {
				return err
			}
		}
		if payment.PaymentNo != businessNo || deref(payment.ProviderTradeNo) != tradeNo {
			if err := add(DiscrepancyTransactionIDMismatch, observation, local, wechat, businessNo, tradeNo, ""); err != nil {
				return err
			}
		}
	case "refund":
		refund, err := findRefund(ctx, tx, businessNo, refundNo)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return add(DiscrepancyMissingLocal, observation, nil, wechat, businessNo, tradeNo, refundNo)
		}
		if err != nil {
			return err
		}
		local := refundValue(refund)
		if (observation.Amount != nil && refund.Amount != *observation.Amount) || !strings.EqualFold(refund.Currency, deref(observation.Currency)) {
			if err := add(DiscrepancyAmountMismatch, observation, local, wechat, businessNo, tradeNo, refundNo); err != nil {
				return err
			}
		}
		if refund.RefundNo != businessNo || deref(refund.ProviderRefundID) != refundNo {
			if err := add(DiscrepancyRefundMismatch, observation, local, wechat, businessNo, tradeNo, refundNo); err != nil {
				return err
			}
		}
		var payment struct{ ProviderTradeNo *string }
		paymentResult := tx.WithContext(ctx).Table("payments").Select("provider_trade_no").Where("id=? AND provider='wechat' AND deleted_at IS NULL", refund.PaymentID).Scan(&payment)
		if paymentResult.Error != nil {
			return paymentResult.Error
		}
		if paymentResult.RowsAffected == 0 || deref(payment.ProviderTradeNo) != tradeNo {
			if err := add(DiscrepancyTransactionIDMismatch, observation, local, wechat, businessNo, tradeNo, refundNo); err != nil {
				return err
			}
		}
		providerStatus := strings.ToUpper(deref(observation.ProviderStatus))
		if (providerStatus == "SUCCESS" && refund.Status != "succeeded") || ((providerStatus == "FAIL" || providerStatus == "CHANGE") && refund.Status == "succeeded") {
			if err := add(DiscrepancyStatusMismatch, observation, local, wechat, businessNo, tradeNo, refundNo); err != nil {
				return err
			}
		}
	case "fund":
		status := deref(observation.ProviderStatus)
		if !strings.Contains(status, "交易") && !strings.Contains(status, "退款") {
			return nil
		}
		if strings.Contains(status, "退款") {
			if _, err := findRefund(ctx, tx, businessNo, tradeNo); err == nil {
				return nil
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}
		if _, err := findPayment(ctx, tx, businessNo, tradeNo); err == nil {
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return add(DiscrepancyMissingLocal, observation, nil, wechat, businessNo, tradeNo, "")
	case "unknown":
		return add(DiscrepancyStatusMismatch, observation, nil, wechat, businessNo, tradeNo, refundNo)
	}
	return nil
}

func findPayment(ctx context.Context, tx *gorm.DB, businessNo, tradeNo string) (localPayment, error) {
	var row localPayment
	base := func() *gorm.DB {
		return tx.WithContext(ctx).Table("payments").Select("id,payment_no,status,amount,currency,provider_trade_no,provider_status,paid_at").Where("provider='wechat' AND deleted_at IS NULL")
	}
	if businessNo != "" {
		err := base().Where("payment_no=?", businessNo).Take(&row).Error
		if err == nil || !errors.Is(err, gorm.ErrRecordNotFound) || tradeNo == "" {
			return row, err
		}
	}
	if tradeNo != "" {
		return row, base().Where("provider_trade_no=?", tradeNo).Take(&row).Error
	}
	return row, gorm.ErrRecordNotFound
}

func findRefund(ctx context.Context, tx *gorm.DB, businessNo, providerNo string) (localRefund, error) {
	var row localRefund
	base := func() *gorm.DB {
		return tx.WithContext(ctx).Table("refunds").Select("id,payment_id,refund_no,status,currency,provider_refund_id,provider_status,amount,requested_at,provider_accepted_at").Where("provider='wechat' AND deleted_at IS NULL")
	}
	if businessNo != "" {
		err := base().Where("refund_no=?", businessNo).Take(&row).Error
		if err == nil || !errors.Is(err, gorm.ErrRecordNotFound) || providerNo == "" {
			return row, err
		}
	}
	if providerNo != "" {
		return row, base().Where("provider_refund_id=?", providerNo).Take(&row).Error
	}
	return row, gorm.ErrRecordNotFound
}

func paymentValue(row localPayment) map[string]any {
	return map[string]any{"id": row.ID, "payment_no": row.PaymentNo, "provider_trade_no": deref(row.ProviderTradeNo), "status": row.Status, "provider_status": deref(row.ProviderStatus), "amount": row.Amount, "currency": row.Currency, "paid_at": row.PaidAt}
}

func refundValue(row localRefund) map[string]any {
	return map[string]any{"id": row.ID, "payment_id": row.PaymentID, "refund_no": row.RefundNo, "provider_refund_no": deref(row.ProviderRefundID), "status": row.Status, "provider_status": deref(row.ProviderStatus), "amount": row.Amount, "currency": row.Currency, "requested_at": row.RequestedAt, "provider_accepted_at": row.ProviderAcceptedAt}
}

func observationValue(row Observation) map[string]any {
	return map[string]any{"line_no": row.LineNo, "entry_kind": row.EntryKind, "business_no": deref(row.BusinessNo), "provider_trade_no": deref(row.ProviderTradeNo), "provider_refund_no": deref(row.ProviderRefundNo), "provider_status": deref(row.ProviderStatus), "amount": row.Amount, "currency": deref(row.Currency), "occurred_at": row.OccurredAt, "raw_hash": row.RawHash}
}

func jsonValue(value any) (datatypes.JSON, error) {
	if value == nil {
		return nil, nil
	}
	data, err := json.Marshal(value)
	return datatypes.JSON(data), err
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
