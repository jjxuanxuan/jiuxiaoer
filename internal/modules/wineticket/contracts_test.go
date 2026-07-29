package wineticket

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"jiuxiaoer-admin/backend-go/internal/modules/order"
	sharedrefund "jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/renewal"
)

type contractPaymentQuery struct {
	state order.ProviderPaymentState
	calls int
}

func (q *contractPaymentQuery) Query(
	context.Context,
	string,
) (order.ProviderPaymentState, error) {
	q.calls++
	return q.state, nil
}

type contractRefundProvider struct {
	calls int
	input sharedrefund.Input
}

func (*contractRefundProvider) Code() string { return "wechat" }

func (p *contractRefundProvider) Refund(
	_ context.Context,
	input sharedrefund.Input,
) (sharedrefund.State, error) {
	p.calls++
	p.input = input
	return sharedrefund.State{RefundNo: input.RefundNo}, nil
}

func (*contractRefundProvider) QueryRefund(
	context.Context,
	string,
) (sharedrefund.State, error) {
	return sharedrefund.State{}, errors.New("not used")
}

func (*contractRefundProvider) ParseRefundCallback(
	context.Context,
	*http.Request,
) (sharedrefund.CallbackEvent, error) {
	return sharedrefund.CallbackEvent{}, errors.New("not used")
}

func TestVerifiedRefundContractFencesRenewalAndPreservesRetail(t *testing.T) {
	paymentNo := "PAY-CONTRACT-1"
	providerTradeNo := "WX-CONTRACT-1"
	query := &contractPaymentQuery{state: order.ProviderPaymentState{
		PaymentNo:       paymentNo,
		ProviderTradeNo: providerTradeNo,
		Status:          "SUCCESS",
		Amount:          1500,
		Currency:        "CNY",
		AmountPresent:   true,
	}}
	delegate := &contractRefundProvider{}
	provider := NewVerifiedWineTicketRefundProvider(query, delegate)

	renewalInput := sharedrefund.Input{
		RefundNo:        "RF-RENEWAL-1",
		PaymentNo:       paymentNo,
		ProviderTradeNo: providerTradeNo,
		BusinessType:    renewal.RenewalCompensationRefundBusiness,
		Amount:          500,
		TotalAmount:     1500,
		Currency:        "CNY",
	}
	if _, err := provider.Refund(context.Background(), renewalInput); err != nil {
		t.Fatal(err)
	}
	if query.calls != 1 || delegate.calls != 1 ||
		delegate.input != renewalInput {
		t.Fatalf(
			"renewal query_calls=%d refund_calls=%d input=%+v",
			query.calls,
			delegate.calls,
			delegate.input,
		)
	}

	retailInput := sharedrefund.Input{
		RefundNo:        "RF-RETAIL-1",
		PaymentNo:       "PAY-RETAIL-1",
		ProviderTradeNo: "WX-RETAIL-1",
		BusinessType:    sharedrefund.RetailAfterSaleRefundBusiness,
		Amount:          800,
		TotalAmount:     1200,
		Currency:        "CNY",
	}
	if _, err := provider.Refund(context.Background(), retailInput); err != nil {
		t.Fatal(err)
	}
	if query.calls != 1 || delegate.calls != 2 ||
		delegate.input != retailInput {
		t.Fatalf(
			"retail query_calls=%d refund_calls=%d input=%+v",
			query.calls,
			delegate.calls,
			delegate.input,
		)
	}
}
