package renewal

import (
	"context"
	"errors"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
)

// RenewalPaymentSettlementHandler 为共享支付注册表提供独立的
// wine_ticket_renewal 业务处理器，避免复用购买结算。
type RenewalPaymentSettlementHandler struct {
	service *RenewalService
}

func NewRenewalPaymentSettlementHandler(
	service *RenewalService,
) *RenewalPaymentSettlementHandler {
	return &RenewalPaymentSettlementHandler{service: service}
}

func (h *RenewalPaymentSettlementHandler) BusinessType() string {
	return RenewalPaymentBusiness
}

// LockBusiness 建立批次 -> 续期的锁顺序。
// 共享订单服务仅在本方法返回后锁定公共支付记录。
func (h *RenewalPaymentSettlementHandler) LockBusiness(
	ctx context.Context,
	tx *gorm.DB,
	renewalID uint64,
) error {
	if h == nil || h.service == nil || tx == nil {
		return problem.Internal("renewal payment settlement is not configured")
	}
	lookup, err := h.service.repo.renewalByID(ctx, tx, renewalID, false)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.NotFound(
			"WT_RENEWAL_NOT_FOUND",
			"wine ticket renewal not found",
		)
	}
	if err != nil {
		return err
	}
	if _, err := h.service.repo.lockLotByID(ctx, tx, lookup.LotID); err != nil {
		return err
	}
	_, err = h.service.repo.lockRenewalAfterLot(ctx, tx, renewalID, lookup.LotID)
	return err
}

func (h *RenewalPaymentSettlementHandler) ApplySuccess(
	ctx context.Context,
	tx *gorm.DB,
	fact order.PaymentSettlementFact,
) error {
	lot, renewal, err := h.lockedRows(ctx, tx, fact.BizID)
	if err != nil {
		return err
	}
	if fact.BizType != RenewalPaymentBusiness || fact.BizID != renewal.ID {
		return problem.Internal("renewal payment business route is invalid")
	}
	if renewal.Status == RenewalStatusCompleted {
		if !sameRenewalPaymentFact(renewal, fact) ||
			!lot.ExpiresAt.Equal(renewal.NewExpiresAt) {
			return problem.Conflict(
				"WT_CONCURRENT_MODIFICATION",
				"completed renewal facts are inconsistent",
			)
		}
		return nil
	}
	if renewal.Status == RenewalStatusCompensatingRefund ||
		renewal.Status == RenewalStatusRefundException ||
		renewal.Status == RenewalStatusRefunded {
		return nil
	}
	switch renewal.Status {
	case RenewalStatusPendingPayment, RenewalStatusPaymentUnknown,
		RenewalStatusApplying, RenewalStatusClosed:
	default:
		return problem.Conflict(
			"WT_CONCURRENT_MODIFICATION",
			"wine ticket renewal cannot accept a successful payment",
		)
	}

	now := h.service.nowShanghai()
	if renewal.Status != RenewalStatusApplying {
		if err := h.service.repo.updateRenewalVersioned(
			ctx,
			tx,
			renewal,
			map[string]any{
				"status":     RenewalStatusApplying,
				"closed_at":  nil,
				"version":    gorm.Expr("version + 1"),
				"updated_at": now,
			},
		); err != nil {
			return err
		}
		renewal.Status = RenewalStatusApplying
		renewal.Version++
	}

	reason, err := h.applicationFailureReason(
		ctx,
		tx,
		lot,
		renewal,
		fact,
		now,
	)
	if err != nil {
		return err
	}
	if reason != "" {
		return h.enterCompensatingRefundTx(
			ctx,
			tx,
			lot,
			renewal,
			fact,
			now,
			reason,
		)
	}

	if err := h.service.repo.updateLotVersioned(
		ctx,
		tx,
		lot.ID,
		lot.Version,
		map[string]any{
			"expires_at":        renewal.NewExpiresAt,
			"expiry_changed_at": now,
			"renewal_count":     lot.RenewalCount + 1,
			"ever_used":         true,
			"status":            LotStatusActive,
			"version":           gorm.Expr("version + 1"),
			"updated_at":        now,
		},
	); err != nil {
		// 支付已确认后的版本更新失败属于本地应用冲突，
		// 必须转为退款，不能造成资金损失。
		if problem.FromError(err).ErrorCode == "WT_CONCURRENT_MODIFICATION" {
			return h.enterCompensatingRefundTx(
				ctx,
				tx,
				lot,
				renewal,
				fact,
				now,
				"lot_update_conflict",
			)
		}
		return err
	}
	if err := h.service.repo.updateRenewalVersioned(
		ctx,
		tx,
		renewal,
		map[string]any{
			"status":       RenewalStatusCompleted,
			"completed_at": now,
			"version":      gorm.Expr("version + 1"),
			"updated_at":   now,
		},
	); err != nil {
		return err
	}
	if err := h.service.createRenewalSettlementAudit(
		ctx,
		tx,
		"wine_ticket.renewal.complete",
		renewal,
		RenewalStatusCompleted,
		map[string]any{
			"renewal_no":       renewal.RenewalNo,
			"lot_no":           lot.LotNo,
			"old_expires_at":   formatShanghai(renewal.OldExpiresAt),
			"new_expires_at":   formatShanghai(renewal.NewExpiresAt),
			"provider_paid_at": formatShanghai(*fact.PaidAt),
		},
	); err != nil {
		return err
	}
	return h.service.createRenewedOutbox(ctx, tx, renewal, lot.LotNo)
}

