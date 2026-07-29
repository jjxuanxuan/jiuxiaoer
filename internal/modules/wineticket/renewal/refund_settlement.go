package renewal

import (
	"context"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

// RenewalRefundSettlementHandler 负责补偿流程的锁计划：
// 批次 -> 续期 -> 公共退款记录（按 ID 升序）-> 支付。
type RenewalRefundSettlementHandler struct {
	service *RenewalService
}

func NewRenewalRefundSettlementHandler(
	service *RenewalService,
) *RenewalRefundSettlementHandler {
	return &RenewalRefundSettlementHandler{service: service}
}

func (h *RenewalRefundSettlementHandler) BusinessType() string {
	return RenewalCompensationRefundBusiness
}

func (h *RenewalRefundSettlementHandler) LockAndApply(
	ctx context.Context,
	tx *gorm.DB,
	command refund.RefundSettlementCommand,
) (refund.RefundSettlementResult, error) {
	if h == nil || h.service == nil || tx == nil {
		return refund.RefundSettlementResult{}, problem.Internal(
			"renewal refund settlement is not configured",
		)
	}
	bizType, renewalID := refund.RefundBusiness(command.Lookup)
	if bizType != RenewalCompensationRefundBusiness || renewalID == 0 {
		return refund.RefundSettlementResult{}, problem.Internal(
			"renewal compensation refund route is invalid",
		)
	}

	lookupRenewal, err := h.service.repo.renewalByID(
		ctx,
		tx,
		renewalID,
		false,
	)
	if err != nil {
		return refund.RefundSettlementResult{}, err
	}
	lot, err := h.service.repo.lockLotByID(ctx, tx, lookupRenewal.LotID)
	if err != nil {
		return refund.RefundSettlementResult{}, err
	}
	renewal, err := h.service.repo.lockRenewalAfterLot(
		ctx,
		tx,
		renewalID,
		lot.ID,
	)
	if err != nil {
		return refund.RefundSettlementResult{}, err
	}
	refunds, err := h.service.repo.lockCompensationRefunds(
		ctx,
		tx,
		renewal.ID,
	)
	if err != nil {
		return refund.RefundSettlementResult{}, err
	}
	current, err := h.service.repo.compensationRefundByID(
		refunds,
		command.Lookup.ID,
	)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return refund.RefundSettlementResult{}, problem.Internal(
			"renewal compensation refund disappeared during settlement",
		)
	}
	if err != nil {
		return refund.RefundSettlementResult{}, err
	}
	if !refund.SameRefundSettlementRoute(command.Lookup, current) {
		return refund.RefundSettlementResult{}, problem.Internal(
			"renewal compensation refund route changed during settlement",
		)
	}
	if command.ClaimedVersion != nil && current.Version != *command.ClaimedVersion {
		return refund.RefundSettlementResult{}, nil
	}

	payment, err := h.service.repo.lockRefundPayment(
		ctx,
		tx,
		current.PaymentID,
	)
	if err != nil {
		return refund.RefundSettlementResult{}, err
	}

	incoming := strings.ToUpper(strings.TrimSpace(command.State.Status))
	mismatch := renewalRefundFactMismatch(
		renewal,
		current,
		payment,
		command.State,
	)
	if current.Status == "succeeded" ||
		renewal.Status == RenewalStatusRefunded ||
		renewal.Status == RenewalStatusCompleted {
		if current.Status == "succeeded" &&
			renewal.Status == RenewalStatusRefunded &&
			incoming == "SUCCESS" &&
			mismatch == "" {
			return refund.RefundSettlementResult{}, nil
		}
		return refund.RefundSettlementResult{
			Reject: problem.Conflict(
				"REFUND_STATUS_REGRESSION",
				"terminal renewal compensation cannot accept this provider observation",
			),
			CallbackErrorCode: "REFUND_STATUS_REGRESSION",
		}, nil
	}
	if mismatch != "" {
		if err := h.markRefundExceptionTx(
			ctx,
			tx,
			renewal,
			current,
			command.State,
			mismatch,
		); err != nil {
			return refund.RefundSettlementResult{}, err
		}
		return refund.RefundSettlementResult{
			Reject: problem.Conflict(
				"REFUND_AMOUNT_MISMATCH",
				"provider refund data does not match renewal compensation",
			),
			CallbackErrorCode: mismatch,
		}, nil
	}

	if renewal.CompensatingRefundID == nil ||
		*renewal.CompensatingRefundID != current.ID {
		if renewalRefundObservationIsIdempotent(current, incoming) {
			return refund.RefundSettlementResult{}, nil
		}
		return refund.RefundSettlementResult{
			Reject: problem.Conflict(
				"REFUND_STATUS_REGRESSION",
				"provider refund observation targets a superseded compensation",
			),
			CallbackErrorCode: "REFUND_STATUS_REGRESSION",
		}, nil
	}

	switch incoming {
	case "SUCCESS":
		return refund.RefundSettlementResult{}, h.applyRefundSuccessTx(
			ctx,
			tx,
			&lot,
			renewal,
			current,
			payment,
			command.State,
		)
	case "PROCESSING":
		return refund.RefundSettlementResult{}, h.applyRefundProcessingTx(
			ctx,
			tx,
			renewal,
			current,
			command.State,
		)
	case "CLOSED":
		return refund.RefundSettlementResult{}, h.replaceClosedRefundTx(
			ctx,
			tx,
			renewal,
			current,
			refunds,
			command.State,
		)
	case "ABNORMAL", "UNKNOWN":
		return refund.RefundSettlementResult{}, h.markRefundExceptionTx(
			ctx,
			tx,
			renewal,
			current,
			command.State,
			incoming,
		)
	default:
		return refund.RefundSettlementResult{
			Reject: problem.InvalidArgument(
				"REFUND_PROVIDER_STATUS_INVALID",
				"unsupported provider refund status",
			),
			CallbackErrorCode: "REFUND_PROVIDER_STATUS_INVALID",
		}, nil
	}
}

