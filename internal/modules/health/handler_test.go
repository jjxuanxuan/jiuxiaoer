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
	cfg.MQ.ConsumerDispatchEnabled = false
	cfg.MQ.ConsumerRealtimeEnabled = false
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

func TestCP1ReleaseProfileMisconfigurationBlocksReadiness(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	cfg.MySQL.Required = false
	cfg.Redis.Required = false
	cfg.RabbitMQ.Required = false
	cfg.CP1.ReleaseProfile = config.CP1ReleaseProfilePhaseOne
	cfg.CP1.PrintEnabled = false
	router := gin.New()
	RegisterRoutes(router, router.Group("/api/v1"), cfg, nil, nil, nil, nil, nil)

	ready := httptest.NewRecorder()
	router.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected incomplete CP1 release profile to block readiness, got %d", ready.Code)
	}
	if !strings.Contains(ready.Body.String(), `"cp1_release_profile":"misconfigured"`) {
		t.Fatalf("expected release-profile status, got %s", ready.Body.String())
	}
}

func TestCP1ReleaseProfileCheckAcceptsCompleteCapabilities(t *testing.T) {
	cfg := completeCP1ReleaseHealthConfig()
	if got := (Handler{cfg: cfg}).checkCP1ReleaseProfile(); got != "ok" {
		t.Fatalf("expected complete release profile, got %q", got)
	}
	cfg.Feature.OrderIdempotencyEnabled = false
	if got := (Handler{cfg: cfg}).checkCP1ReleaseProfile(); got != "misconfigured" {
		t.Fatalf("expected disabled order idempotency to fail release readiness, got %q", got)
	}
	cfg.Feature.OrderIdempotencyEnabled = true
	cfg.Feature.StockReserveEnabled = false
	if got := (Handler{cfg: cfg}).checkCP1ReleaseProfile(); got != "misconfigured" {
		t.Fatalf("expected disabled stock reservation to fail release readiness, got %q", got)
	}
	cfg.Feature.StockReserveEnabled = true
	cfg.CP1.ReleaseProfile = config.CP1ReleaseProfileOff
	if got := (Handler{cfg: cfg}).checkCP1ReleaseProfile(); got != "disabled" {
		t.Fatalf("expected disabled release profile, got %q", got)
	}
}

func TestDispatchConsumerParticipatesInMQReadiness(t *testing.T) {
	cfg := config.Load()
	cfg.Feature.MQPublisherEnabled = false
	cfg.MQ.ConsumerNotificationEnabled = false
	cfg.MQ.ConsumerPrintEnabled = false
	cfg.MQ.ConsumerCacheEnabled = false
	cfg.MQ.ConsumerSecurityEnabled = false
	cfg.MQ.ConsumerDispatchEnabled = true
	cfg.MQ.ConsumerRealtimeEnabled = false
	if !(Handler{cfg: cfg}).mqEnabled() {
		t.Fatal("expected dispatch consumer to participate in MQ readiness")
	}
}

func completeCP1ReleaseHealthConfig() config.Config {
	cfg := config.Load()
	cfg.App.Env = "production"
	cfg.CP1.ReleaseProfile = config.CP1ReleaseProfilePhaseOne
	cfg.CP1.PrintEnabled = true
	cfg.CP1.PrintProvider = "approved-print-provider"
	cfg.CP1.WorkerEnabled = true
	cfg.CP1.ComplianceMode = "enforce"
	cfg.CP1.PickupVerificationMode = "enforce"
	cfg.CP1.DeliveryVerificationMode = "enforce"
	cfg.Realtime.Enabled = true
	cfg.Realtime.RelayEnabled = true
	cfg.Feature.MQPublisherEnabled = true
	cfg.MQ.ConsumerNotificationEnabled = true
	cfg.MQ.ConsumerPrintEnabled = true
	cfg.MQ.ConsumerCacheEnabled = true
	cfg.MQ.ConsumerSecurityEnabled = true
	cfg.MQ.ConsumerDispatchEnabled = true
	cfg.MQ.ConsumerRealtimeEnabled = true
	cfg.MQ.DBFallbackEnabled = true
	cfg.MQ.FailOnTopologyDrift = true
	cfg.Dispatch.Enabled = true
	cfg.Dispatch.WorkerEnabled = true
	cfg.RabbitMQ.URL = "amqp://rabbitmq:5672/"
	cfg.RabbitMQ.Required = true
	return cfg
}
