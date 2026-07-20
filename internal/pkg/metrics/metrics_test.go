package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestRegistryRendersBoundedHTTPLabels 验证注册表 Renders Bounded HTTP标签的预期行为。
func TestRegistryRendersBoundedHTTPLabels(t *testing.T) {
	registry := New("instance-1", "")
	registry.ObserveHTTP(http.MethodGet, "/api/v1/orders/:id", http.StatusOK, "", 25*time.Millisecond)
	registry.IncOutbox("published")
	registry.IncOrderExpiry("expired")
	registry.IncPayment("wechat", "callback_succeeded")
	output := registry.render()
	for _, expected := range []string{
		`jxe_http_requests_total{method="GET",route="/api/v1/orders/:id",status="200",error_code=""} 1`,
		`jxe_outbox_publish_total{result="published"} 1`,
		`jxe_order_expiry_total{result="expired"} 1`,
		`jxe_payment_operations_total{provider="wechat",result="callback_succeeded"} 1`,
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("expected metrics output to contain %q, got %s", expected, output)
		}
	}
}

// TestMetricsHandlerRequiresConfiguredToken 验证指标 Handler Requires Configured 令牌的预期行为。
func TestMetricsHandlerRequiresConfiguredToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := New("instance-1", "0123456789abcdef")
	router := gin.New()
	router.GET("/metrics", registry.Handler)

	unauthorized := httptest.NewRecorder()
	router.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", unauthorized.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer 0123456789abcdef")
	authorized := httptest.NewRecorder()
	router.ServeHTTP(authorized, req)
	if authorized.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", authorized.Code)
	}
}
