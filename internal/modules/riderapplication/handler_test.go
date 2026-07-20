package riderapplication

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/config"
)

// TestSubmitStrictJSONRejectsLegacySecondPhone 验证Submit Strict JSON Rejects Legacy Second 手机号的预期行为。
func TestSubmitStrictJSONRejectsLegacySecondPhone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	router := gin.New()
	api := router.Group("/api/v1")
	RegisterPublicRoutes(api, NewHandler(NewService(cfg, nil, nil, nil)))
	body := `{"name":"张三","phone":"13800138000","code":"123456","account":{"phone":"13900139000"},"service_scope":{"shop_ids":["4201"]}}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/rider-applications", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "idem-test-0001")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "unknown field") {
		t.Fatalf("legacy account.phone must be rejected: status=%d body=%s", response.Code, response.Body.String())
	}
}

// TestRiderApplicationFeatureIsDisabledByDefault 验证骑手申请 Feature Is Disabled By 默认项的预期行为。
func TestRiderApplicationFeatureIsDisabledByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	router := gin.New()
	api := router.Group("/api/v1")
	RegisterPublicRoutes(api, NewHandler(NewService(cfg, nil, nil, nil)))
	body := `{"name":"张三","phone":"13800138000","code":"123456","service_scope":{"shop_ids":["4201"]}}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/rider-applications", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "default-off-test")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "RIDER_APPLICATION_DISABLED") {
		t.Fatalf("feature must be disabled by default: status=%d body=%s", response.Code, response.Body.String())
	}
}
