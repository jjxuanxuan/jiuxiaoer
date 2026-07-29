package renewal

import (
	"bytes"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type RenewalHandler struct {
	service *RenewalService
}

func NewHandler(service *RenewalService) *RenewalHandler {
	return NewRenewalHandler(service)
}

func NewRenewalHandler(service *RenewalService) *RenewalHandler {
	return &RenewalHandler{service: service}
}

// RegisterRenewalCustomerRoutes 暴露完整的客户续期接口。
// 分阶段发布的装配入口应使用细粒度注册方法，确保关闭新续期后，
// 仍可确认支付并查询续期状态。
func RegisterRenewalCustomerRoutes(
	router *gin.RouterGroup,
	handler *RenewalHandler,
) {
	RegisterRenewalContinuityRoutes(router, handler)
	RegisterRenewalCreationRoutes(router, handler)
}

// RegisterRenewalContinuityRoutes 在创建开关关闭后，
// 仍允许查询和对账支付中或支付结果未知的续期单。
func RegisterRenewalContinuityRoutes(
	router *gin.RouterGroup,
	handler *RenewalHandler,
) {
	router.GET("/wine-tickets/renewals", handler.ListRenewals)
	router.GET(
		"/wine-tickets/renewals/:renewal_no",
		handler.GetRenewal,
	)
	router.POST(
		"/wine-tickets/renewals/:renewal_no/payment/confirm",
		handler.ConfirmRenewalPayment,
	)
}

// RegisterRenewalCreationRoutes 暴露续期报价和新续期单创建接口。
func RegisterRenewalCreationRoutes(
	router *gin.RouterGroup,
	handler *RenewalHandler,
) {
	router.GET(
		"/wine-tickets/lots/:lot_no/renewal-quote",
		handler.GetRenewalQuote,
	)
	router.POST(
		"/wine-tickets/lots/:lot_no/renewals",
		handler.CreateRenewal,
	)
}

func (h *RenewalHandler) GetRenewalQuote(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownPackageQuery(c); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.RenewalQuote(
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

func (h *RenewalHandler) CreateRenewal(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	var req RenewalCreateRequest
	if err := decodeStrictJSON(c, &req); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.CreateRenewal(
		c.Request.Context(),
		claims,
		c.Request.Method,
		c.FullPath(),
		c.GetHeader("Idempotency-Key"),
		c.Param("lot_no"),
		req,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.WithStatus(c, http.StatusCreated, item)
}

func (h *RenewalHandler) ListRenewals(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownPackageQuery(
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
		"wine_ticket_renewals",
		claims.CustomerID,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.ListRenewals(
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

func (h *RenewalHandler) GetRenewal(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	if err := rejectUnknownPackageQuery(c); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.Renewal(
		c.Request.Context(),
		claims,
		c.Param("renewal_no"),
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.OK(c, item)
}

func (h *RenewalHandler) ConfirmRenewalPayment(c *gin.Context) {
	claims, ok := customerClaims(c)
	if !ok {
		return
	}
	if err := rejectRenewalConfirmBody(c); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.ConfirmRenewalPayment(
		c.Request.Context(),
		claims,
		c.Request.Method,
		c.FullPath(),
		c.GetHeader("Idempotency-Key"),
		c.Param("renewal_no"),
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	response.OK(c, item)
}

func rejectRenewalConfirmBody(c *gin.Context) error {
	reader := http.MaxBytesReader(c.Writer, c.Request.Body, 1024)
	payload, err := io.ReadAll(reader)
	if err != nil {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"request body is too large or unreadable",
		)
	}
	if len(bytes.TrimSpace(payload)) != 0 {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"payment confirmation request must not contain a body",
		)
	}
	return nil
}
