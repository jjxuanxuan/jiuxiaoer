package response

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

const (
	RequestIDKey = "request_id"
	ErrorCodeKey = "error_code"
)

type Body struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	Data      any    `json:"data"`
	RequestID string `json:"request_id"`
}

type PageBody struct {
	Items         any    `json:"items"`
	NextPageToken string `json:"next_page_token,omitempty"`
}

// OK 使用稳定响应信封包装所有成功响应。
func OK(c *gin.Context, data any) {
	WithStatus(c, http.StatusOK, data)
}

// WithStatus 按指定 HTTP 状态码向客户端输出响应。
func WithStatus(c *gin.Context, status int, data any) {
	c.JSON(status, Body{
		Code:      0,
		Message:   "ok",
		Data:      data,
		RequestID: RequestID(c),
	})
}

// Empty 向客户端输出不包含业务数据的响应。
func Empty(c *gin.Context) {
	OK(c, gin.H{})
}

// Page 向客户端输出分页响应。
func Page(c *gin.Context, items any, nextPageToken string) {
	OK(c, PageBody{Items: items, NextPageToken: nextPageToken})
}

// Error 将业务错误转换为 HTTP 响应，并补充请求上下文。
func Error(c *gin.Context, err error) {
	details := problem.FromError(err)
	if data, ok := details.Data.(map[string]any); ok {
		switch value := data["retry_after_seconds"].(type) {
		case int:
			c.Header("Retry-After", strconv.Itoa(value))
		case int64:
			c.Header("Retry-After", strconv.FormatInt(value, 10))
		case float64:
			c.Header("Retry-After", strconv.Itoa(int(value)))
		}
	}
	details.Instance = c.Request.URL.Path
	details.RequestID = RequestID(c)
	c.Set(ErrorCodeKey, details.ErrorCode)
	c.JSON(details.Status, details)
}

// RequestID 返回当前上下文中的请求 ID。
func RequestID(c *gin.Context) string {
	value, ok := c.Get(RequestIDKey)
	if !ok {
		return ""
	}
	requestID, _ := value.(string)
	return requestID
}

// ErrorCode 返回当前响应中的业务错误码。
func ErrorCode(c *gin.Context) string {
	value, ok := c.Get(ErrorCodeKey)
	if !ok {
		return ""
	}
	errorCode, _ := value.(string)
	return errorCode
}
