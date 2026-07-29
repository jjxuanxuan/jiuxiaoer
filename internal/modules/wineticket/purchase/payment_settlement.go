package purchase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
)

type issuanceCompensationEligibilitySnapshot struct {
	SchemaVersion         uint8   `json:"schema_version"`
	PurchaseNo            string  `json:"purchase_no"`
	PaymentID             string  `json:"payment_id"`
	PaymentNo             string  `json:"payment_no"`
	ProviderStatus        string  `json:"provider_status"`
	ProviderTradeNo       *string `json:"provider_trade_no,omitempty"`
	ProviderPaidAt        string  `json:"provider_paid_at"`
	SettlementErrorCode   string  `json:"settlement_error_code"`
	SettlementAttempts    uint32  `json:"settlement_attempts"`
	LotCount              int64   `json:"lot_count"`
	AllocationCount       int64   `json:"allocation_count"`
	IssueTransactionCount int64   `json:"issue_transaction_count"`
}

func (s *Service) BusinessType() string { return PurchasePaymentBusiness }

// LockBusiness 建立 PRD 规定的购买记录 -> 配额锁顺序。
// 共享支付服务仅在本方法返回后锁定支付记录。
func (s *Service) LockBusiness(ctx context.Context, tx *gorm.DB, purchaseID uint64) error {
	purchase, err := s.repo.LockPurchaseByID(ctx, tx, purchaseID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.NotFound("WT_PURCHASE_NOT_FOUND", "wine ticket purchase not found")
	}
	if err != nil {
		return err
	}
	snapshot, err := parsePurchaseSnapshot(purchase.PackageSnapshot)
	if err != nil {
		return err
	}
	if _, err := s.repo.LockPurchaseQuota(ctx, tx, purchase.CustomerID, snapshot.PackageCode); errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.Internal("wine ticket purchase quota is missing")
	} else {
		return err
	}
}

