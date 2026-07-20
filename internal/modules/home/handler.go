package home

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/customerlocation"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct{ service *Service }

// NewHandler 创建并初始化Handler。
func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// RegisterPublicRoutes 注册公开数据 Routes。
func RegisterPublicRoutes(router *gin.RouterGroup, h *Handler) { router.GET("/home", h.Public) }

// RegisterAdminRoutes 注册管理端 Routes。
func RegisterAdminRoutes(router *gin.RouterGroup, h *Handler) {
	router.GET("/home-slots", h.List)
	router.POST("/home-slots", h.Create)
	router.PUT("/home-slots/:id", h.Update)
	router.POST("/home-slots/:id/status", h.SetStatus)
}

// Public 处理公开数据相关逻辑。
func (h *Handler) Public(c *gin.Context) {
	lat, lng, err := coordinates(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	actor := customerlocation.Actor{}
	if h.service.lbsMode != "off" {
		actor, err = locationActor(c, h.service.locations)
		if err != nil {
			if h.service.lbsMode == "observe" {
				actor = customerlocation.Actor{}
			} else {
				response.Error(c, err)
				return
			}
		}
	}
	out, err := h.service.Public(c.Request.Context(), c.Query("city_code"), lat, lng, c.GetHeader("X-Location-Context"), actor)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, out)
}

func locationActor(c *gin.Context, locations *customerlocation.Service) (customerlocation.Actor, error) {
	if locations == nil || c.GetHeader("X-Location-Context") == "" {
		return customerlocation.Actor{}, nil
	}
	customerID := ""
	if claims, ok := auth.ClaimsFromContext(c); ok {
		if claims.AccountType != "customer" {
			return customerlocation.Actor{}, problem.Forbidden("PERM_FORBIDDEN", "customer account required")
		}
		customerID = claims.CustomerID
	}
	return locations.BuildActor(customerID, c.GetHeader(locations.SessionHeader()))
}

// List 查询首页列表。
func (h *Handler) List(c *gin.Context) {
	claims, ok := claims(c)
	if !ok {
		return
	}
	query, err := pagination.FromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.List(c.Request.Context(), claims, c.Query("city_code"), c.Query("slot_type"), c.Query("status"), query)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, next)
}

// Create 创建首页。
func (h *Handler) Create(c *gin.Context) {
	claims, ok := claims(c)
	if !ok {
		return
	}
	var req SlotWriteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	out, err := h.service.Create(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, out)
}

// Update 更新首页。
func (h *Handler) Update(c *gin.Context) {
	claims, ok := claims(c)
	if !ok {
		return
	}
	var req SlotWriteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	out, err := h.service.Update(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, out)
}

// SetStatus 设置状态。
func (h *Handler) SetStatus(c *gin.Context) {
	claims, ok := claims(c)
	if !ok {
		return
	}
	var req SlotStatusReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	out, err := h.service.SetStatus(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, out)
}

// claims 返回认证声明。
func claims(c *gin.Context) (*auth.Claims, bool) {
	v, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return nil, false
	}
	return v, true
}

// coordinates 返回coordinates。
func coordinates(c *gin.Context) (*float64, *float64, error) {
	a, b := c.Query("lat"), c.Query("lng")
	if a == "" && b == "" {
		return nil, nil, nil
	}
	if a == "" || b == "" {
		return nil, nil, problem.New(422, "LOCATION_REQUIRED", "Unprocessable Entity", "lat and lng are required together")
	}
	lat, err := strconv.ParseFloat(a, 64)
	if err != nil || lat < -90 || lat > 90 {
		return nil, nil, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid lat")
	}
	lng, err := strconv.ParseFloat(b, 64)
	if err != nil || lng < -180 || lng > 180 {
		return nil, nil, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "invalid lng")
	}
	return &lat, &lng, nil
}
