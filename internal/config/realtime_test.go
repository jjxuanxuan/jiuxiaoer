package config

import (
	"strings"
	"testing"
)

// TestRealtimeDefaultsRemainDisabled 验证实时消息 Defaults Remain Disabled的预期行为。
func TestRealtimeDefaultsRemainDisabled(t *testing.T) {
	t.Setenv("JXE_REALTIME_ENABLED", "false")
	t.Setenv("JXE_REALTIME_RELAY_ENABLED", "false")
	t.Setenv("JXE_MQ_CONSUMER_REALTIME_ENABLED", "false")
	cfg := Load()
	if cfg.Realtime.Enabled || cfg.Realtime.RelayEnabled || cfg.MQ.ConsumerRealtimeEnabled {
		t.Fatalf("realtime side effects must default off: %+v", cfg.Realtime)
	}
}

// TestRealtimeConfigDependenciesAndBounds 验证实时消息配置 Dependencies And Bounds的预期行为。
func TestRealtimeConfigDependenciesAndBounds(t *testing.T) {
	cfg := Load()
	cfg.Realtime.Enabled = true
	cfg.MySQL.DSN = ""
	cfg.Redis.Addr = ""
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "MySQL and Redis") {
		t.Fatalf("expected dependency validation, got %v", err)
	}
	cfg.Realtime.Enabled = false
	cfg.Realtime.RelayEnabled = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "RELAY_ENABLED") {
		t.Fatalf("expected relay dependency validation, got %v", err)
	}
	cfg.Realtime.RelayEnabled = false
	cfg.Realtime.CanaryRiderIDs = []string{"101", "101"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "CANARY_RIDER_IDS") {
		t.Fatalf("expected canary validation, got %v", err)
	}
}
