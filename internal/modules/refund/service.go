package refund

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/paygateway"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type Service struct {
	cfg                   config.Config
	repo                  *Repository
	ids                   *snowflake.Generator
	provider              Provider
	idem                  *idempotency.Store
	deliveryReturnClosure DeliveryReturnClosure
}

type DeliveryReturnClosure interface {
	ReconcileRefundWithTx(context.Context, *gorm.DB, uint64) error
}

// NewService 创建并初始化服务。
func NewService(cfg config.Config, db *gorm.DB, ids *snowflake.Generator, provider Provider) *Service {
	return &Service{cfg: cfg, repo: NewRepository(db), ids: ids, provider: provider, idem: idempotency.NewStore(db)}
}

func (s *Service) WithDeliveryReturnClosure(closure DeliveryReturnClosure) *Service {
	s.deliveryReturnClosure = closure
	return s
}

// ProcessCallback 返回Process 回调。
func (s *Service) ProcessCallback(ctx context.Context, providerCode string, request *http.Request, raw []byte) error {
	if s.provider == nil || s.provider.Code() != providerCode {
		return problem.NotFound("REFUND_PROVIDER_NOT_FOUND", "refund provider not found")
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(raw))
	request.Body = io.NopCloser(bytes.NewReader(raw))
	event, err := s.provider.ParseRefundCallback(ctx, request)
	if err != nil {
		_ = s.recordRejected(ctx, providerCode, "invalid:"+hash, hash, "SIGNATURE_INVALID")
		return problem.Unauthorized("REFUND_CALLBACK_INVALID", "refund callback verification failed")
	}
	if event.EventID == "" || event.State.RefundNo == "" {
		_ = s.recordRejected(ctx, providerCode, "invalid:"+hash, hash, "PAYLOAD_INVALID")
		return problem.InvalidArgument("REFUND_CALLBACK_INVALID", "refund callback payload is incomplete")
	}
	if !s.cfg.WeChat.PayMockEnabled && event.MchID != s.cfg.WeChat.PayMchID {
		_ = s.recordRejected(ctx, providerCode, event.EventID, hash, "MERCHANT_IDENTITY_MISMATCH")
		return problem.Unauthorized("REFUND_CALLBACK_INVALID", "refund callback merchant identity mismatch")
	}
	var reject error
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		callback := &Callback{ID: s.ids.Next(), Provider: providerCode, ProviderEventID: event.EventID, PayloadHash: hash, SignatureValid: true, ProcessStatus: "received", ReceivedAt: now, RequestID: requestctx.RequestIDPtr(ctx)}
		created, err := s.repo.CreateCallbackIfAbsent(ctx, tx, callback)
		if err != nil || !created {
			return err
		}
		row, err := s.repo.LockByNo(ctx, tx, event.State.RefundNo)
		if isNotFound(err) {
			return s.repo.UpdateCallback(ctx, tx, callback.ID, map[string]any{"process_status": "ignored", "error_code": "REFUND_NOT_FOUND", "processed_at": &now})
		}
		if err != nil {
			return err
		}
		callback.RefundID = &row.ID
		if err := s.repo.UpdateCallback(ctx, tx, callback.ID, map[string]any{"refund_id": row.ID}); err != nil {
			return err
		}
		if err := s.applyState(ctx, tx, row, event.State); err != nil {
			reject = err
			if isStateMismatch(err) {
				if updateErr := s.repo.Update(ctx, tx, row.ID, map[string]any{"status": "exception", "failure_code": "PROVIDER_DATA_MISMATCH", "failure_detail": err.Error(), "next_retry_at": nil, "locked_by": nil, "locked_until": nil}); updateErr != nil {
					return updateErr
				}
				if s.deliveryReturnClosure != nil {
					if reconcileErr := s.deliveryReturnClosure.ReconcileRefundWithTx(ctx, tx, row.AfterSaleID); reconcileErr != nil {
						return reconcileErr
					}
				}
			}
			code := "REFUND_CALLBACK_REJECTED"
			return s.repo.UpdateCallback(ctx, tx, callback.ID, map[string]any{"process_status": "failed", "error_code": code, "processed_at": &now})
		}
		return s.repo.UpdateCallback(ctx, tx, callback.ID, map[string]any{"process_status": "processed", "processed_at": &now})
	})
	if err != nil {
		return err
	}
	return reject
}

