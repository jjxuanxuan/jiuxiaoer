package ops

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
)

func TestSlotAdminRoutesStrictContractsAndRBAC(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service, db, _ := newSlotAdminTestService(t)
	seedSlotAdminShop(t, db, 201, 101, "310100")
	handler := NewSlotAdminHandler(service)
	engine := gin.New()
	admin := engine.Group(
		"/api/v1/admin/wine-tickets",
		func(c *gin.Context) {
			c.Set(
				"auth_claims",
				slotAdminClaims(
					"wine_ticket_slot:list",
					"wine_ticket_slot:create",
					"wine_ticket_slot:update",
				),
			)
			c.Next()
		},
	)
	RegisterSlotAdminRoutes(admin, handler)

	routes := make(map[string]bool)
	for _, route := range engine.Routes() {
		routes[route.Method+" "+route.Path] = true
	}
	for _, expected := range []string{
		"GET /api/v1/admin/wine-tickets/delivery-time-slots",
		"POST /api/v1/admin/wine-tickets/delivery-time-slots",
		"PUT /api/v1/admin/wine-tickets/delivery-time-slots/:slot_id",
	} {
		if !routes[expected] {
			t.Fatalf("route %s was not registered", expected)
		}
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/admin/wine-tickets/delivery-time-slots",
		strings.NewReader(`{
			"shop_id":"201",
			"service_date":"2026-07-28",
			"start_time":"10:00:00",
			"end_time":"12:00:00",
			"cutoff_at":"2026-07-28T09:00:00+08:00",
			"capacity_orders":4,
			"reserved_orders":0
		}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "slot-strict-json")
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "VALIDATION_FAILED") ||
		!strings.Contains(recorder.Body.String(), "unknown field") {
		t.Fatalf(
			"strict JSON status=%d body=%s",
			recorder.Code,
			recorder.Body.String(),
		)
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(
		http.MethodGet,
		"/api/v1/admin/wine-tickets/delivery-time-slots?status=open",
		nil,
	)
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "VALIDATION_INVALID_QUERY") ||
		recorder.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf(
			"unknown query status=%d cache=%q body=%s",
			recorder.Code,
			recorder.Header().Get("Cache-Control"),
			recorder.Body.String(),
		)
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(
		http.MethodPut,
		"/api/v1/admin/wine-tickets/delivery-time-slots/01",
		strings.NewReader(`{
			"capacity_orders":4,
			"status":"closed",
			"expected_version":1
		}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "slot-strict-id-01")
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "VALIDATION_FAILED") {
		t.Fatalf(
			"decimal id status=%d body=%s",
			recorder.Code,
			recorder.Body.String(),
		)
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(
		http.MethodPost,
		"/api/v1/admin/wine-tickets/delivery-time-slots?force=true",
		strings.NewReader(`{}`),
	)
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "VALIDATION_INVALID_QUERY") {
		t.Fatalf(
			"write query status=%d body=%s",
			recorder.Code,
			recorder.Body.String(),
		)
	}

	missingPermissionEngine := gin.New()
	missingPermissionAdmin := missingPermissionEngine.Group(
		"/api/v1/admin/wine-tickets",
		func(c *gin.Context) {
			c.Set(
				"auth_claims",
				&auth.Claims{AccountType: "admin", AdminUserID: "9001"},
			)
			c.Next()
		},
	)
	RegisterSlotAdminRoutes(missingPermissionAdmin, handler)
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(
		http.MethodGet,
		"/api/v1/admin/wine-tickets/delivery-time-slots",
		nil,
	)
	missingPermissionEngine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden ||
		!strings.Contains(recorder.Body.String(), "PERM_FORBIDDEN") {
		t.Fatalf(
			"RBAC status=%d body=%s",
			recorder.Code,
			recorder.Body.String(),
		)
	}
}
