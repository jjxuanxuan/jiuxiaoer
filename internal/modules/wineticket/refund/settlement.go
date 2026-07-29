package refund

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	sharedrefund "jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	purchasedomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/purchase"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// WineTicketRefundSettlement 在一个共享退款事务中闭环公共资金和酒票权益事实。
// 即使客户退款路由关闭，它仍会保持注册，确保处理中的回调可以继续路由。
type WineTicketRefundSettlement struct {
	repo *refundRepository
	core *serviceCore
	ids  *snowflake.Generator
	now  func() time.Time
}

var (
	_ sharedrefund.RefundSettlementHandler        = (*WineTicketRefundSettlement)(nil)
	_ sharedrefund.RefundSettlementFailureHandler = (*WineTicketRefundSettlement)(nil)
)

func NewWineTicketRefundSettlement(
	db *gorm.DB,
	ids *snowflake.Generator,
) *WineTicketRefundSettlement {
	repo := newRefundRepository(db)
	return &WineTicketRefundSettlement{
		repo: repo, core: newServiceCore(repo, ids), ids: ids, now: time.Now,
	}
}

func (h *WineTicketRefundSettlement) WithNow(now func() time.Time) *WineTicketRefundSettlement {
	if now != nil {
		h.now = now
		h.core.setClock(now)
	}
	return h
}

func (h *WineTicketRefundSettlement) BusinessType() string {
	return WineTicketPurchaseRefundBusiness
}