// ApplyProviderState 应用提供器状态。
func (s *Service) ApplyProviderState(ctx context.Context, refundID uint64, state State) error {
	return s.applyProviderState(ctx, refundID, nil, state)
}

// ApplyClaimedProviderState only applies the result if the worker still owns
// the exact refund version produced by Claim. A callback or operator action
// that advances the row while the provider call is in flight fences out the
// stale result.
func (s *Service) ApplyClaimedProviderState(ctx context.Context, refundID uint64, claimedVersion uint32, state State) error {
	return s.applyProviderState(ctx, refundID, &claimedVersion, state)
}

func (s *Service) applyProviderState(ctx context.Context, refundID uint64, claimedVersion *uint32, state State) error {
	return s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := s.repo.Lock(ctx, tx, refundID)
		if err != nil {
			return err
		}
		if claimedVersion != nil && row.Version != *claimedVersion {
			return nil
		}
		err = s.applyState(ctx, tx, row, state)
		if err != nil && isStateMismatch(err) {
			if updateErr := s.repo.Update(ctx, tx, row.ID, map[string]any{"status": "exception", "failure_code": "PROVIDER_DATA_MISMATCH", "failure_detail": err.Error(), "next_retry_at": nil, "locked_by": nil, "locked_until": nil}); updateErr != nil {
				return updateErr
			}
			if s.deliveryReturnClosure != nil {
				return s.deliveryReturnClosure.ReconcileRefundWithTx(ctx, tx, row.AfterSaleID)
			}
			return nil
		}
		return err
	})
}

