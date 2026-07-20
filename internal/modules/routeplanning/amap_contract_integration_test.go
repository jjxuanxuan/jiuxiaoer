package routeplanning

import (
	"context"
	"os"
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/config"
)

// TestAmapProviderNonProductionContract is opt-in because it consumes a real
// Amap Web Service quota. It uses fixed non-customer GCJ-02 coordinates and is
// never part of the default unit or CI test path.
func TestAmapProviderNonProductionContract(t *testing.T) {
	if os.Getenv("JXE_RUN_AMAP_CONTRACT") != "1" {
		t.Skip("set JXE_RUN_AMAP_CONTRACT=1 with a non-production Amap Web Service key")
	}
	cfg := config.Load()
	if cfg.MapRoute.AmapKey == "" {
		t.Fatal("JXE_MAP_ROUTE_AMAP_KEY is not configured")
	}
	provider, err := NewAmapProvider(cfg.MapRoute.AmapBaseURL, cfg.MapRoute.AmapKey, cfg.MapRoute.Timeout)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.MapRoute.Timeout+500*time.Millisecond)
	defer cancel()
	result, err := provider.Plan(ctx, ProviderRequest{
		Origin:      Coordinate{Latitude: 39.995197, Longitude: 116.466485},
		Destination: Coordinate{Latitude: 40.020642, Longitude: 116.464240},
		Mode:        "electric_bicycle",
		Strategy:    "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != "amap" || result.DistanceM == 0 || result.DurationSeconds == 0 || result.Polyline == "" {
		t.Fatalf("incomplete Amap route: provider=%s distance_m=%d duration_seconds=%d polyline_present=%t steps=%d", result.Provider, result.DistanceM, result.DurationSeconds, result.Polyline != "", len(result.Steps))
	}
	t.Logf("Amap route contract passed: distance_m=%d duration_seconds=%d steps=%d", result.DistanceM, result.DurationSeconds, len(result.Steps))
}
