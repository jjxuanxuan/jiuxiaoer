package requestctx

import (
	"context"
	"strconv"

	"jiuxiaoer-admin/backend-go/internal/pkg/securevalue"
)

type contextKey string

const (
	requestIDKey contextKey = "request_id"
	ipKey        contextKey = "ip"
	userAgentKey contextKey = "user_agent"
	accountIDKey contextKey = "account_id"
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

// WithAccountID 附加已认证的账户 ID（不是角色对象 ID），
// 以便审计写入器持久化稳定的跨角色身份。
func WithAccountID(ctx context.Context, accountID string) context.Context {
	if accountID == "" {
		return ctx
	}
	return context.WithValue(ctx, accountIDKey, accountID)
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

// AccountID 在请求通过认证中间件后返回已认证的账户 ID。
// 匿名请求和系统任务返回零。
func AccountID(ctx context.Context) uint64 {
	value, _ := ctx.Value(accountIDKey).(string)
	id, _ := strconv.ParseUint(value, 10, 64)
	return id
}

// RequestIDPtr 返回当前上下文中的请求 ID 指针。
func RequestIDPtr(ctx context.Context) *string {
	return stringPtr(RequestID(ctx))
}

// IPPtr 返回当前上下文中的客户端 IP 指针。
func IPPtr(ctx context.Context) *string {
	return stringPtr(IP(ctx))
}

// IPHashPtr 是新审计记录中唯一允许使用的 IP 表示形式。
func IPHashPtr(ctx context.Context) *string {
	ip := IP(ctx)
	if ip == "" {
		return nil
	}
	hash := securevalue.Digest(ip)
	return &hash
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