func (h *WineTicketRefundSettlement) LockAndApply(
	ctx context.Context,
	tx *gorm.DB,
	command sharedrefund.RefundSettlementCommand,
) (sharedrefund.RefundSettlementResult, error) {
	if h == nil || h.repo == nil || h.core == nil || h.ids == nil || tx == nil {
		return sharedrefund.RefundSettlementResult{}, problem.Internal("wine ticket refund settlement dependencies are unavailable")
	}
	bizType, businessID := sharedrefund.RefundBusiness(command.Lookup)
	if bizType != WineTicketPurchaseRefundBusiness || businessID == 0 {
		return refundSettlementReject("REFUND_BUSINESS_LINK_INVALID", "wine ticket refund business link is invalid"), nil
	}

	// 现有或替代退款的结算锁顺序与首次申请有意不同：
	// 全部公共退款记录（按 ID 升序）-> 业务退款 -> 购买记录 ->
	// 原始批次（按 ID 升序）-> 分配记录（按 ID 升序）-> 支付。
	commonRows, err := h.repo.lockCommonRefunds(ctx, tx, businessID)
	if err != nil {
		return sharedrefund.RefundSettlementResult{}, err
	}
	target, found := commonRefundByID(commonRows, command.Lookup.ID)
	if !found {
		return refundSettlementReject("REFUND_SETTLEMENT_ROUTE_CHANGED", "common refund is no longer linked to this wine ticket refund"), nil
	}
	if command.ClaimedVersion != nil && target.Version != *command.ClaimedVersion {
		return sharedrefund.RefundSettlementResult{}, nil
	}
	if !sameRefundRoute(command.Lookup, target) {
		return refundSettlementReject("REFUND_SETTLEMENT_ROUTE_CHANGED", "refund settlement route changed while locking"), nil
	}
	business, err := h.repo.lockBusinessRefund(ctx, tx, businessID)
	if err != nil {
		return sharedrefund.RefundSettlementResult{}, err
	}
	currentID, err := currentRefundTip(commonRows, business.CurrentRefundID)
	if err != nil {
		return sharedrefund.RefundSettlementResult{}, err
	}
	purchase, err := h.repo.lockPurchaseByID(ctx, tx, business.PurchaseID)
	if err != nil {
		return sharedrefund.RefundSettlementResult{}, err
	}
	lots, err := h.repo.originalLots(ctx, tx, purchase.ID, true)
	if err != nil {
		return sharedrefund.RefundSettlementResult{}, err
	}
	allocations, err := h.repo.lockAllocations(ctx, tx, business.ID)
	if err != nil {
		return sharedrefund.RefundSettlementResult{}, err
	}
	payment, err := h.repo.paymentByID(ctx, tx, target.PaymentID, true)
	if err != nil {
		return sharedrefund.RefundSettlementResult{}, err
	}

	incoming := strings.ToUpper(strings.TrimSpace(command.State.Status))
	if code, detail := validateWineRefundProviderFact(
		command.State, target, business, purchase, payment,
	); code != "" {
		if err := h.markProviderMismatch(
			ctx, tx, target, business, purchase, currentID, code, detail,
		); err != nil {
			return sharedrefund.RefundSettlementResult{}, err
		}
		return refundSettlementReject(code, detail), nil
	}
	if result, terminal := validateWineRefundTerminalReplay(
		target, business, incoming, currentID,
	); terminal {
		return result, nil
	}

	switch incoming {
	case "PROCESSING":
		if target.ID != currentID {
			return refundSettlementReject("REFUND_STATUS_REGRESSION", "an obsolete refund attempt cannot become processing"), nil
		}
		return sharedrefund.RefundSettlementResult{}, h.applyProcessing(
			ctx, tx, target, business, purchase, command.State, currentID,
		)
	case "CLOSED":
		return sharedrefund.RefundSettlementResult{}, h.applyClosed(
			ctx, tx, target, business, purchase, command.State, currentID,
		)
	case "ABNORMAL":
		if target.ID != currentID {
			return sharedrefund.RefundSettlementResult{}, h.updateCommonAbnormalOnly(
				ctx, tx, target, command.State,
			)
		}
		return sharedrefund.RefundSettlementResult{}, h.applyAbnormal(
			ctx, tx, target, business, purchase, command.State, currentID,
		)
	case "SUCCESS":
		if target.ID != currentID {
			return refundSettlementReject("REFUND_STATUS_REGRESSION", "an obsolete refund attempt cannot become successful"), nil
		}
		if err := validateEntitlementClosure(
			business,
			purchase,
			lots,
			allocations,
			func() (int64, error) {
				return h.repo.purchaseIssueTransactionCount(
					ctx,
					tx,
					purchase.ID,
				)
			},
		); err != nil {
			if markErr := h.markProviderMismatch(
				ctx, tx, target, business, purchase, currentID,
				"REFUND_ENTITLEMENT_MISMATCH", err.Error(),
			); markErr != nil {
				return sharedrefund.RefundSettlementResult{}, markErr
			}
			return refundSettlementReject("REFUND_ENTITLEMENT_MISMATCH", "refund entitlement facts do not match the held purchase"), nil
		}
		return sharedrefund.RefundSettlementResult{}, h.applySuccess(
			ctx, tx, target, business, purchase, payment, lots, allocations,
			command.State, currentID,
		)
	default:
		if err := h.markProviderMismatch(
			ctx, tx, target, business, purchase, currentID,
			"REFUND_PROVIDER_STATUS_INVALID", "unsupported provider refund status",
		); err != nil {
			return sharedrefund.RefundSettlementResult{}, err
		}
		return refundSettlementReject("REFUND_PROVIDER_STATUS_INVALID", "unsupported provider refund status"), nil
	}
}

