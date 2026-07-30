package cart

import (
	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
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
	router.GET("/items", handler.GetCart)
	router.POST("/items", handler.AddItem)
	router.POST("/repurchase", handler.Repurchase)
	router.PUT("/items/:id", handler.UpdateItem)
	router.DELETE("/items/:id", handler.DeleteItem)
	router.PATCH("/items/:id/selection", handler.SetItemSelection)
	router.POST("/selection", handler.SetShopSelection)
	router.DELETE("/items", handler.ClearItems)
}

// RegisterFrequentPurchaseRoutes 注册位于购物车资源之外的常购清单读取接口。
func RegisterFrequentPurchaseRoutes(router *gin.RouterGroup, handler *Handler) {
	router.GET("/frequent-purchases", handler.ListFrequentPurchases)
}

// ListFrequentPurchases 查询本人常购清单。
func (h *Handler) ListFrequentPurchases(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	resp, err := h.service.ListFrequentPurchases(c.Request.Context(), claims, c.GetHeader("X-Location-Context"))
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

// Repurchase 将常购商品批量放入本人购物车。
func (h *Handler) Repurchase(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req RepurchaseReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	resp, err := h.service.Repurchase(
		c.Request.Context(),
		claims,
		c.Request.Method,
		c.FullPath(),
		c.GetHeader("Idempotency-Key"),
		c.GetHeader("X-Location-Context"),
		req,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

// SetItemSelection 设置明细选中状态。
func (h *Handler) SetItemSelection(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req CartItemSelectionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	resp, err := h.service.SetItemSelection(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

// SetShopSelection 设置门店选中状态。
func (h *Handler) SetShopSelection(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req CartSelectionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	resp, err := h.service.SetShopSelection(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

// ClearItems 清空明细。
func (h *Handler) ClearItems(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	if err := h.service.ClearItems(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Query("shop_id")); err != nil {
		response.Error(c, err)
		return
	}
	response.Empty(c)
}

// GetCart 获取购物车。
func (h *Handler) GetCart(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	resp, err := h.service.GetCart(c.Request.Context(), claims)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

// AddItem 添加明细。
func (h *Handler) AddItem(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req CartItemAddReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	resp, err := h.service.AddItem(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

// UpdateItem 更新明细。
func (h *Handler) UpdateItem(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req CartItemUpdateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	resp, err := h.service.UpdateItem(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, resp)
}

// DeleteItem 删除明细。
func (h *Handler) DeleteItem(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	if err := h.service.DeleteItem(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id")); err != nil {
		response.Error(c, err)
		return
	}
	response.Empty(c)
}
