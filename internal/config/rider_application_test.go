package config

import (
	"strings"
	"testing"
)

// TestRiderApplicationDefaultsRemainDisabled 验证骑手申请 Defaults Remain Disabled的预期行为。
func TestRiderApplicationDefaultsRemainDisabled(t *testing.T) {
	t.Setenv("JXE_RIDER_APPLICATION_ENABLED", "false")
	cfg := Load()
	if cfg.RiderApplication.Enabled {
		t.Fatal("rider application must default to disabled")
	}
	if cfg.RiderApplication.TokenTTL.String() != "30m0s" || cfg.RiderApplication.MaxShops != 50 {
		t.Fatalf("unexpected rider application defaults: %+v", cfg.RiderApplication)
	}
}

// TestRiderApplicationRequiresMySQLAndRedisWhenEnabled 验证骑手申请 Requires My SQL And Redis When 启用状态的预期行为。
func TestRiderApplicationRequiresMySQLAndRedisWhenEnabled(t *testing.T) {
	cfg := Load()
	cfg.RiderApplication.Enabled = true
	cfg.MySQL.DSN = ""
	cfg.Redis.Addr = ""
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "rider application requires configured MySQL and Redis") {
		t.Fatalf("expected rider application dependency validation, got %v", err)
	}
}
