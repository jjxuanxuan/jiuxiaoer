package notification

import "context"

type ProviderError struct {
	Code      string
	Retryable bool
	Unknown   bool
}

// Error 返回当前错误的文本描述。
func (e *ProviderError) Error() string { return e.Code }

type FakeProvider struct{ Failure *ProviderError }

// Send 发送Send 结果。
func (p *FakeProvider) Send(_ context.Context, req SendRequest) (SendResult, error) {
	if p.Failure != nil {
		return SendResult{}, p.Failure
	}
	return SendResult{ProviderRequestID: req.ProviderRequestID, Status: "succeeded"}, nil
}

// Query 查询Send 结果。
func (p *FakeProvider) Query(_ context.Context, providerRequestID string) (SendResult, error) {
	if p.Failure != nil && !p.Failure.Unknown {
		return SendResult{}, p.Failure
	}
	return SendResult{ProviderRequestID: providerRequestID, Status: "succeeded"}, nil
}

type UnavailableProvider struct{}

// Send 发送Send 结果。
func (*UnavailableProvider) Send(context.Context, SendRequest) (SendResult, error) {
	return SendResult{}, &ProviderError{Code: "provider_not_configured", Retryable: false}
}

// Query 查询Send 结果。
func (*UnavailableProvider) Query(context.Context, string) (SendResult, error) {
	return SendResult{}, &ProviderError{Code: "provider_not_configured", Retryable: false}
}
