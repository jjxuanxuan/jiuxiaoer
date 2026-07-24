package notification

import (
	"github.com/gin-gonic/gin"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct{ service *Service }

// NewHandler 创建并初始化Handler。
func NewHandler(s *Service) *Handler { return &Handler{service: s} }

// RegisterCustomerRoutes 注册客户路由。
func RegisterCustomerRoutes(g *gin.RouterGroup, h *Handler) {
	g.GET("", h.ListMessages)
	g.POST("/:id/read", h.Read)
	g.POST("/read-all", h.ReadAll)
}

// RegisterAdminRoutes 注册管理端路由。
func RegisterAdminRoutes(g *gin.RouterGroup, h *Handler) {
	g.GET("/notification-deliveries", h.ListDeliveries)
	g.POST("/notification-deliveries/:id/retry", h.Retry)
	g.GET("/notification-templates", h.ListTemplates)
	g.POST("/notification-templates", h.CreateTemplate)
	g.PATCH("/notification-templates/:id", h.UpdateTemplate)
}

// getClaims 获取认证声明。
func getClaims(c *gin.Context) (*auth.Claims, bool) {
	v, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
	}
	return v, ok
}

// ListMessages 查询Messages列表。
func (h *Handler) ListMessages(c *gin.Context) {
	v, ok := getClaims(c)
	if !ok {
		return
	}
	q, e := pagination.FromGin(c)
	if e != nil {
		response.Error(c, e)
		return
	}
	x, n, e := h.service.ListMessages(c.Request.Context(), v, q)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.Page(c, x, n)
}

// Read 读取通知。
func (h *Handler) Read(c *gin.Context) {
	v, ok := getClaims(c)
	if !ok {
		return
	}
	x, e := h.service.Read(c.Request.Context(), v, c.Param("id"))
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, x)
}

// ReadAll 读取All。
func (h *Handler) ReadAll(c *gin.Context) {
	v, ok := getClaims(c)
	if !ok {
		return
	}
	x, e := h.service.ReadAll(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"))
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, x)
}

// ListDeliveries 查询Deliveries列表。
func (h *Handler) ListDeliveries(c *gin.Context) {
	v, ok := getClaims(c)
	if !ok {
		return
	}
	q, e := pagination.FromGin(c)
	if e != nil {
		response.Error(c, e)
		return
	}
	x, n, e := h.service.ListDeliveries(c.Request.Context(), v, q, c.Query("status"))
	if e != nil {
		response.Error(c, e)
		return
	}
	response.Page(c, x, n)
}

// Retry 重试通知。
func (h *Handler) Retry(c *gin.Context) {
	v, ok := getClaims(c)
	if !ok {
		return
	}
	var r RetryReq
	if e := c.ShouldBindJSON(&r); e != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", e.Error()))
		return
	}
	x, e := h.service.Retry(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), r)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, x)
}

// ListTemplates 查询Templates列表。
func (h *Handler) ListTemplates(c *gin.Context) {
	v, ok := getClaims(c)
	if !ok {
		return
	}
	q, e := pagination.FromGin(c)
	if e != nil {
		response.Error(c, e)
		return
	}
	x, n, e := h.service.ListTemplates(c.Request.Context(), v, q)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.Page(c, x, n)
}

// CreateTemplate 创建通知模板。
func (h *Handler) CreateTemplate(c *gin.Context) {
	v, ok := getClaims(c)
	if !ok {
		return
	}
	var r TemplateReq
	if e := c.ShouldBindJSON(&r); e != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", e.Error()))
		return
	}
	x, e := h.service.CreateTemplate(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), r)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, x)
}

// UpdateTemplate 更新Template。
func (h *Handler) UpdateTemplate(c *gin.Context) {
	v, ok := getClaims(c)
	if !ok {
		return
	}
	var r TemplateReq
	if e := c.ShouldBindJSON(&r); e != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", e.Error()))
		return
	}
	x, e := h.service.UpdateTemplate(c.Request.Context(), v, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), r)
	if e != nil {
		response.Error(c, e)
		return
	}
	response.OK(c, x)
}