// LockAndApplyFailure 在不绕过续期保护标准锁顺序的前提下，
// 处理支付机构传输或提交失败。无论可重试还是永久失败，
// 都不会释放 active_lot_id 或变更批次权益。
func (h *RenewalRefundSettlementHandler) LockAndApplyFailure(
	ctx context.Context,
	tx *gorm.DB,
	command refund.RefundSettlementFailureCommand,
) error {
	if h == nil || h.service == nil || tx == nil {
		return problem.Internal("renewal refund settlement is not configured")
	}
	bizType, renewalID := refund.RefundBusiness(command.Lookup)
	if bizType != RenewalCompensationRefundBusiness || renewalID == 0 {
		return problem.Internal("renewal compensation refund failure route is invalid")
	}

	// 先无锁解析批次，再建立标准锁前缀：
	// 批次 -> 续期 -> 全部公共退款记录（按 ID 升序）-> 支付。
	lookupRenewal, err := h.service.repo.renewalByID(
		ctx, tx, renewalID, false,
	)
	if err != nil {
		return err
	}
	lot, err := h.service.repo.lockLotByID(ctx, tx, lookupRenewal.LotID)
	if err != nil {
		return err
	}
	renewal, err := h.service.repo.lockRenewalAfterLot(
		ctx, tx, renewalID, lot.ID,
	)
	if err != nil {
		return err
	}
	rows, err := h.service.repo.lockCompensationRefunds(ctx, tx, renewal.ID)
	if err != nil {
		return err
	}
	current, err := h.service.repo.compensationRefundByID(
		rows, command.Lookup.ID,
	)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.Internal("renewal compensation refund disappeared during failure handling")
	}
	if err != nil {
		return err
	}
	if !refund.SameRefundSettlementRoute(command.Lookup, current) {
		return problem.Internal("renewal compensation refund failure route changed while locking")
	}
	if current.Version != command.ClaimedVersion ||
		!renewalWorkerFailureApplicable(current.Status) {
		return nil
	}
	if renewal.CompensatingRefundID == nil ||
		*renewal.CompensatingRefundID != current.ID {
		// 已认领的过期尝试不得变更当前有效保护记录，
		// 后续状态归替代尝试或当前尝试负责。
		return nil
	}
	payment, err := h.service.repo.lockRefundPayment(
		ctx,
		tx,
		current.PaymentID,
	)
	if err != nil {
		return err
	}
	localBizType, localBizID := refund.RefundBusiness(current)
	if localBizType != RenewalCompensationRefundBusiness ||
		localBizID != renewal.ID ||
		renewal.PaymentID == nil || *renewal.PaymentID != current.PaymentID ||
		current.PaymentID != payment.ID || payment.Status != "succeeded" ||
		current.Amount != renewal.FeeAmount ||
		current.TotalAmount != payment.Amount ||
		current.Currency != renewal.Currency || payment.Currency != renewal.Currency {
		return problem.Internal("renewal compensation refund local links are inconsistent")
	}
	if renewal.Status == RenewalStatusCompleted ||
		renewal.Status == RenewalStatusClosed ||
		renewal.Status == RenewalStatusRefunded {
		return problem.Conflict(
			"REFUND_STATUS_REGRESSION",
			"terminal renewal cannot accept a provider failure",
		)
	}

	occurredAt := command.OccurredAt.In(shanghaiLocation).Truncate(time.Millisecond)
	if command.Retryable {
		if command.NextRetryAt == nil {
			return problem.Internal("retryable renewal refund failure has no next retry time")
		}
		status := current.Status
		if status == "creating" {
			status = "submission_unknown"
		}
		if err := h.service.repo.updateRefundVersioned(
			ctx,
			tx,
			current,
			map[string]any{
				"status": status, "next_retry_at": *command.NextRetryAt,
				"locked_by": nil, "locked_until": nil,
				"failure_code": command.Code, "failure_detail": command.Detail,
			},
		); err != nil {
			return err
		}
		if renewal.Status == RenewalStatusRefundException {
			return h.service.repo.updateRenewalVersioned(
				ctx,
				tx,
				renewal,
				map[string]any{
					"status":     RenewalStatusCompensatingRefund,
					"version":    gorm.Expr("version + 1"),
					"updated_at": occurredAt,
				},
			)
		}
		return nil
	}

	if err := h.service.repo.updateRefundVersioned(
		ctx,
		tx,
		current,
		map[string]any{
			"status": "exception", "next_retry_at": nil,
			"locked_by": nil, "locked_until": nil, "failed_at": occurredAt,
			"failure_code": command.Code, "failure_detail": command.Detail,
		},
	); err != nil {
		return err
	}
	if renewal.Status != RenewalStatusRefundException {
		if err := h.service.repo.updateRenewalVersioned(
			ctx,
			tx,
			renewal,
			map[string]any{
				"status":     RenewalStatusRefundException,
				"version":    gorm.Expr("version + 1"),
				"updated_at": occurredAt,
			},
		); err != nil {
			return err
		}
	}
	return h.service.createRenewalSettlementAudit(
		ctx,
		tx,
		"wine_ticket.renewal.refund_exception",
		renewal,
		RenewalStatusRefundException,
		map[string]any{
			"renewal_no":      renewal.RenewalNo,
			"refund_no":       current.RefundNo,
			"reason_code":     command.Code,
			"provider_status": "PROVIDER_CALL_FAILED",
		},
	)
}