// applyState 应用状态。
func (s *Service) applyState(ctx context.Context, tx *gorm.DB, row Row, state State) error {
	currencyMismatch := state.Currency != "" && state.Currency != row.Currency
	if state.CurrencyRequired && state.Currency == "" {
		currencyMismatch = true
	}
	if state.RefundNo != row.RefundNo || state.Amount != row.Amount || state.TotalAmount != row.TotalAmount || currencyMismatch {
		return problem.Conflict("REFUND_AMOUNT_MISMATCH", "provider refund data does not match local reservation")
	}
	if row.ProviderRefundID != nil && *row.ProviderRefundID != "" && state.ProviderRefundID != "" && *row.ProviderRefundID != state.ProviderRefundID {
		return problem.Conflict("REFUND_PROVIDER_ID_MISMATCH", "provider refund id changed")
	}
	payment, err := s.repo.LockPayment(ctx, tx, row.PaymentID)
	if err != nil {
		return err
	}
	if state.PaymentNo != payment.PaymentNo {
		return problem.Conflict("REFUND_PAYMENT_MISMATCH", "provider payment number does not match local payment")
	}
	incomingStatus := strings.ToUpper(strings.TrimSpace(state.Status))
	if row.Status == "succeeded" && incomingStatus != "SUCCESS" {
		return problem.Conflict("REFUND_STATUS_REGRESSION", "provider refund status cannot regress after success")
	}
	providerID := optional(state.ProviderRefundID)
	providerStatus := optional(state.Status)
	switch incomingStatus {
	case "SUCCESS":
		if row.Status == "succeeded" {
			return nil
		}
		order, err := s.repo.LockOrder(ctx, tx, row.OrderID)
		if err != nil {
			return err
		}
		afterSale, err := s.repo.LockAfterSale(ctx, tx, row.AfterSaleID)
		if err != nil {
			return err
		}
		if payment.RefundedAmount+row.Amount > payment.Amount || order.RefundedAmount+row.Amount > order.PaidAmount || afterSale.RefundedAmount+row.Amount > afterSale.ApprovedAmount {
			return problem.Conflict("REFUND_AMOUNT_EXCEEDED", "refund ledger amount exceeded")
		}
		items, err := s.repo.Items(ctx, tx, row.ID)
		if err != nil {
			return err
		}
		for _, item := range items {
			if err := s.repo.ApplyItem(ctx, tx, item.AfterSaleItemID, item.Amount); err != nil {
				return problem.Conflict("REFUND_ITEM_AMOUNT_EXCEEDED", "refund item amount exceeded")
			}
		}
		now := time.Now()
		if state.SucceededAt != nil {
			now = *state.SucceededAt
		}
		if err := tx.WithContext(ctx).Model(&Payment{}).Where("id=? AND refunded_amount+?<=amount", payment.ID, row.Amount).Updates(map[string]any{"refunded_amount": gorm.Expr("refunded_amount+?", row.Amount), "version": gorm.Expr("version+1")}).Error; err != nil {
			return err
		}
		orderStatus := "partial_refunded"
		if order.RefundedAmount+row.Amount == order.PaidAmount {
			orderStatus = "refunded"
		}
		financialStatus := "refunding"
		if order.RefundedAmount+row.Amount == order.PaidAmount {
			financialStatus = "refunded"
		}
		if err := tx.WithContext(ctx).Model(&Order{}).Where("id=? AND refunded_amount+?<=paid_amount", order.ID, row.Amount).Updates(map[string]any{"refunded_amount": gorm.Expr("refunded_amount+?", row.Amount), "after_sale_status": orderStatus, "status": financialStatus, "version": gorm.Expr("version+1")}).Error; err != nil {
			return err
		}
		afterStatus := afterSale.Status
		if afterSale.RefundedAmount+row.Amount == afterSale.ApprovedAmount {
			afterStatus = "completed"
		}
		if err := tx.WithContext(ctx).Model(&AfterSale{}).Where("id=? AND refunded_amount+?<=approved_amount", afterSale.ID, row.Amount).Updates(map[string]any{"refunded_amount": gorm.Expr("refunded_amount+?", row.Amount), "status": afterStatus, "version": gorm.Expr("version+1")}).Error; err != nil {
			return err
		}
		if err := s.repo.Update(ctx, tx, row.ID, map[string]any{"status": "succeeded", "provider_refund_id": providerID, "provider_status": providerStatus, "succeeded_at": &now, "locked_by": nil, "locked_until": nil, "next_retry_at": nil, "failure_code": nil, "failure_detail": nil}); err != nil {
			return err
		}
		if err := s.outbox(ctx, tx, "refund.succeeded", row.ID, map[string]any{"refund_id": id(row.ID), "refund_no": row.RefundNo, "after_sale_id": id(row.AfterSaleID), "order_id": id(row.OrderID), "amount": row.Amount}); err != nil {
			return err
		}
		if s.deliveryReturnClosure != nil {
			return s.deliveryReturnClosure.ReconcileRefundWithTx(ctx, tx, row.AfterSaleID)
		}
		return nil
	case "PROCESSING":
		next := time.Now().Add(refundPollDelay(row.Attempts))
		return s.repo.Update(ctx, tx, row.ID, map[string]any{"status": "pending", "provider_refund_id": providerID, "provider_status": providerStatus, "next_retry_at": &next, "locked_by": nil, "locked_until": nil, "failure_code": nil, "failure_detail": nil})
	case "CLOSED":
		now := time.Now()
		if err := s.repo.Update(ctx, tx, row.ID, map[string]any{"status": "failed", "provider_refund_id": providerID, "provider_status": providerStatus, "failed_at": &now, "failure_code": state.Status, "locked_by": nil, "locked_until": nil, "next_retry_at": nil}); err != nil {
			return err
		}
		if s.deliveryReturnClosure != nil {
			return s.deliveryReturnClosure.ReconcileRefundWithTx(ctx, tx, row.AfterSaleID)
		}
		return nil
	case "ABNORMAL":
		now := time.Now()
		if err := s.repo.Update(ctx, tx, row.ID, map[string]any{"status": "exception", "provider_refund_id": providerID, "provider_status": providerStatus, "failed_at": &now, "failure_code": state.Status, "locked_by": nil, "locked_until": nil, "next_retry_at": nil}); err != nil {
			return err
		}
		if s.deliveryReturnClosure != nil {
			return s.deliveryReturnClosure.ReconcileRefundWithTx(ctx, tx, row.AfterSaleID)
		}
		return nil
	default:
		return problem.InvalidArgument("REFUND_PROVIDER_STATUS_INVALID", "unsupported provider refund status")
	}
}