func (h *RenewalPaymentSettlementHandler) ApplyTerminal(
	ctx context.Context,
	tx *gorm.DB,
	fact order.PaymentSettlementFact,
) error {
	lot, renewal, err := h.lockedRows(ctx, tx, fact.BizID)
	if err != nil {
		return err
	}
	if !sameRenewalPaymentFact(renewal, fact) {
		return problem.Conflict(
			"PAYMENT_PROVIDER_DATA_MISMATCH",
			"payment fact does not match wine ticket renewal",
		)
	}
	return h.service.closeUnpaidRenewalTx(
		ctx,
		tx,
		&lot,
		renewal,
		fact.ProviderStatus,
	)
}

func (h *RenewalPaymentSettlementHandler) ApplyException(
	ctx context.Context,
	tx *gorm.DB,
	fact order.PaymentSettlementFact,
	reason string,
) error {
	_, renewal, err := h.lockedRows(ctx, tx, fact.BizID)
	if err != nil {
		return err
	}
	if renewal.Status == RenewalStatusCompleted ||
		renewal.Status == RenewalStatusClosed ||
		renewal.Status == RenewalStatusCompensatingRefund ||
		renewal.Status == RenewalStatusRefundException ||
		renewal.Status == RenewalStatusRefunded {
		return nil
	}
	if renewal.Status == RenewalStatusPaymentUnknown {
		return nil
	}
	now := h.service.nowShanghai()
	if err := h.service.repo.updateRenewalVersioned(
		ctx,
		tx,
		renewal,
		map[string]any{
			"status":     RenewalStatusPaymentUnknown,
			"version":    gorm.Expr("version + 1"),
			"updated_at": now,
		},
	); err != nil {
		return err
	}
	return h.service.createRenewalSettlementAudit(
		ctx,
		tx,
		"wine_ticket.renewal.payment_unknown",
		renewal,
		RenewalStatusPaymentUnknown,
		map[string]any{
			"renewal_no":      renewal.RenewalNo,
			"reason_code":     strings.TrimSpace(reason),
			"provider_status": fact.ProviderStatus,
		},
	)
}

func (h *RenewalPaymentSettlementHandler) lockedRows(
	ctx context.Context,
	tx *gorm.DB,
	renewalID uint64,
) (core.Lot, Renewal, error) {
	if h == nil || h.service == nil || tx == nil || renewalID == 0 {
		return core.Lot{}, Renewal{}, problem.Internal(
			"renewal payment settlement is not configured",
		)
	}
	lookup, err := h.service.repo.renewalByID(ctx, tx, renewalID, false)
	if err != nil {
		return core.Lot{}, Renewal{}, err
	}
	lot, err := h.service.repo.lockLotByID(ctx, tx, lookup.LotID)
	if err != nil {
		return core.Lot{}, Renewal{}, err
	}
	renewal, err := h.service.repo.lockRenewalAfterLot(
		ctx,
		tx,
		renewalID,
		lot.ID,
	)
	return lot, renewal, err
}

func (h *RenewalPaymentSettlementHandler) applicationFailureReason(
	ctx context.Context,
	tx *gorm.DB,
	lot core.Lot,
	renewal Renewal,
	fact order.PaymentSettlementFact,
	now time.Time,
) (string, error) {
	switch {
	case !sameRenewalPaymentFact(renewal, fact):
		return "payment_business_fact_mismatch", nil
	case fact.PaidAt == nil:
		return "provider_paid_at_missing", nil
	case fact.PaidAt.After(renewal.OldExpiresAt):
		return "provider_paid_after_old_expiry", nil
	case !renewal.NewExpiresAt.After(now):
		return "settlement_reached_fixed_new_expiry", nil
	case lot.ID != renewal.LotID || lot.OwnerCustomerID != renewal.CustomerID:
		return "renewal_target_owner_changed", nil
	case lot.Status != LotStatusActive || lot.AvailableQuantity == 0:
		return "renewal_target_not_active", nil
	case !lot.ExpiresAt.Equal(renewal.OldExpiresAt):
		return "renewal_target_expiry_changed", nil
	case renewal.ExpectedLotVersion == ^uint(0) ||
		lot.Version != renewal.ExpectedLotVersion+1:
		return "renewal_target_version_changed", nil
	case renewal.ExtensionDays == 0 ||
		!renewal.NewExpiresAt.Equal(
			renewalNewExpiry(
				renewal.OldExpiresAt,
				int(renewal.ExtensionDays),
			),
		):
		return "renewal_fixed_target_invalid", nil
	}
	var policy core.RenewalPolicy
	if err := decodePolicyJSON(
		renewal.PolicySnapshot,
		&policy,
		"schema_version",
		"enabled",
		"extension_days",
		"max_count",
		"grace_days",
		"fee_amount",
	); err != nil {
		return "renewal_policy_snapshot_invalid", nil
	}
	if policy.SchemaVersion != 1 || !policy.Enabled || policy.GraceDays != 0 ||
		policy.ExtensionDays != int(renewal.ExtensionDays) ||
		policy.FeeAmount != renewal.FeeAmount ||
		policy.MaxCount < 0 ||
		lot.RenewalCount >= uint(policy.MaxCount) {
		return "renewal_policy_target_mismatch", nil
	}
	currentPolicy, err := h.service.repo.policySnapshot(ctx, tx, lot.PurchaseID)
	if err != nil {
		return "", err
	}
	if renewalPolicyDigest(currentPolicy) !=
		renewalPolicyDigest(renewal.PolicySnapshot) {
		return "renewal_purchase_policy_changed", nil
	}
	blocked, err := h.service.repo.hasActiveHold(ctx, tx, lot.ID)
	if err != nil {
		return "", err
	}
	if blocked {
		return "renewal_target_has_active_hold", nil
	}
	return "", nil
}

