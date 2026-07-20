package admin

import (
	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
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
	router.POST("/products", handler.CreateProduct)
	router.PUT("/products/:id", handler.UpdateProduct)
	router.POST("/products/:id/on-sale", handler.ProductOnSale)
	router.POST("/products/:id/off-sale", handler.ProductOffSale)
	router.GET("/orders", handler.ListOrders)
	router.GET("/orders/:id", handler.GetOrder)
	router.GET("/stocks", handler.ListStocks)
	router.POST("/stocks/adjust", handler.AdjustStock)
	router.GET("/audit-logs", handler.ListAuditLogs)
	router.GET("/merchants", handler.ListMerchants)
	router.POST("/merchants/:id/review", handler.ReviewMerchant)
}

// CreateProduct 创建商品。
func (h *Handler) CreateProduct(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req ProductCreateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	item, err := h.service.CreateProduct(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

// UpdateProduct 更新商品。
func (h *Handler) UpdateProduct(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req ProductUpdateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	item, err := h.service.UpdateProduct(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

// 商品上架
func (h *Handler) ProductOnSale(c *gin.Context) {
	h.setProductStatus(c, "on_sale")
}

// 商品下架
func (h *Handler) ProductOffSale(c *gin.Context) {
	h.setProductStatus(c, "off_sale")
}

// setProductStatus 设置商品状态。
func (h *Handler) setProductStatus(c *gin.Context, status string) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	item, err := h.service.SetProductStatus(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), status)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

// ListOrders 查询订单列表。
func (h *Handler) ListOrders(c *gin.Context) {
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
	items, nextPageToken, err := h.service.ListOrders(c.Request.Context(), claims, query, c.Query("status"))
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, nextPageToken)
}

// GetOrder 获取订单。
func (h *Handler) GetOrder(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	item, err := h.service.GetOrder(c.Request.Context(), claims, c.Param("id"))
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

// ListStocks 查询Stocks列表。
func (h *Handler) ListStocks(c *gin.Context) {
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
	items, nextPageToken, err := h.service.ListStocks(c.Request.Context(), claims, query, c.Query("shop_id"))
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, nextPageToken)
}

// AdjustStock 调整库存。
func (h *Handler) AdjustStock(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req StockAdjustReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	item, err := h.service.AdjustStock(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

// ListAuditLogs 查询审计 Logs列表。
func (h *Handler) ListAuditLogs(c *gin.Context) {
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
	items, nextPageToken, err := h.service.ListAuditLogs(c.Request.Context(), claims, query)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, nextPageToken)
}

// ListMerchants 查询Merchants列表。
func (h *Handler) ListMerchants(c *gin.Context) {
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
	items, nextPageToken, err := h.service.ListMerchants(c.Request.Context(), claims, query, c.Query("review_status"))
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, nextPageToken)
}

// ReviewMerchant 审核商户。
func (h *Handler) ReviewMerchant(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req MerchantReviewReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	item, err := h.service.ReviewMerchant(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}
