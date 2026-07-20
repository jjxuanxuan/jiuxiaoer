package order

import (
	"context"
	"net/http"
	"time"
)

type CreateProviderPaymentInput struct {
	PaymentNo   string
	Description string
	Amount      int64
	Currency    string
	OpenID      string
	ExpiresAt   time.Time
}

type ProviderPaymentResult struct {
	ProviderTradeNo  string
	ProviderPrepayID string
	Status           string
	RequestID        string
	ClientPayload    map[string]any
}

type ProviderPaymentState struct {
	ProviderTradeNo string
	PaymentNo       string
	Status          string
	AppID           string
	MchID           string
	Amount          int64
	Currency        string
	AmountPresent   bool
	RequestID       string
	PaidAt          *time.Time
}

type ProviderOperationResult struct {
	RequestID string
}

type PaymentCallbackEvent struct {
	EventID         string
	ProviderTradeNo string
	PaymentNo       string
	Status          string
	Amount          int64
	Currency        string
	PaidAt          *time.Time
	AppID           string
	MchID           string
}

type PaymentProvider interface {
	Code() string
	Create(ctx context.Context, input CreateProviderPaymentInput) (ProviderPaymentResult, error)
	Query(ctx context.Context, paymentNo string) (ProviderPaymentState, error)
	Close(ctx context.Context, paymentNo string) (ProviderOperationResult, error)
	ParseCallback(ctx context.Context, request *http.Request) (PaymentCallbackEvent, error)
	Shutdown()
}