func (h *RenewalRefundSettlementHandler) applyRefundSuccessTx(
	ctx context.Context,
	tx *gorm.DB,
	lot *core.Lot,
	renewal Renewal,
	row refund.Row,
	payment refund.Payment,
	state refund.State,
) error {
	if row.Status == "succeeded" && renewal.Status == RenewalStatusRefunded {
		return h.service.expireLotAfterRenewalGuardRelease(
			ctx,
			tx,
			lot,
			renewal,
			h.service.nowShanghai(),
			"compensation_refunded",
		)
	}
	if renewal.Status == RenewalStatusCompleted {
		return problem.Conflict(
			"REFUND_STATUS_REGRESSION",
			"completed renewal cannot accept a compensation refund",
		)
	}
	if payment.RefundedAmount+row.Amount > payment.Amount {
		return h.markRefundExceptionTx(
			ctx,
			tx,
			renewal,
			row,
			state,
			"REFUND_AMOUNT_EXCEEDED",
		)
	}
	paymentUpdated, err := h.service.repo.incrementRefundedPaymentAmount(
		ctx,
		tx,
		payment.ID,
		row.Amount,
	)
	if err != nil {
		return err
	}
	if !paymentUpdated {
		return problem.Conflict(
			"REFUND_AMOUNT_EXCEEDED",
			"renewal compensation exceeds remaining payment amount",
		)
	}

	now := h.service.nowShanghai()
	succeededAt := now
	if state.SucceededAt != nil {
		succeededAt = state.SucceededAt.In(shanghaiLocation).Truncate(time.Millisecond)
	}
	values := renewalRefundProviderValues(state)
	values["status"] = "succeeded"
	values["succeeded_at"] = succeededAt
	values["failed_at"] = nil
	values["next_retry_at"] = nil
	values["locked_by"] = nil
	values["locked_until"] = nil
	values["failure_code"] = nil
	values["failure_detail"] = nil
	if err := h.service.repo.updateRefundVersioned(
		ctx,
		tx,
		row,
		values,
	); err != nil {
		return err
	}
	if err := h.service.repo.updateRenewalVersioned(
		ctx,
		tx,
		renewal,
		map[string]any{
			"status":      RenewalStatusRefunded,
			"refunded_at": succeededAt,
			"version":     gorm.Expr("version + 1"),
			"updated_at":  now,
		},
	); err != nil {
		return err
	}
	if err := h.service.createRenewalSettlementAudit(
		ctx,
		tx,
		"wine_ticket.renewal.compensation_refunded",
		renewal,
		RenewalStatusRefunded,
		map[string]any{
			"renewal_no": renewal.RenewalNo,
			"refund_no":  row.RefundNo,
			"amount":     row.Amount,
		},
	); err != nil {
		return err
	}
	return h.service.expireLotAfterRenewalGuardRelease(
		ctx,
		tx,
		lot,
		renewal,
		now,
		"compensation_refunded",
	)
}

