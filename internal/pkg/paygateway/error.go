package paygateway

import (
	"errors"
	"fmt"
)

// Error 是与服务商无关的支付网关失败形式。它让业务模块不依赖具体 SDK，
// 同时保留安全重试和支持诊断所需的服务商代码与请求 ID。
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

// As 在 err 包含服务商无关的 Error 时返回网关元数据。
func As(err error) (*ProviderError, bool) {
	var target *ProviderError
	if !errors.As(err, &target) {
		return nil, false
	}
	return target, true
}

// IsCode 判断网关是否返回了指定的稳定服务商代码。
func IsCode(err error, code string) bool {
	target, ok := As(err)
	return ok && target.Code == code
}

// Code 返回稳定的服务商代码；如果失败没有结构化服务商响应，则返回 fallback。
func Code(err error, fallback string) string {
	if target, ok := As(err); ok && target.Code != "" {
		return target.Code
	}
	return fallback
}

// Retryable 默认将未知传输失败视为可重试；明确的服务商失败会覆盖此默认行为。
func Retryable(err error) bool {
	if target, ok := As(err); ok {
		return target.Retryable
	}
	return true
}

// RequestID 返回服务商提供的请求 ID；未提供时返回空值。
func RequestID(err error) string {
	if target, ok := As(err); ok {
		return target.RequestID
	}
	return ""
}
