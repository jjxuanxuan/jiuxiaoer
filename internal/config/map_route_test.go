package config

import (
	"strings"
	"testing"
)

func TestMapRouteDefaultsAreSafe(t *testing.T) {
	t.Setenv("JXE_MAP_ROUTE_ENABLED", "false")
	t.Setenv("JXE_MAP_ROUTE_PROVIDER", "fake")
	t.Setenv("JXE_MAP_ROUTE_AMAP_KEY", "")
	cfg := Load()
	if cfg.MapRoute.Enabled || cfg.MapRoute.Provider != "fake" || cfg.MapRoute.AmapKey != "" {
		t.Fatalf("unsafe map route defaults: %#v", cfg.MapRoute)
	}
}

func TestEnabledAmapRouteRequiresDependenciesAndOfficialEndpoint(t *testing.T) {
	cfg := Load()
	cfg.MapRoute.Enabled = true
	cfg.MapRoute.Provider = "amap"
	cfg.MapRoute.AmapKey = "test-key"
	cfg.MapRoute.AmapBaseURL = "http://example.com"
	cfg.MySQL.DSN = ""
	cfg.Redis.Addr = ""
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "map route requires configured MySQL and Redis") || !strings.Contains(err.Error(), "official HTTPS endpoint") {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestMapRouteStrategyUsesModeSpecificAllowlist(t *testing.T) {
	cfg := Load()
	cfg.MapRoute.Strategy = "caller-controlled-value"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "JXE_MAP_ROUTE_STRATEGY") {
		t.Fatalf("unexpected validation error: %v", err)
	}
	cfg = Load()
	cfg.MapRoute.Mode = "driving"
	cfg.MapRoute.Strategy = "32"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("driving strategy 32 should be allowed: %v", err)
	}
}