func refundPollDelay(attempts uint32) time.Duration {
	switch {
	case attempts <= 5:
		return time.Minute
	case attempts == 6:
		return 5 * time.Minute
	case attempts == 7:
		return 10 * time.Minute
	case attempts == 8:
		return 20 * time.Minute
	default:
		return 30 * time.Minute
	}
}

// isStateMismatch 判断状态 Mismatch是否成立。
func isStateMismatch(err error) bool {
	var details *problem.Details
	if !errors.As(err, &details) {
		return false
	}
	switch details.ErrorCode {
	case "REFUND_AMOUNT_MISMATCH", "REFUND_PROVIDER_ID_MISMATCH", "REFUND_PAYMENT_MISMATCH", "REFUND_AMOUNT_EXCEEDED", "REFUND_ITEM_AMOUNT_EXCEEDED":
		return true
	default:
		return false
	}
}

// MarkAttemptError 标记尝试错误的状态。
func (s *Service) MarkAttemptError(ctx context.Context, refundID uint64, cause error) error {
	return s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := s.repo.Lock(ctx, tx, refundID)
		if err != nil {
			return err
		}
		if row.Status == "succeeded" || row.Status == "failed" || row.Status == "exception" {
			return nil
		}
		status := row.Status
		if status == "creating" {
			status = "submission_unknown"
		}
		next := time.Now().Add(refundPollDelay(row.Attempts))
		detail := cause.Error()
		if len(detail) > 500 {
			detail = detail[:500]
		}
		code := paygateway.Code(cause, "PROVIDER_UNAVAILABLE")
		return s.repo.Update(ctx, tx, row.ID, map[string]any{"status": status, "next_retry_at": &next, "locked_by": nil, "locked_until": nil, "failure_code": code, "failure_detail": detail})
	})
}

func (s *Service) MarkPermanentError(ctx context.Context, refundID uint64, code string, cause error) error {
	return s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := s.repo.Lock(ctx, tx, refundID)
		if err != nil {
			return err
		}
		if row.Status == "succeeded" {
			return nil
		}
		detail := "provider rejected refund request"
		if cause != nil {
			detail = cause.Error()
		}
		if len(detail) > 500 {
			detail = detail[:500]
		}
		now := time.Now()
		return s.repo.Update(ctx, tx, row.ID, map[string]any{"status": "exception", "next_retry_at": nil, "locked_by": nil, "locked_until": nil, "failed_at": &now, "failure_code": code, "failure_detail": detail})
	})
}

// List 查询DTO列表列表。
func (s *Service) List(ctx context.Context, claims *auth.Claims, status string, query pagination.Query) ([]DTO, string, error) {
	if _, err := adminPermission(claims, "refund:list"); err != nil {
		return nil, "", err
	}
	rows, err := s.repo.List(ctx, status, query.Offset, query.PageSize)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		rows = rows[:query.PageSize]
		next = pagination.NextPageToken(query)
	}
	result := make([]DTO, 0, len(rows))
	for _, row := range rows {
		result = append(result, dto(row))
	}
	return result, next, nil
}

// Detail 返回Detail。
func (s *Service) Detail(ctx context.Context, claims *auth.Claims, refundNo string) (DTO, error) {
	if _, err := adminPermission(claims, "refund:view"); err != nil {
		return DTO{}, err
	}
	row, err := s.repo.ByNo(ctx, refundNo)
	if isNotFound(err) {
		return DTO{}, problem.NotFound("REFUND_NOT_FOUND", "refund not found")
	}
	return dto(row), err
}

