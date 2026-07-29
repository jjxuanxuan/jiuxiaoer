package order

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/paygateway"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

// BusinessPaymentCreateInput 是与非零售聚合在同一事务中创建的公共资金侧草稿。
// 价格、所有权、业务标识和过期时间必须已从可信服务端状态推导得出。
type BusinessPaymentCreateInput struct {
	PaymentID      uint64
	PaymentNo      string
	BizType        string
	BizID          uint64
	CustomerID     uint64
	Channel        string
	Provider       string
	Amount         int64
	Currency       string
	ExpiresAt      time.Time
	IdempotencyKey string
}

// CreateBusinessPaymentTx 持久化非零售公共支付记录。
// 调用方拥有外围业务事务，调用前必须按标准顺序锁定聚合。
func (s *Service) CreateBusinessPaymentTx(ctx context.Context, tx *gorm.DB, input BusinessPaymentCreateInput) (Payment, error) {
	input.PaymentNo = strings.TrimSpace(input.PaymentNo)
	input.BizType = strings.TrimSpace(input.BizType)
	input.Channel = strings.TrimSpace(input.Channel)
	input.Provider = strings.TrimSpace(input.Provider)
	input.Currency = strings.ToUpper(strings.TrimSpace(input.Currency))
	if tx == nil {
		return Payment{}, problem.Internal("business payment transaction is required")
	}
	if s.payment == nil || !s.cfg.WeChat.PayEnabled || input.Provider != s.payment.Code() {
		return Payment{}, problem.New(503, "PAYMENT_PROVIDER_UNAVAILABLE", "Service Unavailable", "payment provider is unavailable")
	}
	if input.PaymentID == 0 || input.BizID == 0 || input.CustomerID == 0 ||
		input.PaymentNo == "" || input.BizType == "" || input.BizType == RetailOrderPaymentBusiness ||
		input.Channel == "" || input.Amount <= 0 || input.Currency != "CNY" ||
		!input.ExpiresAt.After(time.Now()) {
		return Payment{}, problem.InvalidArgument("PAYMENT_BUSINESS_INPUT_INVALID", "invalid business payment input")
	}
	if s.paymentSettlements == nil {
		return Payment{}, problem.Internal("payment settlement registry is not initialized")
	}
	if _, ok := s.paymentSettlements.resolve(input.BizType); !ok {
		return Payment{}, problem.Internal("payment settlement handler is not registered")
	}
	bizID := input.BizID
	row := Payment{
		ID:             input.PaymentID,
		PaymentNo:      input.PaymentNo,
		BizType:        stringPtr(input.BizType),
		BizID:          &bizID,
		OrderID:        nil,
		CustomerID:     input.CustomerID,
		Channel:        input.Channel,
		Provider:       input.Provider,
		Status:         "creating",
		Amount:         input.Amount,
		Currency:       input.Currency,
		ExpiresAt:      &input.ExpiresAt,
		IdempotencyKey: optionalString(input.IdempotencyKey),
	}
	if err := s.repo.CreatePayment(ctx, tx, row); err != nil {
		return Payment{}, err
	}
	return row, nil
}

