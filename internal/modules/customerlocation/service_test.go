package customerlocation

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/amap"
	"jiuxiaoer-admin/backend-go/internal/modules/servicearea"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

type countingProvider struct {
	mu       sync.Mutex
	calls    int
	fail     bool
	duration map[float64]uint64
}

func (p *countingProvider) Reverse(context.Context, amap.Coordinate) (amap.AdministrativeLocation, error) {
	return amap.AdministrativeLocation{}, nil
}

func (p *countingProvider) Estimate(_ context.Context, origin, _ amap.Coordinate) (amap.RouteEstimate, error) {
	p.mu.Lock()
	p.calls++
	fail := p.fail
	duration := p.duration[origin.Longitude]
	p.mu.Unlock()
	if fail {
		return amap.RouteEstimate{}, &amap.ProviderError{Kind: amap.ErrorTimeout}
	}
	return amap.RouteEstimate{DistanceM: duration * 2, DurationSeconds: duration}, nil
}

func TestRankCandidatesRefinesOnlyThreeAndSelectsFastest(t *testing.T) {
	provider := &countingProvider{duration: map[float64]uint64{1: 300, 2: 100, 3: 200}}
	service := testService(nil, provider)
	rows := make([]servicearea.ResolvedShop, 5)
	for index := range rows {
		rows[index] = servicearea.ResolvedShop{ID: uint64(index + 1), MerchantID: 1, Name: "shop", Latitude: 22, Longitude: float64(index + 1), DistanceM: int64(100 + index), ServiceAreaVersion: 1}
	}

	shops, selected, source, degraded := service.rankCandidates(context.Background(), amap.Coordinate{Latitude: 22.5, Longitude: 113.9}, rows)
	if provider.calls != 3 {
		t.Fatalf("expected exactly three provider calls, got %d", provider.calls)
	}
	if selected == nil || selected.ID != "2" || source != "provider" || degraded {
		t.Fatalf("unexpected ranking result: selected=%+v source=%s degraded=%v", selected, source, degraded)
	}
	if len(shops) != 5 || shops[0].RouteDurationSeconds == nil || *shops[0].RouteDurationSeconds != 100 {
		t.Fatalf("unexpected candidate list: %+v", shops)
	}
}

func TestRankCandidatesFallsBackToStableLocalDistance(t *testing.T) {
	provider := &countingProvider{fail: true, duration: map[float64]uint64{}}
	service := testService(nil, provider)
	rows := []servicearea.ResolvedShop{
		{ID: 2, MerchantID: 1, Longitude: 2, DistanceM: 200, Priority: 10, ServiceAreaVersion: 1},
		{ID: 1, MerchantID: 1, Longitude: 1, DistanceM: 100, Priority: 1, ServiceAreaVersion: 1},
	}

	_, selected, source, degraded := service.rankCandidates(context.Background(), amap.Coordinate{}, rows)
	if selected == nil || selected.ID != "1" || source != "local_distance" || !degraded {
		t.Fatalf("unexpected fallback: selected=%+v source=%s degraded=%v", selected, source, degraded)
	}
}

func TestCacheAndIdempotencyKeysDoNotExposeLocationOrRawKey(t *testing.T) {
	mini := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	service := testService(client, &countingProvider{})
	point := amap.Coordinate{Latitude: 22.54321, Longitude: 113.98765}
	for _, key := range []string{service.regeoCacheKey(point), service.routeCacheKey(42, 7, point), service.switchReplayKey("loc_example", "raw-secret-key")} {
		if strings.Contains(key, "22.54321") || strings.Contains(key, "113.98765") || strings.Contains(key, "raw-secret-key") || strings.Contains(key, "loc_example") {
			t.Fatalf("sensitive input leaked into cache key %q", key)
		}
	}

	req := SwitchShopRequest{ShopID: "42", ExpectedVersion: 1}
	response := SwitchShopResponse{Version: 2, SelectionSource: "manual"}
	if err := service.setSwitchReplay(context.Background(), "loc_example", "key-a", idempotency.RequestHash(req), response, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("set replay: %v", err)
	}
	replayed, found, err := service.switchReplay(context.Background(), "loc_example", "key-a", idempotency.RequestHash(req))
	if err != nil || !found || replayed.Version != 2 {
		t.Fatalf("get replay: response=%+v found=%v err=%v", replayed, found, err)
	}
	changed := SwitchShopRequest{ShopID: "43", ExpectedVersion: 1}
	if _, _, err := service.switchReplay(context.Background(), "loc_example", "key-a", idempotency.RequestHash(changed)); problem.FromError(err).ErrorCode != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("expected reused key conflict, got %v", err)
	}
}

func TestBuildActorRequiresBoundedAnonymousSession(t *testing.T) {
	service := testService(nil, &countingProvider{})
	actor, err := service.BuildActor("42", "")
	if err != nil || actor.Type != "customer" || actor.ID != "42" {
		t.Fatalf("customer actor: %+v err=%v", actor, err)
	}
	if _, err := service.BuildActor("", "short"); problem.FromError(err).ErrorCode != "ANONYMOUS_SESSION_REQUIRED" {
		t.Fatalf("expected anonymous session validation, got %v", err)
	}
	actor, err = service.BuildActor("", "anonymous-session-123")
	if err != nil || actor.Type != "anonymous" || actor.SessionHash == "" || actor.SessionHash == "anonymous-session-123" {
		t.Fatalf("anonymous actor: %+v err=%v", actor, err)
	}
}

func testService(client *goredis.Client, provider amap.Provider) *Service {
	cfg := config.Load().CustomerLBS
	cfg.RouteRefineEnabled = true
	cfg.RegeocodeEnabled = true
	cfg.MaxRouteCandidates = 3
	cfg.MaxConcurrency = 3
	cfg.CacheHMACSecret = "customer-lbs-test-hmac-secret-123456789"
	return NewService(cfg, nil, client, provider, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}
