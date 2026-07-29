package cabinet

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

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func RegisterCustomerRoutes(router *gin.RouterGroup, handler *Handler) {
	router.GET("/wine-tickets/cabinet", handler.GetCabinet)
	router.GET("/wine-tickets/lots", handler.ListLots)
	router.GET("/wine-tickets/lots/:lot_no", handler.GetLot)
	router.GET("/wine-tickets/transactions", handler.ListTransactions)
}

func (h *Handler) GetCabinet(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownQuery(c, "page_size", "page_token"); err != nil {
		response.Error(c, err)
		return
	}
	query, err := pagination.FromGin(c, "wine_ticket_cabinet", claims.CustomerID)
	if err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.Cabinet(c.Request.Context(), claims, query)
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.OK(c, item)
}

func (h *Handler) ListLots(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownQuery(
		c,
		"product_id",
		"status",
		"page_size",
		"page_token",
	); err != nil {
		response.Error(c, err)
		return
	}
	query, err := pagination.FromGin(c, "wine_ticket_lots", claims.CustomerID)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.ListLots(
		c.Request.Context(),
		claims,
		query,
		c.Query("product_id"),
		c.Query("status"),
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.Page(c, items, next)
}

func (h *Handler) GetLot(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	item, err := h.service.Lot(
		c.Request.Context(),
		claims,
		c.Param("lot_no"),
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.OK(c, item)
}

func (h *Handler) ListTransactions(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownQuery(
		c,
		"lot_no",
		"transaction_type",
		"page_size",
		"page_token",
	); err != nil {
		response.Error(c, err)
		return
	}
	query, err := pagination.FromGin(
		c,
		"wine_ticket_transactions",
		claims.CustomerID,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.ListTransactions(
		c.Request.Context(),
		claims,
		query,
		c.Query("lot_no"),
		c.Query("transaction_type"),
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.Page(c, items, next)
}

func customerClaims(c *gin.Context) (*auth.Claims, bool) {
	c.Header("Cache-Control", "private, no-store")
	claims, ok := auth.ClaimsFromContext(c)
	if !ok || claims.AccountType != "customer" {
		response.Error(
			c,
			problem.Unauthorized(
				"AUTH_UNAUTHORIZED",
				"customer authentication required",
			),
		)
		return nil, false
	}
	return claims, true
}

func rejectUnknownQuery(c *gin.Context, allowed ...string) error {
	allow := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allow[key] = struct{}{}
	}
	for key := range c.Request.URL.Query() {
		if _, ok := allow[key]; !ok {
			return problem.InvalidArgument(
				"VALIDATION_INVALID_QUERY",
				"unknown query parameter: "+key,
			)
		}
	}
	return nil
}