// Retry 重试退款。
func (s *Service) Retry(ctx context.Context, claims *auth.Claims, method, path, key, refundNo string) error {
	actorID, err := adminPermission(claims, "refund:retry")
	if err != nil {
		return err
	}
	return s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), claims.AccountType, actorID, method, path, key, idempotency.RequestHash(map[string]any{"refund_no": refundNo, "action": "retry"}))
		if err != nil {
			return err
		}
		if !started {
			return s.cachedWrite(ctx, tx, claims.AccountType, actorID, path, key)
		}
		var retryCount int64
		if err := tx.WithContext(ctx).Model(&Audit{}).Where("actor_type='admin' AND actor_id=? AND action='refund.retry' AND created_at>=?", actorID, time.Now().Add(-time.Hour)).Count(&retryCount).Error; err != nil {
			return err
		}
		if retryCount >= 10 {
			return problem.TooManyRequests("REFUND_RETRY_RATE_LIMITED", "refund retry rate limit exceeded")
		}
		row, err := s.repo.LockByNo(ctx, tx, refundNo)
		if isNotFound(err) {
			return problem.NotFound("REFUND_NOT_FOUND", "refund not found")
		}
		if err != nil {
			return err
		}
		providerStatus := refundProviderStatus(row)
		failureCode := ""
		if row.FailureCode != nil {
			failureCode = strings.TrimSpace(*row.FailureCode)
		}
		resumeNotEnough := row.Status == "exception" && failureCode == "NOT_ENOUGH"
		if row.Status == "succeeded" || row.Status == "creating" {
			return problem.Conflict("REFUND_RETRY_NOT_ALLOWED", "refund cannot be retried in current status")
		}
		if row.Status == "exception" && providerStatus == "ABNORMAL" {
			return problem.Conflict("REFUND_MANUAL_ACTION_REQUIRED", "abnormal refund must be handled in WeChat Pay before reconciliation")
		}
		if row.Status == "exception" && !resumeNotEnough {
			return problem.Conflict("REFUND_RETRY_NOT_ALLOWED", "refund exception requires manual investigation")
		}
		if row.Status == "failed" && providerStatus != "CLOSED" {
			return problem.Conflict("REFUND_RETRY_NOT_ALLOWED", "failed refund cannot be retried in current provider status")
		}
		if row.Status != "pending" && row.Status != "submission_unknown" && row.Status != "failed" && !resumeNotEnough {
			return problem.Conflict("REFUND_RETRY_NOT_ALLOWED", "refund cannot be retried in current status")
		}
		payment, err := s.repo.LockPayment(ctx, tx, row.PaymentID)
		if err != nil {
			return err
		}
		if row.Status == "failed" {
			existing, existingErr := s.repo.ReplacementByOriginal(ctx, tx, row.ID)
			if existingErr == nil {
				return s.idem.Succeed(ctx, tx, claims.AccountType, actorID, path, key, map[string]any{"status": existing.Status, "refund_no": existing.RefundNo, "replaces_refund_no": row.RefundNo})
			}
			if !isNotFound(existingErr) {
				return existingErr
			}
		}
		reserved, err := s.repo.ReservedExcept(ctx, tx, row.PaymentID, row.ID)
		if err != nil {
			return err
		}
		if payment.RefundedAmount+reserved+row.Amount > payment.Amount {
			return problem.Conflict("REFUND_AMOUNT_EXCEEDED", "refund retry exceeds remaining payment amount")
		}
		var recent int64
		if err := tx.WithContext(ctx).Model(&Audit{}).Where("resource_type='refund' AND resource_id=? AND action='refund.retry' AND created_at>=?", row.ID, time.Now().Add(-5*time.Minute)).Count(&recent).Error; err != nil {
			return err
		}
		if recent > 0 {
			return problem.TooManyRequests("REFUND_RETRY_COOLDOWN", "refund retry is cooling down")
		}
		now := time.Now()
		if row.Status != "failed" {
			nextStatus := row.Status
			values := map[string]any{"status": nextStatus, "next_retry_at": now, "locked_by": nil, "locked_until": nil}
			if resumeNotEnough {
				nextStatus = "submission_unknown"
				values["status"] = nextStatus
				values["failure_code"] = "NOT_ENOUGH_MANUAL_RETRY"
				values["failure_detail"] = nil
				values["failed_at"] = nil
			}
			if err := s.repo.Update(ctx, tx, row.ID, values); err != nil {
				return err
			}
			if err := s.audit(ctx, tx, actorID, "refund.retry", row, values); err != nil {
				return err
			}
			if err := s.outbox(ctx, tx, "refund.retry_requested", row.ID, map[string]any{"refund_id": id(row.ID), "refund_no": row.RefundNo}); err != nil {
				return err
			}
			return s.idem.Succeed(ctx, tx, claims.AccountType, actorID, path, key, map[string]any{"status": nextStatus, "refund_no": row.RefundNo})
		}

		items, err := s.repo.Items(ctx, tx, row.ID)
		if err != nil {
			return err
		}
		newID := s.ids.Next()
		newNo := "RF" + id(newID)
		reason := row.Reason
		if reason == "" {
			reason = "after-sale refund"
		}
		notifyURL := row.NotifyURL
		if notifyURL == nil || strings.TrimSpace(*notifyURL) == "" {
			notifyURL = optional(s.cfg.WeChat.RefundNotifyURL)
		}
		replacement := &Row{ID: newID, AfterSaleID: row.AfterSaleID, OrderID: row.OrderID, PaymentID: row.PaymentID, ReplacesRefundID: &row.ID, RefundNo: newNo, Provider: row.Provider, Status: "creating", Currency: row.Currency, Reason: reason, NotifyURL: notifyURL, Amount: row.Amount, TotalAmount: row.TotalAmount, RequestedAt: now, NextRetryAt: &now, Version: 1}
		replacementItems := make([]RefundItem, 0, len(items))
		for _, item := range items {
			replacementItems = append(replacementItems, RefundItem{ID: s.ids.Next(), RefundID: newID, AfterSaleItemID: item.AfterSaleItemID, Amount: item.Amount, Quantity: item.Quantity})
		}
		if err := s.repo.CreateReplacement(ctx, tx, replacement, replacementItems); err != nil {
			return err
		}
		values := map[string]any{"replacement_refund_id": id(newID), "replacement_refund_no": newNo, "replaces_refund_no": row.RefundNo}
		if err := s.audit(ctx, tx, actorID, "refund.retry", row, values); err != nil {
			return err
		}
		if err := s.outbox(ctx, tx, "refund.retry_requested", newID, map[string]any{"refund_id": id(newID), "refund_no": newNo, "replaces_refund_id": id(row.ID), "replaces_refund_no": row.RefundNo}); err != nil {
			return err
		}
		return s.idem.Succeed(ctx, tx, claims.AccountType, actorID, path, key, map[string]any{"status": "creating", "refund_no": newNo, "replaces_refund_no": row.RefundNo})
	})
}

