package search

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func RegisterPublicRoutes(router *gin.RouterGroup, handler *Handler) {
	router.GET("/search/discovery", handler.Discovery)
}

func RegisterCustomerRoutes(router *gin.RouterGroup, handler *Handler) {
	router.POST("/search/events", handler.RecordEvent)
	router.DELETE("/search/history", handler.ClearHistory)
}

func (h *Handler) Discovery(c *gin.Context) {
	historyLimit, err := queryLimit(c.Query("history_limit"), 10, h.service.MaxHistory())
	if err != nil {
		response.Error(c, err)
		return
	}
	hotLimit, err := queryLimit(c.Query("hot_limit"), 10, 20)
	if err != nil {
		response.Error(c, err)
		return
	}
	claims, _ := auth.ClaimsFromContext(c)
	value, err := h.service.Discovery(c.Request.Context(), claims, c.GetHeader("X-Location-Context"), c.GetHeader(h.service.SessionHeader()), historyLimit, hotLimit)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, value)
}

func (h *Handler) RecordEvent(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "customer authentication is required"))
		return
	}
	var req EventRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	value, err := h.service.RecordEvent(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"), c.GetHeader("X-Location-Context"), c.GetHeader(h.service.SessionHeader()), req)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, value)
}

func (h *Handler) ClearHistory(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "customer authentication is required"))
		return
	}
	value, err := h.service.ClearHistory(c.Request.Context(), claims, c.Request.Method, c.FullPath(), c.GetHeader("Idempotency-Key"))
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, value)
}

func (s *Service) SessionHeader() string {
	if s.locations == nil || strings.TrimSpace(s.locations.SessionHeader()) == "" {
		return "X-Session-ID"
	}
	return s.locations.SessionHeader()
}

func queryLimit(raw string, fallback, maximum int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > maximum {
		return 0, problem.InvalidArgument("SEARCH_LIMIT_INVALID", "search limit is outside the allowed range")
	}
	return value, nil
}
