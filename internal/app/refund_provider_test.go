package app

import (
	"context"
	"errors"
	"net/http"
	"testing"

	sharedrefund "jiuxiaoer-admin/backend-go/internal/modules/refund"
)

type applicationRefundProviderStub struct{}

func (*applicationRefundProviderStub) Code() string { return "wechat" }

func (*applicationRefundProviderStub) Refund(
	context.Context,
	sharedrefund.Input,
) (sharedrefund.State, error) {
	return sharedrefund.State{}, errors.New("not used")
}

func (*applicationRefundProviderStub) QueryRefund(
	context.Context,
	string,
) (sharedrefund.State, error) {
	return sharedrefund.State{}, errors.New("not used")
}

func (*applicationRefundProviderStub) ParseRefundCallback(
	context.Context,
	*http.Request,
) (sharedrefund.CallbackEvent, error) {
	return sharedrefund.CallbackEvent{}, errors.New("not used")
}

func TestApplicationRefundProviderPreservesBaseWhenWineTicketDisabled(
	t *testing.T,
) {
	base := &applicationRefundProviderStub{}

	got := applicationRefundProvider(false, nil, base)

	if got != base {
		t.Fatalf("wine-ticket disabled provider=%T, want original %T", got, base)
	}
}
