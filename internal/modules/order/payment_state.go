package order

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/paygateway"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

// ConfirmPayment performs the miniapp's backend confirmation. It only queries
// payments owned by the caller and never trusts wx.requestPayment as final.
func (s *Service) ConfirmPayment(ctx context.Context, claims *auth.Claims, orderIDRaw string) (PaymentDTO, error) {
	if s.payment == nil || !s.cfg.WeChat.PayEnabled {
		return PaymentDTO{}, problem.New(http.StatusServiceUnavailable, "PAYMENT_PROVIDER_UNAVAILABLE", "Service Unavailable", "payment provider is unavailable")
	}
	customerID, err := customerIDFromClaims(claims, "payment:view")
	if err != nil {
		return PaymentDTO{}, err
	}
	orderID, err := parseID(orderIDRaw)
	if err != nil {
		return PaymentDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid order id")
	}
	payment, err := s.customerPaymentForConfirmation(ctx, customerID, orderID)
	if err != nil {
		return PaymentDTO{}, err
	}
	return s.confirmCustomerPayment(ctx, customerID, payment)
}

// ConfirmPaymentIdempotent is the HTTP command variant of ConfirmPayment. A
// completed response is persisted before it is returned, so a client retry
// with the same key never issues another provider query. Provider and storage
// failures release the claim; the store's bounded processing lease remains a
// final crash-safety fallback.
func (s *Service) ConfirmPaymentIdempotent(ctx context.Context, claims *auth.Claims, method, path, key, orderIDRaw string) (PaymentDTO, error) {
	customerID, err := customerIDFromClaims(claims, "payment:view")
	if err != nil {
		return PaymentDTO{}, err
	}
	if err := validatePaymentConfirmIdempotencyKey(key); err != nil {
		return PaymentDTO{}, err
	}
	orderID, err := parseID(orderIDRaw)
	if err != nil {
		return PaymentDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid order id")
	}

	requestHash := idempotency.RequestHash(map[string]any{"order_id": orderID})
	var result PaymentDTO
	if replayed, replayErr := s.idStore.ReplayCompleted(ctx, s.repo.DB(), claims.AccountType, customerID, path, key, requestHash, &result); replayErr != nil {
		return PaymentDTO{}, replayErr
	} else if replayed {
		return result, nil
	}

	payment, err := s.customerPaymentForConfirmation(ctx, customerID, orderID)
	if err != nil {
		return PaymentDTO{}, err
	}

	started := false
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		claimed, startErr := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, customerID, method, path, key, requestHash)
		if startErr != nil {
			return startErr
		}
		started = claimed
		if claimed {
			return nil
		}
		cached, cacheErr := s.idStore.CachedResponse(ctx, tx, claims.AccountType, customerID, path, key, &result)
		if cacheErr != nil {
			return cacheErr
		}
		if !cached {
			return problem.Conflict("IDEMPOTENCY_CONFLICT", "idempotency response missing")
		}
		return nil
	})
	if err != nil {
		return PaymentDTO{}, err
	}
	if !started {
		return result, nil
	}

	result, err = s.confirmCustomerPayment(ctx, customerID, payment)
	if err != nil {
		s.releasePaymentConfirmClaim(ctx, claims.AccountType, customerID, path, key)
		return PaymentDTO{}, err
	}
	if err := s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.idStore.Succeed(ctx, tx, claims.AccountType, customerID, path, key, result)
	}); err != nil {
		s.releasePaymentConfirmClaim(ctx, claims.AccountType, customerID, path, key)
		return PaymentDTO{}, err
	}
	return result, nil
}

func validatePaymentConfirmIdempotencyKey(key string) error {
	if key == "" {
		return problem.InvalidArgument("IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key is required")
	}
	if len(key) < 8 || len(key) > 128 {
		return problem.InvalidArgument("IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must be between 8 and 128 characters")
	}
	return nil
}

func (s *Service) customerPaymentForConfirmation(ctx context.Context, customerID, orderID uint64) (Payment, error) {
	payment, err := s.repo.GetCustomerPayment(ctx, customerID, orderID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Payment{}, problem.NotFound("PAYMENT_NOT_FOUND", "payment not found")
	}
	if err != nil {
		return Payment{}, err
	}
	return payment, nil
}

