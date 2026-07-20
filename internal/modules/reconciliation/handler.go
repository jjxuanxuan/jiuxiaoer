package reconciliation

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct{ service *Service }

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func RegisterAdminRoutes(group *gin.RouterGroup, handler *Handler) {
	group.GET("/runs", handler.ListRuns)
	group.POST("/runs", handler.RunBill)
	group.GET("/discrepancies", handler.ListDiscrepancies)
	group.POST("/discrepancies/:id/resolve", handler.ResolveDiscrepancy)
}

func (h *Handler) RunBill(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var request struct {
		BillDate string `json:"bill_date" binding:"required"`
		BillType string `json:"bill_type" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	result, err := h.service.RunBillManual(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), request.BillDate, request.BillType)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, result)
}

func (h *Handler) ListRuns(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	limit, err := queryLimit(c, 50, 200)
	if err != nil {
		response.Error(c, err)
		return
	}
	rows, err := h.service.ListRuns(c.Request.Context(), claims, limit)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, rows)
}

func (h *Handler) ListDiscrepancies(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	limit, err := queryLimit(c, 100, 500)
	if err != nil {
		response.Error(c, err)
		return
	}
	rows, err := h.service.ListDiscrepancies(c.Request.Context(), claims, c.Query("status"), limit)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, rows)
}

func (h *Handler) ResolveDiscrepancy(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var request struct {
		HandlingNote string `json:"handling_note" binding:"required,min=3,max=1000"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	if err := h.service.ResolveDiscrepancy(c.Request.Context(), claims, c.Param("id"), request.HandlingNote); err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, gin.H{"status": "resolved"})
}

func queryLimit(c *gin.Context, fallback, maximum int) (int, error) {
	value := c.Query("limit")
	if value == "" {
		return fallback, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > maximum {
		return 0, problem.InvalidArgument("VALIDATION_FAILED", "limit is outside the allowed range")
	}
	return limit, nil
}
