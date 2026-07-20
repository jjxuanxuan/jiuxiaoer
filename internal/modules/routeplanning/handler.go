package routeplanning

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct{ service *Service }

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func RegisterRoutes(router *gin.RouterGroup, handler *Handler) {
	router.GET("/orders/:id/route", handler.Current)
}

func (h *Handler) Current(c *gin.Context) {
	if len(c.Request.URL.Query()) != 0 {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", "query parameters are not supported"))
		return
	}
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
		return
	}
	plan, err := h.service.Current(c.Request.Context(), claims, c.Param("id"), c.ClientIP())
	if err != nil {
		response.Error(c, err)
		return
	}
	response.WithStatus(c, http.StatusOK, plan)
}
