package customerlocation

import (
	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

func RegisterAdminRoutes(router *gin.RouterGroup, handler *Handler) {
	router.GET("/service-cities", handler.ListAdminCities)
	router.POST("/service-cities", handler.CreateAdminCity)
	router.PUT("/service-cities/:id", handler.UpdateAdminCity)
	router.POST("/service-cities/:id/status", handler.SetAdminCityStatus)
	router.GET("/delivery-promise-policies", handler.ListAdminPolicies)
	router.POST("/delivery-promise-policies", handler.CreateAdminPolicy)
	router.PUT("/delivery-promise-policies/:id", handler.UpdateAdminPolicy)
	router.POST("/delivery-promise-policies/:id/status", handler.SetAdminPolicyStatus)
}

func (h *Handler) ListAdminCities(c *gin.Context) {
	claims, ok := adminClaims(c)
	if !ok {
		return
	}
	query, err := pagination.FromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.ListAdminCities(c.Request.Context(), claims, query)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, next)
}

func (h *Handler) CreateAdminCity(c *gin.Context) {
	claims, ok := adminClaims(c)
	if !ok {
		return
	}
	var req ServiceCityWriteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	out, err := h.service.CreateAdminCity(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, out)
}

func (h *Handler) UpdateAdminCity(c *gin.Context) {
	claims, ok := adminClaims(c)
	if !ok {
		return
	}
	var req ServiceCityWriteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	out, err := h.service.UpdateAdminCity(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, out)
}

func (h *Handler) SetAdminCityStatus(c *gin.Context) {
	claims, ok := adminClaims(c)
	if !ok {
		return
	}
	var req ResourceStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	out, err := h.service.SetAdminCityStatus(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, out)
}

func (h *Handler) ListAdminPolicies(c *gin.Context) {
	claims, ok := adminClaims(c)
	if !ok {
		return
	}
	query, err := pagination.FromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.ListAdminPolicies(c.Request.Context(), claims, query)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, next)
}

func (h *Handler) CreateAdminPolicy(c *gin.Context) {
	claims, ok := adminClaims(c)
	if !ok {
		return
	}
	var req PromisePolicyWriteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	out, err := h.service.CreateAdminPolicy(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, out)
}

func (h *Handler) UpdateAdminPolicy(c *gin.Context) {
	claims, ok := adminClaims(c)
	if !ok {
		return
	}
	var req PromisePolicyWriteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	out, err := h.service.UpdateAdminPolicy(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, out)
}

func (h *Handler) SetAdminPolicyStatus(c *gin.Context) {
	claims, ok := adminClaims(c)
	if !ok {
		return
	}
	var req ResourceStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	out, err := h.service.SetAdminPolicyStatus(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.Param("id"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, out)
}

func adminClaims(c *gin.Context) (*auth.Claims, bool) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return nil, false
	}
	return claims, true
}
