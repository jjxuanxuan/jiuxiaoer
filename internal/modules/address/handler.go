package address

import (
	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct{ service *Service }

// NewHandler 创建并初始化Handler。
func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// RegisterRoutes 注册Routes。
func RegisterRoutes(router *gin.RouterGroup, handler *Handler) {
	router.GET("", handler.List)
	router.POST("", handler.Create)
	router.PUT("/:id", handler.Update)
	router.DELETE("/:id", handler.Delete)
	router.POST("/:id/set-default", handler.SetDefault)
}

// List 查询地址列表。
func (h *Handler) List(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	items, err := h.service.List(c.Request.Context(), claims)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, "")
}

// Create 创建地址。
func (h *Handler) Create(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	var req AddressUpsertReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("ADDRESS_INVALID", err.Error()))
		return
	}
	item, err := h.service.Create(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), req)
	writeItem(c, item, err)
}

// Update 更新地址。
func (h *Handler) Update(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	var req AddressUpdateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("ADDRESS_INVALID", err.Error()))
		return
	}
	item, err := h.service.Update(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	writeItem(c, item, err)
}

// Delete 删除地址。
func (h *Handler) Delete(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	if err := h.service.Delete(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id")); err != nil {
		response.Error(c, err)
		return
	}
	response.Empty(c)
}

// SetDefault 设置默认项。
func (h *Handler) SetDefault(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	item, err := h.service.SetDefault(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"))
	writeItem(c, item, err)
}

// customerClaims 返回用户认证声明。
func customerClaims(c *gin.Context) (*auth.Claims, bool) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return nil, false
	}
	return claims, true
}

// writeItem 写入明细。
func writeItem(c *gin.Context, item AddressDTO, err error) {
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}