// Reconcile schedules a provider query after an operator has handled an
// ABNORMAL refund in the WeChat Pay merchant platform. It never marks funds as
// succeeded from administrator input.
func (s *Service) Reconcile(ctx context.Context, claims *auth.Claims, method, path, key, refundNo string) error {
	actorID, err := adminPermission(claims, "refund:retry")
	if err != nil {
		return err
	}
	return s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), claims.AccountType, actorID, method, path, key, idempotency.RequestHash(map[string]any{"refund_no": refundNo, "action": "reconcile"}))
		if err != nil {
			return err
		}
		if !started {
			return s.cachedWrite(ctx, tx, claims.AccountType, actorID, path, key)
		}
		row, err := s.repo.LockByNo(ctx, tx, refundNo)
		if isNotFound(err) {
			return problem.NotFound("REFUND_NOT_FOUND", "refund not found")
		}
		if err != nil {
			return err
		}
		if row.Status != "exception" || refundProviderStatus(row) != "ABNORMAL" {
			return problem.Conflict("REFUND_RECONCILE_NOT_ALLOWED", "only abnormal refunds can be reconciled by this action")
		}
		now := time.Now()
		values := map[string]any{"status": "pending", "next_retry_at": now, "locked_by": nil, "locked_until": nil, "failure_code": nil, "failure_detail": nil}
		if err := s.repo.Update(ctx, tx, row.ID, values); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, actorID, "refund.reconcile_requested", row, values); err != nil {
			return err
		}
		if err := s.outbox(ctx, tx, "refund.retry_requested", row.ID, map[string]any{"refund_id": id(row.ID), "refund_no": row.RefundNo, "action": "reconcile"}); err != nil {
			return err
		}
		return s.idem.Succeed(ctx, tx, claims.AccountType, actorID, path, key, map[string]any{"status": "pending", "refund_no": row.RefundNo})
	})
}

