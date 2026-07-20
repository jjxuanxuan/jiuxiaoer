package problem

import (
	"errors"
	"net/http"
)

// Details 是 response.Error 使用的统一错误响应结构。
// ErrorCode 面向客户端保持稳定，Detail 只放安全、简短的诊断信息。
type Details struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Instance  string `json:"instance,omitempty"`
	ErrorCode string `json:"error_code"`
	RequestID string `json:"request_id,omitempty"`
	Data      any    `json:"data,omitempty"`
}

// Error 返回当前错误的文本描述。
func (d *Details) Error() string {
	if d.Detail != "" {
		return d.Detail
	}
	return d.Title
}

// New 创建并初始化详情。
func New(status int, code string, title string, detail string) *Details {
	return &Details{
		Type:      "https://api.jiuxiaoer.com/problems/" + code,
		Title:     title,
		Status:    status,
		Detail:    detail,
		ErrorCode: code,
	}
}

// Internal 创建并返回内部错误的问题详情。
func Internal(detail string) *Details {
	return New(http.StatusInternalServerError, "INTERNAL_ERROR", "Internal Error", detail)
}

// InvalidArgument 创建并返回参数无效的问题详情。
func InvalidArgument(code string, detail string) *Details {
	return New(http.StatusBadRequest, code, "Invalid Argument", detail)
}

// Unauthorized 创建并返回未认证的问题详情。
func Unauthorized(code string, detail string) *Details {
	return New(http.StatusUnauthorized, code, "Unauthorized", detail)
}

// Forbidden 创建并返回无权限的问题详情。
func Forbidden(code string, detail string) *Details {
	return New(http.StatusForbidden, code, "Forbidden", detail)
}

// NotFound 创建并返回资源未找到的问题详情。
func NotFound(code string, detail string) *Details {
	return New(http.StatusNotFound, code, "Not Found", detail)
}

// Conflict 创建并返回资源冲突的问题详情。
func Conflict(code string, detail string) *Details {
	return New(http.StatusConflict, code, "Conflict", detail)
}

// TooManyRequests 创建并返回请求过多的问题详情。
func TooManyRequests(code string, detail string) *Details {
	return New(http.StatusTooManyRequests, code, "Too Many Requests", detail)
}

// RequestTooLarge 创建并返回请求体过大的问题详情。
func RequestTooLarge(code string, detail string) *Details {
	return New(http.StatusRequestEntityTooLarge, code, "Request Entity Too Large", detail)
}

// GatewayTimeout 创建并返回网关超时的问题详情。
func GatewayTimeout(code string, detail string) *Details {
	return New(http.StatusGatewayTimeout, code, "Gateway Timeout", detail)
}

// FromError 保留业务错误细节，并把未知错误收敛为 INTERNAL_ERROR。
func FromError(err error) *Details {
	if err == nil {
		return nil
	}

	var details *Details
	if errors.As(err, &details) {
		return details
	}

	return Internal("internal server error")
}
