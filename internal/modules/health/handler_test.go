package health

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/config"
)

// TestLiveAndReadySeparateProcessFromDependencies 验证Live And 就绪状态 Separate Process From Dependencies的预期行为。
func TestLiveAndReadySeparateProcessFromDependencies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	cfg.MySQL.Required = true
	router := gin.New()
	RegisterRoutes(router, router.Group("/api/v1"), cfg, nil, nil, nil, nil, nil)

	live := httptest.NewRecorder()
	router.ServeHTTP(live, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if live.Code != http.StatusOK {
		t.Fatalf("expected livez 200, got %d", live.Code)
	}

	ready := httptest.NewRecorder()
	router.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected readyz 503, got %d", ready.Code)
	}
	if !strings.Contains(ready.Body.String(), `"mysql":"disabled"`) {
		t.Fatalf("expected dependency status, got %s", ready.Body.String())
	}
}

// TestOptionalDependenciesDoNotBlockReadiness 验证Optional Dependencies Do 不 Block Readiness的预期行为。
func TestOptionalDependenciesDoNotBlockReadiness(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	cfg.Feature.MQPublisherEnabled = false
	cfg.MQ.ConsumerNotificationEnabled = false
	cfg.MQ.ConsumerPrintEnabled = false
	cfg.MQ.ConsumerCacheEnabled = false
	cfg.MQ.ConsumerSecurityEnabled = false
	cfg.MQ.FailOnTopologyDrift = false
	cfg.MySQL.Required = false
	cfg.Redis.Required = false
	cfg.RabbitMQ.Required = false
	router := gin.New()
	RegisterRoutes(router, router.Group("/api/v1"), cfg, nil, nil, nil, nil, nil)

	ready := httptest.NewRecorder()
	router.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("expected optional dependencies not to block readiness, got %d", ready.Code)
	}
}

// TestReadinessMetricsIdentifyRequiredDependencyFailure 验证Readiness 指标 Identify Required Dependency Failure的预期行为。
func TestReadinessMetricsIdentifyRequiredDependencyFailure(t *testing.T) {
	cfg := config.Load()
	cfg.Feature.MQPublisherEnabled = false
	cfg.MySQL.Required = true
	handler := Handler{cfg: cfg}

	samples := handler.collectMetrics()
	if samples[0].Name != "jxe_readiness" || samples[0].Value != 0 {
		t.Fatalf("expected readiness gauge to be zero, got %+v", samples[0])
	}
	for _, sample := range samples {
		if sample.Name == "jxe_dependency_ready" && sample.Labels["dependency"] == "mysql" {
			if sample.Value != 0 || sample.Labels["required"] != "true" {
				t.Fatalf("expected required mysql failure metric, got %+v", sample)
			}
			return
		}
	}
	t.Fatal("mysql dependency metric not found")
}
