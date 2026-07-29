package reminder

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestReminderCustomerRoutesStrictJSONAndQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newReminderTestDB(t)
	service := NewReminderService(db, snowflake.New(210))
	handler := NewReminderHandler(service)
	engine := gin.New()
	api := engine.Group("/api/v1", func(c *gin.Context) {
		c.Set("auth_claims", reminderCustomer(
			"9001",
			"wine_ticket_notification_consent:create",
			"wine_ticket_notification_consent:view",
		))
		c.Next()
	})
	RegisterReminderCustomerRoutes(api, handler)

	routes := make(map[string]bool)
	for _, route := range engine.Routes() {
		routes[route.Method+" "+route.Path] = true
	}
	for _, route := range []string{
		"GET /api/v1/wine-tickets/notification-subscriptions",
		"POST /api/v1/wine-tickets/notification-subscriptions",
	} {
		if !routes[route] {
			t.Fatalf("route %s was not registered", route)
		}
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/wine-tickets/notification-subscriptions",
		strings.NewReader(`{
			"scene":"expiry_reminder",
			"template_code":"template-v1",
			"consent_result":"accepted",
			"unexpected":true
		}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "strict-consent-01")
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "VALIDATION_FAILED") ||
		!strings.Contains(recorder.Body.String(), "unknown field") {
		t.Fatalf("strict JSON rejection status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(
		http.MethodGet,
		"/api/v1/wine-tickets/notification-subscriptions?scene=expiry_reminder&customer_id=9002",
		nil,
	)
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "VALIDATION_INVALID_QUERY") {
		t.Fatalf("unknown ownership query status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
