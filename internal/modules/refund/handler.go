package refund

import (
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct{ service *Service }

// NewHandler 创建并初始化Handler。
func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// RegisterCallbackRoute 注册回调路由。
func RegisterCallbackRoute(api *gin.RouterGroup, handler *Handler) {
	api.POST("/refunds/:provider/callbacks", handler.Callback)
}

// RegisterAdminRoutes 注册管理端 Routes。
func RegisterAdminRoutes(group *gin.RouterGroup, handler *Handler) {
	group.GET("", handler.List)
	group.GET("/:id", handler.Detail)
	group.POST("/:id/retry", handler.Retry)
	group.POST("/:id/mark-exception", handler.MarkException)
}

// Callback 处理回调相关逻辑。
func (h *Handler) Callback(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 256*1024+1))
	if err != nil || len(body) > 256*1024 {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"code": "FAIL", "message": "callback rejected"})
		return
	}
	if err := h.service.ProcessCallback(c.Request.Context(), c.Param("provider"), c.Request, body); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "FAIL", "message": "callback rejected"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": "SUCCESS", "message": "成功"})
}

// List 查询退款列表。
func (h *Handler) List(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	query, err := pagination.FromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.List(c.Request.Context(), claims, c.Query("status"), query)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, next)
}

// Detail 处理Detail相关逻辑。
func (h *Handler) Detail(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	item, err := h.service.Detail(c.Request.Context(), claims, c.Param("id"))
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

// Retry 重试退款。
func (h *Handler) Retry(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	if err := h.service.Retry(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id")); err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, gin.H{"status": "pending"})
}

// MarkException 标记Exception的状态。
func (h *Handler) MarkException(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req struct {
		Reason string `json:"reason" binding:"required,min=3,max=500"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	if err := h.service.MarkException(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req.Reason); err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, gin.H{"status": "exception"})
}
