package delivery

import (
	"context"
	"errors"
	"io"

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
	router.POST("/orders/:id/accept", handler.Accept)
	router.POST("/orders/:id/pickup", handler.Pickup)
	router.POST("/orders/:id/start", handler.Start)
	router.POST("/orders/:id/complete", handler.Complete)
}

// List 查询配送列表。
func (h *Handler) List(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	query, err := pagination.FromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, nextPageToken, err := h.service.List(c.Request.Context(), claims, query, c.Query("status"))
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, nextPageToken)
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
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	item, err := h.service.AcceptWithVersion(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req.ExpectedAssignmentVersion)
	if err != nil {
		response.Error(c, err)
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
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	item, err := h.service.PickupWithCode(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req.PickupCode)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

// Start 启动当前实例的后台处理流程。
func (h *Handler) Start(c *gin.Context) {
	h.transition(c, h.service.Start)
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
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	item, err := h.service.CompleteWithCode(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req.DeliveryCode)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

// transition 处理状态流转相关逻辑。
func (h *Handler) transition(c *gin.Context, fn func(ctx context.Context, claims *auth.Claims, method string, path string, key string, deliveryID string) (DeliveryOrderDTO, error)) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	item, err := fn(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"))
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}