func (s *Service) confirmCustomerPayment(ctx context.Context, customerID uint64, payment Payment) (PaymentDTO, error) {
	if s.payment == nil || !s.cfg.WeChat.PayEnabled {
		return PaymentDTO{}, problem.New(http.StatusServiceUnavailable, "PAYMENT_PROVIDER_UNAVAILABLE", "Service Unavailable", "payment provider is unavailable")
	}
	if payment.Provider != s.payment.Code() {
		return PaymentDTO{}, problem.Conflict("PAYMENT_PROVIDER_MISMATCH", "payment provider does not match configured provider")
	}
	if payment.Status == "succeeded" || (payment.Status != "creating" && payment.Status != "pending") {
		return paymentDTO(payment), nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, s.cfg.WeChat.HTTPTimeout)
	state, queryErr := s.payment.Query(queryCtx, payment.PaymentNo)
	cancel()
	if queryErr != nil {
		s.logPaymentProviderFailure(payment.PaymentNo, queryErr, "client_retry")
		s.metrics.IncPayment(payment.Provider, "confirm_query_failed")
		detail := problem.New(http.StatusServiceUnavailable, "PAYMENT_CONFIRM_RETRYABLE", "Service Unavailable", "payment confirmation is temporarily unavailable; retry is safe")
		detail.Data = map[string]any{"retryable": paygateway.Retryable(queryErr), "provider_code": paygateway.Code(queryErr, "PROVIDER_UNAVAILABLE"), "provider_request_id": paygateway.RequestID(queryErr)}
		return PaymentDTO{}, detail
	}
	s.log.Info("payment provider call completed", slog.String("operation", "payment.query"), slog.String("payment_no", payment.PaymentNo), slog.String("provider_status", state.Status), slog.String("provider_request_id", state.RequestID))
	result, err := s.ApplyProviderPaymentState(ctx, payment.PaymentNo, payment.Provider, state, "customer", customerID, "confirm:"+payment.PaymentNo)
	if err != nil {
		s.metrics.IncPayment(payment.Provider, "confirm_apply_failed")
		return PaymentDTO{}, err
	}
	s.metrics.IncPayment(payment.Provider, "confirm_succeeded")
	return result, nil
}

func (s *Service) releasePaymentConfirmClaim(ctx context.Context, actorType string, actorID uint64, path, key string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := s.repo.DB().WithContext(cleanupCtx).Transaction(func(tx *gorm.DB) error {
		return s.idStore.Fail(cleanupCtx, tx, actorType, actorID, path, key)
	}); err != nil {
		s.log.Warn("failed to release payment confirmation idempotency claim", slog.String("path", path), slog.String("error", err.Error()))
	}
}

func (s *Service) logPaymentProviderFailure(paymentNo string, err error, decision string) {
	attrs := []any{slog.String("payment_no", paymentNo), slog.String("provider_code", paygateway.Code(err, "PROVIDER_UNAVAILABLE")), slog.String("provider_request_id", paygateway.RequestID(err)), slog.Bool("provider_retryable", paygateway.Retryable(err)), slog.String("retry_decision", decision)}
	if providerErr, ok := paygateway.As(err); ok {
		attrs = append(attrs, slog.String("operation", providerErr.Operation), slog.Int("http_status", providerErr.HTTPStatus))
	}
	s.log.Warn("payment provider call failed", attrs...)
}

// ApplyProviderPaymentState is the shared transaction used by callbacks,
// customer confirmation and reconciliation workers.
func (s *Service) ApplyProviderPaymentState(ctx context.Context, paymentNo, provider string, state ProviderPaymentState, actorType string, actorID uint64, key string) (PaymentDTO, error) {
	var result PaymentDTO
	var reject error
	err := s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		lookup, err := s.repo.GetPaymentByNo(ctx, tx, paymentNo, provider)
		if err != nil {
			return err
		}
		orderRow, err := s.repo.LockOrder(ctx, tx, lookup.OrderID)
		if err != nil {
			return err
		}
		payment, err := s.repo.LockPaymentByNo(ctx, tx, paymentNo, provider)
		if err != nil {
			return err
		}
		updated, stateReject, err := s.applyProviderPaymentStateTx(ctx, tx, orderRow, payment, state, actorType, actorID, key)
		if err != nil {
			return err
		}
		reject = stateReject
		result = paymentDTO(updated)
		return nil
	})
	if err != nil {
		return PaymentDTO{}, err
	}
	return result, reject
}