// SubmitBusinessPayment 在聚合和公共支付草稿提交后调用支付机构。
// 网络结果未知时保持 creating 状态，由共享对账任务完成收敛。
func (s *Service) SubmitBusinessPayment(ctx context.Context, paymentID uint64, openID, description string) (PaymentDTO, error) {
	if s.payment == nil || !s.cfg.WeChat.PayEnabled {
		return PaymentDTO{}, problem.New(503, "PAYMENT_PROVIDER_UNAVAILABLE", "Service Unavailable", "payment provider is unavailable")
	}
	payment, err := s.repo.GetPaymentByID(ctx, nil, paymentID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return PaymentDTO{}, problem.NotFound("PAYMENT_NOT_FOUND", "payment not found")
	}
	if err != nil {
		return PaymentDTO{}, err
	}
	bizType, _ := paymentBusiness(payment)
	if bizType == RetailOrderPaymentBusiness || payment.OrderID != nil {
		return PaymentDTO{}, problem.InvalidArgument("PAYMENT_BUSINESS_INPUT_INVALID", "retail payments use the order payment facade")
	}
	if payment.Status == "pending" && len(payment.ClientPayload) > 0 {
		return paymentDTO(payment), nil
	}
	if payment.Status != "creating" || payment.ExpiresAt == nil || !payment.ExpiresAt.After(time.Now()) {
		return PaymentDTO{}, problem.Conflict("PAYMENT_INVALID_STATUS", "payment cannot be submitted")
	}
	if strings.TrimSpace(openID) == "" {
		return PaymentDTO{}, problem.Conflict("WECHAT_IDENTITY_REQUIRED", "wechat identity is required for payment")
	}
	if strings.TrimSpace(description) == "" {
		description = s.cfg.WeChat.PayDescription
	}

	providerCtx, cancel := context.WithTimeout(ctx, s.cfg.WeChat.HTTPTimeout)
	providerResult, providerErr := s.payment.Create(providerCtx, CreateProviderPaymentInput{
		PaymentNo: payment.PaymentNo, Description: description, Amount: payment.Amount,
		Currency: payment.Currency, OpenID: openID, ExpiresAt: *payment.ExpiresAt,
	})
	cancel()
	if providerErr != nil {
		s.logPaymentProviderFailure(payment.PaymentNo, providerErr, "reconcile_or_retry")
		if s.metrics != nil {
			s.metrics.IncPayment(payment.Provider, "business_create_failed")
		}
		next := time.Now()
		_ = s.repo.DB().WithContext(context.WithoutCancel(ctx)).Transaction(func(tx *gorm.DB) error {
			locked, lockErr := s.repo.LockPaymentByID(context.WithoutCancel(ctx), tx, payment.ID)
			if lockErr != nil {
				return lockErr
			}
			if locked.Status != "creating" {
				return nil
			}
			values := map[string]any{
				"next_reconcile_at": &next,
				"failure_code":      paygateway.Code(providerErr, "PROVIDER_UNAVAILABLE"),
				"version":           gorm.Expr("version + 1"),
			}
			if !paygateway.Retryable(providerErr) {
				values["status"] = "failed"
				values["failed_at"] = &next
				values["next_reconcile_at"] = nil
			}
			return s.repo.UpdatePayment(context.WithoutCancel(ctx), tx, payment.ID, values)
		})
		message := "payment creation was rejected by the provider"
		if paygateway.Retryable(providerErr) {
			message = "payment creation result is unknown; backend reconciliation has been scheduled"
		}
		detail := problem.New(503, "PAYMENT_PROVIDER_UNAVAILABLE", "Service Unavailable", message)
		detail.Data = map[string]any{
			"retryable":           paygateway.Retryable(providerErr),
			"provider_code":       paygateway.Code(providerErr, "PROVIDER_UNAVAILABLE"),
			"provider_request_id": paygateway.RequestID(providerErr),
		}
		return PaymentDTO{}, detail
	}

	payload := jsonData(providerResult.ClientPayload)
	nextReconcileAt := nextPaymentReconcileAt(payment, time.Now())
	var result PaymentDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, err := s.repo.LockPaymentByID(ctx, tx, payment.ID)
		if err != nil {
			return err
		}
		if !samePaymentBusiness(payment, locked) {
			return problem.Internal("payment business registry changed during provider submission")
		}
		if locked.Status == "pending" && len(locked.ClientPayload) > 0 {
			result = paymentDTO(locked)
			return nil
		}
		if locked.Status != "creating" {
			return problem.Conflict("PAYMENT_INVALID_STATUS", "payment cannot accept provider submission result")
		}
		if err := s.repo.UpdatePayment(ctx, tx, locked.ID, map[string]any{
			"status":             "pending",
			"provider_status":    optionalString(providerResult.Status),
			"provider_prepay_id": optionalString(providerResult.ProviderPrepayID),
			"provider_trade_no":  optionalString(providerResult.ProviderTradeNo),
			"client_payload":     payload,
			"next_reconcile_at":  nextReconcileAt,
			"failure_code":       nil,
			"version":            gorm.Expr("version + 1"),
		}); err != nil {
			return err
		}
		locked.Status = "pending"
		locked.ProviderStatus = optionalString(providerResult.Status)
		locked.ProviderPrepayID = optionalString(providerResult.ProviderPrepayID)
		locked.ProviderTradeNo = optionalString(providerResult.ProviderTradeNo)
		locked.ClientPayload = payload
		result = paymentDTO(locked)
		businessType, businessID := paymentBusiness(locked)
		return s.createOutbox(ctx, tx, "payment.created", "payment", locked.ID, map[string]any{
			"payment_id": idString(locked.ID), "biz_type": businessType,
			"biz_id": idString(businessID), "provider": locked.Provider,
			"provider_request_id": providerResult.RequestID,
		})
	})
	if err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), s.cfg.WeChat.HTTPTimeout)
		closeResult, closeErr := s.payment.Close(closeCtx, payment.PaymentNo)
		cancel()
		if closeErr != nil {
			s.logPaymentProviderFailure(payment.PaymentNo, closeErr, "manual_investigation")
			if s.metrics != nil {
				s.metrics.IncPayment(payment.Provider, "business_create_compensation_failed")
			}
		} else {
			s.log.Info("payment provider call completed", slog.String("operation", "payment.close"), slog.String("payment_no", payment.PaymentNo), slog.String("provider_request_id", closeResult.RequestID))
		}
		return PaymentDTO{}, err
	}
	if s.metrics != nil {
		s.metrics.IncPayment(payment.Provider, "business_create_succeeded")
	}
	return result, nil
}

// BusinessPayment 向所属业务服务返回资金状态，
// 不暴露可变仓储句柄。
func (s *Service) BusinessPayment(ctx context.Context, customerID uint64, bizType string, bizID uint64) (Payment, error) {
	if s.payment == nil {
		return Payment{}, problem.New(503, "PAYMENT_PROVIDER_UNAVAILABLE", "Service Unavailable", "payment provider is unavailable")
	}
	row, err := s.repo.GetBusinessPayment(ctx, strings.TrimSpace(bizType), bizID, s.payment.Code())
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && row.CustomerID != customerID) {
		return Payment{}, problem.NotFound("PAYMENT_NOT_FOUND", "payment not found")
	}
	return row, err
}

// ConfirmBusinessPayment 对客户持有的非零售支付执行可信支付机构查询，
// 并通过已注册结算处理器路由已验证状态。
// 幂等性由所属业务接口负责。
func (s *Service) ConfirmBusinessPayment(ctx context.Context, customerID uint64, bizType string, bizID uint64) (PaymentDTO, error) {
	payment, err := s.BusinessPayment(ctx, customerID, bizType, bizID)
	if err != nil {
		return PaymentDTO{}, err
	}
	resolvedType, resolvedID := paymentBusiness(payment)
	if resolvedType == RetailOrderPaymentBusiness || resolvedType != strings.TrimSpace(bizType) || resolvedID != bizID || payment.OrderID != nil {
		return PaymentDTO{}, problem.Internal("payment business registry is inconsistent")
	}
	return s.confirmCustomerPayment(ctx, customerID, payment)
}
