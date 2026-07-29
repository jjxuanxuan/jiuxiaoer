package refund

import (
	"context"
	"net/http"
	"strings"

	"jiuxiaoer-admin/backend-go/internal/modules/order"
	sharedrefund "jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/pkg/paygateway"
)

// RefundOriginalPaymentQuerier 是提交退款前必须使用的 APIv3 交易查询能力。
// 生产微信支付实现会同时实现本接口和 sharedrefund.Provider。
type RefundOriginalPaymentQuerier interface {
	Query(context.Context, string) (order.ProviderPaymentState, error)
}

// VerifiedWineTicketRefundProvider 将 APIv3 原支付事实作为退款提交防线，
// 回调和退款查询仍原样委托给底层实现。
//
// 该包装器可安全用作应用的共享退款实现：只有明确注册的酒票业务路由
// 会经过这道防线，零售及无关路由保持原样委托。
// 查询传输失败仍可重试；明确的非 SUCCESS 状态或金额、币种不匹配
// 属于永久性支付机构预检异常，因此绝不会释放酒票冻结。
type VerifiedWineTicketRefundProvider struct {
	payments              RefundOriginalPaymentQuerier
	refunds               sharedrefund.Provider
	verifiedBusinessTypes map[string]struct{}
}

var _ sharedrefund.Provider = (*VerifiedWineTicketRefundProvider)(nil)

func NewVerifiedWineTicketRefundProvider(
	payments RefundOriginalPaymentQuerier,
	refunds sharedrefund.Provider,
	businessTypes ...string,
) *VerifiedWineTicketRefundProvider {
	if len(businessTypes) == 0 {
		businessTypes = []string{WineTicketPurchaseRefundBusiness}
	}
	verifiedBusinessTypes := make(map[string]struct{}, len(businessTypes))
	for _, businessType := range businessTypes {
		if normalized := strings.TrimSpace(businessType); normalized != "" {
			verifiedBusinessTypes[normalized] = struct{}{}
		}
	}
	return &VerifiedWineTicketRefundProvider{
		payments:              payments,
		refunds:               refunds,
		verifiedBusinessTypes: verifiedBusinessTypes,
	}
}

func (p *VerifiedWineTicketRefundProvider) Code() string {
	if p == nil || p.refunds == nil {
		return ""
	}
	return p.refunds.Code()
}

func (p *VerifiedWineTicketRefundProvider) Refund(
	ctx context.Context,
	input sharedrefund.Input,
) (sharedrefund.State, error) {
	if p == nil || p.refunds == nil {
		return sharedrefund.State{}, refundPreflightError(
			"REFUND_PROVIDER_UNAVAILABLE",
			"refund provider is unavailable",
			true,
		)
	}
	if _, verified := p.verifiedBusinessTypes[strings.TrimSpace(input.BusinessType)]; !verified {
		return p.refunds.Refund(ctx, input)
	}
	if p.payments == nil {
		return sharedrefund.State{}, refundPreflightError(
			"REFUND_PROVIDER_UNAVAILABLE",
			"original payment query provider is unavailable",
			true,
		)
	}
	state, err := p.payments.Query(ctx, input.PaymentNo)
	if err != nil {
		return sharedrefund.State{}, err
	}
	if strings.ToUpper(strings.TrimSpace(state.Status)) != "SUCCESS" {
		return sharedrefund.State{}, refundPreflightError(
			"ORIGINAL_PAYMENT_NOT_SUCCESS",
			"the original WeChat Pay transaction is not successful",
			false,
		)
	}
	if state.PaymentNo != input.PaymentNo {
		return sharedrefund.State{}, refundPreflightError(
			"ORIGINAL_PAYMENT_MISMATCH",
			"the queried WeChat Pay transaction number does not match",
			false,
		)
	}
	if strings.TrimSpace(input.ProviderTradeNo) == "" ||
		strings.TrimSpace(state.ProviderTradeNo) == "" ||
		state.ProviderTradeNo != input.ProviderTradeNo {
		return sharedrefund.State{}, refundPreflightError(
			"ORIGINAL_PAYMENT_PROVIDER_ID_MISMATCH",
			"the queried WeChat Pay transaction id does not match",
			false,
		)
	}
	if !state.AmountPresent || state.Amount != input.TotalAmount ||
		state.Currency == "" || state.Currency != input.Currency {
		return sharedrefund.State{}, refundPreflightError(
			"ORIGINAL_PAYMENT_AMOUNT_MISMATCH",
			"the queried WeChat Pay transaction amount or currency does not match",
			false,
		)
	}
	return p.refunds.Refund(ctx, input)
}

func (p *VerifiedWineTicketRefundProvider) QueryRefund(
	ctx context.Context,
	refundNo string,
) (sharedrefund.State, error) {
	if p == nil || p.refunds == nil {
		return sharedrefund.State{}, refundPreflightError(
			"REFUND_PROVIDER_UNAVAILABLE",
			"refund provider is unavailable",
			true,
		)
	}
	return p.refunds.QueryRefund(ctx, refundNo)
}

func (p *VerifiedWineTicketRefundProvider) ParseRefundCallback(
	ctx context.Context,
	request *http.Request,
) (sharedrefund.CallbackEvent, error) {
	if p == nil || p.refunds == nil {
		return sharedrefund.CallbackEvent{}, refundPreflightError(
			"REFUND_PROVIDER_UNAVAILABLE",
			"refund provider is unavailable",
			true,
		)
	}
	return p.refunds.ParseRefundCallback(ctx, request)
}

func refundPreflightError(code, message string, retryable bool) error {
	return &paygateway.ProviderError{
		Operation: "refund.original_payment_query",
		Code:      code, Message: message, Retryable: retryable,
	}
}