// applyProviderPaymentStateTx validates provider identity and amounts before it
// dispatches to the single payment-success ledger transaction.
func (s *Service) applyProviderPaymentStateTx(ctx context.Context, tx *gorm.DB, orderRow Order, payment Payment, state ProviderPaymentState, actorType string, actorID uint64, key string) (Payment, error, error) {
	providerStatus := strings.ToUpper(strings.TrimSpace(state.Status))
	// Provider queries and callbacks can arrive out of order. Once a payment
	// has been validated as successful, a stale non-success state must not
	// overwrite its success evidence or schedule another reconciliation.
	if payment.Status == "succeeded" && providerStatus != "SUCCESS" {
		return payment, nil, nil
	}
	amountPresent := state.AmountPresent || state.Amount != 0 || strings.TrimSpace(state.Currency) != ""
	mismatchCode := ""
	if !s.cfg.WeChat.PayMockEnabled && (state.AppID != s.cfg.WeChat.MiniAppID || state.MchID != s.cfg.WeChat.PayMchID) {
		mismatchCode = "PROVIDER_IDENTITY_MISMATCH"
	} else if state.PaymentNo != payment.PaymentNo || (amountPresent && (state.Amount != payment.Amount || state.Currency != payment.Currency)) || (providerStatus == "SUCCESS" && (!amountPresent || strings.TrimSpace(state.ProviderTradeNo) == "")) || (payment.ProviderTradeNo != nil && *payment.ProviderTradeNo != "" && state.ProviderTradeNo != "" && *payment.ProviderTradeNo != state.ProviderTradeNo) {
		mismatchCode = "PROVIDER_DATA_MISMATCH"
	}
	if mismatchCode != "" {
		reject := problem.Conflict("PAYMENT_PROVIDER_DATA_MISMATCH", "provider payment data does not match local payment")
		if err := s.markPaymentExceptionTx(ctx, tx, orderRow, payment, state, mismatchCode, reject.Error()); err != nil {
			return Payment{}, nil, err
		}
		payment.Status = "exception"
		payment.ProviderStatus = optionalString(state.Status)
		payment.FailureCode = stringPtr(mismatchCode)
		payment.NextReconcileAt = nil
		return payment, reject, nil
	}

	switch providerStatus {
	case "SUCCESS":
		if payment.Status == "succeeded" {
			return payment, nil, nil
		}
		if orderRow.Status == "pending_payment" {
			event := PaymentCallbackEvent{ProviderTradeNo: state.ProviderTradeNo, PaymentNo: state.PaymentNo, Status: providerStatus, Amount: state.Amount, Currency: state.Currency, PaidAt: state.PaidAt, AppID: state.AppID, MchID: state.MchID}
			if err := s.applyPaymentSuccess(ctx, tx, orderRow, payment, event, actorType, actorID, key); err != nil {
				return Payment{}, nil, err
			}
			payment.Status = "succeeded"
			payment.ProviderStatus = stringPtr(providerStatus)
			payment.ProviderTradeNo = optionalString(state.ProviderTradeNo)
			payment.PaidAt = state.PaidAt
			if payment.PaidAt == nil {
				now := time.Now()
				payment.PaidAt = &now
			}
			payment.FailureCode = nil
			payment.NextReconcileAt = nil
			return payment, nil, nil
		}

		// A provider-confirmed payment after the local order was closed must
		// never deduct already released stock. Preserve the money fact and
		// raise a payment exception for manual fulfillment/refund handling.
		paidAt := state.PaidAt
		if paidAt == nil {
			now := time.Now()
			paidAt = &now
		}
		if err := s.repo.UpdatePayment(ctx, tx, payment.ID, map[string]any{"status": "succeeded", "provider_status": providerStatus, "provider_trade_no": optionalString(state.ProviderTradeNo), "paid_at": paidAt, "failure_code": nil, "next_reconcile_at": nil, "version": gorm.Expr("version + 1")}); err != nil {
			return Payment{}, nil, err
		}
		if err := s.repo.UpdateOrder(ctx, tx, orderRow.ID, map[string]any{"status": "payment_exception", "pay_status": "succeeded", "paid_amount": payment.Amount, "paid_at": paidAt, "version": gorm.Expr("version + 1")}); err != nil {
			return Payment{}, nil, err
		}
		if err := s.createOutbox(ctx, tx, "payment.exception", "payment", payment.ID, map[string]any{"payment_id": idString(payment.ID), "order_id": idString(orderRow.ID), "reason_code": "PAYMENT_AFTER_ORDER_CLOSED", "provider_request_id": state.RequestID}); err != nil {
			return Payment{}, nil, err
		}
		payment.Status = "succeeded"
		payment.ProviderStatus = stringPtr(providerStatus)
		payment.ProviderTradeNo = optionalString(state.ProviderTradeNo)
		payment.PaidAt = paidAt
		payment.NextReconcileAt = nil
		return payment, nil, nil

	case "NOTPAY", "USERPAYING":
		next := nextPaymentReconcileAt(payment, time.Now())
		if err := s.repo.UpdatePayment(ctx, tx, payment.ID, map[string]any{"provider_status": providerStatus, "next_reconcile_at": next, "failure_code": nil, "version": gorm.Expr("version + 1")}); err != nil {
			return Payment{}, nil, err
		}
		payment.ProviderStatus = stringPtr(providerStatus)
		payment.NextReconcileAt = &next
		payment.FailureCode = nil
		return payment, nil, nil

	case "CLOSED", "REVOKED", "PAYERROR":
		localStatus := "closed"
		if providerStatus == "PAYERROR" {
			localStatus = "failed"
		}
		now := time.Now()
		if err := s.repo.UpdatePayment(ctx, tx, payment.ID, map[string]any{"status": localStatus, "provider_status": providerStatus, "failed_at": &now, "failure_code": providerStatus, "next_reconcile_at": nil, "version": gorm.Expr("version + 1")}); err != nil {
			return Payment{}, nil, err
		}
		payment.Status = localStatus
		payment.ProviderStatus = stringPtr(providerStatus)
		payment.FailedAt = &now
		payment.FailureCode = stringPtr(providerStatus)
		payment.NextReconcileAt = nil
		return payment, nil, nil
	default:
		return Payment{}, nil, problem.InvalidArgument("PAYMENT_PROVIDER_STATUS_INVALID", "unsupported provider payment status")
	}
}

