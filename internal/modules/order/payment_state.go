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

// ConfirmPayment 执行小程序支付的后端确认。它只查询调用方拥有的支付记录，
// 绝不把 wx.requestPayment 的结果视为最终结果。
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

// ConfirmPaymentIdempotent 是 ConfirmPayment 的 HTTP 命令版本。完成的响应
// 会先持久化再返回，因此客户端使用同一键重试时不会再次查询服务商。
// 服务商或存储失败会释放认领；存储层的有界处理租约仍作为最终崩溃保护。
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
	claimID := s.idGen.Next()
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		claimed, startErr := s.idStore.Start(ctx, tx, claimID, claims.AccountType, customerID, method, path, key, requestHash)
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
		s.releasePaymentConfirmClaim(ctx, claimID, claims.AccountType, customerID, path, key)
		return PaymentDTO{}, err
	}
	if err := s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.idStore.SucceedOwned(ctx, tx, claimID, claims.AccountType, customerID, path, key, result)
	}); err != nil {
		s.releasePaymentConfirmClaim(ctx, claimID, claims.AccountType, customerID, path, key)
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
	bizType, _ := paymentBusiness(payment)
	reconcilableExternalException :=
		payment.Status == "exception" &&
			bizType != RetailOrderPaymentBusiness &&
			payment.ProviderStatus != nil &&
			strings.EqualFold(strings.TrimSpace(*payment.ProviderStatus), "SUCCESS")
	if payment.Status == "succeeded" ||
		(payment.Status != "creating" &&
			payment.Status != "pending" &&
			!reconcilableExternalException) {
		return paymentDTO(payment), nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, s.cfg.WeChat.HTTPTimeout)
	state, queryErr := s.payment.Query(queryCtx, payment.PaymentNo)
	cancel()
	if queryErr != nil {
		s.logPaymentProviderFailure(payment.PaymentNo, queryErr, "client_retry")
		if s.metrics != nil {
			s.metrics.IncPayment(payment.Provider, "confirm_query_failed")
		}
		detail := problem.New(http.StatusServiceUnavailable, "PAYMENT_CONFIRM_RETRYABLE", "Service Unavailable", "payment confirmation is temporarily unavailable; retry is safe")
		detail.Data = map[string]any{"retryable": paygateway.Retryable(queryErr), "provider_code": paygateway.Code(queryErr, "PROVIDER_UNAVAILABLE"), "provider_request_id": paygateway.RequestID(queryErr)}
		return PaymentDTO{}, detail
	}
	s.log.Info("payment provider call completed", slog.String("operation", "payment.query"), slog.String("payment_no", payment.PaymentNo), slog.String("provider_status", state.Status), slog.String("provider_request_id", state.RequestID))
	result, err := s.ApplyProviderPaymentState(ctx, payment.PaymentNo, payment.Provider, state, "customer", customerID, "confirm:"+payment.PaymentNo)
	if err != nil {
		if s.metrics != nil {
			s.metrics.IncPayment(payment.Provider, "confirm_apply_failed")
		}
		return PaymentDTO{}, err
	}
	if s.metrics != nil {
		s.metrics.IncPayment(payment.Provider, "confirm_succeeded")
	}
	return result, nil
}

func (s *Service) releasePaymentConfirmClaim(ctx context.Context, claimID uint64, actorType string, actorID uint64, path, key string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := s.repo.DB().WithContext(cleanupCtx).Transaction(func(tx *gorm.DB) error {
		return s.idStore.FailOwned(cleanupCtx, tx, claimID, actorType, actorID, path, key)
	}); err != nil && !idempotency.IsClaimLost(err) {
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

// ApplyProviderPaymentState 是回调、客户确认和对账工作进程共用的事务。
func (s *Service) ApplyProviderPaymentState(ctx context.Context, paymentNo, provider string, state ProviderPaymentState, actorType string, actorID uint64, key string) (PaymentDTO, error) {
	var result PaymentDTO
	var reject error
	var externalHandler PaymentSettlementHandler
	err := s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		lookup, err := s.repo.GetPaymentByNo(ctx, tx, paymentNo, provider)
		if err != nil {
			return err
		}
		bizType, bizID := paymentBusiness(lookup)
		if bizType == RetailOrderPaymentBusiness {
			if lookup.OrderID == nil || bizID == 0 {
				return problem.Internal("retail payment order registry is incomplete")
			}
			orderRow, err := s.repo.LockOrder(ctx, tx, *lookup.OrderID)
			if err != nil {
				return err
			}
			payment, err := s.repo.LockPaymentByNo(ctx, tx, paymentNo, provider)
			if err != nil {
				return err
			}
			if !samePaymentBusiness(lookup, payment) {
				return problem.Internal("payment business registry changed during settlement")
			}
			updated, stateReject, err := s.applyProviderPaymentStateTx(ctx, tx, orderRow, payment, state, actorType, actorID, key)
			if err != nil {
				return err
			}
			reject = stateReject
			result = paymentDTO(updated)
			return nil
		}

		handler, err := s.externalSettlementHandler(lookup)
		if err != nil {
			return err
		}
		externalHandler = handler
		if err := handler.LockBusiness(ctx, tx, bizID); err != nil {
			return err
		}
		payment, err := s.repo.LockPaymentByNo(ctx, tx, paymentNo, provider)
		if err != nil {
			return err
		}
		if !samePaymentBusiness(lookup, payment) {
			return problem.Internal("payment business registry changed during settlement")
		}
		updated, stateReject, err := s.applyExternalPaymentStateTx(ctx, tx, handler, payment, state)
		if err != nil {
			return err
		}
		reject = stateReject
		result = paymentDTO(updated)
		return nil
	})
	if err != nil {
		if externalHandler != nil &&
			strings.EqualFold(strings.TrimSpace(state.Status), "SUCCESS") {
			persistErr := s.persistExternalPaymentSettlementFailure(
				ctx,
				paymentNo,
				provider,
				state,
				settlementFailureCode(err),
			)
			if persistErr != nil {
				return PaymentDTO{}, errors.Join(err, persistErr)
			}
		}
		return PaymentDTO{}, err
	}
	return result, reject
}

