package provisioning

import (
	"github.com/gin-gonic/gin"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct{ service *Service }

// NewHandler 创建并初始化Handler。
func NewHandler(s *Service) *Handler { return &Handler{service: s} }

// RegisterRoutes 注册Routes。
func RegisterRoutes(g *gin.RouterGroup, h *Handler) {
	g.POST("/merchants/provision", h.ProvisionMerchant)
	g.POST("/merchants/:id/users", h.CreateMerchantUser)
	g.PUT("/merchant-users/:id/shops", h.AuthorizeShops)
	g.PATCH("/merchant-users/:id/role", h.UpdateMerchantUserRole)
	g.PATCH("/accounts/:id/status", h.AccountStatus)
	g.POST("/accounts/:id/reset-password", h.ResetPassword)
	g.POST("/riders", h.CreateRider)
	g.POST("/riders/:id/review", h.ReviewRider)
	g.PATCH("/riders/:id/status", h.RiderStatus)
	g.GET("/provisioning-operations/:id", h.GetOperation)
}

// pc 返回pc。
func pc(c *gin.Context) (*auth.Claims, bool) {
	v, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
	}
	return v, ok
}

// bad 判断bad。
func bad(c *gin.Context, e error) bool {
	if e != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", e.Error()))
		return true
	}
	return false
}

// ProvisionMerchant 开通商户。
func (h *Handler) ProvisionMerchant(c *gin.Context) {
	v, ok := pc(c)
	if !ok {
		return
	}
	var r MerchantProvisionReq
	if bad(c, c.ShouldBindJSON(&r)) {
		return
	}
	x, e := h.service.ProvisionMerchant(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), r)
	done(c, x, e)
}

// CreateMerchantUser 创建商户用户。
func (h *Handler) CreateMerchantUser(c *gin.Context) {
	v, ok := pc(c)
	if !ok {
		return
	}
	var r MerchantUserReq
	if bad(c, c.ShouldBindJSON(&r)) {
		return
	}
	x, e := h.service.CreateMerchantUser(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), r)
	done(c, x, e)
}

// AuthorizeShops 为Shops授予访问权限。
func (h *Handler) AuthorizeShops(c *gin.Context) {
	v, ok := pc(c)
	if !ok {
		return
	}
	var r ShopAuthorizationReq
	if bad(c, c.ShouldBindJSON(&r)) {
		return
	}
	x, e := h.service.AuthorizeShops(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), r)
	done(c, x, e)
}

// UpdateMerchantUserRole 调整商家用户角色并使旧权限快照失效。
func (h *Handler) UpdateMerchantUserRole(c *gin.Context) {
	v, ok := pc(c)
	if !ok {
		return
	}
	var r MerchantUserRoleReq
	if bad(c, c.ShouldBindJSON(&r)) {
		return
	}
	x, e := h.service.UpdateMerchantUserRole(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), r)
	done(c, x, e)
}

// AccountStatus 处理账户状态相关逻辑。
func (h *Handler) AccountStatus(c *gin.Context) {
	v, ok := pc(c)
	if !ok {
		return
	}
	var r AccountStatusReq
	if bad(c, c.ShouldBindJSON(&r)) {
		return
	}
	x, e := h.service.AccountStatus(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), r)
	done(c, x, e)
}

// ResetPassword 重置密码。
func (h *Handler) ResetPassword(c *gin.Context) {
	v, ok := pc(c)
	if !ok {
		return
	}
	var r ResetPasswordReq
	if bad(c, c.ShouldBindJSON(&r)) {
		return
	}
	x, e := h.service.ResetPassword(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), r)
	done(c, x, e)
}

// CreateRider 创建骑手。
func (h *Handler) CreateRider(c *gin.Context) {
	v, ok := pc(c)
	if !ok {
		return
	}
	var r RiderCreateReq
	if bad(c, c.ShouldBindJSON(&r)) {
		return
	}
	x, e := h.service.CreateRider(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), r)
	done(c, x, e)
}

// ReviewRider 审核骑手。
func (h *Handler) ReviewRider(c *gin.Context) {
	v, ok := pc(c)
	if !ok {
		return
	}
	var r RiderReviewReq
	if bad(c, c.ShouldBindJSON(&r)) {
		return
	}
	x, e := h.service.ReviewRider(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), r)
	done(c, x, e)
}

// RiderStatus 处理骑手状态相关逻辑。
func (h *Handler) RiderStatus(c *gin.Context) {
	v, ok := pc(c)
	if !ok {
		return
	}
	var r RiderStatusReq
	if bad(c, c.ShouldBindJSON(&r)) {
		return
	}
	x, e := h.service.RiderStatus(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), r)
	done(c, x, e)
}

// GetOperation 获取操作。
func (h *Handler) GetOperation(c *gin.Context) {
	v, ok := pc(c)
	if !ok {
		return
	}
	x, e := h.service.GetOperation(c.Request.Context(), v, c.Param("id"))
	done(c, x, e)
}

// done 处理done相关逻辑。
func done(c *gin.Context, x any, e error) {
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, x)
}