func (s *Service) ApplySuccess(ctx context.Context, tx *gorm.DB, fact order.PaymentSettlementFact) error {
	purchase, quota, snapshot, err := s.lockedSettlementRows(ctx, tx, fact)
	if err != nil {
		return err
	}
	if purchase.Status == PurchaseStatusIssued {
		return s.verifyIssuedPurchase(ctx, tx, purchase)
	}
	switch purchase.Status {
	case PurchaseStatusPendingPayment, PurchaseStatusPaymentUnknown, PurchaseStatusSettlementException:
	default:
		return problem.Conflict("WT_CONCURRENT_MODIFICATION", "wine ticket purchase cannot accept a successful payment")
	}
	if fact.PaidAt == nil {
		return problem.Internal("provider paid_at is required for wine ticket issuance")
	}
	paidAt := fact.PaidAt.In(shanghaiLocation).Truncate(time.Millisecond)
	expiresAt := paidAt.AddDate(0, 0, int(snapshot.ValidityDays)).Truncate(time.Millisecond)
	now := s.nowShanghai()
	if !expiresAt.After(now) {
		return s.createIssuanceCompensationTx(
			ctx,
			tx,
			purchase,
			quota,
			fact,
			paidAt,
			now,
			"validity_elapsed_before_issuance",
		)
	}
	if quota.ReservedQuantity < purchase.PackageQuantity {
		return problem.Internal("wine ticket purchase quota reservation is inconsistent")
	}
	if purchase.PackageQuantity > ^uint(0)-quota.ConsumedQuantity {
		return problem.Internal("wine ticket purchase quota consumption overflow")
	}

	lot, err := s.repo.PurchaseLot(ctx, tx, purchase.ID)
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		lotID := s.ids.Next()
		lot = core.Lot{
			ID: lotID, LotNo: "WTL" + idString(lotID),
			OwnerCustomerID: purchase.CustomerID, PurchaseID: purchase.ID,
			SourceType: LotSourcePurchase, IssuerMerchantID: purchase.IssuerMerchantID,
			ProductID: purchase.ProductID, RedeemCityCode: purchase.RedeemCityCode,
			TotalQuantity:     purchase.TotalBottleQuantity,
			AvailableQuantity: purchase.TotalBottleQuantity,
			OriginalExpiresAt: expiresAt, ExpiresAt: expiresAt, ExpiryChangedAt: now,
			Status: LotStatusActive, Version: 1, CreatedAt: now, UpdatedAt: now,
		}
		actionKey := purchaseIssueActionKey(purchase.ID, lot.ID)
		mutation, err := s.assets.Issue(
			ctx,
			core.NewTransactionAssetRepository(tx),
			core.IssueCommand{
				Lot:             lot,
				TransactionType: TransactionTypePurchaseIssue,
				BizType:         "purchase",
				BizID:           purchase.ID,
				ActionKey:       actionKey,
				Metadata: map[string]any{
					"purchase_no":      purchase.PurchaseNo,
					"package_no":       snapshot.PackageNo,
					"provider_paid_at": formatShanghai(paidAt),
				},
				OccurredAt: now,
			},
		)
		if err != nil {
			return err
		}
		lot = mutation.Lot
	case err != nil:
		return err
	default:
		actionKey := purchaseIssueActionKey(purchase.ID, lot.ID)
		entry, transactionErr := s.repo.TransactionByActionKey(
			ctx,
			tx,
			actionKey,
		)
		if transactionErr != nil {
			return problem.Internal("wine ticket lot exists without its immutable issuance transaction")
		}
		if !validPurchaseIssueEvidence(
			purchase,
			lot,
			entry,
			expiresAt,
		) ||
			lot.AvailableQuantity != purchase.TotalBottleQuantity ||
			!lot.ExpiresAt.Equal(expiresAt) {
			return problem.Internal("existing wine ticket issuance fact is inconsistent")
		}
	}

	if err := s.repo.UpdatePurchaseQuota(ctx, tx, quota.ID, map[string]any{
		"reserved_quantity": quota.ReservedQuantity - purchase.PackageQuantity,
		"consumed_quantity": quota.ConsumedQuantity + purchase.PackageQuantity,
		"version":           gorm.Expr("version + 1"), "updated_at": now,
	}); err != nil {
		return err
	}
	if err := s.repo.UpdatePurchase(ctx, tx, purchase.ID, map[string]any{
		"status": PurchaseStatusIssued, "paid_amount": fact.Amount,
		"paid_at": paidAt, "issued_at": now,
		"version": gorm.Expr("version + 1"), "updated_at": now,
	}); err != nil {
		return err
	}
	if err := s.createSettlementAudit(ctx, tx, "wine_ticket.purchase.issue", purchase.ID, purchase.Status, PurchaseStatusIssued, map[string]any{
		"purchase_no": purchase.PurchaseNo, "lot_no": lot.LotNo,
		"quantity": purchase.TotalBottleQuantity,
	}); err != nil {
		return err
	}
	return s.createWineTicketOutbox(ctx, tx, "wine_ticket.purchase_issued", "wine_ticket_purchase", purchase.ID, map[string]any{
		"purchase_no": purchase.PurchaseNo, "lot_no": lot.LotNo,
		"customer_id":           idString(purchase.CustomerID),
		"total_bottle_quantity": purchase.TotalBottleQuantity,
		"expires_at":            formatShanghai(expiresAt),
	})
}

