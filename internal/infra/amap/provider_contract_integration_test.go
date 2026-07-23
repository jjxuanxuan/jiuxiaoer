package amap_test

import (
	"context"
	"os"
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/amap"
)

// This opt-in contract consumes the separately configured customer-LBS Amap
// quota. Fixed non-customer coordinates keep it safe to run outside CI.
func TestCustomerLBSAmapProviderNonProductionContract(t *testing.T) {
	if os.Getenv("JXE_RUN_C_LBS_AMAP_CONTRACT") != "1" {
		t.Skip("set JXE_RUN_C_LBS_AMAP_CONTRACT=1 with a non-production customer-LBS Amap key")
	}
	cfg := config.Load()
	if cfg.CustomerLBS.AmapKey == "" {
		t.Fatal("JXE_C_LBS_AMAP_KEY is not configured")
	}
	provider, err := amap.NewClient(
		cfg.CustomerLBS.AmapBaseURL,
		cfg.CustomerLBS.AmapKey,
		cfg.CustomerLBS.RegeocodeTimeout,
		cfg.CustomerLBS.RouteTimeout,
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	origin := amap.Coordinate{Latitude: 22.541000, Longitude: 113.931000}
	destination := amap.Coordinate{Latitude: 22.552000, Longitude: 113.942000}
	location, err := provider.Reverse(ctx, destination)
	if err != nil {
		t.Fatalf("real reverse geocode failed: %v", err)
	}
	if location.Province == "" || location.City == "" || location.District == "" || len(location.DistrictCode) != 6 || location.FormattedAddress == "" {
		t.Fatalf("incomplete reverse geocode result: %+v", location)
	}
	route, err := provider.Estimate(ctx, origin, destination)
	if err != nil {
		t.Fatalf("real route estimate failed: %v", err)
	}
	if route.DistanceM == 0 || route.DurationSeconds == 0 {
		t.Fatalf("incomplete route estimate: %+v", route)
	}
	t.Logf("customer LBS Amap contract passed: district_code=%s distance_m=%d duration_seconds=%d", location.DistrictCode, route.DistanceM, route.DurationSeconds)
}
