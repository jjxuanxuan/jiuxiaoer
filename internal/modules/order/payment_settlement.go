package order

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

const RetailOrderPaymentBusiness = "retail_order"

const wineTicketRenewalPaymentBusiness = "wine_ticket_renewal"

// PaymentSettlementFact 是已验证的支付机构事实，
// 会在推进公共支付记录的同一事务中传给业务处理器。
type PaymentSettlementFact struct {
	PaymentID         uint64
	PaymentNo         string
	BizType           string
	BizID             uint64
	CustomerID        uint64
	Amount            int64
	Currency          string
	Provider          string
	ProviderTradeNo   *string
	ProviderStatus    string
	PaidAt            *time.Time
	FailureCode       *string
	ProviderRequestID string
	ReconcileAttempts uint32
}

// PaymentSettlementHandler 只负责业务侧状态。
// LockBusiness 必须在锁定公共支付记录前获取处理器的标准业务锁；
// 其余方法在该锁计划建立后运行。
type PaymentSettlementHandler interface {
	BusinessType() string
	LockBusiness(context.Context, *gorm.DB, uint64) error
	ApplySuccess(context.Context, *gorm.DB, PaymentSettlementFact) error
	ApplyTerminal(context.Context, *gorm.DB, PaymentSettlementFact) error
	ApplyException(context.Context, *gorm.DB, PaymentSettlementFact, string) error
}

type paymentSettlementRegistry struct {
	mu       sync.RWMutex
	handlers map[string]PaymentSettlementHandler
}

func newPaymentSettlementRegistry() *paymentSettlementRegistry {
	return &paymentSettlementRegistry{handlers: make(map[string]PaymentSettlementHandler)}
}