func refundProviderStatus(row Row) string {
	if row.ProviderStatus != nil && strings.TrimSpace(*row.ProviderStatus) != "" {
		return strings.ToUpper(strings.TrimSpace(*row.ProviderStatus))
	}
	if row.FailureCode != nil {
		return strings.ToUpper(strings.TrimSpace(*row.FailureCode))
	}
	return ""
}

// MarkException 标记Exception的状态。
func (s *Service) MarkException(ctx context.Context, claims *auth.Claims, method, path, key, refundNo, reason string) error {
	actorID, err := adminPermission(claims, "refund:exception")
	if err != nil {
		return err
	}
	return s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), claims.AccountType, actorID, method, path, key, idempotency.RequestHash(map[string]any{"refund_no": refundNo, "reason": reason}))
		if err != nil {
			return err
		}
		if !started {
			return s.cachedWrite(ctx, tx, claims.AccountType, actorID, path, key)
		}
		row, err := s.repo.LockByNo(ctx, tx, refundNo)
		if isNotFound(err) {
			return problem.NotFound("REFUND_NOT_FOUND", "refund not found")
		}
		if err != nil {
			return err
		}
		if row.Status == "succeeded" {
			return problem.Conflict("REFUND_EXCEPTION_NOT_ALLOWED", "succeeded refund cannot be marked exception")
		}
		now := time.Now()
		values := map[string]any{"status": "exception", "failure_code": "MANUAL_INVESTIGATION", "failure_detail": strings.TrimSpace(reason), "failed_at": &now, "next_retry_at": nil, "locked_by": nil, "locked_until": nil}
		if err := s.repo.Update(ctx, tx, row.ID, values); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, actorID, "refund.mark_exception", row, values); err != nil {
			return err
		}
		if err := s.outbox(ctx, tx, "refund.exception_marked", row.ID, map[string]any{"refund_id": id(row.ID), "refund_no": row.RefundNo}); err != nil {
			return err
		}
		return s.idem.Succeed(ctx, tx, claims.AccountType, actorID, path, key, map[string]any{"status": "exception"})
	})
}

// cachedWrite 返回缓存写入。
func (s *Service) cachedWrite(ctx context.Context, tx *gorm.DB, actorType string, actorID uint64, path, key string) error {
	var response map[string]any
	ok, err := s.idem.CachedResponse(ctx, tx, actorType, actorID, path, key, &response)
	if err != nil {
		return err
	}
	if !ok {
		return problem.Conflict("IDEMPOTENCY_IN_PROGRESS", "request is still processing")
	}
	return nil
}

// recordRejected 返回记录 Rejected。
func (s *Service) recordRejected(ctx context.Context, provider, eventID, hash, code string) error {
	now := time.Now()
	_, err := s.repo.CreateCallbackIfAbsent(ctx, s.repo.DB(), &Callback{ID: s.ids.Next(), Provider: provider, ProviderEventID: eventID, PayloadHash: hash, SignatureValid: false, ProcessStatus: "failed", ErrorCode: &code, ReceivedAt: now, ProcessedAt: &now, RequestID: requestctx.RequestIDPtr(ctx)})
	return err
}

