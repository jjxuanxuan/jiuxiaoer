package catalog

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
)

func TestRegisterPackageRoutesAndStrictCreateJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service, _ := newPackageTestService(t)
	handler := NewHandler(service)
	engine := gin.New()
	api := engine.Group("/api/v1")
	RegisterPublicRoutes(api, handler)
	admin := api.Group("/admin/wine-tickets", func(c *gin.Context) {
		c.Set("auth_claims", packageAdminClaims("wine_ticket_package:create"))
		c.Next()
	})
	RegisterAdminRoutes(admin, handler)

	routes := make(map[string]struct{})
	for _, route := range engine.Routes() {
		routes[route.Method+" "+route.Path] = struct{}{}
	}
	for _, expected := range []string{
		"GET /api/v1/wine-tickets/packages",
		"GET /api/v1/wine-tickets/packages/:package_no",
		"GET /api/v1/admin/wine-tickets/packages",
		"POST /api/v1/admin/wine-tickets/packages",
		"GET /api/v1/admin/wine-tickets/packages/:package_no",
		"PUT /api/v1/admin/wine-tickets/packages/:package_no",
		"POST /api/v1/admin/wine-tickets/packages/:package_no/publish",
		"POST /api/v1/admin/wine-tickets/packages/:package_no/unpublish",
	} {
		if _, ok := routes[expected]; !ok {
			t.Fatalf("route %s was not registered; got %v", expected, routes)
		}
	}

	reqBody := validPackageWriteRequest()
	payload, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatal(err)
	}
	payload = bytes.Replace(payload, []byte(`"package_code"`), []byte(`"unknown_field":true,"package_code"`), 1)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/wine-tickets/packages", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "strict-json-01")
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "VALIDATION_FAILED") || !strings.Contains(recorder.Body.String(), "unknown field") {
		t.Fatalf("unknown JSON field was not rejected: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestAdminRBACAndPublicUnknownQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service, _ := newPackageTestService(t)
	handler := NewHandler(service)
	engine := gin.New()
	api := engine.Group("/api/v1")
	RegisterPublicRoutes(api, handler)
	admin := api.Group("/admin/wine-tickets", func(c *gin.Context) {
		c.Set("auth_claims", &auth.Claims{AccountType: "admin", AdminUserID: "9001"})
		c.Next()
	})
	RegisterAdminRoutes(admin, handler)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/wine-tickets/packages", nil)
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "PERM_FORBIDDEN") {
		t.Fatalf("missing RBAC permission was not rejected: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/v1/wine-tickets/packages?status=published", nil)
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "VALIDATION_INVALID_QUERY") {
		t.Fatalf("undocumented public query was not rejected: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