func (h *RenewalPaymentSettlementHandler) enterCompensatingRefundTx(
	ctx context.Context,
	tx *gorm.DB,
	lot core.Lot,
	renewal Renewal,
	fact order.PaymentSettlementFact,
	now time.Time,
	reason string,
) error {
	if renewal.CompensatingRefundID != nil {
		return nil
	}
	if fact.PaymentID == 0 || fact.Amount <= 0 || fact.Currency != "CNY" {
		return problem.Internal("paid renewal cannot create a compensation refund")
	}
	refundID := h.service.ids.Next()
	bizType := RenewalCompensationRefundBusiness
	bizID := renewal.ID
	compensation := refund.Row{
		ID:          refundID,
		PaymentID:   fact.PaymentID,
		RefundNo:    "WTRF" + idString(refundID),
		Provider:    fact.Provider,
		Status:      "creating",
		Currency:    fact.Currency,
		Reason:      "酒票续期未生效自动退款",
		BizType:     &bizType,
		BizID:       &bizID,
		Amount:      fact.Amount,
		TotalAmount: fact.Amount,
		RequestedAt: now,
		Version:     1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if strings.TrimSpace(compensation.Provider) == "" {
		compensation.Provider = "wechat"
	}
	if err := h.service.repo.createCompensationRefund(
		ctx,
		tx,
		&compensation,
	); err != nil {
		return err
	}
	if err := h.service.repo.updateRenewalVersioned(
		ctx,
		tx,
		renewal,
		map[string]any{
			"status":                 RenewalStatusCompensatingRefund,
			"compensating_refund_id": refundID,
			"version":                gorm.Expr("version + 1"),
			"updated_at":             now,
		},
	); err != nil {
		return err
	}
	return h.service.createRenewalSettlementAudit(
		ctx,
		tx,
		"wine_ticket.renewal.compensating_refund",
		renewal,
		RenewalStatusCompensatingRefund,
		renewalCompensationSnapshot{
			SchemaVersion:      1,
			ReasonCode:         reason,
			ProviderPaidAt:     renewalOptionalTime(fact.PaidAt),
			SettlementAt:       formatShanghai(now),
			OldExpiresAt:       formatShanghai(renewal.OldExpiresAt),
			NewExpiresAt:       formatShanghai(renewal.NewExpiresAt),
			ExpectedLotVersion: renewal.ExpectedLotVersion,
		},
	)
}

func sameRenewalPaymentFact(
	renewal Renewal,
	fact order.PaymentSettlementFact,
) bool {
	return fact.BizType == RenewalPaymentBusiness &&
		fact.BizID == renewal.ID &&
		renewal.PaymentID != nil &&
		*renewal.PaymentID == fact.PaymentID &&
		renewal.CustomerID == fact.CustomerID &&
		renewal.FeeAmount == fact.Amount &&
		renewal.Currency == fact.Currency
}

func renewalOptionalTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return formatShanghai(*value)
}

func (s *RenewalService) createRenewalSettlementAudit(
	ctx context.Context,
	tx *gorm.DB,
	action string,
	renewal Renewal,
	toStatus string,
	after any,
) error {
	version := uint64(renewal.Version + 1)
	return s.core.createAudit(ctx, tx, map[string]any{
		"id":            s.ids.Next(),
		"actor_type":    "system",
		"actor_id":      0,
		"action":        action,
		"resource_type": "wine_ticket_renewal",
		"resource_id":   renewal.ID,
		"before_data":   datatypes.JSON([]byte("{}")),
		"after_data":    jsonData(after),
		"result":        "success",
		"before_status": renewal.Status,
		"after_status":  toStatus,
		"version":       version,
		"request_id":    requestctx.RequestIDPtr(ctx),
	})
}
