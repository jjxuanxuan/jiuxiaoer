package redemption

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type RedemptionHandler struct {
	service *RedemptionService
}

func NewRedemptionHandler(service *RedemptionService) *RedemptionHandler {
	return &RedemptionHandler{service: service}
}

// RegisterRedemptionCustomerRoutes 暴露完整的客户核销接口。
// 分阶段发布的装配入口应使用细粒度注册方法，确保关闭创建开关后，
// 历史记录仍可查询，可取消的核销单也不会失去处理入口。
func RegisterRedemptionCustomerRoutes(router *gin.RouterGroup, handler *RedemptionHandler) {
	RegisterRedemptionContinuityRoutes(router, handler)
	RegisterRedemptionCreationRoutes(router, handler)
}

// RegisterRedemptionContinuityRoutes 保持现有核销单可查询，
// 并在事故停线期间保留取货前取消闭环。
func RegisterRedemptionContinuityRoutes(
	router *gin.RouterGroup,
	handler *RedemptionHandler,
) {
	router.GET("/wine-tickets/redemptions", handler.List)
	router.GET("/wine-tickets/redemptions/:redemption_no", handler.Detail)
	router.POST("/wine-tickets/redemptions/:redemption_no/cancel", handler.Cancel)
}

// RegisterRedemptionCreationRoutes 暴露配送时段查询和新核销单创建接口；
// 关闭核销分支时两者会同时下线。
func RegisterRedemptionCreationRoutes(
	router *gin.RouterGroup,
	handler *RedemptionHandler,
) {
	router.GET("/wine-tickets/delivery-time-slots", handler.ListDeliveryTimeSlots)
	router.POST("/wine-tickets/redemptions", handler.Create)
}

func (h *RedemptionHandler) ListDeliveryTimeSlots(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownPackageQuery(
		c, "product_id", "quantity", "address_id", "address_version", "date_from", "date_to",
	); err != nil {
		response.Error(c, err)
		return
	}
	query, err := h.service.ParseDeliveryTimeSlotQuery(
		c.Query("product_id"), c.Query("quantity"), c.Query("address_id"),
		c.Query("address_version"), c.Query("date_from"), c.Query("date_to"),
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, err := h.service.DeliveryTimeSlots(c.Request.Context(), claims, query)
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.OK(c, items)
}

func (h *RedemptionHandler) List(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownPackageQuery(c, "status", "page_size", "page_token"); err != nil {
		response.Error(c, err)
		return
	}
	query, err := pagination.FromGin(c, "wine_ticket_redemptions", claims.CustomerID)
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
	c.Header("Cache-Control", "private, no-store")
	response.Page(c, items, next)
}

func (h *RedemptionHandler) Create(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownPackageQuery(c); err != nil {
		response.Error(c, err)
		return
	}
	var req RedemptionCreateRequest
	if err := decodeStrictJSON(c, &req); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.Create(
		c.Request.Context(), claims, c.Request.Method, c.FullPath(),
		c.GetHeader("Idempotency-Key"), req,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.WithStatus(c, http.StatusCreated, item)
}

func (h *RedemptionHandler) Detail(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownPackageQuery(c); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.Detail(
		c.Request.Context(), claims, c.Param("redemption_no"),
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.OK(c, item)
}

func (h *RedemptionHandler) Cancel(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownPackageQuery(c); err != nil {
		response.Error(c, err)
		return
	}
	var req RedemptionCancelRequest
	if err := decodeStrictJSON(c, &req); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.Cancel(
		c.Request.Context(), claims, c.Request.Method, c.FullPath(),
		c.GetHeader("Idempotency-Key"), c.Param("redemption_no"), req,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.OK(c, item)
}
