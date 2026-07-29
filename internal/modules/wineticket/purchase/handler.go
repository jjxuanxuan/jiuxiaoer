package purchase

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

const maxRequestBodyBytes = 64 << 10

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func RegisterCustomerRoutes(router *gin.RouterGroup, handler *Handler) {
	RegisterContinuityRoutes(router, handler)
	RegisterCreationRoutes(router, handler)
}

func RegisterContinuityRoutes(router *gin.RouterGroup, handler *Handler) {
	router.GET("/wine-tickets/purchases", handler.ListPurchases)
	router.GET("/wine-tickets/purchases/:purchase_no", handler.GetPurchase)
	router.POST(
		"/wine-tickets/purchases/:purchase_no/payment/confirm",
		handler.ConfirmPurchasePayment,
	)
}

func RegisterCreationRoutes(router *gin.RouterGroup, handler *Handler) {
	router.POST("/wine-tickets/purchases", handler.CreatePurchase)
}

func (h *Handler) ListPurchases(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownQuery(
		c,
		"status",
		"page_size",
		"page_token",
	); err != nil {
		response.Error(c, err)
		return
	}
	query, err := pagination.FromGin(
		c,
		"wine_ticket_purchases",
		claims.CustomerID,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.ListPurchases(
		c.Request.Context(),
		claims,
		query,
		c.Query("status"),
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.Page(c, items, next)
}

func (h *Handler) CreatePurchase(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	var request PurchaseCreateRequest
	if err := decodeStrictJSON(c, &request); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.CreatePurchase(
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

func (h *Handler) GetPurchase(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	item, err := h.service.Purchase(
		c.Request.Context(),
		claims,
		c.Param("purchase_no"),
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.OK(c, item)
}

func (h *Handler) ConfirmPurchasePayment(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	item, err := h.service.ConfirmPurchasePayment(
		c.Request.Context(),
		claims,
		c.Request.Method,
		c.FullPath(),
		c.GetHeader("Idempotency-Key"),
		c.Param("purchase_no"),
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.OK(c, item)
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

func decodeStrictJSON(c *gin.Context, out any) error {
	reader := http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodyBytes)
	payload, err := io.ReadAll(reader)
	if err != nil {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"request body is too large or unreadable",
		)
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"request body must be a JSON object",
		)
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return problem.InvalidArgument("VALIDATION_FAILED", safeJSONError(err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"request body must contain exactly one JSON object",
		)
	}
	return nil
}

func safeJSONError(err error) string {
	message := err.Error()
	for _, marker := range []string{
		"unknown field",
		"cannot unmarshal",
		"invalid character",
		"unexpected EOF",
	} {
		if bytes.Contains([]byte(message), []byte(marker)) {
			return message
		}
	}
	return "invalid JSON request body"
}
