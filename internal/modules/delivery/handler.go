package delivery

import (
	"errors"
	"io"
	"strings"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/deliveryverification"
	"jiuxiaoer-admin/backend-go/internal/modules/dispatch"
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
	router.GET("/orders", handler.List)
	router.GET("/orders/:id", handler.Detail)
	router.POST("/orders/:id/accept", handler.Accept)
	router.POST("/orders/:id/pickup", handler.Pickup)
	router.POST("/orders/:id/complete", handler.Complete)
}

// Detail returns fulfillment details only for the rider holding the current
// active assignment.
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

// List 查询配送列表。
func (h *Handler) List(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	status, err := deliveryListStatusFromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	query, err := pagination.FromGin(c, "rider", claims.RiderID)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, nextPageToken, err := h.service.List(c.Request.Context(), claims, query, status)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, nextPageToken)
}

func deliveryListStatusFromGin(c *gin.Context) (string, error) {
	allowed := map[string]struct{}{"page_size": {}, "page_token": {}, "status": {}}
	for key := range c.Request.URL.Query() {
		if _, ok := allowed[key]; !ok {
			return "", problem.InvalidArgument("VALIDATION_INVALID_QUERY", "unknown query parameter: "+key)
		}
	}
	status := strings.TrimSpace(c.Query("status"))
	if len(status) > 32 {
		return "", problem.InvalidArgument("VALIDATION_INVALID_QUERY", "status is too long")
	}
	return status, nil
}

// Accept 接受并处理配送。
func (h *Handler) Accept(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req dispatch.GrabReq
	if err := c.ShouldBindJSON(&req); err != nil {
		h.actionError(c, claims, "delivery_accept", problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	item, err := h.service.AcceptWithVersion(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req.ExpectedAssignmentVersion)
	if err != nil {
		h.actionError(c, claims, "delivery_accept", err)
		return
	}
	response.OK(c, item)
}

// Pickup 处理Pickup相关逻辑。
func (h *Handler) Pickup(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req deliveryverification.CodeReq
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		h.actionError(c, claims, "delivery_pickup", problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	item, err := h.service.PickupWithCode(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req.PickupCode)
	if err != nil {
		h.actionError(c, claims, "delivery_pickup", err)
		return
	}
	response.OK(c, item)
}

// Complete 处理Complete相关逻辑。
func (h *Handler) Complete(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	var req deliveryverification.CodeReq
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		h.actionError(c, claims, "delivery_complete", problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	item, err := h.service.CompleteWithCode(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req.DeliveryCode)
	if err != nil {
		h.actionError(c, claims, "delivery_complete", err)
		return
	}
	response.OK(c, item)
}

func (h *Handler) actionError(c *gin.Context, claims *auth.Claims, action string, cause error) {
	if err := h.service.AuditFailure(c.Request.Context(), claims, action, c.Param("id"), cause); err != nil {
		response.Error(c, err)
		return
	}
	response.Error(c, cause)
}