// LockAndApplyFailure 在后台任务遇到传输或永久错误时，
// 仍沿用现有退款的锁计划。两个分支都不会恢复分配记录，
// 也不会让已冻结批次重新可用。
func (h *WineTicketRefundSettlement) LockAndApplyFailure(
	ctx context.Context,
	tx *gorm.DB,
	command sharedrefund.RefundSettlementFailureCommand,
) error {
	if h == nil || h.repo == nil || h.core == nil || h.ids == nil || tx == nil {
		return problem.Internal("wine ticket refund settlement dependencies are unavailable")
	}
	bizType, businessID := sharedrefund.RefundBusiness(command.Lookup)
	if bizType != WineTicketPurchaseRefundBusiness || businessID == 0 {
		return problem.Internal("wine ticket refund failure business link is invalid")
	}
	commonRows, err := h.repo.lockCommonRefunds(ctx, tx, businessID)
	if err != nil {
		return err
	}
	target, found := commonRefundByID(commonRows, command.Lookup.ID)
	if !found || !sameRefundRoute(command.Lookup, target) {
		return problem.Internal("wine ticket refund failure route changed while locking")
	}
	if target.Version != command.ClaimedVersion ||
		!workerRefundFailureApplicable(target.Status) {
		return nil
	}
	business, err := h.repo.lockBusinessRefund(ctx, tx, businessID)
	if err != nil {
		return err
	}
	currentID, err := currentRefundTip(commonRows, business.CurrentRefundID)
	if err != nil {
		return err
	}
	if target.ID != currentID {
		return nil
	}
	purchase, err := h.repo.lockPurchaseByID(ctx, tx, business.PurchaseID)
	if err != nil {
		return err
	}
	lots, err := h.repo.originalLots(ctx, tx, purchase.ID, true)
	if err != nil {
		return err
	}
	allocations, err := h.repo.lockAllocations(ctx, tx, business.ID)
	if err != nil {
		return err
	}
	payment, err := h.repo.paymentByID(ctx, tx, target.PaymentID, true)
	if err != nil {
		return err
	}
	if err := validateWineRefundLocalLinks(
		business, purchase, target, payment,
	); err != nil {
		return err
	}
	if err := validateEntitlementClosure(
		business,
		purchase,
		lots,
		allocations,
		func() (int64, error) {
			return h.repo.purchaseIssueTransactionCount(
				ctx,
				tx,
				purchase.ID,
			)
		},
	); err != nil {
		return problem.Internal("wine ticket refund hold is inconsistent during provider failure")
	}

	occurredAt := command.OccurredAt.In(shanghaiLocation).Truncate(time.Millisecond)
	if command.Retryable {
		if command.NextRetryAt == nil {
			return problem.Internal("retryable refund failure has no next retry time")
		}
		commonStatus := target.Status
		businessStatus := business.Status
		if target.Status == "creating" {
			commonStatus = "submission_unknown"
			businessStatus = RefundStatusSubmissionUnknown
		}
		if err := h.repo.updateCommonRefund(ctx, tx, target, map[string]any{
			"status": commonStatus, "next_retry_at": *command.NextRetryAt,
			"locked_by": nil, "locked_until": nil,
			"failure_code": command.Code, "failure_detail": command.Detail,
		}); err != nil {
			return err
		}
		if business.CurrentRefundID != currentID || business.Status != businessStatus {
			if err := h.repo.updateBusinessRefund(ctx, tx, business, map[string]any{
				"current_refund_id": currentID, "status": businessStatus,
				"updated_at": occurredAt,
			}); err != nil {
				return err
			}
		}
		// 之前的异常只能通过受控的公共重试重新进入冻结中状态，
		// 且仍不得释放批次冻结。
		if purchase.Status == PurchaseStatusRefundException {
			return h.repo.updatePurchaseVersioned(ctx, tx, purchase, map[string]any{
				"status": PurchaseStatusRefundHolding, "updated_at": occurredAt,
			})
		}
		return nil
	}

	if err := h.repo.updateCommonRefund(ctx, tx, target, map[string]any{
		"status": "exception", "next_retry_at": nil,
		"locked_by": nil, "locked_until": nil, "failed_at": occurredAt,
		"failure_code": command.Code, "failure_detail": command.Detail,
	}); err != nil {
		return err
	}
	if err := h.repo.updateBusinessRefund(ctx, tx, business, map[string]any{
		"current_refund_id": currentID, "status": RefundStatusException,
		"updated_at": occurredAt,
	}); err != nil {
		return err
	}
	if purchase.Status != PurchaseStatusRefundException {
		if err := h.repo.updatePurchaseVersioned(ctx, tx, purchase, map[string]any{
			"status": PurchaseStatusRefundException, "updated_at": occurredAt,
		}); err != nil {
			return err
		}
	}
	return h.core.createWineTicketOutbox(
		ctx, tx, "wine_ticket.refund_exception", "wine_ticket_refund", business.ID,
		map[string]any{
			"refund_no":   business.WineTicketRefundNo,
			"purchase_no": purchase.PurchaseNo, "error_code": command.Code,
		},
	)
}

