package reminder

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

const maxConsentBodyBytes = 64 << 10

type ReminderHandler struct {
	service *ReminderService
}

func NewHandler(service *ReminderService) *ReminderHandler {
	return NewReminderHandler(service)
}

func NewReminderHandler(service *ReminderService) *ReminderHandler {
	return &ReminderHandler{service: service}
}

// RegisterReminderCustomerRoutes 接收已认证的 /api/v1 路由组。
func RegisterReminderCustomerRoutes(router *gin.RouterGroup, handler *ReminderHandler) {
	router.GET("/wine-tickets/notification-subscriptions", handler.GetLatestConsent)
	router.POST("/wine-tickets/notification-subscriptions", handler.RecordConsent)
}

func (h *ReminderHandler) GetLatestConsent(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	if err := rejectUnknownQuery(c, "scene"); err != nil {
		response.Error(c, err)
		return
	}
	claims, ok := reminderCustomerClaims(c)
	if !ok {
		return
	}
	item, err := h.service.LatestConsent(c.Request.Context(), claims, c.Query("scene"))
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

func (h *ReminderHandler) RecordConsent(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	if err := rejectUnknownQuery(c); err != nil {
		response.Error(c, err)
		return
	}
	claims, ok := reminderCustomerClaims(c)
	if !ok {
		return
	}
	var req NotificationConsentCreateRequest
	if err := decodeStrictJSON(c, &req); err != nil {
		response.Error(c, err)
		return
	}
	item, err := h.service.RecordConsent(
		c.Request.Context(), claims, c.Request.Method, c.FullPath(),
		c.GetHeader("Idempotency-Key"), req,
	)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.WithStatus(c, http.StatusCreated, item)
}

func rejectUnknownQuery(c *gin.Context, allowed ...string) error {
	allow := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allow[key] = struct{}{}
	}
	for key := range c.Request.URL.Query() {
		if _, ok := allow[key]; !ok {
			return problem.InvalidArgument(
				"VALIDATION_INVALID_QUERY",
				"unknown query parameter: "+key,
			)
		}
	}
	return nil
}

func decodeStrictJSON(c *gin.Context, out any) error {
	reader := http.MaxBytesReader(c.Writer, c.Request.Body, maxConsentBodyBytes)
	payload, err := io.ReadAll(reader)
	if err != nil {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"request body is too large or unreadable",
		)
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"request body must be a JSON object",
		)
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return problem.InvalidArgument("VALIDATION_FAILED", safeJSONError(err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"request body must contain exactly one JSON object",
		)
	}
	return nil
}

func safeJSONError(err error) string {
	message := err.Error()
	if strings.Contains(message, "unknown field") ||
		strings.Contains(message, "cannot unmarshal") ||
		strings.Contains(message, "invalid character") ||
		strings.Contains(message, "unexpected EOF") {
		return message
	}
	return "invalid JSON request body"
}

func reminderCustomerClaims(c *gin.Context) (*auth.Claims, bool) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "customer authentication required"))
		return nil, false
	}
	return claims, true
}
