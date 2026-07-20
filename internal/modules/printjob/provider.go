package printjob

import (
	"context"
	"errors"
)

type ProviderError struct {
	Code      string
	Retryable bool
	Unknown   bool
}

// Error 返回当前错误的文本描述。
func (e *ProviderError) Error() string { return e.Code }

// FakeProvider is deterministic and side-effect free. It is used for local,
// integration and fault-injection tests until an approved device adapter is configured.
type FakeProvider struct {
	Failure      *ProviderError
	QueryFailure *ProviderError
}

// Submit 返回Submit。
func (p *FakeProvider) Submit(_ context.Context, req PrintRequest) (PrintResult, error) {
	if p.Failure != nil {
		return PrintResult{}, p.Failure
	}
	if req.ProviderRequestID == "" {
		return PrintResult{}, errors.New("provider request id is required")
	}
	return PrintResult{ProviderRequestID: req.ProviderRequestID, Status: "succeeded"}, nil
}

// Query 查询打印结果。
func (p *FakeProvider) Query(_ context.Context, providerRequestID string) (PrintResult, error) {
	if p.QueryFailure != nil {
		return PrintResult{}, p.QueryFailure
	}
	if providerRequestID == "" {
		return PrintResult{}, errors.New("provider request id is required")
	}
	return PrintResult{ProviderRequestID: providerRequestID, Status: "succeeded"}, nil
}

type UnavailableProvider struct{}

// Submit 返回Submit。
func (*UnavailableProvider) Submit(context.Context, PrintRequest) (PrintResult, error) {
	return PrintResult{}, &ProviderError{Code: "provider_not_configured", Retryable: false}
}

// Query 查询打印结果。
func (*UnavailableProvider) Query(context.Context, string) (PrintResult, error) {
	return PrintResult{}, &ProviderError{Code: "provider_not_configured", Retryable: false}
}
