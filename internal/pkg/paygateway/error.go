package paygateway

import (
	"errors"
	"fmt"
)

// Error is the provider-neutral form of a payment gateway failure. It keeps
// business modules independent from a concrete SDK while preserving the
// provider code and request ID needed for safe retries and support diagnostics.
type ProviderError struct {
	Operation  string
	HTTPStatus int
	Code       string
	Message    string
	RequestID  string
	Retryable  bool
	Cause      error
}

func (e *ProviderError) Error() string {
	if e == nil {
		return "payment gateway call failed"
	}
	message := e.Message
	if message == "" {
		message = "payment gateway call failed"
	}
	return fmt.Sprintf("%s: status=%d code=%s request_id=%s message=%s", e.Operation, e.HTTPStatus, e.Code, e.RequestID, message)
}

func (e *ProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// As returns gateway metadata when err contains a provider-neutral Error.
func As(err error) (*ProviderError, bool) {
	var target *ProviderError
	if !errors.As(err, &target) {
		return nil, false
	}
	return target, true
}

// IsCode reports whether the gateway returned the given stable provider code.
func IsCode(err error, code string) bool {
	target, ok := As(err)
	return ok && target.Code == code
}

// Code returns a stable provider code, or fallback when the failure had no
// structured provider response.
func Code(err error, fallback string) string {
	if target, ok := As(err); ok && target.Code != "" {
		return target.Code
	}
	return fallback
}

// Retryable treats unknown transport failures as retryable. Explicit provider
// failures override that default.
func Retryable(err error) bool {
	if target, ok := As(err); ok {
		return target.Retryable
	}
	return true
}

// RequestID returns the provider request ID when one was supplied.
func RequestID(err error) string {
	if target, ok := As(err); ok {
		return target.RequestID
	}
	return ""
}
