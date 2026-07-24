package store

import (
	"sort"
	"strconv"
	"strings"
	"time"

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
	router.GET("/orders", handler.ListOrders)
	router.GET("/orders/:id", handler.DetailOrder)
	router.POST("/orders/:id/accept", handler.AcceptOrder)
	router.POST("/orders/:id/start-preparing", handler.StartPreparingOrder)
	router.POST("/orders/:id/prepare", handler.PrepareOrder)
	router.PATCH("/shops/:id/business-status", handler.UpdateBusinessStatus)
	router.GET("/shop-products", handler.ListShopProducts)
	router.POST("/shop-products", handler.CreateShopProduct)
	router.PATCH("/shop-products/:id", handler.UpdateShopProduct)
	router.PATCH("/shop-products/:id/stock", handler.AdjustStock)
}

// DetailOrder 返回授权门店的商家订单详情。
func (h *Handler) DetailOrder(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	item, err := h.service.DetailOrder(c.Request.Context(), claims, c.Param("id"))
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
	filters, err := storeOrderListFiltersFromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	query, err := pagination.FromGin(c, storePaginationScope(claims)...)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, nextPageToken, err := h.service.ListOrders(c.Request.Context(), claims, query, filters)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, nextPageToken)
}

// AcceptOrder 接受并处理订单。
func (h *Handler) AcceptOrder(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req StoreOrderActionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	item, err := h.service.AcceptOrder(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

// PrepareOrder 准备订单。
func (h *Handler) PrepareOrder(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req StoreOrderActionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	item, err := h.service.PrepareOrder(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

// StartPreparingOrder 启动Preparing 订单处理流程。
func (h *Handler) StartPreparingOrder(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req StoreOrderActionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	item, err := h.service.StartPreparingOrder(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

// UpdateBusinessStatus 更新Business 状态。
func (h *Handler) UpdateBusinessStatus(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req BusinessStatusReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	item, err := h.service.UpdateBusinessStatus(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

// ListShopProducts 查询门店商品列表。
func (h *Handler) ListShopProducts(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	filters, err := storeInventoryFiltersFromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	query, err := pagination.FromGin(c, storePaginationScope(claims)...)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, nextPageToken, err := h.service.ListShopProducts(c.Request.Context(), claims, query, filters)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, nextPageToken)
}

// storePaginationScope 将游标绑定到准确的商户主体及其当前门店授权快照。
// 经过排序后，等价的 JWT 门店集合不受声明顺序影响，会生成相同指纹。
func storePaginationScope(claims *auth.Claims) []string {
	if claims == nil {
		return []string{"merchant_id", "", "merchant_user_id", "", "authorized_shop_ids", ""}
	}
	shopIDs := append([]string(nil), claims.AuthorizedShopIDs...)
	sort.Strings(shopIDs)
	return []string{
		"merchant_id", claims.MerchantID,
		"merchant_user_id", claims.MerchantUserID,
		"authorized_shop_ids", strings.Join(shopIDs, ","),
	}
}

func storeOrderListFiltersFromGin(c *gin.Context) (StoreOrderListFilters, error) {
	if err := rejectUnknownStoreQuery(c, map[string]struct{}{
		"page_size": {}, "page_token": {}, "shop_id": {}, "status": {}, "keyword": {},
		"order_no": {}, "paid_from": {}, "paid_to": {}, "order_by": {},
	}); err != nil {
		return StoreOrderListFilters{}, err
	}
	if raw := strings.TrimSpace(c.Query("order_by")); raw != "" && strings.ToLower(raw) != "created_at desc,id desc" {
		return StoreOrderListFilters{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "order_by must be created_at desc,id desc")
	}
	filters := StoreOrderListFilters{
		Status: strings.TrimSpace(c.Query("status")), Keyword: strings.TrimSpace(c.Query("keyword")),
		OrderNo: strings.TrimSpace(c.Query("order_no")),
	}
	if len(filters.Status) > 32 || len([]rune(filters.Keyword)) > 64 || len(filters.OrderNo) > 64 {
		return StoreOrderListFilters{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "store order query is too long")
	}
	if raw := strings.TrimSpace(c.Query("shop_id")); raw != "" {
		shopID, err := parseID(raw)
		if err != nil {
			return StoreOrderListFilters{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid shop_id")
		}
		filters.ShopID = shopID
	}
	var err error
	if filters.PaidFrom, err = parseOptionalQueryTime(c.Query("paid_from")); err != nil {
		return StoreOrderListFilters{}, err
	}
	if filters.PaidTo, err = parseOptionalQueryTime(c.Query("paid_to")); err != nil {
		return StoreOrderListFilters{}, err
	}
	if filters.PaidFrom != nil && filters.PaidTo != nil && filters.PaidFrom.After(*filters.PaidTo) {
		return StoreOrderListFilters{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "paid_from must not be after paid_to")
	}
	return filters, nil
}

func storeInventoryFiltersFromGin(c *gin.Context) (StoreInventoryFilters, error) {
	if err := rejectUnknownStoreQuery(c, map[string]struct{}{
		"page_size": {}, "page_token": {}, "shop_id": {}, "status": {}, "keyword": {},
		"low_stock_only": {}, "order_by": {},
	}); err != nil {
		return StoreInventoryFilters{}, err
	}
	if raw := strings.TrimSpace(c.Query("order_by")); raw != "" && strings.ToLower(raw) != "updated_at desc,id desc" {
		return StoreInventoryFilters{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "order_by must be updated_at desc,id desc")
	}
	filters := StoreInventoryFilters{Status: strings.TrimSpace(c.Query("status")), Keyword: strings.TrimSpace(c.Query("keyword"))}
	if len(filters.Status) > 32 || len([]rune(filters.Keyword)) > 64 {
		return StoreInventoryFilters{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "inventory query is too long")
	}
	if raw := strings.TrimSpace(c.Query("shop_id")); raw != "" {
		shopID, err := parseID(raw)
		if err != nil {
			return StoreInventoryFilters{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid shop_id")
		}
		filters.ShopID = shopID
	}
	if raw := strings.TrimSpace(c.Query("low_stock_only")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return StoreInventoryFilters{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "low_stock_only must be true or false")
		}
		filters.LowStockOnly = value
	}
	return filters, nil
}

func rejectUnknownStoreQuery(c *gin.Context, allowed map[string]struct{}) error {
	for key := range c.Request.URL.Query() {
		if _, ok := allowed[key]; !ok {
			return problem.InvalidArgument("VALIDATION_INVALID_QUERY", "unknown query parameter: "+key)
		}
	}
	return nil
}

func parseOptionalQueryTime(raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "time query must use RFC3339")
	}
	return &value, nil
}

// CreateShopProduct 创建门店商品。
func (h *Handler) CreateShopProduct(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req ShopProductCreateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	item, err := h.service.CreateShopProduct(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

// UpdateShopProduct 更新门店商品。
func (h *Handler) UpdateShopProduct(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req ShopProductUpdateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	item, err := h.service.UpdateShopProduct(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
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
	item, err := h.service.AdjustStock(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}
