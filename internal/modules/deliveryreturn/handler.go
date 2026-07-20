package deliveryreturn

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct{ service *Service }

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func RegisterRiderRoutes(group *gin.RouterGroup, handler *Handler) {
	group.POST("/orders/:id/returns", handler.Create)
	group.GET("/returns/:id", handler.RiderDetail)
	group.POST("/returns/:id/arrive", handler.Arrive)
}

func RegisterStoreRoutes(group *gin.RouterGroup, handler *Handler) {
	group.GET("/delivery-returns", handler.StoreList)
	group.GET("/delivery-returns/:id", handler.StoreDetail)
	group.POST("/delivery-returns/:id/receive", handler.Receive)
}

func RegisterAdminRoutes(group *gin.RouterGroup, handler *Handler) {
	group.GET("/delivery-returns", handler.AdminList)
	group.GET("/delivery-returns/:id", handler.AdminDetail)
	group.POST("/delivery-returns/:id/approve", handler.Approve)
}

func (h *Handler) Create(c *gin.Context) {
	claims, ok := returnClaims(c)
	if !ok {
		return
	}
	var req CreateReq
	if !bindReturn(c, &req) {
		h.service.AuditInvalidRequest(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.Param("id"))
		return
	}
	item, err := h.service.Create(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	status := 201
	if item.Deduplicated {
		status = 200
	}
	response.WithStatus(c, status, item)
}

func (h *Handler) RiderDetail(c *gin.Context) {
	claims, ok := returnClaims(c)
	if !ok {
		return
	}
	item, err := h.service.RiderDetail(c.Request.Context(), claims, c.Param("id"))
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

func (h *Handler) Arrive(c *gin.Context) {
	claims, ok := returnClaims(c)
	if !ok {
		return
	}
	var req ArriveReq
	if !bindReturn(c, &req) {
		return
	}
	item, err := h.service.Arrive(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	finishReturn(c, item, err)
}

func (h *Handler) Approve(c *gin.Context) {
	claims, ok := returnClaims(c)
	if !ok {
		return
	}
	var req ApproveReq
	if !bindReturn(c, &req) {
		return
	}
	item, err := h.service.Approve(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	finishReturn(c, item, err)
}

func (h *Handler) Receive(c *gin.Context) {
	claims, ok := returnClaims(c)
	if !ok {
		return
	}
	var req ReceiveReq
	if !bindReturn(c, &req) {
		return
	}
	item, err := h.service.Receive(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	finishReturn(c, item, err)
}

func (h *Handler) AdminDetail(c *gin.Context) {
	claims, ok := returnClaims(c)
	if !ok {
		return
	}
	item, err := h.service.AdminDetail(c.Request.Context(), claims, c.Param("id"))
	finishReturn(c, item, err)
}

func (h *Handler) StoreDetail(c *gin.Context) {
	claims, ok := returnClaims(c)
	if !ok {
		return
	}
	item, err := h.service.StoreDetail(c.Request.Context(), claims, c.Param("id"))
	finishReturn(c, item, err)
}

func (h *Handler) AdminList(c *gin.Context) {
	h.list(c, true)
}

func (h *Handler) StoreList(c *gin.Context) {
	h.list(c, false)
}

func (h *Handler) list(c *gin.Context, admin bool) {
	claims, ok := returnClaims(c)
	if !ok {
		return
	}
	query, err := pagination.FromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	input := ListQuery{Status: strings.TrimSpace(c.Query("status")), Offset: query.Offset, Limit: query.PageSize}
	var items []DTO
	var more bool
	if admin {
		items, more, err = h.service.AdminList(c.Request.Context(), claims, input)
	} else {
		items, more, err = h.service.StoreList(c.Request.Context(), claims, input)
	}
	if err != nil {
		response.Error(c, err)
		return
	}
	next := ""
	if more {
		next = pagination.NextPageToken(query)
	}
	response.Page(c, items, next)
}

func finishReturn(c *gin.Context, item DTO, err error) {
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

func returnClaims(c *gin.Context) (*auth.Claims, bool) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("UNAUTHENTICATED", "unauthorized"))
	}
	return claims, ok
}

func bindReturn(c *gin.Context, target any) bool {
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", "request body must contain exactly one JSON object"))
		return false
	}
	if err := binding.Validator.ValidateStruct(target); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return false
	}
	return true
}