func (h *RenewalRefundSettlementHandler) applyRefundProcessingTx(
	ctx context.Context,
	tx *gorm.DB,
	renewal Renewal,
	row refund.Row,
	state refund.State,
) error {
	providerStatus := ""
	if row.ProviderStatus != nil {
		providerStatus = strings.ToUpper(strings.TrimSpace(*row.ProviderStatus))
	}
	if row.Status == "pending" &&
		providerStatus == "PROCESSING" &&
		sameOptionalProviderRefundID(row, state.ProviderRefundID) &&
		renewal.Status == RenewalStatusCompensatingRefund {
		return nil
	}
	now := h.service.nowShanghai()
	next := now.Add(time.Minute)
	values := renewalRefundProviderValues(state)
	values["status"] = "pending"
	values["next_retry_at"] = next
	values["locked_by"] = nil
	values["locked_until"] = nil
	values["failure_code"] = nil
	values["failure_detail"] = nil
	if state.ProviderCreatedAt != nil && row.ProviderAcceptedAt == nil {
		values["provider_accepted_at"] = state.ProviderCreatedAt.In(
			shanghaiLocation,
		).Truncate(time.Millisecond)
	}
	if err := h.service.repo.updateRefundVersioned(
		ctx,
		tx,
		row,
		values,
	); err != nil {
		return err
	}
	if renewal.Status == RenewalStatusRefundException {
		return h.service.repo.updateRenewalVersioned(
			ctx,
			tx,
			renewal,
			map[string]any{
				"status":     RenewalStatusCompensatingRefund,
				"version":    gorm.Expr("version + 1"),
				"updated_at": now,
			},
		)
	}
	return nil
}

