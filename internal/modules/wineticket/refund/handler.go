package refund

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type RefundHandler struct {
	service *RefundService
}

func NewRefundHandler(service *RefundService) *RefundHandler {
	return &RefundHandler{service: service}
}

// RegisterRefundCustomerRoutes 暴露完整的客户退款接口。
// 分阶段发布的装配入口应使用细粒度注册方法，确保关闭新退款申请后，
// 处理中退款的状态仍可查询。
func RegisterRefundCustomerRoutes(router *gin.RouterGroup, handler *RefundHandler) {
	RegisterRefundContinuityRoutes(router, handler)
	RegisterRefundCreationRoutes(router, handler)
}

// RegisterRefundContinuityRoutes 在共享回调、查询和后台结算链路
// 处理现有退款期间，保持客户侧状态可见。
func RegisterRefundContinuityRoutes(
	router *gin.RouterGroup,
	handler *RefundHandler,
) {
	router.GET("/wine-tickets/refunds", handler.List)
	router.GET("/wine-tickets/refunds/:refund_no", handler.Detail)
}

// RegisterRefundCreationRoutes 只暴露退款报价和新退款创建接口。
func RegisterRefundCreationRoutes(
	router *gin.RouterGroup,
	handler *RefundHandler,
) {
	router.GET("/wine-tickets/purchases/:purchase_no/refund-quote", handler.Quote)
	router.POST("/wine-tickets/purchases/:purchase_no/refunds", handler.Create)
}

func (h *RefundHandler) Quote(c *gin.Context) {
	refundNoStore(c)
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownPackageQuery(c); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.Quote(
		c.Request.Context(), claims, c.Param("purchase_no"),
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	refundNoStore(c)
	response.OK(c, item)
}

func (h *RefundHandler) Create(c *gin.Context) {
	refundNoStore(c)
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownPackageQuery(c); err != nil {
		response.Error(c, err)
		return
	}
	var request RefundCreateRequest
	if err := decodeStrictJSON(c, &request); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.Create(
		c.Request.Context(), claims, c.Request.Method, c.FullPath(),
		c.GetHeader("Idempotency-Key"), c.Param("purchase_no"), request,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	refundNoStore(c)
	response.WithStatus(c, http.StatusAccepted, item)
}

func (h *RefundHandler) List(c *gin.Context) {
	refundNoStore(c)
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownPackageQuery(c, "status", "page_size", "page_token"); err != nil {
		response.Error(c, err)
		return
	}
	query, err := pagination.FromGin(c, "wine_ticket_refunds", claims.CustomerID)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.List(
		c.Request.Context(), claims, query, c.Query("status"),
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	refundNoStore(c)
	response.Page(c, items, next)
}

func (h *RefundHandler) Detail(c *gin.Context) {
	refundNoStore(c)
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownPackageQuery(c); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.Detail(
		c.Request.Context(), claims, c.Param("refund_no"),
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	refundNoStore(c)
	response.OK(c, item)
}

func refundNoStore(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	c.Header("Pragma", "no-cache")
}