func (s *Service) markPaymentExceptionTx(ctx context.Context, tx *gorm.DB, orderRow Order, payment Payment, state ProviderPaymentState, code, detail string) error {
	if len(detail) > 500 {
		detail = detail[:500]
	}
	if err := s.repo.UpdatePayment(ctx, tx, payment.ID, map[string]any{"status": "exception", "provider_status": optionalString(state.Status), "failure_code": code, "next_reconcile_at": nil, "version": gorm.Expr("version + 1")}); err != nil {
		return err
	}
	if err := s.repo.UpdateOrder(ctx, tx, orderRow.ID, map[string]any{"status": "payment_exception", "version": gorm.Expr("version + 1")}); err != nil {
		return err
	}
	return s.createOutbox(ctx, tx, "payment.exception", "payment", payment.ID, map[string]any{"payment_id": idString(payment.ID), "order_id": idString(orderRow.ID), "reason_code": code, "detail": detail, "provider_request_id": state.RequestID})
}

func nextPaymentReconcileAt(payment Payment, now time.Time) time.Time {
	var delay time.Duration
	switch {
	case payment.ReconcileAttempts <= 4:
		delay = 15 * time.Second
	case payment.ReconcileAttempts <= 8:
		delay = 30 * time.Second
	case payment.ReconcileAttempts <= 10:
		delay = time.Minute
	case payment.ReconcileAttempts == 11:
		delay = 2 * time.Minute
	default:
		delay = 5 * time.Minute
	}
	next := now.Add(delay)
	if payment.ExpiresAt != nil && next.After(*payment.ExpiresAt) {
		return *payment.ExpiresAt
	}
	return next
}

// MarkPaymentReconcileError preserves the local money state after a timeout or
// provider failure and schedules a safe query retry.
func (s *Service) MarkPaymentReconcileError(ctx context.Context, payment Payment, cause error) error {
	return s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, err := s.repo.LockPaymentByNo(ctx, tx, payment.PaymentNo, payment.Provider)
		if err != nil {
			return err
		}
		if locked.Status != "creating" && locked.Status != "pending" {
			return nil
		}
		if locked.ReconcileAttempts < payment.ReconcileAttempts {
			locked.ReconcileAttempts = payment.ReconcileAttempts
		}
		next := nextPaymentReconcileAt(locked, time.Now())
		return s.repo.UpdatePayment(ctx, tx, locked.ID, map[string]any{"next_reconcile_at": next, "failure_code": paygateway.Code(cause, "PROVIDER_UNAVAILABLE"), "version": gorm.Expr("version + 1")})
	})
}
