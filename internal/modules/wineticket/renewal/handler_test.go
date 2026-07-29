package renewal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestRenewalCustomerRoutesAndStrictInputContracts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newRenewalTestDB(t)
	service := NewRenewalService(
		db,
		snowflake.New(307),
		renewalTestQuoteSecret,
	)
	handler := NewRenewalHandler(service)
	engine := gin.New()
	api := engine.Group("/api/v1", func(c *gin.Context) {
		c.Set("auth_claims", renewalCustomerClaims(
			9001,
			"wine_ticket_renewal:quote",
			"wine_ticket_renewal:create",
			"wine_ticket_renewal:view",
			"wine_ticket_payment:confirm",
		))
		c.Next()
	})
	RegisterRenewalCustomerRoutes(api, handler)

	routes := make(map[string]bool)
	for _, route := range engine.Routes() {
		routes[route.Method+" "+route.Path] = true
	}
	for _, route := range []string{
		"GET /api/v1/wine-tickets/lots/:lot_no/renewal-quote",
		"POST /api/v1/wine-tickets/lots/:lot_no/renewals",
		"GET /api/v1/wine-tickets/renewals",
		"GET /api/v1/wine-tickets/renewals/:renewal_no",
		"POST /api/v1/wine-tickets/renewals/:renewal_no/payment/confirm",
	} {
		if !routes[route] {
			t.Fatalf("route %s was not registered", route)
		}
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/wine-tickets/lots/WTL9001/renewals",
		strings.NewReader(`{
			"expected_lot_version":1,
			"quote_token":"abcdefghijklmnopqrstuvwxyz0123456789",
			"fee_amount":1
		}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "renewal-strict-create-01")
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "VALIDATION_FAILED") ||
		!strings.Contains(recorder.Body.String(), "unknown field") {
		t.Fatalf(
			"strict create status=%d body=%s",
			recorder.Code,
			recorder.Body.String(),
		)
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(
		http.MethodPost,
		"/api/v1/wine-tickets/renewals/WTRN9001/payment/confirm",
		strings.NewReader(`{}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "renewal-strict-confirm-01")
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "VALIDATION_FAILED") {
		t.Fatalf(
			"confirm body status=%d body=%s",
			recorder.Code,
			recorder.Body.String(),
		)
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(
		http.MethodGet,
		"/api/v1/wine-tickets/renewals?customer_id=other",
		nil,
	)
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "VALIDATION_INVALID_QUERY") ||
		recorder.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf(
			"ownership query status=%d cache=%q body=%s",
			recorder.Code,
			recorder.Header().Get("Cache-Control"),
			recorder.Body.String(),
		)
	}
}