func (r *paymentSettlementRegistry) register(handler PaymentSettlementHandler) error {
	if handler == nil {
		return fmt.Errorf("payment settlement handler is required")
	}
	bizType := strings.TrimSpace(handler.BusinessType())
	if bizType == "" || bizType == RetailOrderPaymentBusiness {
		return fmt.Errorf("invalid external payment business type %q", bizType)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.handlers[bizType]; exists {
		return fmt.Errorf("payment settlement handler already registered for %q", bizType)
	}
	r.handlers[bizType] = handler
	return nil
}

func (r *paymentSettlementRegistry) resolve(bizType string) (PaymentSettlementHandler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	handler, ok := r.handlers[bizType]
	return handler, ok
}

// WithPaymentSettlementHandler 注册应用启动期业务处理器。
// 重复或格式错误的注册会触发 panic，因为把资金静默路由到任意处理器
// 不属于可恢复的运行时状态。
func (s *Service) WithPaymentSettlementHandler(handler PaymentSettlementHandler) *Service {
	if s.paymentSettlements == nil {
		s.paymentSettlements = newPaymentSettlementRegistry()
	}
	if err := s.paymentSettlements.register(handler); err != nil {
		panic(err)
	}
	return s
}

func paymentBusiness(payment Payment) (string, uint64) {
	if payment.BizType == nil || strings.TrimSpace(*payment.BizType) == "" {
		if payment.OrderID == nil {
			return RetailOrderPaymentBusiness, 0
		}
		return RetailOrderPaymentBusiness, *payment.OrderID
	}
	bizID := uint64(0)
	if payment.BizID != nil {
		bizID = *payment.BizID
	}
	return strings.TrimSpace(*payment.BizType), bizID
}

func (s *Service) externalSettlementHandler(payment Payment) (PaymentSettlementHandler, error) {
	bizType, bizID := paymentBusiness(payment)
	if bizType == RetailOrderPaymentBusiness {
		return nil, nil
	}
	if bizID == 0 {
		return nil, problem.Internal("payment business registry is incomplete")
	}
	if s.paymentSettlements == nil {
		return nil, problem.Internal("payment settlement registry is not initialized")
	}
	handler, ok := s.paymentSettlements.resolve(bizType)
	if !ok {
		return nil, problem.Internal("payment settlement handler is not registered")
	}
	return handler, nil
}

func samePaymentBusiness(left, right Payment) bool {
	leftType, leftID := paymentBusiness(left)
	rightType, rightID := paymentBusiness(right)
	return leftType == rightType && leftID == rightID
}

func (s *Service) applyExternalPaymentStateTx(
	ctx context.Context,
	tx *gorm.DB,
	handler PaymentSettlementHandler,
	payment Payment,
	state ProviderPaymentState,
) (Payment, error, error) {
	providerStatus := strings.ToUpper(strings.TrimSpace(state.Status))
	if (payment.Status == "succeeded" ||
		(payment.ProviderStatus != nil &&
			strings.EqualFold(strings.TrimSpace(*payment.ProviderStatus), "SUCCESS"))) &&
		providerStatus != "SUCCESS" {
		return payment, nil, nil
	}
	bizType, bizID := paymentBusiness(payment)
	fact := PaymentSettlementFact{
		PaymentID: payment.ID, PaymentNo: payment.PaymentNo, BizType: bizType, BizID: bizID,
		CustomerID: payment.CustomerID, Amount: payment.Amount, Currency: payment.Currency,
		Provider: payment.Provider, ProviderTradeNo: optionalString(state.ProviderTradeNo),
		ProviderStatus: providerStatus, PaidAt: state.PaidAt, ProviderRequestID: state.RequestID,
		ReconcileAttempts: payment.ReconcileAttempts,
	}

	if mismatchCode := s.paymentProviderMismatch(payment, state, providerStatus); mismatchCode != "" {
		reject := problem.Conflict("PAYMENT_PROVIDER_DATA_MISMATCH", "provider payment data does not match local payment")
		fact.FailureCode = stringPtr(mismatchCode)
		if err := handler.ApplyException(ctx, tx, fact, mismatchCode); err != nil {
			return Payment{}, nil, err
		}
		if err := s.repo.UpdatePayment(ctx, tx, payment.ID, map[string]any{
			"status": "exception", "provider_status": optionalString(state.Status),
			"failure_code": mismatchCode, "next_reconcile_at": nil,
			"version": gorm.Expr("version + 1"),
		}); err != nil {
			return Payment{}, nil, err
		}
		payment.Status, payment.ProviderStatus, payment.FailureCode, payment.NextReconcileAt = "exception", optionalString(state.Status), stringPtr(mismatchCode), nil
		return payment, reject, nil
	}

	switch providerStatus {
	case "SUCCESS":
		if payment.Status == "succeeded" {
			return payment, nil, nil
		}
		if err := handler.ApplySuccess(ctx, tx, fact); err != nil {
			return Payment{}, nil, err
		}
		if err := s.repo.UpdatePayment(ctx, tx, payment.ID, map[string]any{
			"status": "succeeded", "provider_status": providerStatus,
			"provider_trade_no": optionalString(state.ProviderTradeNo), "paid_at": state.PaidAt,
			"failure_code": nil, "next_reconcile_at": nil,
			"version": gorm.Expr("version + 1"),
		}); err != nil {
			return Payment{}, nil, err
		}
		payment.Status, payment.ProviderStatus, payment.ProviderTradeNo, payment.PaidAt = "succeeded", stringPtr(providerStatus), optionalString(state.ProviderTradeNo), state.PaidAt
		payment.FailureCode, payment.NextReconcileAt = nil, nil
		return payment, nil, nil
	case "NOTPAY", "USERPAYING":
		next := nextPaymentReconcileAt(payment, time.Now())
		if err := s.repo.UpdatePayment(ctx, tx, payment.ID, map[string]any{
			"provider_status": providerStatus, "next_reconcile_at": next,
			"failure_code": nil, "version": gorm.Expr("version + 1"),
		}); err != nil {
			return Payment{}, nil, err
		}
		payment.ProviderStatus, payment.NextReconcileAt, payment.FailureCode = stringPtr(providerStatus), &next, nil
		return payment, nil, nil
	case "CLOSED", "REVOKED", "PAYERROR":
		localStatus := "closed"
		if providerStatus == "PAYERROR" {
			localStatus = "failed"
		}
		now := time.Now()
		fact.FailureCode = stringPtr(providerStatus)
		if err := handler.ApplyTerminal(ctx, tx, fact); err != nil {
			return Payment{}, nil, err
		}
		if err := s.repo.UpdatePayment(ctx, tx, payment.ID, map[string]any{
			"status": localStatus, "provider_status": providerStatus, "failed_at": &now,
			"failure_code": providerStatus, "next_reconcile_at": nil,
			"version": gorm.Expr("version + 1"),
		}); err != nil {
			return Payment{}, nil, err
		}
		payment.Status, payment.ProviderStatus, payment.FailedAt, payment.FailureCode, payment.NextReconcileAt = localStatus, stringPtr(providerStatus), &now, stringPtr(providerStatus), nil
		return payment, nil, nil
	default:
		return Payment{}, nil, problem.InvalidArgument("PAYMENT_PROVIDER_STATUS_INVALID", "unsupported provider payment status")
	}
}

func (s *Service) paymentProviderMismatch(payment Payment, state ProviderPaymentState, providerStatus string) string {
	amountPresent := state.AmountPresent || state.Amount != 0 || strings.TrimSpace(state.Currency) != ""
	bizType, _ := paymentBusiness(payment)
	if providerStatus == "SUCCESS" &&
		bizType != RetailOrderPaymentBusiness &&
		state.PaidAt == nil {
		return "PROVIDER_PAID_AT_MISSING"
	}
	if !s.cfg.WeChat.PayMockEnabled && (state.AppID != s.cfg.WeChat.MiniAppID || state.MchID != s.cfg.WeChat.PayMchID) {
		return "PROVIDER_IDENTITY_MISMATCH"
	}
	if state.PaymentNo != payment.PaymentNo ||
		(amountPresent && (state.Amount != payment.Amount || state.Currency != payment.Currency)) ||
		(providerStatus == "SUCCESS" && (!amountPresent || strings.TrimSpace(state.ProviderTradeNo) == "")) ||
		(payment.ProviderTradeNo != nil && *payment.ProviderTradeNo != "" && state.ProviderTradeNo != "" && *payment.ProviderTradeNo != state.ProviderTradeNo) {
		return "PROVIDER_DATA_MISMATCH"
	}
	return ""
}
