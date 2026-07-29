package refund

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"jiuxiaoer-admin/backend-go/internal/modules/order"
	sharedrefund "jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/pkg/paygateway"
)

type refundProviderTestPaymentQuery struct {
	state order.ProviderPaymentState
	err   error
	calls int
}

func (q *refundProviderTestPaymentQuery) Query(
	context.Context,
	string,
) (order.ProviderPaymentState, error) {
	q.calls++
	return q.state, q.err
}

type refundProviderTestDelegate struct {
	calls int
	input sharedrefund.Input
}

func (p *refundProviderTestDelegate) Code() string { return "wechat" }
func (p *refundProviderTestDelegate) Refund(
	_ context.Context,
	input sharedrefund.Input,
) (sharedrefund.State, error) {
	p.calls++
	p.input = input
	return sharedrefund.State{
		RefundNo: input.RefundNo, PaymentNo: input.PaymentNo,
		Status: "PROCESSING", Amount: input.Amount,
		TotalAmount: input.TotalAmount, Currency: input.Currency,
	}, nil
}
func (p *refundProviderTestDelegate) QueryRefund(
	context.Context,
	string,
) (sharedrefund.State, error) {
	return sharedrefund.State{}, errors.New("not used")
}
func (p *refundProviderTestDelegate) ParseRefundCallback(
	context.Context,
	*http.Request,
) (sharedrefund.CallbackEvent, error) {
	return sharedrefund.CallbackEvent{}, errors.New("not used")
}

func TestVerifiedRefundProviderQueriesSuccessfulOriginalPaymentBeforeSubmit(t *testing.T) {
	query := &refundProviderTestPaymentQuery{state: order.ProviderPaymentState{
		ProviderTradeNo: "wx-pay-1", PaymentNo: "PAY-1", Status: "SUCCESS",
		Amount: 1200, Currency: "CNY", AmountPresent: true,
	}}
	delegate := &refundProviderTestDelegate{}
	provider := NewVerifiedWineTicketRefundProvider(query, delegate)
	input := sharedrefund.Input{
		RefundNo: "RF-1", PaymentNo: "PAY-1", ProviderTradeNo: "wx-pay-1",
		BusinessType: WineTicketPurchaseRefundBusiness,
		Amount:       1200, TotalAmount: 1200, Currency: "CNY",
	}
	state, err := provider.Refund(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if query.calls != 1 || delegate.calls != 1 || state.Status != "PROCESSING" {
		t.Fatalf("query_calls=%d refund_calls=%d state=%+v", query.calls, delegate.calls, state)
	}
}

func TestVerifiedRefundProviderFailsClosedOnPaymentMismatch(t *testing.T) {
	query := &refundProviderTestPaymentQuery{state: order.ProviderPaymentState{
		ProviderTradeNo: "wx-pay-1", PaymentNo: "PAY-1", Status: "SUCCESS",
		Amount: 1199, Currency: "CNY", AmountPresent: true,
	}}
	delegate := &refundProviderTestDelegate{}
	provider := NewVerifiedWineTicketRefundProvider(query, delegate)
	_, err := provider.Refund(context.Background(), sharedrefund.Input{
		RefundNo: "RF-1", PaymentNo: "PAY-1", ProviderTradeNo: "wx-pay-1",
		BusinessType: WineTicketPurchaseRefundBusiness,
		Amount:       1200, TotalAmount: 1200, Currency: "CNY",
	})
	if !paygateway.IsCode(err, "ORIGINAL_PAYMENT_AMOUNT_MISMATCH") ||
		paygateway.Retryable(err) || delegate.calls != 0 {
		t.Fatalf("error=%v retryable=%v refund_calls=%d", err, paygateway.Retryable(err), delegate.calls)
	}
}

func TestVerifiedRefundProviderRequiresMatchingProviderTransactionID(t *testing.T) {
	query := &refundProviderTestPaymentQuery{state: order.ProviderPaymentState{
		ProviderTradeNo: "wx-pay-other", PaymentNo: "PAY-1", Status: "SUCCESS",
		Amount: 1200, Currency: "CNY", AmountPresent: true,
	}}
	delegate := &refundProviderTestDelegate{}
	provider := NewVerifiedWineTicketRefundProvider(query, delegate)
	_, err := provider.Refund(context.Background(), sharedrefund.Input{
		RefundNo: "RF-1", PaymentNo: "PAY-1", ProviderTradeNo: "wx-pay-1",
		BusinessType: WineTicketPurchaseRefundBusiness,
		Amount:       1200, TotalAmount: 1200, Currency: "CNY",
	})
	if !paygateway.IsCode(err, "ORIGINAL_PAYMENT_PROVIDER_ID_MISMATCH") ||
		paygateway.Retryable(err) || delegate.calls != 0 {
		t.Fatalf(
			"error=%v retryable=%v refund_calls=%d",
			err,
			paygateway.Retryable(err),
			delegate.calls,
		)
	}
}

func TestVerifiedRefundProviderPreservesRetailProviderBehavior(t *testing.T) {
	query := &refundProviderTestPaymentQuery{
		err: errors.New("retail refunds must not query the wine-ticket fence"),
	}
	delegate := &refundProviderTestDelegate{}
	provider := NewVerifiedWineTicketRefundProvider(query, delegate)
	input := sharedrefund.Input{
		RefundNo:        "RF-RETAIL-1",
		PaymentNo:       "PAY-RETAIL-1",
		ProviderTradeNo: "wx-retail-1",
		BusinessType:    sharedrefund.RetailAfterSaleRefundBusiness,
		Amount:          800,
		TotalAmount:     1200,
		Currency:        "CNY",
	}

	state, err := provider.Refund(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if query.calls != 0 || delegate.calls != 1 {
		t.Fatalf(
			"query_calls=%d refund_calls=%d",
			query.calls,
			delegate.calls,
		)
	}
	if delegate.input != input || state.RefundNo != input.RefundNo {
		t.Fatalf(
			"delegate input/state changed: input=%+v state=%+v",
			delegate.input,
			state,
		)
	}
}
