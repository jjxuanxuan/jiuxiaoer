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

// TestOpenAPICoversRegisteredBusinessRoutes 验证 OpenAPI 覆盖已注册的业务路由。
func TestOpenAPICoversRegisteredBusinessRoutes(t *testing.T) {
	cfg := config.Load()
	cfg.WineTicket.Enabled = true
	cfg.WineTicket.PackageReadEnabled = true
	cfg.WineTicket.AdminEnabled = true
	cfg.WineTicket.PurchaseEnabled = true
	cfg.WineTicket.GiftEnabled = true
	cfg.WineTicket.ReminderEnabled = true
	cfg.WineTicket.RedemptionEnabled = true
	cfg.WineTicket.RenewalEnabled = true
	cfg.WineTicket.RefundEnabled = true
	router := NewRouter(Dependencies{
		Config: cfg,
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

// TestProductionRouterDoesNotRegisterMockRoutes 验证生产路由器不会注册模拟路由。
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

func TestWineTicketPackageRoutesAreFailClosedByMasterAndBranchFlags(t *testing.T) {
	publicRoutes := []string{
		"GET /api/v1/wine-tickets/packages",
		"GET /api/v1/wine-tickets/packages/:package_no",
	}
	adminRoutes := []string{
		"GET /api/v1/admin/wine-tickets/packages",
		"POST /api/v1/admin/wine-tickets/packages",
		"GET /api/v1/admin/wine-tickets/packages/:package_no",
		"PUT /api/v1/admin/wine-tickets/packages/:package_no",
		"POST /api/v1/admin/wine-tickets/packages/:package_no/publish",
		"POST /api/v1/admin/wine-tickets/packages/:package_no/unpublish",
		"GET /api/v1/admin/wine-tickets/purchases",
		"GET /api/v1/admin/wine-tickets/lots",
		"GET /api/v1/admin/wine-tickets/delivery-time-slots",
		"POST /api/v1/admin/wine-tickets/delivery-time-slots",
		"PUT /api/v1/admin/wine-tickets/delivery-time-slots/:slot_id",
		"GET /api/v1/admin/wine-tickets/exceptions",
		"GET /api/v1/admin/wine-tickets/exceptions/:exception_no",
		"POST /api/v1/admin/wine-tickets/exceptions/:exception_no/resolution",
	}
	tests := []struct {
		name       string
		master     bool
		publicRead bool
		admin      bool
		wantPublic bool
		wantAdmin  bool
	}{
		{name: "all switches off"},
		{name: "branch switches cannot bypass master", publicRead: true, admin: true},
		{name: "master alone exposes nothing", master: true},
		{name: "public branch only", master: true, publicRead: true, wantPublic: true},
		{name: "admin branch only", master: true, admin: true, wantAdmin: true},
		{name: "both branches", master: true, publicRead: true, admin: true, wantPublic: true, wantAdmin: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Load()
			cfg.WineTicket.Enabled = test.master
			cfg.WineTicket.PackageReadEnabled = test.publicRead
			cfg.WineTicket.AdminEnabled = test.admin
			router := NewRouter(Dependencies{
				Config: cfg,
				Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			routes := make(map[string]bool)
			for _, route := range router.Routes() {
				routes[route.Method+" "+route.Path] = true
			}
			for _, route := range publicRoutes {
				if routes[route] != test.wantPublic {
					t.Errorf("public route %q registered=%t want=%t", route, routes[route], test.wantPublic)
				}
			}
			for _, route := range adminRoutes {
				if routes[route] != test.wantAdmin {
					t.Errorf("admin route %q registered=%t want=%t", route, routes[route], test.wantAdmin)
				}
			}
		})
	}
}

func TestWineTicketAdminPackageRoutesUseProtectedAuthenticationChain(t *testing.T) {
	cfg := config.Load()
	cfg.WineTicket.Enabled = true
	cfg.WineTicket.AdminEnabled = true
	router := NewRouter(Dependencies{
		Config: cfg,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/admin/wine-tickets/packages", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated admin package request status=%d want=%d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestWineTicketCustomerRoutesAreFailClosedByMasterAndBranchFlags(t *testing.T) {
	purchaseContinuityRoutes := []string{
		"GET /api/v1/wine-tickets/purchases",
		"GET /api/v1/wine-tickets/purchases/:purchase_no",
		"POST /api/v1/wine-tickets/purchases/:purchase_no/payment/confirm",
	}
	purchaseCreationRoutes := []string{
		"POST /api/v1/wine-tickets/purchases",
	}
	cabinetRoutes := []string{
		"GET /api/v1/wine-tickets/cabinet",
		"GET /api/v1/wine-tickets/lots",
		"GET /api/v1/wine-tickets/lots/:lot_no",
		"GET /api/v1/wine-tickets/transactions",
	}
	redemptionContinuityRoutes := []string{
		"GET /api/v1/wine-tickets/redemptions",
		"GET /api/v1/wine-tickets/redemptions/:redemption_no",
		"POST /api/v1/wine-tickets/redemptions/:redemption_no/cancel",
	}
	redemptionCreationRoutes := []string{
		"GET /api/v1/wine-tickets/delivery-time-slots",
		"POST /api/v1/wine-tickets/redemptions",
	}
	giftPreviewRoutes := []string{
		"GET /api/v1/wine-tickets/gift-claims/preview",
	}
	giftContinuityRoutes := []string{
		"GET /api/v1/wine-tickets/gifts",
		"GET /api/v1/wine-tickets/gifts/:gift_no",
		"POST /api/v1/wine-tickets/gifts/:gift_no/cancel",
		"POST /api/v1/wine-tickets/gift-claims",
	}
	giftCreationRoutes := []string{
		"POST /api/v1/wine-tickets/gifts",
		"POST /api/v1/wine-tickets/gifts/:gift_no/share-tokens",
	}
	reminderRoutes := []string{
		"GET /api/v1/wine-tickets/notification-subscriptions",
		"POST /api/v1/wine-tickets/notification-subscriptions",
	}
	renewalContinuityRoutes := []string{
		"GET /api/v1/wine-tickets/renewals",
		"GET /api/v1/wine-tickets/renewals/:renewal_no",
		"POST /api/v1/wine-tickets/renewals/:renewal_no/payment/confirm",
	}
	renewalCreationRoutes := []string{
		"GET /api/v1/wine-tickets/lots/:lot_no/renewal-quote",
		"POST /api/v1/wine-tickets/lots/:lot_no/renewals",
	}
	refundContinuityRoutes := []string{
		"GET /api/v1/wine-tickets/refunds",
		"GET /api/v1/wine-tickets/refunds/:refund_no",
	}
	refundCreationRoutes := []string{
		"GET /api/v1/wine-tickets/purchases/:purchase_no/refund-quote",
		"POST /api/v1/wine-tickets/purchases/:purchase_no/refunds",
	}
	tests := []struct {
		name       string
		master     bool
		purchase   bool
		redemption bool
		gift       bool
		reminder   bool
		renewal    bool
		refund     bool
	}{
		{name: "all switches off"},
		{
			name:     "branch switches cannot bypass master",
			purchase: true, redemption: true, gift: true, reminder: true, renewal: true, refund: true,
		},
		{name: "master alone preserves continuity routes", master: true},
		{
			name:   "purchase opens only new purchase action",
			master: true, purchase: true,
		},
		{
			name:   "gift opens only new gift and share token actions",
			master: true, gift: true,
		},
		{
			name:   "reminder opens notification consent only",
			master: true, reminder: true,
		},
		{
			name:   "other creation branches do not affect continuity routes",
			master: true, redemption: true, renewal: true, refund: true,
		},
	}

	assertRoutes := func(t *testing.T, registered map[string]bool, routes []string, want bool) {
		t.Helper()
		for _, route := range routes {
			if registered[route] != want {
				t.Errorf("route %q registered=%t want=%t", route, registered[route], want)
			}
		}
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Load()
			cfg.WineTicket = config.WineTicketConfig{
				Enabled:           test.master,
				PurchaseEnabled:   test.purchase,
				RedemptionEnabled: test.redemption,
				GiftEnabled:       test.gift,
				ReminderEnabled:   test.reminder,
				RenewalEnabled:    test.renewal,
				RefundEnabled:     test.refund,
			}
			router := NewRouter(Dependencies{
				Config: cfg,
				Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			registered := make(map[string]bool)
			for _, route := range router.Routes() {
				registered[route.Method+" "+route.Path] = true
			}
			assertRoutes(t, registered, purchaseContinuityRoutes, test.master)
			assertRoutes(t, registered, purchaseCreationRoutes, test.master && test.purchase)
			assertRoutes(t, registered, cabinetRoutes, test.master)
			assertRoutes(t, registered, redemptionContinuityRoutes, test.master)
			assertRoutes(t, registered, redemptionCreationRoutes, test.master && test.redemption)
			assertRoutes(t, registered, giftPreviewRoutes, test.master)
			assertRoutes(t, registered, giftContinuityRoutes, test.master)
			assertRoutes(t, registered, giftCreationRoutes, test.master && test.gift)
			assertRoutes(t, registered, reminderRoutes, test.master && test.reminder)
			assertRoutes(t, registered, renewalContinuityRoutes, test.master)
			assertRoutes(t, registered, renewalCreationRoutes, test.master && test.renewal)
			assertRoutes(t, registered, refundContinuityRoutes, test.master)
			assertRoutes(t, registered, refundCreationRoutes, test.master && test.refund)
		})
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
