package requestctx

import "context"

type contextKey string

const (
	requestIDKey contextKey = "request_id"
	ipKey        contextKey = "ip"
	userAgentKey contextKey = "user_agent"
)

// WithRequestID 把请求 ID 写入标准 context，供 service 层审计和 outbox 使用。
func WithRequestID(ctx context.Context, requestID string) context.Context {
	if requestID == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey, requestID)
}

// WithHTTPMeta 保存审计日志需要的 HTTP 元信息。
func WithHTTPMeta(ctx context.Context, ip string, userAgent string) context.Context {
	if ip != "" {
		ctx = context.WithValue(ctx, ipKey, ip)
	}
	if userAgent != "" {
		ctx = context.WithValue(ctx, userAgentKey, userAgent)
	}
	return ctx
}

// RequestID 返回当前上下文中的请求 ID。
func RequestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey).(string)
	return value
}

// IP 返回当前上下文中的客户端 IP。
func IP(ctx context.Context) string {
	value, _ := ctx.Value(ipKey).(string)
	return value
}

// UserAgent 返回当前上下文中的用户代理信息。
func UserAgent(ctx context.Context) string {
	value, _ := ctx.Value(userAgentKey).(string)
	return value
}

// RequestIDPtr 返回当前上下文中的请求 ID 指针。
func RequestIDPtr(ctx context.Context) *string {
	return stringPtr(RequestID(ctx))
}

// IPPtr 返回当前上下文中的客户端 IP 指针。
func IPPtr(ctx context.Context) *string {
	return stringPtr(IP(ctx))
}

// UserAgentPtr 返回当前上下文中的用户代理信息指针。
func UserAgentPtr(ctx context.Context) *string {
	return stringPtr(UserAgent(ctx))
}

// stringPtr 将非空字符串转换为字符串指针。
func stringPtr(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