// outbox 返回发件箱事件。
func (s *Service) outbox(ctx context.Context, tx *gorm.DB, eventType string, aggregateID uint64, payload any) error {
	body, _ := json.Marshal(payload)
	return s.repo.CreateOutbox(ctx, tx, Outbox{ID: s.ids.Next(), EventID: uuid.NewString(), EventType: eventType, AggregateType: "refund", AggregateID: aggregateID, Payload: datatypes.JSON(body), Status: "pending", RequestID: requestctx.RequestIDPtr(ctx)})
}

// audit 返回审计。
func (s *Service) audit(ctx context.Context, tx *gorm.DB, actorID uint64, action string, before Row, after any) error {
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	return tx.WithContext(ctx).Create(&Audit{ID: s.ids.Next(), ActorType: "admin", ActorID: actorID, Action: action, ResourceType: "refund", ResourceID: before.ID, BeforeData: datatypes.JSON(beforeJSON), AfterData: datatypes.JSON(afterJSON), Result: "success", RequestID: requestctx.RequestIDPtr(ctx), IP: requestctx.IPPtr(ctx), UserAgent: requestctx.UserAgentPtr(ctx)}).Error
}

type DTO struct {
	ID               string `json:"id"`
	RefundNo         string `json:"refund_no"`
	AfterSaleID      string `json:"after_sale_id"`
	OrderID          string `json:"order_id"`
	PaymentID        string `json:"payment_id"`
	ReplacesRefundID string `json:"replaces_refund_id,omitempty"`
	Provider         string `json:"provider"`
	ProviderRefundID string `json:"provider_refund_id,omitempty"`
	Status           string `json:"status"`
	ProviderStatus   string `json:"provider_status,omitempty"`
	Amount           int64  `json:"amount"`
	TotalAmount      int64  `json:"total_amount"`
	Currency         string `json:"currency"`
	Attempts         uint32 `json:"attempts"`
	FailureCode      string `json:"failure_code,omitempty"`
	FailureDetail    string `json:"failure_detail,omitempty"`
	RequestedAt      string `json:"requested_at"`
	SucceededAt      string `json:"succeeded_at,omitempty"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

// dto 返回DTO。
func dto(row Row) DTO {
	v := DTO{ID: id(row.ID), RefundNo: row.RefundNo, AfterSaleID: id(row.AfterSaleID), OrderID: id(row.OrderID), PaymentID: id(row.PaymentID), Provider: row.Provider, Status: row.Status, Amount: row.Amount, TotalAmount: row.TotalAmount, Currency: row.Currency, Attempts: row.Attempts, RequestedAt: row.RequestedAt.Format(time.RFC3339Nano), CreatedAt: row.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: row.UpdatedAt.Format(time.RFC3339Nano)}
	if row.ReplacesRefundID != nil {
		v.ReplacesRefundID = id(*row.ReplacesRefundID)
	}
	if row.ProviderRefundID != nil {
		v.ProviderRefundID = *row.ProviderRefundID
	}
	if row.ProviderStatus != nil {
		v.ProviderStatus = *row.ProviderStatus
	}
	if row.FailureCode != nil {
		v.FailureCode = *row.FailureCode
	}
	if row.FailureDetail != nil {
		v.FailureDetail = *row.FailureDetail
	}
	if row.SucceededAt != nil {
		v.SucceededAt = row.SucceededAt.Format(time.RFC3339Nano)
	}
	return v
}

// adminPermission 返回管理端权限。
func adminPermission(claims *auth.Claims, permission string) (uint64, error) {
	if claims == nil || claims.AccountType != "admin" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "admin account required")
	}
	actor, err := strconv.ParseUint(claims.AdminUserID, 10, 64)
	if err != nil {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "invalid admin identity")
	}
	for _, code := range claims.Permissions {
		if code == permission {
			return actor, nil
		}
	}
	return 0, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
}

// id 返回ID。
func id(value uint64) string { return strconv.FormatUint(value, 10) }

// optional 返回optional。
func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
