package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
)

type routerSMSProvider struct{}

func (routerSMSProvider) SendVerificationCode(context.Context, string, string, time.Duration) error {
	return nil
}

// TestOpenAPICoversRegisteredBusinessRoutes 验证打开 API Covers Registered Business Routes的预期行为。
func TestOpenAPICoversRegisteredBusinessRoutes(t *testing.T) {
	router := NewRouter(Dependencies{
		Config: config.Load(),
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/docs/openapi.yaml", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("load openapi document: status=%d", recorder.Code)
	}

	pathPattern := regexp.MustCompile(`(?m)^  (/[^:]+):$`)
	documented := make(map[string]bool)
	for _, match := range pathPattern.FindAllStringSubmatch(recorder.Body.String(), -1) {
		documented[match[1]] = true
	}
	for _, route := range router.Routes() {
		if !strings.HasPrefix(route.Path, "/api/v1") {
			continue
		}
		if strings.Contains(route.Path, "/docs/") || strings.Contains(route.Path, "/swagger") {
			continue
		}
		path := strings.TrimPrefix(route.Path, "/api/v1")
		segments := strings.Split(path, "/")
		for index, segment := range segments {
			if strings.HasPrefix(segment, ":") {
				segments[index] = "{" + strings.TrimPrefix(segment, ":") + "}"
			}
		}
		path = strings.Join(segments, "/")
		if !documented[path] {
			t.Errorf("registered route is missing from OpenAPI: %s %s", route.Method, route.Path)
		}
	}
}

// TestProductionRouterDoesNotRegisterMockRoutes 验证Production 路由器 Does 不 Register Mock Routes的预期行为。
func TestProductionRouterDoesNotRegisterMockRoutes(t *testing.T) {
	cfg := config.Load()
	cfg.App.Env = "production"
	cfg.Feature.SMSMockEnabled = false
	cfg.Feature.PaymentMockEnabled = false
	cfg.WeChat.AuthMockEnabled = false
	cfg.WeChat.PayMockEnabled = false
	router := NewRouter(Dependencies{Config: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	for _, route := range router.Routes() {
		if route.Path == "/api/v1/auth/customer/send-code" || route.Path == "/api/v1/auth/customer/sms-login" || route.Path == "/api/v1/auth/rider/send-code" || route.Path == "/api/v1/auth/rider/sms-login" || route.Path == "/api/v1/orders/:id/pay/mock" {
			t.Fatalf("production router registered mock route %s", route.Path)
		}
	}
}

func TestProductionRouterRegistersTencentCloudSMSRoutes(t *testing.T) {
	cfg := config.Load()
	cfg.Feature.SMSMockEnabled = false
	cfg.SMS.Enabled = true
	router := NewRouter(Dependencies{
		Config:      cfg,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		SMSProvider: auth.SMSProvider(routerSMSProvider{}),
	})
	routes := make(map[string]bool)
	for _, route := range router.Routes() {
		routes[route.Method+" "+route.Path] = true
	}
	for _, required := range []string{
		"POST /api/v1/auth/customer/send-code",
		"POST /api/v1/auth/customer/sms-login",
		"POST /api/v1/auth/rider/send-code",
		"POST /api/v1/auth/rider/sms-login",
	} {
		if !routes[required] {
			t.Fatalf("real SMS route is missing: %s", required)
		}
	}
}

// TestRiderOnboardingRoutesSupportSelfApplicationAndAdminProvisioning 验证自主申请与管理员创建两条骑手入驻路径并存。
func TestRiderOnboardingRoutesSupportSelfApplicationAndAdminProvisioning(t *testing.T) {
	router := NewRouter(Dependencies{Config: config.Load(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	routes := make(map[string]bool)
	for _, route := range router.Routes() {
		routes[route.Method+" "+route.Path] = true
	}
	for _, required := range []string{
		"POST /api/v1/rider-applications",
		"POST /api/v1/auth/rider/send-code",
		"POST /api/v1/auth/rider/sms-login",
		"POST /api/v1/auth/rider-application/sms-login",
		"POST /api/v1/admin/rider-applications/:id/review",
		"POST /api/v1/admin/riders",
		"POST /api/v1/admin/riders/:id/review",
		"PATCH /api/v1/admin/riders/:id/status",
	} {
		if !routes[required] {
			t.Fatalf("required rider onboarding route is missing: %s", required)
		}
	}
	if routes["POST /api/v1/auth/rider/login"] {
		t.Fatal("legacy rider password login route must not be registered")
	}
}