func (h *WineTicketRefundSettlement) applyProcessing(
	ctx context.Context,
	tx *gorm.DB,
	common commonRefundRow,
	business WineTicketRefund,
	purchase purchasedomain.Purchase,
	state sharedrefund.State,
	currentID uint64,
) error {
	now := h.now().In(shanghaiLocation).Truncate(time.Millisecond)
	next := now.Add(time.Minute)
	values := providerCommonValues(state)
	values["status"] = "pending"
	values["next_retry_at"] = next
	values["locked_by"] = nil
	values["locked_until"] = nil
	values["failure_code"] = nil
	values["failure_detail"] = nil
	if err := h.repo.updateCommonRefund(ctx, tx, common, values); err != nil {
		return err
	}
	if err := h.repo.updateBusinessRefund(ctx, tx, business, map[string]any{
		"current_refund_id": currentID,
		"status":            RefundStatusProcessing,
		"updated_at":        now,
	}); err != nil {
		return err
	}
	if purchase.Status == PurchaseStatusRefundException {
		return h.repo.updatePurchaseVersioned(ctx, tx, purchase, map[string]any{
			"status": PurchaseStatusRefundHolding, "updated_at": now,
		})
	}
	return nil
}

func (h *WineTicketRefundSettlement) applyClosed(
	ctx context.Context,
	tx *gorm.DB,
	common commonRefundRow,
	business WineTicketRefund,
	purchase purchasedomain.Purchase,
	state sharedrefund.State,
	currentID uint64,
) error {
	now := h.now().In(shanghaiLocation).Truncate(time.Millisecond)
	values := providerCommonValues(state)
	values["status"] = "failed"
	values["failed_at"] = now
	values["failure_code"] = "CLOSED"
	values["failure_detail"] = nil
	values["next_retry_at"] = nil
	values["locked_by"] = nil
	values["locked_until"] = nil
	if err := h.repo.updateCommonRefund(ctx, tx, common, values); err != nil {
		return err
	}
	if common.ID != currentID {
		return nil
	}
	replacementID := h.ids.Next()
	bizType, bizID := WineTicketPurchaseRefundBusiness, business.ID
	replacement := commonRefundRow{
		ID:               replacementID,
		PaymentID:        common.PaymentID,
		ReplacesRefundID: &common.ID,
		RefundNo:         "WTRFC" + idString(replacementID),
		BizType:          &bizType,
		BizID:            &bizID,
		Provider:         common.Provider,
		Status:           "creating",
		Currency:         common.Currency,
		Reason:           common.Reason,
		NotifyURL:        common.NotifyURL,
		Amount:           common.Amount,
		TotalAmount:      common.TotalAmount,
		NextRetryAt:      &now,
		RequestedAt:      now,
		Version:          1,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := h.repo.createCommonRefund(ctx, tx, &replacement); err != nil {
		return err
	}
	if err := h.repo.updateBusinessRefund(ctx, tx, business, map[string]any{
		"current_refund_id": replacementID,
		"status":            RefundStatusSubmitting,
		"updated_at":        now,
	}); err != nil {
		return err
	}
	if purchase.Status == PurchaseStatusRefundException {
		if err := h.repo.updatePurchaseVersioned(ctx, tx, purchase, map[string]any{
			"status": PurchaseStatusRefundHolding, "updated_at": now,
		}); err != nil {
			return err
		}
	}
	return h.core.createWineTicketOutbox(
		ctx, tx, "wine_ticket.refund_retry_pending", "wine_ticket_refund", business.ID,
		map[string]any{
			"refund_no": business.WineTicketRefundNo, "purchase_no": purchase.PurchaseNo,
			"common_refund_id":        idString(replacementID),
			"closed_common_refund_id": idString(common.ID),
			"replacement_refund_id":   idString(replacementID),
			"replacement_refund_no":   replacement.RefundNo,
		},
	)
}

func (h *WineTicketRefundSettlement) applyAbnormal(
	ctx context.Context,
	tx *gorm.DB,
	common commonRefundRow,
	business WineTicketRefund,
	purchase purchasedomain.Purchase,
	state sharedrefund.State,
	currentID uint64,
) error {
	now := h.now().In(shanghaiLocation).Truncate(time.Millisecond)
	if err := h.updateCommonAbnormalOnly(ctx, tx, common, state); err != nil {
		return err
	}
	if err := h.repo.updateBusinessRefund(ctx, tx, business, map[string]any{
		"current_refund_id": currentID,
		"status":            RefundStatusException,
		"updated_at":        now,
	}); err != nil {
		return err
	}
	if purchase.Status != PurchaseStatusRefundException {
		if err := h.repo.updatePurchaseVersioned(ctx, tx, purchase, map[string]any{
			"status": PurchaseStatusRefundException, "updated_at": now,
		}); err != nil {
			return err
		}
	}
	return h.core.createWineTicketOutbox(
		ctx, tx, "wine_ticket.refund_exception", "wine_ticket_refund", business.ID,
		map[string]any{
			"refund_no":   business.WineTicketRefundNo,
			"purchase_no": purchase.PurchaseNo, "provider_status": "ABNORMAL",
		},
	)
}

func (h *WineTicketRefundSettlement) updateCommonAbnormalOnly(
	ctx context.Context,
	tx *gorm.DB,
	common commonRefundRow,
	state sharedrefund.State,
) error {
	now := h.now().In(shanghaiLocation).Truncate(time.Millisecond)
	values := providerCommonValues(state)
	values["status"] = "exception"
	values["failed_at"] = now
	values["failure_code"] = "ABNORMAL"
	values["failure_detail"] = nil
	values["next_retry_at"] = nil
	values["locked_by"] = nil
	values["locked_until"] = nil
	return h.repo.updateCommonRefund(ctx, tx, common, values)
}

func (h *WineTicketRefundSettlement) applySuccess(
	ctx context.Context,
	tx *gorm.DB,
	common commonRefundRow,
	business WineTicketRefund,
	purchase purchasedomain.Purchase,
	payment refundPayment,
	lots []core.Lot,
	allocations []RefundAllocation,
	state sharedrefund.State,
	currentID uint64,
) error {
	now := h.now().In(shanghaiLocation).Truncate(time.Millisecond)
	if state.SucceededAt != nil {
		now = state.SucceededAt.In(shanghaiLocation).Truncate(time.Millisecond)
	}
	if business.RefundKind == RefundKindUserUnused {
		for _, allocation := range allocations {
			updated, err := h.repo.consumeHeldAllocation(
				ctx,
				tx,
				allocation.ID,
				now,
			)
			if err != nil {
				return err
			}
			if !updated {
				return problem.Conflict("WT_CONCURRENT_MODIFICATION", "refund allocation changed concurrently")
			}
		}
		for _, lot := range lots {
			if err := h.repo.updateLotVersioned(ctx, tx, lot, map[string]any{
				"status": LotStatusRefunded, "updated_at": now,
			}); err != nil {
				return err
			}
		}
	}
	paymentUpdated, err := h.repo.incrementPaymentRefundedAmount(
		ctx,
		tx,
		payment.ID,
		common.Amount,
		now,
	)
	if err != nil {
		return err
	}
	if !paymentUpdated {
		return problem.Conflict("REFUND_AMOUNT_EXCEEDED", "refund exceeds the original payment")
	}
	if err := h.repo.updatePurchaseVersioned(ctx, tx, purchase, map[string]any{
		"status": PurchaseStatusRefunded, "refunded_at": now, "updated_at": now,
	}); err != nil {
		return err
	}
	if err := h.repo.updateBusinessRefund(ctx, tx, business, map[string]any{
		"current_refund_id": currentID,
		"status":            RefundStatusSucceeded,
		"succeeded_at":      now,
		"updated_at":        now,
	}); err != nil {
		return err
	}
	commonValues := providerCommonValues(state)
	commonValues["status"] = "succeeded"
	commonValues["succeeded_at"] = now
	commonValues["next_retry_at"] = nil
	commonValues["locked_by"] = nil
	commonValues["locked_until"] = nil
	commonValues["failure_code"] = nil
	commonValues["failure_detail"] = nil
	if err := h.repo.updateCommonRefund(ctx, tx, common, commonValues); err != nil {
		return err
	}
	if err := h.core.createSettlementAudit(
		ctx, tx, "wine_ticket.refund.succeed", purchase.ID,
		purchase.Status, PurchaseStatusRefunded,
		map[string]any{
			"refund_no":   business.WineTicketRefundNo,
			"purchase_no": purchase.PurchaseNo, "amount": common.Amount,
			"refund_kind": business.RefundKind,
		},
	); err != nil {
		return err
	}
	return h.core.createWineTicketOutbox(
		ctx, tx, "wine_ticket.refund_succeeded", "wine_ticket_refund", business.ID,
		map[string]any{
			"refund_no":   business.WineTicketRefundNo,
			"purchase_no": purchase.PurchaseNo,
			"customer_id": idString(business.CustomerID), "amount": common.Amount,
		},
	)
}

func (h *WineTicketRefundSettlement) markProviderMismatch(
	ctx context.Context,
	tx *gorm.DB,
	common commonRefundRow,
	business WineTicketRefund,
	purchase purchasedomain.Purchase,
	currentID uint64,
	code, detail string,
) error {
	if len(detail) > 500 {
		detail = detail[:500]
	}
	now := h.now().In(shanghaiLocation).Truncate(time.Millisecond)
	if err := h.repo.updateCommonRefund(ctx, tx, common, map[string]any{
		"status": "exception", "failure_code": code, "failure_detail": detail,
		"failed_at": now, "next_retry_at": nil, "locked_by": nil, "locked_until": nil,
	}); err != nil {
		return err
	}
	if common.ID != currentID || business.Status == RefundStatusSucceeded ||
		business.Status == RefundStatusCancelled {
		return nil
	}
	if err := h.repo.updateBusinessRefund(ctx, tx, business, map[string]any{
		"current_refund_id": currentID,
		"status":            RefundStatusException,
		"updated_at":        now,
	}); err != nil {
		return err
	}
	if purchase.Status != PurchaseStatusRefundException {
		if err := h.repo.updatePurchaseVersioned(ctx, tx, purchase, map[string]any{
			"status": PurchaseStatusRefundException, "updated_at": now,
		}); err != nil {
			return err
		}
	}
	return h.core.createWineTicketOutbox(
		ctx, tx, "wine_ticket.refund_exception", "wine_ticket_refund", business.ID,
		map[string]any{
			"refund_no":   business.WineTicketRefundNo,
			"purchase_no": purchase.PurchaseNo, "error_code": code,
		},
	)
}

func validateWineRefundProviderFact(
	state sharedrefund.State,
	common commonRefundRow,
	business WineTicketRefund,
	purchase purchasedomain.Purchase,
	payment refundPayment,
) (string, string) {
	currencyMismatch := state.Currency != "" && state.Currency != common.Currency
	if state.CurrencyRequired && state.Currency == "" {
		currencyMismatch = true
	}
	if state.RefundNo != common.RefundNo ||
		state.Amount != common.Amount || state.TotalAmount != common.TotalAmount ||
		currencyMismatch {
		return "REFUND_AMOUNT_MISMATCH", "provider refund amount, currency, or refund number does not match the local reservation"
	}
	if common.ProviderRefundID != nil && *common.ProviderRefundID != "" &&
		state.ProviderRefundID != "" && *common.ProviderRefundID != state.ProviderRefundID {
		return "REFUND_PROVIDER_ID_MISMATCH", "provider refund id changed"
	}
	if state.PaymentNo != payment.PaymentNo {
		return "REFUND_PAYMENT_MISMATCH", "provider payment number does not match the original payment"
	}
	if common.PaymentID != payment.ID || common.Amount != business.Amount ||
		common.Currency != business.Currency || business.PurchaseID != purchase.ID ||
		purchase.PaymentID != payment.ID || purchase.CustomerID != business.CustomerID ||
		payment.BizType == nil || *payment.BizType != PurchasePaymentBusiness ||
		payment.BizID == nil || *payment.BizID != purchase.ID ||
		payment.OrderID != nil || payment.CustomerID != business.CustomerID ||
		payment.Provider != "wechat" || payment.Status != "succeeded" ||
		payment.Currency != common.Currency || payment.Amount != common.TotalAmount ||
		purchase.PaidAmount != payment.Amount || common.AfterSaleID != nil ||
		common.OrderID != nil {
		return "REFUND_BUSINESS_LINK_INVALID", "local payment and wine ticket refund links are inconsistent"
	}
	return "", ""
}

func validateWineRefundLocalLinks(
	business WineTicketRefund,
	purchase purchasedomain.Purchase,
	common commonRefundRow,
	payment refundPayment,
) error {
	if common.PaymentID != payment.ID || common.Amount != business.Amount ||
		common.Currency != business.Currency || business.PurchaseID != purchase.ID ||
		purchase.PaymentID != payment.ID || purchase.CustomerID != business.CustomerID ||
		payment.BizType == nil || *payment.BizType != PurchasePaymentBusiness ||
		payment.BizID == nil || *payment.BizID != purchase.ID ||
		payment.OrderID != nil || payment.CustomerID != business.CustomerID ||
		payment.Provider != "wechat" || payment.Status != "succeeded" ||
		payment.Currency != common.Currency || payment.Amount != common.TotalAmount ||
		purchase.PaidAmount != payment.Amount || common.AfterSaleID != nil ||
		common.OrderID != nil {
		return problem.Internal("local payment and wine ticket refund links are inconsistent")
	}
	return nil
}

func validateEntitlementClosure(
	business WineTicketRefund,
	purchase purchasedomain.Purchase,
	lots []core.Lot,
	allocations []RefundAllocation,
	issueTransactionCount func() (int64, error),
) error {
	switch business.RefundKind {
	case RefundKindUserUnused:
		if len(lots) == 0 || len(allocations) != len(lots) {
			return errors.New("refund allocations do not cover original purchase lots")
		}
		byLot := make(map[uint64]RefundAllocation, len(allocations))
		var allocationTotal uint
		for _, allocation := range allocations {
			if allocation.Status != RefundAllocationHeld || allocation.Quantity == 0 {
				return errors.New("refund allocation is not held")
			}
			if _, duplicate := byLot[allocation.LotID]; duplicate {
				return errors.New("duplicate refund allocation")
			}
			byLot[allocation.LotID] = allocation
			allocationTotal += allocation.Quantity
		}
		var lotTotal uint
		for _, lot := range lots {
			allocation, ok := byLot[lot.ID]
			if !ok || allocation.Quantity != lot.TotalQuantity ||
				lot.OwnerCustomerID != business.CustomerID ||
				lot.PurchaseID != purchase.ID || lot.SourceType != LotSourcePurchase ||
				lot.AvailableQuantity != 0 || lot.Status != LotStatusDepleted ||
				!allocation.SourceExpiresAt.Equal(lot.ExpiresAt) {
				return errors.New("held lot no longer matches the refund allocation")
			}
			lotTotal += lot.TotalQuantity
		}
		if lotTotal != purchase.TotalBottleQuantity ||
			allocationTotal != purchase.TotalBottleQuantity {
			return errors.New("refund allocation quantity does not equal the purchase quantity")
		}
	case RefundKindIssueCompensation:
		if len(lots) != 0 || len(allocations) != 0 {
			return errors.New("issuance compensation must not have lots or refund allocations")
		}
		if issueTransactionCount == nil {
			return errors.New("issuance compensation issue facts are unavailable")
		}
		issueCount, err := issueTransactionCount()
		if err != nil {
			return err
		}
		if issueCount != 0 {
			return errors.New("issuance compensation purchase has an issue transaction")
		}
	default:
		return errors.New("unsupported wine ticket refund kind")
	}
	return nil
}

func validateWineRefundTerminalReplay(
	common commonRefundRow,
	business WineTicketRefund,
	incoming string,
	currentID uint64,
) (sharedrefund.RefundSettlementResult, bool) {
	switch common.Status {
	case "succeeded":
		if incoming == "SUCCESS" && business.Status == RefundStatusSucceeded &&
			business.CurrentRefundID == common.ID {
			return sharedrefund.RefundSettlementResult{}, true
		}
		return refundSettlementReject("REFUND_STATUS_REGRESSION", "provider refund status cannot regress after success"), true
	case "failed":
		if incoming == "CLOSED" {
			return sharedrefund.RefundSettlementResult{}, true
		}
		return refundSettlementReject("REFUND_STATUS_REGRESSION", "a CLOSED refund attempt cannot change terminal state"), true
	case "exception":
		if incoming == "ABNORMAL" &&
			(common.ID != currentID || business.Status == RefundStatusException) {
			return sharedrefund.RefundSettlementResult{}, true
		}
	}
	if common.Status == "pending" && incoming == "PROCESSING" &&
		common.ID == currentID && business.Status == RefundStatusProcessing &&
		business.CurrentRefundID == currentID {
		return sharedrefund.RefundSettlementResult{}, true
	}
	return sharedrefund.RefundSettlementResult{}, false
}

func commonRefundByID(rows []commonRefundRow, id uint64) (commonRefundRow, bool) {
	for _, row := range rows {
		if row.ID == id {
			return row, true
		}
	}
	return commonRefundRow{}, false
}

func currentRefundTip(rows []commonRefundRow, initial uint64) (uint64, error) {
	if initial == 0 {
		return 0, problem.Internal("wine ticket refund has no current common refund")
	}
	known := make(map[uint64]struct{}, len(rows))
	for _, row := range rows {
		known[row.ID] = struct{}{}
	}
	if _, ok := known[initial]; !ok {
		return 0, problem.Internal("wine ticket current common refund is missing")
	}
	current := initial
	visited := map[uint64]struct{}{current: {}}
	for {
		next := uint64(0)
		for _, row := range rows {
			if row.ReplacesRefundID == nil || *row.ReplacesRefundID != current {
				continue
			}
			if next != 0 {
				return 0, problem.Internal("common refund replacement chain is forked")
			}
			next = row.ID
		}
		if next == 0 {
			return current, nil
		}
		if _, duplicate := visited[next]; duplicate {
			return 0, problem.Internal("common refund replacement chain is cyclic")
		}
		visited[next] = struct{}{}
		current = next
	}
}

func sameRefundRoute(lookup sharedrefund.Row, locked commonRefundRow) bool {
	lookupType, lookupID := sharedrefund.RefundBusiness(lookup)
	lockedType := ""
	if locked.BizType != nil {
		lockedType = strings.TrimSpace(*locked.BizType)
	}
	lockedID := uint64(0)
	if locked.BizID != nil {
		lockedID = *locked.BizID
	}
	return lookup.ID == locked.ID && lookup.RefundNo == locked.RefundNo &&
		lookup.PaymentID == locked.PaymentID && lookupType == lockedType &&
		lookupID == lockedID
}

func providerCommonValues(state sharedrefund.State) map[string]any {
	values := map[string]any{
		"provider_status": strings.ToUpper(strings.TrimSpace(state.Status)),
	}
	if state.ProviderRefundID != "" {
		values["provider_refund_id"] = state.ProviderRefundID
	}
	if state.ProviderCreatedAt != nil {
		values["provider_accepted_at"] = state.ProviderCreatedAt.In(shanghaiLocation).Truncate(time.Millisecond)
	}
	return values
}

func refundSettlementReject(code, detail string) sharedrefund.RefundSettlementResult {
	return sharedrefund.RefundSettlementResult{
		Reject: problem.Conflict(code, detail), CallbackErrorCode: code,
	}
}

func workerRefundFailureApplicable(status string) bool {
	switch status {
	case "creating", "submission_unknown", "pending":
		return true
	default:
		return false
	}
}

func (h *WineTicketRefundSettlement) String() string {
	return fmt.Sprintf("WineTicketRefundSettlement(%s)", h.BusinessType())
}
