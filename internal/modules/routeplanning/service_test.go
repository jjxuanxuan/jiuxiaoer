package routeplanning

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
)

type countingProvider struct {
	mu      sync.Mutex
	calls   int
	err     error
	blocked <-chan struct{}
}

func (p *countingProvider) Plan(ctx context.Context, _ ProviderRequest) (ProviderResult, error) {
	if p.blocked != nil {
		select {
		case <-p.blocked:
		case <-ctx.Done():
			return ProviderResult{}, ctx.Err()
		}
	}
	p.mu.Lock()
	p.calls++
	err := p.err
	p.mu.Unlock()
	if err != nil {
		return ProviderResult{}, err
	}
	return ProviderResult{DistanceM: 1200, DurationSeconds: 240, Polyline: "116.1,39.1;116.2,39.2", Steps: []RouteStep{{Instruction: "直行", DistanceM: 1200, DurationSeconds: 240, Polyline: "116.1,39.1;116.2,39.2"}}, Provider: "amap"}, nil
}

func (p *countingProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type routeTestAccount struct {
	ID        uint64
	Status    string
	DeletedAt *time.Time
}

func (routeTestAccount) TableName() string { return "accounts" }

type routeTestRider struct {
	ID, AccountID        uint64
	Status, ReviewStatus string
	DeletedAt            *time.Time
}

func (routeTestRider) TableName() string { return "riders" }

type routeTestDelivery struct {
	ID, OrderID                       uint64
	RiderID                           *uint64
	Status                            string
	PickupSnapshot, RecipientSnapshot []byte
	DeletedAt                         *time.Time
}

func (routeTestDelivery) TableName() string { return "delivery_orders" }

type routeTestRuntime struct {
	RiderID                        uint64 `gorm:"primaryKey"`
	Latitude, Longitude, AccuracyM *float64
	CoordinateSystem               string
	CapturedAt                     *time.Time
}

func (routeTestRuntime) TableName() string { return "rider_runtime_states" }

type serviceFixture struct {
	service  *Service
	db       *gorm.DB
	mini     *miniredis.Miniredis
	provider *countingProvider
	claims   *auth.Claims
	cfg      config.MapRouteConfig
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+stringsForTest(t.Name())+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&routeTestAccount{}, &routeTestRider{}, &routeTestDelivery{}, &routeTestRuntime{}); err != nil {
		t.Fatal(err)
	}
	mini := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mini.Addr(), MaxRetries: -1, DialTimeout: 20 * time.Millisecond, ReadTimeout: 20 * time.Millisecond, WriteTimeout: 20 * time.Millisecond})
	t.Cleanup(func() { _ = redisClient.Close() })
	cfg := config.Load().MapRoute
	cfg.Enabled, cfg.CacheTTL, cfg.StaleTTL = true, time.Minute, 10*time.Minute
	provider := &countingProvider{}
	service := NewService(cfg, db, redisClient, provider, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	riderID, accountID := uint64(20), uint64(10)
	now := time.Now()
	lat, lng, accuracy := 39.90909, 116.434307, 10.0
	pickup, _ := json.Marshal(map[string]any{"latitude": 39.90816, "longitude": 116.434446, "coordinate_system": "gcj02"})
	recipient, _ := json.Marshal(map[string]any{"latitude": 39.91816, "longitude": 116.444446, "coordinate_system": "gcj02"})
	if err := db.Create(&routeTestAccount{ID: accountID, Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&routeTestRider{ID: riderID, AccountID: accountID, Status: "active", ReviewStatus: "approved"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&routeTestRuntime{RiderID: riderID, Latitude: &lat, Longitude: &lng, AccuracyM: &accuracy, CoordinateSystem: "gcj02", CapturedAt: &now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&routeTestDelivery{ID: 30, OrderID: 40, RiderID: &riderID, Status: "accepted", PickupSnapshot: pickup, RecipientSnapshot: recipient}).Error; err != nil {
		t.Fatal(err)
	}
	claims := &auth.Claims{AccountType: "rider", AccountID: "10", RiderID: "20", Permissions: []string{"delivery:route"}}
	return &serviceFixture{service: service, db: db, mini: mini, provider: provider, claims: claims, cfg: cfg}
}

func TestCurrentReturnsProviderThenFreshCache(t *testing.T) {
	f := newServiceFixture(t)
	first, err := f.service.Current(context.Background(), f.claims, "30", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Source != "provider" || first.Stage != "pickup" || first.CoordinateSystem != "gcj02" {
		t.Fatalf("unexpected first plan: %#v", first)
	}
	second, err := f.service.Current(context.Background(), f.claims, "30", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if second.Source != "cache" || f.provider.count() != 1 {
		t.Fatalf("expected one provider call and a cache hit: calls=%d source=%s", f.provider.count(), second.Source)
	}
}

func TestCurrentUsesRecipientWhileDelivering(t *testing.T) {
	f := newServiceFixture(t)
	if err := f.db.Model(&routeTestDelivery{}).Where("id=30").Update("status", "delivering").Error; err != nil {
		t.Fatal(err)
	}
	plan, err := f.service.Current(context.Background(), f.claims, "30", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Stage != "delivery" || plan.Destination.Latitude != 39.91816 {
		t.Fatalf("unexpected delivery destination: %#v", plan)
	}
}

func TestCurrentFallsBackToStaleCache(t *testing.T) {
	f := newServiceFixture(t)
	if _, err := f.service.Current(context.Background(), f.claims, "30", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	f.mini.FastForward(f.cfg.CacheTTL + time.Second)
	f.provider.mu.Lock()
	f.provider.err = &ProviderError{Kind: ProviderTimeout}
	f.provider.mu.Unlock()
	plan, err := f.service.Current(context.Background(), f.claims, "30", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Source != "stale_cache" || !plan.Degraded {
		t.Fatalf("expected stale degraded route: %#v", plan)
	}
}

func TestCurrentRejectsWrongRiderAndStaleLocation(t *testing.T) {
	f := newServiceFixture(t)
	other := *f.claims
	other.RiderID = "21"
	if _, err := f.service.Current(context.Background(), &other, "30", "127.0.0.1"); problem.FromError(err).ErrorCode != "DELIVERY_ROUTE_FORBIDDEN" {
		t.Fatalf("wrong rider error = %v", err)
	}
	old := time.Now().Add(-10 * time.Minute)
	if err := f.db.Model(&routeTestRuntime{}).Where("rider_id=20").Update("captured_at", old).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Current(context.Background(), f.claims, "30", "127.0.0.1"); problem.FromError(err).ErrorCode != "RIDER_LOCATION_STALE" {
		t.Fatalf("stale location error = %v", err)
	}
}

func TestCurrentFailsClosedWithoutRedis(t *testing.T) {
	f := newServiceFixture(t)
	f.mini.Close()
	_, err := f.service.Current(context.Background(), f.claims, "30", "127.0.0.1")
	if problem.FromError(err).ErrorCode != "ROUTE_CACHE_UNAVAILABLE" {
		t.Fatalf("error = %v", err)
	}
}

func TestCurrentSingleflightCoalescesProviderCalls(t *testing.T) {
	f := newServiceFixture(t)
	release := make(chan struct{})
	f.provider.blocked = release
	const callers = 12
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func(index int) {
			_, err := f.service.Current(context.Background(), f.claims, "30", fmt.Sprintf("127.0.0.%d", index+1))
			errs <- err
		}(i)
	}
	time.Sleep(30 * time.Millisecond)
	close(release)
	for i := 0; i < callers; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if f.provider.count() != 1 {
		t.Fatalf("provider calls = %d, want 1", f.provider.count())
	}
}

func TestCurrentRequiresFeatureAndExactPermission(t *testing.T) {
	f := newServiceFixture(t)
	f.service.cfg.Enabled = false
	if _, err := f.service.Current(context.Background(), f.claims, "30", "127.0.0.1"); problem.FromError(err).ErrorCode != "MAP_ROUTE_DISABLED" {
		t.Fatalf("disabled error = %v", err)
	}
	f.service.cfg.Enabled = true
	claims := *f.claims
	claims.Permissions = []string{"delivery:list"}
	if _, err := f.service.Current(context.Background(), &claims, "30", "127.0.0.1"); problem.FromError(err).ErrorCode != "DELIVERY_ROUTE_FORBIDDEN" {
		t.Fatalf("permission error = %v", err)
	}
}

func TestCurrentChecksStatusBeforeReturningCachedRoute(t *testing.T) {
	f := newServiceFixture(t)
	if _, err := f.service.Current(context.Background(), f.claims, "30", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&routeTestDelivery{}).Where("id=30").Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Current(context.Background(), f.claims, "30", "127.0.0.1"); problem.FromError(err).ErrorCode != "DELIVERY_ROUTE_NOT_AVAILABLE" {
		t.Fatalf("completed delivery error = %v", err)
	}
	if f.provider.count() != 1 {
		t.Fatalf("provider calls = %d, want 1", f.provider.count())
	}
}

func TestCurrentRateLimitsEveryConfiguredScopeButNotFreshCache(t *testing.T) {
	tests := []struct {
		name  string
		scope string
		apply func(*Service)
	}{
		{name: "rider", scope: "rider", apply: func(service *Service) {
			service.cfg.RiderRatePerMinute, service.cfg.AccountRatePerMinute, service.cfg.IPRatePerMinute = 1, 100, 100
		}},
		{name: "account", scope: "account", apply: func(service *Service) {
			service.cfg.RiderRatePerMinute, service.cfg.AccountRatePerMinute, service.cfg.IPRatePerMinute = 100, 1, 100
		}},
		{name: "ip", scope: "ip", apply: func(service *Service) {
			service.cfg.RiderRatePerMinute, service.cfg.AccountRatePerMinute, service.cfg.IPRatePerMinute = 100, 100, 1
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newServiceFixture(t)
			test.apply(f.service)
			if _, err := f.service.Current(context.Background(), f.claims, "30", "203.0.113.8"); err != nil {
				t.Fatal(err)
			}
			if cached, err := f.service.Current(context.Background(), f.claims, "30", "203.0.113.8"); err != nil || cached.Source != "cache" {
				t.Fatalf("fresh cache must bypass provider protection limit: plan=%#v err=%v", cached, err)
			}
			for _, key := range f.mini.Keys() {
				if strings.HasPrefix(key, "route:fresh:") {
					f.mini.Del(key)
				}
			}
			_, err := f.service.Current(context.Background(), f.claims, "30", "203.0.113.8")
			details := problem.FromError(err)
			if details.ErrorCode != "ROUTE_RATE_LIMITED" {
				t.Fatalf("error = %v", err)
			}
			data, ok := details.Data.(map[string]any)
			if !ok || data["scope"] != test.scope || data["retry_after_seconds"] != 60 {
				t.Fatalf("rate limit data = %#v", details.Data)
			}
			if f.provider.count() != 1 {
				t.Fatalf("provider calls = %d, want 1", f.provider.count())
			}
		})
	}
}

func TestCurrentRejectsWhenGlobalProviderConcurrencyIsFull(t *testing.T) {
	f := newServiceFixture(t)
	for index := 0; index < cap(f.service.semaphore); index++ {
		f.service.semaphore <- struct{}{}
	}
	defer func() {
		for index := 0; index < cap(f.service.semaphore); index++ {
			<-f.service.semaphore
		}
	}()
	_, err := f.service.Current(context.Background(), f.claims, "30", "127.0.0.1")
	details := problem.FromError(err)
	if details.ErrorCode != "ROUTE_RATE_LIMITED" {
		t.Fatalf("error = %v", err)
	}
	data, _ := details.Data.(map[string]any)
	if data["scope"] != "global_concurrency" || f.provider.count() != 0 {
		t.Fatalf("data=%#v provider calls=%d", data, f.provider.count())
	}
}

func TestCurrentReturnsStableProviderErrorWithoutStaleCache(t *testing.T) {
	f := newServiceFixture(t)
	f.provider.err = &ProviderError{Kind: ProviderTimeout, Code: "must-not-leak"}
	_, err := f.service.Current(context.Background(), f.claims, "30", "127.0.0.1")
	details := problem.FromError(err)
	if details.Status != 503 || details.ErrorCode != "ROUTE_PROVIDER_TIMEOUT" || strings.Contains(details.Error(), "must-not-leak") {
		t.Fatalf("provider error = %#v", details)
	}
}

func TestRouteObservabilityDoesNotExposeSecretsOrCoordinates(t *testing.T) {
	f := newServiceFixture(t)
	var logs bytes.Buffer
	f.service.log = slog.New(slog.NewJSONHandler(&logs, nil))
	f.service.cfg.AmapKey = "super-secret-amap-key"
	ctx := requestctx.WithRequestID(context.Background(), "route-request-1")
	if _, err := f.service.Current(ctx, f.claims, "30", "203.0.113.8"); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"super-secret-amap-key", "39.90909", "116.434307", "116.434446", "203.0.113.8", "116.1,39.1"} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatalf("log leaked %q: %s", forbidden, logs.String())
		}
		for _, key := range f.mini.Keys() {
			if strings.Contains(key, forbidden) {
				t.Fatalf("Redis key leaked %q: %s", forbidden, key)
			}
		}
	}
	for _, sample := range f.service.metrics.collect() {
		for label, value := range sample.Labels {
			if label == "rider_id" || label == "delivery_id" || strings.Contains(value, "30") || strings.Contains(value, "116.") || strings.Contains(value, "39.") {
				t.Fatalf("unsafe metric label: %s=%s in %#v", label, value, sample)
			}
		}
	}
}

func TestValidateProviderResultAllowsZeroLengthRoute(t *testing.T) {
	if err := validateProviderResult(ProviderResult{Provider: "fake"}); err != nil {
		t.Fatalf("zero length route should be valid: %v", err)
	}
}

func stringsForTest(value string) string {
	value = strings.NewReplacer("/", "_", " ", "_").Replace(value)
	return value
}
