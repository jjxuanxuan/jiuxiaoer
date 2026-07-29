package ops

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type SlotAdminHandler struct {
	service *SlotAdminService
}

func NewSlotAdminHandler(service *SlotAdminService) *SlotAdminHandler {
	return &SlotAdminHandler{service: service}
}

// RegisterSlotAdminRoutes 接收受保护的
// /api/v1/admin/wine-tickets 路由组。
func RegisterSlotAdminRoutes(
	router *gin.RouterGroup,
	handler *SlotAdminHandler,
) {
	router.GET("/delivery-time-slots", handler.List)
	router.POST("/delivery-time-slots", handler.Create)
	router.PUT("/delivery-time-slots/:slot_id", handler.Update)
}

func (h *SlotAdminHandler) List(c *gin.Context) {
	claims, ok := adminClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownPackageQuery(
		c,
		"shop_id",
		"service_date",
		"page_size",
		"page_token",
	); err != nil {
		response.Error(c, err)
		return
	}
	authorizedShops, err := slotAdminAuthorizedShops(claims)
	if err != nil {
		response.Error(c, err)
		return
	}
	scope := "global"
	if len(authorizedShops) != 0 {
		values := make([]string, 0, len(authorizedShops))
		for _, shopID := range authorizedShops {
			values = append(values, strconv.FormatUint(shopID, 10))
		}
		scope = strings.Join(values, ",")
	}
	query, err := pagination.FromGin(
		c,
		"wine_ticket_admin_delivery_time_slots",
		claims.AdminUserID,
		scope,
		c.Query("shop_id"),
		c.Query("service_date"),
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.List(
		c.Request.Context(),
		claims,
		query,
		c.Query("shop_id"),
		c.Query("service_date"),
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.Page(c, items, next)
}

func (h *SlotAdminHandler) Create(c *gin.Context) {
	claims, ok := adminClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownPackageQuery(c); err != nil {
		response.Error(c, err)
		return
	}
	var request SlotAdminCreateRequest
	if err := decodeStrictJSON(c, &request); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.Create(
		c.Request.Context(),
		claims,
		c.Request.Method,
		c.FullPath(),
		c.GetHeader("Idempotency-Key"),
		request,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.WithStatus(c, http.StatusCreated, item)
}

func (h *SlotAdminHandler) Update(c *gin.Context) {
	claims, ok := adminClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownPackageQuery(c); err != nil {
		response.Error(c, err)
		return
	}
	var request SlotAdminUpdateRequest
	if err := decodeStrictJSON(c, &request); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.Update(
		c.Request.Context(),
		claims,
		c.Request.Method,
		c.FullPath(),
		c.GetHeader("Idempotency-Key"),
		c.Param("slot_id"),
		request,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.OK(c, item)
}
