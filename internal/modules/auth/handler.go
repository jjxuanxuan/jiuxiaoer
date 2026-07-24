package auth

import (
	"strings"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct {
	service *Service
}

// NewHandler 创建并初始化Handler。
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// RegisterRoutes 注册Routes。
func RegisterRoutes(router *gin.RouterGroup, handler *Handler) {
	if handler.service.cfg.SMS.Enabled && handler.service.sms != nil {
		router.POST("/customer/send-code", handler.SendCustomerCode)
		router.POST("/customer/sms-login", handler.CustomerSMSLogin)
		router.POST("/rider/send-code", handler.SendRiderCode)
		router.POST("/rider/sms-login", handler.RiderSMSLogin)
	}
	if handler.service.cfg.WeChat.AuthEnabled {
		router.POST("/customer/wechat-login", handler.WeChatLogin)
		router.POST("/customer/phone-bind", handler.AuthRequired(), handler.PhoneBind)
	}
	router.POST("/admin/login", handler.AdminLogin)
	router.POST("/merchant/login", handler.MerchantLogin)
	router.POST("/refresh", handler.Refresh)
	router.POST("/logout", handler.LogoutAuth(), handler.Logout)
}

// SendRiderCode 发送骑手登录验证码。
func (h *Handler) SendRiderCode(c *gin.Context) {
	var req SendCodeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	if err := h.service.SendRiderCode(c.Request.Context(), req); err != nil {
		response.Error(c, err)
		return
	}
	response.Empty(c)
}

// RiderSMSLogin 使用手机号验证码登录正式骑手账号。
func (h *Handler) RiderSMSLogin(c *gin.Context) {
	var req SmsLoginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	resp, err := h.service.RiderSMSLogin(c.Request.Context(), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

// WeChatLogin 处理We Chat Login相关逻辑。
func (h *Handler) WeChatLogin(c *gin.Context) {
	var req WeChatLoginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	resp, err := h.service.WeChatLogin(c.Request.Context(), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

// PhoneBind 处理手机号绑定相关逻辑。
func (h *Handler) PhoneBind(c *gin.Context) {
	claims, ok := ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req PhoneBindReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	resp, err := h.service.BindWeChatPhone(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

// SendCustomerCode 发送用户代码。
func (h *Handler) SendCustomerCode(c *gin.Context) {
	var req SendCodeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	if err := h.service.SendCustomerCode(c.Request.Context(), req); err != nil {
		response.Error(c, err)
		return
	}
	response.Empty(c)
}

// CustomerSMSLogin 处理用户 SMS Login相关逻辑。
func (h *Handler) CustomerSMSLogin(c *gin.Context) {
	var req SmsLoginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	resp, err := h.service.CustomerSMSLogin(c.Request.Context(), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

// AdminLogin 处理管理端 Login相关逻辑。
func (h *Handler) AdminLogin(c *gin.Context) {
	h.passwordLogin(c, "admin")
}

// MerchantLogin 处理商户 Login相关逻辑。
func (h *Handler) MerchantLogin(c *gin.Context) {
	h.passwordLogin(c, "merchant")
}

// passwordLogin 处理密码 Login相关逻辑。
func (h *Handler) passwordLogin(c *gin.Context, accountType string) {
	var req PasswordLoginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	resp, err := h.service.PasswordLogin(c.Request.Context(), accountType, req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

// Refresh 刷新认证。
func (h *Handler) Refresh(c *gin.Context) {
	var req RefreshReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	resp, err := h.service.Refresh(c.Request.Context(), req.RefreshToken)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

// Logout 处理Logout相关逻辑。
func (h *Handler) Logout(c *gin.Context) {
	claims, _ := c.Get("auth_claims")
	authClaims, ok := claims.(*Claims)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	if err := h.service.Logout(c.Request.Context(), authClaims); err != nil {
		response.Error(c, err)
		return
	}
	response.Empty(c)
}

// AuthRequired 返回认证 Required。
func (h *Handler) AuthRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Handler 只负责挂载 claims；授权放在能拿到对象范围的 service 方法里。
		rawToken, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "missing bearer token"))
			c.Abort()
			return
		}
		claims, err := h.service.VerifyAccess(c.Request.Context(), rawToken)
		if err != nil {
			response.Error(c, err)
			c.Abort()
			return
		}
		c.Request = c.Request.WithContext(requestctx.WithAccountID(c.Request.Context(), claims.AccountID))
		c.Set("auth_claims", claims)
		c.Next()
	}
}

// 当不记名令牌存在时，OptionalAuth 会附加经过验证的客户声明
// 存在，同时仍然允许匿名位置上下文请求。
// 一个坏令牌永远不会默默地降级为匿名。
func (h *Handler) OptionalAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		header := strings.TrimSpace(c.GetHeader("Authorization"))
		if header == "" {
			c.Next()
			return
		}
		rawToken, ok := bearerToken(header)
		if !ok {
			response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "invalid bearer token"))
			c.Abort()
			return
		}
		claims, err := h.service.VerifyAccess(c.Request.Context(), rawToken)
		if err != nil {
			response.Error(c, err)
			c.Abort()
			return
		}
		c.Request = c.Request.WithContext(requestctx.WithAccountID(c.Request.Context(), claims.AccountID))
		c.Set("auth_claims", claims)
		c.Next()
	}
}

// LogoutAuth 返回Logout 认证。
func (h *Handler) LogoutAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 登出要允许同一个 token 重复调用：第二次请求可能已经在黑名单中。
		rawToken, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "missing bearer token"))
			c.Abort()
			return
		}
		claims, err := h.service.VerifyAccessForLogout(c.Request.Context(), rawToken)
		if err != nil {
			response.Error(c, err)
			c.Abort()
			return
		}
		c.Request = c.Request.WithContext(requestctx.WithAccountID(c.Request.Context(), claims.AccountID))
		c.Set("auth_claims", claims)
		c.Next()
	}
}

// ClaimsFromContext 从上下文返回认证声明。
func ClaimsFromContext(c *gin.Context) (*Claims, bool) {
	value, ok := c.Get("auth_claims")
	if !ok {
		return nil, false
	}
	claims, ok := value.(*Claims)
	return claims, ok
}

// bearerToken 返回 Bearer 令牌。
func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	return token, token != ""
}
