package customerlocation

import (
	"strings"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct{ service *Service }

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func RegisterRoutes(router *gin.RouterGroup, handler *Handler, optionalAuth gin.HandlerFunc) {
	router.GET("/service-cities", handler.ListCities)
	locations := router.Group("/location-contexts")
	locations.Use(optionalAuth)
	locations.POST("", handler.Resolve)
	locations.POST("/city", handler.CreateCityContext)
	locations.GET("/:id/service-shops", handler.ServiceShops)
	locations.PUT("/:id/service-shop", handler.SwitchShop)
}

func (h *Handler) Resolve(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	var req ResolveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	out, err := h.service.Resolve(c.Request.Context(), actor, ClientMeta{IP: c.ClientIP()}, req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, out)
}

func (h *Handler) CreateCityContext(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	var req CityContextRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	out, err := h.service.CreateCityContext(c.Request.Context(), actor, ClientMeta{IP: c.ClientIP()}, req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, out)
}

func (h *Handler) ListCities(c *gin.Context) {
	query, err := pagination.FromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.ListCities(c.Request.Context(), c.Query("keyword"), query)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, next)
}

func (h *Handler) ServiceShops(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	query, err := pagination.FromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	if query.PageSize > 50 {
		response.Error(c, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "page_size must be between 1 and 50"))
		return
	}
	items, err := h.service.ServiceShops(c.Request.Context(), actor, c.Param("id"))
	if err != nil {
		response.Error(c, err)
		return
	}
	start := query.Offset
	if start > len(items) {
		start = len(items)
	}
	end := minInt(start+query.PageSize, len(items))
	response.Page(c, items[start:end], "")
}

func (h *Handler) SwitchShop(c *gin.Context) {
	actor, ok := h.actor(c)
	if !ok {
		return
	}
	var req SwitchShopRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	out, err := h.service.SwitchShop(c.Request.Context(), actor, c.Param("id"), c.GetHeader("Idempotency-Key"), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, out)
}

func (h *Handler) actor(c *gin.Context) (Actor, bool) {
	if claims, ok := auth.ClaimsFromContext(c); ok {
		if claims.AccountType != "customer" || strings.TrimSpace(claims.CustomerID) == "" {
			response.Error(c, problem.Forbidden("PERM_FORBIDDEN", "customer account required"))
			return Actor{}, false
		}
		actor, err := h.service.BuildActor(claims.CustomerID, "")
		if err != nil {
			response.Error(c, err)
			return Actor{}, false
		}
		return actor, true
	}
	actor, err := h.service.BuildActor("", c.GetHeader(h.service.SessionHeader()))
	if err != nil {
		response.Error(c, err)
		return Actor{}, false
	}
	return actor, true
}
