package deliveryverification

import (
	"github.com/gin-gonic/gin"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct{ service *Service }

// NewHandler 创建并初始化Handler。
func NewHandler(s *Service) *Handler { return &Handler{service: s} }

// RegisterStoreRoutes 注册门店路由。
func RegisterStoreRoutes(g *gin.RouterGroup, h *Handler) {
	g.GET("/orders/:id/verification", h.GetStore)
}

// RegisterCustomerRoutes 注册客户路由。
func RegisterCustomerRoutes(g *gin.RouterGroup, h *Handler) {
	g.GET("/:id/verification", h.GetCustomer)
}

// RegisterAdminRoutes 注册管理端路由。
func RegisterAdminRoutes(g *gin.RouterGroup, h *Handler) {
	g.POST("/deliveries/:id/verification/unlock", h.Unlock)
}

// vc 返回验证控制器。
func vc(c *gin.Context) (*auth.Claims, bool) {
	v, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
	}
	return v, ok
}

// GetStore 获取门店。
func (h *Handler) GetStore(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	v, ok := vc(c)
	if !ok {
		return
	}
	x, e := h.service.GetStore(c.Request.Context(), v, c.Param("id"))
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, x)
}

// GetCustomer 获取用户。
func (h *Handler) GetCustomer(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	v, ok := vc(c)
	if !ok {
		return
	}
	x, e := h.service.GetCustomer(c.Request.Context(), v, c.Param("id"))
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, x)
}

// Unlock 解锁deliveryverification。
func (h *Handler) Unlock(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	v, ok := vc(c)
	if !ok {
		return
	}
	var r UnlockReq
	if e := c.ShouldBindJSON(&r); e != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", e.Error()))
		return
	}
	x, e := h.service.Unlock(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), r)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, x)
}
