package app

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
	"jiuxiaoer-admin/backend-go/internal/pkg/securevalue"
)

// requestIDMiddleware 返回请求ID中间件。
func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 如果上游已经传入请求 ID，则复用它，便于网关日志和 API 日志串联。
		requestID := strings.TrimSpace(c.GetHeader("X-Request-ID"))
		if len(requestID) > 128 || strings.ContainsAny(requestID, "\r\n") {
			requestID = ""
		}
		if requestID == "" {
			requestID = "req_" + uuid.NewString()
		}
		c.Set(response.RequestIDKey, requestID)
		c.Header("X-Request-ID", requestID)
		ctx := requestctx.WithRequestID(c.Request.Context(), requestID)
		ctx = requestctx.WithHTTPMeta(ctx, c.ClientIP(), c.Request.UserAgent())
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// accessLogMiddleware 返回访问日志中间件。
func accessLogMiddleware(log *slog.Logger, registry *metrics.Registry) gin.HandlerFunc {
	return func(c *gin.Context) {
		startedAt := time.Now()
		c.Next()

		latency := time.Since(startedAt)
		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		registry.ObserveHTTP(c.Request.Method, route, c.Writer.Status(), response.ErrorCode(c), latency)
		ipHash := ""
		if ip := strings.TrimSpace(c.ClientIP()); ip != "" {
			ipHash = securevalue.Digest(ip)
		}
		log.Info("http request",
			slog.String("request_id", response.RequestID(c)),
			slog.String("method", c.Request.Method),
			slog.String("path", c.Request.URL.Path),
			slog.Int("status", c.Writer.Status()),
			slog.Int64("latency_ms", latency.Milliseconds()),
			slog.String("account_id", accountID(c)),
			slog.String("account_type", accountType(c)),
			slog.String("ip_hash", ipHash),
			slog.String("user_agent", c.Request.UserAgent()),
			slog.String("error_code", response.ErrorCode(c)),
		)
	}
}

// requestLimitsMiddleware 返回请求 Limits 中间件。
func requestLimitsMiddleware(timeout time.Duration, maxBodyBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.ContentLength > maxBodyBytes {
			response.Error(c, problem.RequestTooLarge("REQUEST_TOO_LARGE", "request body exceeds configured limit"))
			c.Abort()
			return
		}
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
		}
		// WebSocket 按设计是长连接。保留请求头和请求体限制，但不要把普通
		// API 请求截止时间附加到升级后的连接；心跳和会话截止时间由实时运行时管理。
		if strings.EqualFold(c.GetHeader("Upgrade"), "websocket") && strings.Contains(strings.ToLower(c.GetHeader("Connection")), "upgrade") {
			c.Next()
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
		if ctx.Err() == context.DeadlineExceeded && !c.Writer.Written() {
			response.Error(c, problem.GatewayTimeout("REQUEST_TIMEOUT", "request deadline exceeded"))
			c.Abort()
		}
	}
}

// accountID 返回账户ID。
func accountID(c *gin.Context) string {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok || claims == nil {
		return ""
	}
	return claims.AccountID
}

// accountType 返回账户 Type。
func accountType(c *gin.Context) string {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok || claims == nil {
		return ""
	}
	return claims.AccountType
}

// recoveryMiddleware 返回异常恢复中间件。
func recoveryMiddleware(log *slog.Logger) gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered any) {
		// panic 会被转换为 problem 响应；后续可在这里补充堆栈采集。
		log.Error("panic recovered",
			slog.String("request_id", response.RequestID(c)),
			slog.Any("panic", recovered),
		)
		response.Error(c, problem.Internal("internal server error"))
		c.Abort()
	})
}
