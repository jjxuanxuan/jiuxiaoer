package refund

import (
	"context"
	"net/http"
	"time"
)

type Input struct {
	RefundNo, PaymentNo, ProviderTradeNo, Reason, NotifyURL, Currency string
	Amount, TotalAmount                                               int64
}
type State struct {
	ProviderRefundID, RefundNo, PaymentNo, Status, Currency string
	Amount, TotalAmount                                     int64
	SucceededAt                                             *time.Time
}
type CallbackEvent struct {
	EventID      string
	AppID, MchID string
	State        State
}
type Provider interface {
	Code() string
	Refund(context.Context, Input) (State, error)
	QueryRefund(context.Context, string) (State, error)
	ParseRefundCallback(context.Context, *http.Request) (CallbackEvent, error)
}