// persistExternalPaymentSettlementFailure 记录已验证为 SUCCESS、
// 但业务结算事务回滚的场景。支付机构资金事实和业务异常会在新的短事务中提交，
// 随后由共享对账任务安全回放原结算。
func (s *Service) persistExternalPaymentSettlementFailure(
	ctx context.Context,
	paymentNo string,
	provider string,
	state ProviderPaymentState,
	reason string,
) error {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return s.repo.DB().WithContext(persistCtx).Transaction(func(tx *gorm.DB) error {
		lookup, err := s.repo.GetPaymentByNo(
			persistCtx,
			tx,
			paymentNo,
			provider,
		)
		if err != nil {
			return err
		}
		handler, err := s.externalSettlementHandler(lookup)
		if err != nil {
			return err
		}
		_, bizID := paymentBusiness(lookup)
		if err := handler.LockBusiness(persistCtx, tx, bizID); err != nil {
			return err
		}
		payment, err := s.repo.LockPaymentByNo(
			persistCtx,
			tx,
			paymentNo,
			provider,
		)
		if err != nil {
			return err
		}
		if !samePaymentBusiness(lookup, payment) {
			return problem.Internal(
				"payment business registry changed while recording settlement failure",
			)
		}
		if payment.Status == "succeeded" {
			return nil
		}
		bizType, bizID := paymentBusiness(payment)
		fact := PaymentSettlementFact{
			PaymentID:         payment.ID,
			PaymentNo:         payment.PaymentNo,
			BizType:           bizType,
			BizID:             bizID,
			CustomerID:        payment.CustomerID,
			Amount:            payment.Amount,
			Currency:          payment.Currency,
			Provider:          payment.Provider,
			ProviderTradeNo:   optionalString(state.ProviderTradeNo),
			ProviderStatus:    "SUCCESS",
			PaidAt:            state.PaidAt,
			ProviderRequestID: state.RequestID,
			ReconcileAttempts: payment.ReconcileAttempts,
		}
		if err := handler.ApplyException(
			persistCtx,
			tx,
			fact,
			reason,
		); err != nil {
			return err
		}
		next := time.Now().Add(15 * time.Second)
		return s.repo.UpdatePayment(persistCtx, tx, payment.ID, map[string]any{
			"status":            "exception",
			"provider_status":   "SUCCESS",
			"provider_trade_no": optionalString(state.ProviderTradeNo),
			"paid_at":           state.PaidAt,
			"failure_code":      reason,
			"next_reconcile_at": next,
			"version":           gorm.Expr("version + 1"),
		})
	})
}

func settlementFailureCode(err error) string {
	detail := problem.FromError(err)
	if detail != nil {
		code := strings.TrimSpace(detail.ErrorCode)
		if code != "" {
			return code
		}
	}
	return "BUSINESS_SETTLEMENT_FAILED"
}

// applyProviderPaymentStateTx 在进入唯一的支付成功记账事务前，
// 校验服务商身份和金额。
func (s *Service) applyProviderPaymentStateTx(ctx context.Context, tx *gorm.DB, orderRow Order, payment Payment, state ProviderPaymentState, actorType string, actorID uint64, key string) (Payment, error, error) {
	providerStatus := strings.ToUpper(strings.TrimSpace(state.Status))
	// 服务商查询和回调可能乱序到达。支付一旦验证成功，过期的非成功状态
	// 不得覆盖成功证据，也不得再次安排对账。
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

		// 本地订单关闭后才由服务商确认的支付，绝不能扣减已释放的库存。
		// 保留资金事实并产生支付异常，交由人工履约或退款处理。
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

// MarkPaymentReconcileError 在超时或服务商失败后保留本地资金状态，
// 并安排安全的查询重试。
func (s *Service) MarkPaymentReconcileError(ctx context.Context, payment Payment, cause error) error {
	return s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, err := s.repo.LockPaymentByNo(ctx, tx, payment.PaymentNo, payment.Provider)
		if err != nil {
			return err
		}
		if locked.Status != "creating" &&
			locked.Status != "pending" &&
			locked.Status != "exception" {
			return nil
		}
		if locked.ReconcileAttempts < payment.ReconcileAttempts {
			locked.ReconcileAttempts = payment.ReconcileAttempts
		}
		now := time.Now()
		next := nextPaymentReconcileAt(locked, now)
		bizType, _ := paymentBusiness(locked)
		if bizType != RetailOrderPaymentBusiness && locked.ExpiresAt != nil && !locked.ExpiresAt.After(now) {
			next = now.Add(15 * time.Second)
		}
		return s.repo.UpdatePayment(ctx, tx, locked.ID, map[string]any{"next_reconcile_at": next, "failure_code": paygateway.Code(cause, "PROVIDER_UNAVAILABLE"), "version": gorm.Expr("version + 1")})
	})
}
