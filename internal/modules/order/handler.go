package order

import (
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/paygateway"
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
	router.POST("", handler.Create)
	router.GET("", handler.List)
	router.GET("/:id", handler.Detail)
	router.POST("/:id/cancel", handler.Cancel)
	router.POST("/:id/payments", handler.CreatePayment)
	router.GET("/:id/payment", handler.GetPayment)
	router.POST("/:id/payment/confirm", handler.ConfirmPayment)
	if handler.service.cfg.Feature.PaymentMockEnabled {
		router.POST("/:id/pay/mock", handler.MockPay)
	}
}

// RegisterCallbackRoute 注册回调路由。
func RegisterCallbackRoute(api *gin.RouterGroup, handler *Handler) {
	if handler.service.cfg.WeChat.PayEnabled {
		api.POST("/payments/:provider/callbacks", handler.PaymentCallback)
	}
}

// Create 创建订单。
func (h *Handler) Create(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req OrderCreateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		h.actionError(c, claims, "order_create", "", problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	resp, err := h.service.Create(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), req)
	if err != nil {
		h.actionError(c, claims, "order_create", "", err)
		return
	}
	response.OK(c, resp)
}

// List 查询订单列表。
func (h *Handler) List(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	filters, err := customerOrderListFiltersFromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	query, err := pagination.FromGin(c, "customer", claims.CustomerID)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, nextPageToken, err := h.service.List(c.Request.Context(), claims, query, filters)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, nextPageToken)
}

func customerOrderListFiltersFromGin(c *gin.Context) (CustomerOrderListFilters, error) {
	allowed := map[string]struct{}{"page_size": {}, "page_token": {}, "status": {}, "order_by": {}}
	for key := range c.Request.URL.Query() {
		if _, ok := allowed[key]; !ok {
			return CustomerOrderListFilters{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "unknown query parameter: "+key)
		}
	}
	if raw := strings.TrimSpace(c.Query("order_by")); raw != "" && strings.ToLower(raw) != "created_at desc,id desc" {
		return CustomerOrderListFilters{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "order_by must be created_at desc,id desc")
	}
	status := strings.TrimSpace(c.Query("status"))
	if len(status) > 32 {
		return CustomerOrderListFilters{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "status is too long")
	}
	return CustomerOrderListFilters{Status: status}, nil
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

// Cancel 取消订单。
func (h *Handler) Cancel(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req OrderCancelReq
	if err := c.ShouldBindJSON(&req); err != nil {
		h.actionError(c, claims, "order_cancel", c.Param("id"), problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	item, err := h.service.Cancel(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		h.actionError(c, claims, "order_cancel", c.Param("id"), err)
		return
	}
	response.OK(c, item)
}

// MockPay 处理Mock Pay相关逻辑。
func (h *Handler) MockPay(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req MockPayReq
	if err := c.ShouldBindJSON(&req); err != nil {
		h.actionError(c, claims, "payment_mock", c.Param("id"), problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	item, err := h.service.MockPay(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		h.actionError(c, claims, "payment_mock", c.Param("id"), err)
		return
	}
	response.OK(c, item)
}

// CreatePayment 创建支付。
func (h *Handler) CreatePayment(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req PaymentCreateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		h.actionError(c, claims, "payment_create", c.Param("id"), problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	item, err := h.service.CreatePayment(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		h.actionError(c, claims, "payment_create", c.Param("id"), err)
		return
	}
	response.OK(c, item)
}

// GetPayment 获取支付。
func (h *Handler) GetPayment(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	item, err := h.service.GetPayment(c.Request.Context(), claims, c.Param("id"))
	if err != nil {
		h.actionError(c, claims, "payment_confirm", c.Param("id"), err)
		return
	}
	response.OK(c, item)
}

func (h *Handler) actionError(c *gin.Context, claims *auth.Claims, action, orderID string, cause error) {
	if err := h.service.AuditFailure(c.Request.Context(), claims, action, orderID, cause); err != nil {
		response.Error(c, err)
		return
	}
	response.Error(c, cause)
}

// ConfirmPayment 查询微信支付并返回后端确认的支付状态。
// 小程序客户端回调绝不会被视为资金结果。
func (h *Handler) ConfirmPayment(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	item, err := h.service.ConfirmPaymentIdempotent(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"))
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

// PaymentCallback 处理支付回调相关逻辑。
func (h *Handler) PaymentCallback(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, paygateway.MaxCallbackBodyBytes+1))
	if err != nil || int64(len(body)) > paygateway.MaxCallbackBodyBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"code": "FAIL", "message": "callback rejected"})
		return
	}
	if err := h.service.ProcessPaymentCallback(c.Request.Context(), c.Param("provider"), c.Request, body); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "FAIL", "message": "callback rejected"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": "SUCCESS", "message": "成功"})
}
