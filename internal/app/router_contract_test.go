package app

import (
	"context"
	"errors"
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
	"jiuxiaoer-admin/backend-go/internal/modules/compliance"
)

type routerSMSProvider struct{}

func (routerSMSProvider) SendVerificationCode(context.Context, string, string, time.Duration) error {
	return nil
}

type wiringComplianceProvider struct {
	code          string
	callbackCalls int
}

func (p *wiringComplianceProvider) Code() string { return p.code }

func (*wiringComplianceProvider) CreateSession(context.Context, compliance.ProviderSessionRequest) (compliance.ProviderSession, error) {
	return compliance.ProviderSession{}, nil
}

func (p *wiringComplianceProvider) ParseCallback(context.Context, http.Header, []byte) (compliance.ProviderCallback, error) {
	p.callbackCalls++
	return compliance.ProviderCallback{}, context.Canceled
}

func (*wiringComplianceProvider) Query(context.Context, string) (compliance.ProviderResult, error) {
	return compliance.ProviderResult{}, nil
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
	cfg.CP1.ComplianceMode = "off"
	router := NewRouter(Dependencies{Config: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	for _, route := range router.Routes() {
		if route.Path == "/api/v1/auth/customer/send-code" || route.Path == "/api/v1/auth/customer/sms-login" || route.Path == "/api/v1/auth/rider/send-code" || route.Path == "/api/v1/auth/rider/sms-login" || route.Path == "/api/v1/orders/:id/pay/mock" {
			t.Fatalf("production router registered mock route %s", route.Path)
		}
	}
}

func TestRouterWiresMatchingInjectedComplianceProvider(t *testing.T) {
	cfg := config.Load()
	cfg.CP1.ComplianceMode = "enforce"
	cfg.CP1.IdentityProvider = "contract-provider"
	provider := &wiringComplianceProvider{code: "contract-provider"}
	router := NewRouter(Dependencies{
		Config:             cfg,
		Log:                slog.New(slog.NewTextHandler(io.Discard, nil)),
		ComplianceProvider: provider,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/identity-verifications/contract-provider/callbacks", strings.NewReader(`{}`))
	router.ServeHTTP(recorder, request)
	if provider.callbackCalls != 1 {
		t.Fatalf("callback adapter calls=%d want=1", provider.callbackCalls)
	}
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("callback status=%d want=%d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestRouterRejectsMismatchedInjectedComplianceProvider(t *testing.T) {
	cfg := config.Load()
	cfg.CP1.ComplianceMode = "enforce"
	cfg.CP1.IdentityProvider = "configured-provider"
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("router must reject an adapter whose code differs from configuration")
		}
	}()
	_ = routerComplianceProvider(Dependencies{
		Config:             cfg,
		ComplianceProvider: &wiringComplianceProvider{code: "different-provider"},
	})
}

func TestNewServerRejectsUnregisteredComplianceProviderBeforeInfrastructureStartup(t *testing.T) {
	cfg := config.Load()
	cfg.App.Env = "test"
	cfg.CP1.ComplianceMode = "enforce"
	cfg.CP1.IdentityProvider = "unregistered-contract-provider"
	_, err := NewServer(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !errors.Is(err, compliance.ErrProviderNotRegistered) {
		t.Fatalf("startup error=%v want ErrProviderNotRegistered", err)
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