func (h *RenewalRefundSettlementHandler) replaceClosedRefundTx(
	ctx context.Context,
	tx *gorm.DB,
	renewal Renewal,
	row refund.Row,
	rows []refund.Row,
	state refund.State,
) error {
	if replacement, err := h.service.repo.replacementFor(rows, row.ID); err == nil {
		if renewal.CompensatingRefundID != nil &&
			*renewal.CompensatingRefundID == replacement.ID {
			return nil
		}
		return problem.Conflict(
			"REFUND_STATUS_REGRESSION",
			"renewal compensation replacement link is inconsistent",
		)
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}

	now := h.service.nowShanghai()
	values := renewalRefundProviderValues(state)
	values["status"] = "failed"
	values["failed_at"] = now
	values["next_retry_at"] = nil
	values["locked_by"] = nil
	values["locked_until"] = nil
	values["failure_code"] = "CLOSED"
	values["failure_detail"] = "provider closed the compensation refund"
	if err := h.service.repo.updateRefundVersioned(
		ctx,
		tx,
		row,
		values,
	); err != nil {
		return err
	}

	replacementID := h.service.ids.Next()
	bizType := RenewalCompensationRefundBusiness
	bizID := renewal.ID
	replacement := refund.Row{
		ID:               replacementID,
		PaymentID:        row.PaymentID,
		ReplacesRefundID: &row.ID,
		RefundNo:         "WTRF" + idString(replacementID),
		Provider:         row.Provider,
		Status:           "creating",
		Currency:         row.Currency,
		Reason:           row.Reason,
		BizType:          &bizType,
		BizID:            &bizID,
		NotifyURL:        row.NotifyURL,
		Amount:           row.Amount,
		TotalAmount:      row.TotalAmount,
		RequestedAt:      now,
		Version:          1,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := h.service.repo.createCompensationRefund(
		ctx,
		tx,
		&replacement,
	); err != nil {
		return err
	}
	if err := h.service.repo.updateRenewalVersioned(
		ctx,
		tx,
		renewal,
		map[string]any{
			"status":                 RenewalStatusCompensatingRefund,
			"compensating_refund_id": replacementID,
			"version":                gorm.Expr("version + 1"),
			"updated_at":             now,
		},
	); err != nil {
		return err
	}
	return h.service.createRenewalSettlementAudit(
		ctx,
		tx,
		"wine_ticket.renewal.compensation_replaced",
		renewal,
		RenewalStatusCompensatingRefund,
		map[string]any{
			"renewal_no":            renewal.RenewalNo,
			"closed_refund_no":      row.RefundNo,
			"replacement_refund_no": replacement.RefundNo,
		},
	)
}

func (h *RenewalRefundSettlementHandler) markRefundExceptionTx(
	ctx context.Context,
	tx *gorm.DB,
	renewal Renewal,
	row refund.Row,
	state refund.State,
	reason string,
) error {
	if renewal.Status == RenewalStatusCompleted ||
		renewal.Status == RenewalStatusClosed ||
		renewal.Status == RenewalStatusRefunded {
		return problem.Conflict(
			"REFUND_STATUS_REGRESSION",
			"terminal renewal cannot enter refund exception",
		)
	}
	incoming := strings.ToUpper(strings.TrimSpace(state.Status))
	providerStatus := ""
	if row.ProviderStatus != nil {
		providerStatus = strings.ToUpper(strings.TrimSpace(*row.ProviderStatus))
	}
	if row.Status == "exception" &&
		providerStatus == incoming &&
		renewal.Status == RenewalStatusRefundException {
		return nil
	}
	now := h.service.nowShanghai()
	values := renewalRefundProviderValues(state)
	values["status"] = "exception"
	values["next_retry_at"] = nil
	values["locked_by"] = nil
	values["locked_until"] = nil
	values["failure_code"] = reason
	values["failure_detail"] = "renewal compensation refund requires reconciliation"
	if err := h.service.repo.updateRefundVersioned(
		ctx,
		tx,
		row,
		values,
	); err != nil {
		return err
	}
	if renewal.Status != RenewalStatusRefundException {
		if err := h.service.repo.updateRenewalVersioned(
			ctx,
			tx,
			renewal,
			map[string]any{
				"status":     RenewalStatusRefundException,
				"version":    gorm.Expr("version + 1"),
				"updated_at": now,
			},
		); err != nil {
			return err
		}
	}
	return h.service.createRenewalSettlementAudit(
		ctx,
		tx,
		"wine_ticket.renewal.refund_exception",
		renewal,
		RenewalStatusRefundException,
		map[string]any{
			"renewal_no":      renewal.RenewalNo,
			"refund_no":       row.RefundNo,
			"reason_code":     reason,
			"provider_status": incoming,
		},
	)
}

func renewalRefundFactMismatch(
	renewal Renewal,
	row refund.Row,
	payment refund.Payment,
	state refund.State,
) string {
	bizType, bizID := refund.RefundBusiness(row)
	currencyMismatch := state.Currency != "" && state.Currency != row.Currency
	if state.CurrencyRequired && state.Currency == "" {
		currencyMismatch = true
	}
	switch {
	case bizType != RenewalCompensationRefundBusiness ||
		bizID != renewal.ID:
		return "REFUND_BUSINESS_LINK_INVALID"
	case renewal.PaymentID == nil ||
		*renewal.PaymentID != row.PaymentID ||
		row.PaymentID != payment.ID:
		return "REFUND_PAYMENT_LINK_INVALID"
	case row.Amount != renewal.FeeAmount ||
		row.TotalAmount != payment.Amount ||
		row.Currency != renewal.Currency ||
		payment.Currency != renewal.Currency:
		return "REFUND_LOCAL_AMOUNT_INVALID"
	case state.RefundNo != row.RefundNo ||
		state.PaymentNo != payment.PaymentNo:
		return "REFUND_PAYMENT_MISMATCH"
	case state.Amount != row.Amount ||
		state.TotalAmount != row.TotalAmount ||
		currencyMismatch:
		return "REFUND_AMOUNT_MISMATCH"
	case row.ProviderRefundID != nil &&
		*row.ProviderRefundID != "" &&
		state.ProviderRefundID != "" &&
		*row.ProviderRefundID != state.ProviderRefundID:
		return "REFUND_PROVIDER_ID_MISMATCH"
	default:
		return ""
	}
}

func renewalRefundProviderValues(state refund.State) map[string]any {
	values := map[string]any{
		"provider_status": optionalRenewalRefundString(state.Status),
	}
	if strings.TrimSpace(state.ProviderRefundID) != "" {
		values["provider_refund_id"] = strings.TrimSpace(state.ProviderRefundID)
	}
	return values
}

func optionalRenewalRefundString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func sameOptionalProviderRefundID(row refund.Row, incoming string) bool {
	incoming = strings.TrimSpace(incoming)
	if row.ProviderRefundID == nil || *row.ProviderRefundID == "" {
		return incoming == ""
	}
	return incoming == "" || *row.ProviderRefundID == incoming
}

func renewalRefundObservationIsIdempotent(
	row refund.Row,
	incoming string,
) bool {
	providerStatus := ""
	if row.ProviderStatus != nil {
		providerStatus = strings.ToUpper(strings.TrimSpace(*row.ProviderStatus))
	}
	switch incoming {
	case "SUCCESS":
		return row.Status == "succeeded"
	case "CLOSED":
		return row.Status == "failed" && providerStatus == "CLOSED"
	case "ABNORMAL", "UNKNOWN":
		return row.Status == "exception" && providerStatus == incoming
	default:
		return false
	}
}

func renewalWorkerFailureApplicable(status string) bool {
	switch status {
	case "creating", "submission_unknown", "pending":
		return true
	default:
		return false
	}
}