// createIssuanceCompensationTx 处理支付机构已确认、
// 但已无法生成可用批次的永久零权益分支。
// 它在同一个购买记录 -> 配额 -> 支付结算事务中运行，
// 且不会创建批次、分配记录或发放流水。
func (s *Service) createIssuanceCompensationTx(
	ctx context.Context,
	tx *gorm.DB,
	purchase Purchase,
	quota PurchaseQuota,
	fact order.PaymentSettlementFact,
	paidAt time.Time,
	now time.Time,
	reasonCode string,
) error {
	if quota.ReservedQuantity < purchase.PackageQuantity {
		return problem.Internal(
			"wine ticket purchase quota reservation is inconsistent",
		)
	}
	if purchase.PackageQuantity > ^uint(0)-quota.ConsumedQuantity {
		return problem.Internal(
			"wine ticket purchase quota consumption overflow",
		)
	}

	lotCount, allocationCount, issueCount, activeRefundCount, err :=
		s.repo.issuanceCompensationFactCounts(ctx, tx, purchase.ID)
	if err != nil {
		return err
	}
	if lotCount != 0 || allocationCount != 0 || issueCount != 0 {
		return problem.Internal(
			"paid wine ticket purchase has entitlement facts and cannot be auto-refunded",
		)
	}

	if activeRefundCount != 0 {
		return problem.Internal(
			"wine ticket purchase already has an active refund",
		)
	}

	businessID := s.ids.Next()
	commonID := s.ids.Next()
	businessNo := "WTRF" + idString(businessID)
	commonNo := "WTRFC" + idString(commonID)
	eligibility := issuanceCompensationEligibilitySnapshot{
		SchemaVersion:         1,
		PurchaseNo:            purchase.PurchaseNo,
		PaymentID:             idString(fact.PaymentID),
		PaymentNo:             fact.PaymentNo,
		ProviderStatus:        fact.ProviderStatus,
		ProviderTradeNo:       fact.ProviderTradeNo,
		ProviderPaidAt:        formatShanghai(paidAt),
		SettlementErrorCode:   reasonCode,
		SettlementAttempts:    fact.ReconcileAttempts,
		LotCount:              lotCount,
		AllocationCount:       allocationCount,
		IssueTransactionCount: issueCount,
	}
	business := issuanceCompensationRefund{
		ID:                  businessID,
		WineTicketRefundNo:  businessNo,
		PurchaseID:          purchase.ID,
		CustomerID:          purchase.CustomerID,
		CurrentRefundID:     commonID,
		RefundKind:          RefundKindIssueCompensation,
		Amount:              fact.Amount,
		Currency:            fact.Currency,
		ReasonCode:          "system_issuance_failed",
		ReasonText:          stringPointer("支付成功但酒票已无法安全发放，系统自动原路退款"),
		EligibilitySnapshot: refundJSON(eligibility),
		Status:              RefundStatusHolding,
		Version:             1,
		RequestedAt:         now,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	bizType, bizID := WineTicketPurchaseRefundBusiness, businessID
	common := commonRefundRow{
		ID:          commonID,
		PaymentID:   fact.PaymentID,
		RefundNo:    commonNo,
		BizType:     &bizType,
		BizID:       &bizID,
		Provider:    fact.Provider,
		Status:      "creating",
		Currency:    fact.Currency,
		Reason:      "酒票发放失败自动退款",
		Amount:      fact.Amount,
		TotalAmount: fact.Amount,
		RequestedAt: now,
		NextRetryAt: timePtr(now),
		Version:     1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if strings.TrimSpace(common.Provider) == "" {
		common.Provider = "wechat"
	}
	if err := s.repo.createIssuanceCompensationRefund(
		ctx,
		tx,
		&business,
		&common,
	); err != nil {
		return err
	}
	if err := s.repo.UpdatePurchaseQuota(ctx, tx, quota.ID, map[string]any{
		"reserved_quantity": quota.ReservedQuantity - purchase.PackageQuantity,
		"consumed_quantity": quota.ConsumedQuantity + purchase.PackageQuantity,
		"version":           gorm.Expr("version + 1"),
		"updated_at":        now,
	}); err != nil {
		return err
	}
	if err := s.repo.UpdatePurchase(ctx, tx, purchase.ID, map[string]any{
		"status":      PurchaseStatusRefundHolding,
		"paid_amount": fact.Amount,
		"paid_at":     paidAt,
		"version":     gorm.Expr("version + 1"),
		"updated_at":  now,
	}); err != nil {
		return err
	}
	if err := s.createSettlementAudit(
		ctx,
		tx,
		"wine_ticket.purchase.issuance_compensation",
		purchase.ID,
		purchase.Status,
		PurchaseStatusRefundHolding,
		map[string]any{
			"purchase_no": purchase.PurchaseNo,
			"refund_no":   businessNo,
			"reason_code": reasonCode,
			"amount":      fact.Amount,
		},
	); err != nil {
		return err
	}
	return s.createWineTicketOutbox(
		ctx,
		tx,
		"wine_ticket.refund_created",
		"wine_ticket_refund",
		businessID,
		map[string]any{
			"refund_no":   businessNo,
			"purchase_no": purchase.PurchaseNo,
			"customer_id": idString(purchase.CustomerID),
			"status":      RefundStatusHolding,
		},
	)
}

func (s *Service) ApplyTerminal(ctx context.Context, tx *gorm.DB, fact order.PaymentSettlementFact) error {
	purchase, quota, _, err := s.lockedSettlementRows(ctx, tx, fact)
	if err != nil {
		return err
	}
	if purchase.Status == PurchaseStatusIssued || purchase.Status == PurchaseStatusClosed {
		return nil
	}
	switch purchase.Status {
	case PurchaseStatusPendingPayment, PurchaseStatusPaymentUnknown, PurchaseStatusSettlementException:
	default:
		return nil
	}
	if quota.ReservedQuantity < purchase.PackageQuantity {
		return problem.Internal("wine ticket purchase quota reservation is inconsistent")
	}
	now := s.nowShanghai()
	if err := s.repo.UpdatePurchaseQuota(ctx, tx, quota.ID, map[string]any{
		"reserved_quantity": quota.ReservedQuantity - purchase.PackageQuantity,
		"version":           gorm.Expr("version + 1"), "updated_at": now,
	}); err != nil {
		return err
	}
	if err := s.repo.UpdatePurchase(ctx, tx, purchase.ID, map[string]any{
		"status": PurchaseStatusClosed, "version": gorm.Expr("version + 1"), "updated_at": now,
	}); err != nil {
		return err
	}
	return s.createSettlementAudit(ctx, tx, "wine_ticket.purchase.close", purchase.ID, purchase.Status, PurchaseStatusClosed, map[string]any{
		"purchase_no": purchase.PurchaseNo, "provider_status": fact.ProviderStatus,
	})
}

func (s *Service) ApplyException(ctx context.Context, tx *gorm.DB, fact order.PaymentSettlementFact, reason string) error {
	purchase, _, _, err := s.lockedSettlementRows(ctx, tx, fact)
	if err != nil {
		return err
	}
	switch purchase.Status {
	case PurchaseStatusIssued,
		PurchaseStatusClosed,
		PurchaseStatusRefundHolding,
		PurchaseStatusRefundException,
		PurchaseStatusRefunded,
		PurchaseStatusSettlementException:
		// 权益或退款状态机进入后续状态后，延迟到达的回调或查询失败只属于补充证据，
		// 不得回退已成功发放或处于处理中、终态的退款。
		return nil
	case PurchaseStatusPendingPayment, PurchaseStatusPaymentUnknown:
		// 只有这些状态允许由结算失败继续推进。
	default:
		return problem.Internal("wine ticket purchase has an invalid payment settlement state")
	}
	now := s.nowShanghai()
	if err := s.repo.UpdatePurchase(ctx, tx, purchase.ID, map[string]any{
		"status":  PurchaseStatusSettlementException,
		"version": gorm.Expr("version + 1"), "updated_at": now,
	}); err != nil {
		return err
	}
	return s.createSettlementAudit(ctx, tx, "wine_ticket.purchase.settlement_exception", purchase.ID, purchase.Status, PurchaseStatusSettlementException, map[string]any{
		"purchase_no": purchase.PurchaseNo, "reason_code": reason,
		"provider_status": fact.ProviderStatus,
	})
}

func (s *Service) lockedSettlementRows(
	ctx context.Context,
	tx *gorm.DB,
	fact order.PaymentSettlementFact,
) (Purchase, PurchaseQuota, purchasePackageSnapshot, error) {
	if fact.BizType != PurchasePaymentBusiness || fact.BizID == 0 {
		return Purchase{}, PurchaseQuota{}, purchasePackageSnapshot{}, problem.Internal("wine ticket payment business link is invalid")
	}
	purchase, err := s.repo.LockPurchaseByID(ctx, tx, fact.BizID)
	if err != nil {
		return Purchase{}, PurchaseQuota{}, purchasePackageSnapshot{}, err
	}
	if purchase.PaymentID != fact.PaymentID || purchase.CustomerID != fact.CustomerID ||
		purchase.PayableAmount != fact.Amount || purchase.Currency != fact.Currency {
		return Purchase{}, PurchaseQuota{}, purchasePackageSnapshot{}, problem.Conflict("PAYMENT_PROVIDER_DATA_MISMATCH", "payment fact does not match wine ticket purchase")
	}
	snapshot, err := parsePurchaseSnapshot(purchase.PackageSnapshot)
	if err != nil {
		return Purchase{}, PurchaseQuota{}, purchasePackageSnapshot{}, err
	}
	quota, err := s.repo.LockPurchaseQuota(ctx, tx, purchase.CustomerID, snapshot.PackageCode)
	if err != nil {
		return Purchase{}, PurchaseQuota{}, purchasePackageSnapshot{}, err
	}
	return purchase, quota, snapshot, nil
}

func (s *Service) verifyIssuedPurchase(ctx context.Context, tx *gorm.DB, purchase Purchase) error {
	lot, err := s.repo.PurchaseLot(ctx, tx, purchase.ID)
	if err != nil {
		return problem.Internal("issued wine ticket purchase has no lot")
	}
	entry, err := s.repo.TransactionByActionKey(ctx, tx, purchaseIssueActionKey(purchase.ID, lot.ID))
	if err != nil {
		return problem.Internal("issued wine ticket purchase has no issuance transaction")
	}
	snapshot, err := parsePurchaseSnapshot(purchase.PackageSnapshot)
	if err != nil || purchase.PaidAt == nil {
		return problem.Internal("issued wine ticket purchase lineage is incomplete")
	}
	expectedOriginalExpiry := purchase.PaidAt.
		In(shanghaiLocation).
		Truncate(time.Millisecond).
		AddDate(0, 0, int(snapshot.ValidityDays))
	if !validPurchaseIssueEvidence(
		purchase,
		lot,
		entry,
		expectedOriginalExpiry,
	) {
		return problem.Internal("issued wine ticket purchase facts are inconsistent")
	}
	return nil
}

func validPurchaseIssueEvidence(
	purchase Purchase,
	lot core.Lot,
	entry core.Transaction,
	expectedOriginalExpiry time.Time,
) bool {
	return lot.OwnerCustomerID == purchase.CustomerID &&
		lot.PurchaseID == purchase.ID &&
		lot.SourceType == LotSourcePurchase &&
		lot.SourceLotID == nil &&
		lot.SourceGiftID == nil &&
		lot.IssuerMerchantID == purchase.IssuerMerchantID &&
		lot.ProductID == purchase.ProductID &&
		lot.RedeemCityCode == purchase.RedeemCityCode &&
		lot.TotalQuantity == purchase.TotalBottleQuantity &&
		lot.OriginalExpiresAt.Equal(expectedOriginalExpiry) &&
		entry.LotID == lot.ID &&
		entry.OwnerCustomerID == purchase.CustomerID &&
		entry.TransactionType == TransactionTypePurchaseIssue &&
		entry.QuantityDelta == int(purchase.TotalBottleQuantity) &&
		entry.BeforeAvailableQuantity == 0 &&
		entry.AfterAvailableQuantity == purchase.TotalBottleQuantity &&
		entry.BizType == "purchase" &&
		entry.BizID == purchase.ID &&
		entry.ActionKey == purchaseIssueActionKey(purchase.ID, lot.ID)
}

func (c *serviceCore) createSettlementAudit(
	ctx context.Context,
	tx *gorm.DB,
	action string,
	purchaseID uint64,
	fromStatus, toStatus string,
	after any,
) error {
	version := uint64(1)
	return c.repo.CreateAudit(ctx, tx, map[string]any{
		"id": c.ids.Next(), "actor_type": "system", "actor_id": 0,
		"action": action, "resource_type": "wine_ticket_purchase",
		"resource_id": purchaseID, "after_data": jsonData(after), "result": "success",
		"before_status": fromStatus, "after_status": toStatus, "version": version,
		"request_id": requestctx.RequestIDPtr(ctx),
	})
}

func purchaseIssueActionKey(purchaseID, lotID uint64) string {
	return fmt.Sprintf("purchase_issue:%d:%d", purchaseID, lotID)
}
