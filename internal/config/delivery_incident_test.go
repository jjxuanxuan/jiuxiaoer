package config

import (
	"strings"
	"testing"
	"time"
)

func TestDeliveryIncidentDefaultsFailClosed(t *testing.T) {
	for _, key := range []string{
		"JXE_DELIVERY_INCIDENT_ENABLED",
		"JXE_DELIVERY_INCIDENT_AUTO_RESOLVE_ENABLED",
		"JXE_DELIVERY_INCIDENT_NOTIFICATION_ENABLED",
		"JXE_DELIVERY_INCIDENT_RIDER_ALLOWLIST",
		"JXE_DELIVERY_INCIDENT_SHOP_ALLOWLIST",
		"JXE_EVIDENCE_VIEW_BASE_URL",
		"JXE_EVIDENCE_VIEW_SECRET",
	} {
		t.Setenv(key, "")
	}
	cfg := Load().DeliveryIncident
	if cfg.Enabled || cfg.AutoResolveEnabled || cfg.NotificationEnabled || len(cfg.RiderAllowlist) != 0 || len(cfg.ShopAllowlist) != 0 {
		t.Fatalf("delivery incident defaults are not fail-closed: %+v", cfg)
	}
	if cfg.CreateRatePerHour != 20 || cfg.EvidenceRatePerHour != 30 || cfg.CreateIPRatePerHour != 100 || cfg.EvidenceIPRatePerHour != 150 {
		t.Fatalf("unexpected delivery incident limits: %+v", cfg)
	}
	if cfg.EvidenceViewBaseURL != "" || cfg.EvidenceViewSecret != "" || cfg.EvidenceViewTTL != 5*time.Minute {
		t.Fatalf("unexpected evidence view defaults: %+v", cfg)
	}
}

func TestDeliveryIncidentEvidenceViewConfiguration(t *testing.T) {
	cfg := Load()
	cfg.DeliveryIncident.EvidenceViewBaseURL = "https://media.example.test/private/evidence"
	cfg.DeliveryIncident.EvidenceViewSecret = strings.Repeat("s", 32)
	cfg.DeliveryIncident.EvidenceViewTTL = 5 * time.Minute
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid evidence view configuration rejected: %v", err)
	}
	cfg.App.Env = "production"
	cfg.DeliveryIncident.EvidenceViewBaseURL = "http://media.example.test/private/evidence"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("insecure evidence view URL was accepted: %v", err)
	}
}

func TestEnabledDeliveryIncidentRequiresEvidenceViewOutsideLocal(t *testing.T) {
	cfg := Load()
	cfg.App.Env = "preprod"
	cfg.DeliveryIncident.Enabled = true
	cfg.DeliveryIncident.EvidenceViewBaseURL = ""
	cfg.DeliveryIncident.EvidenceViewSecret = ""
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "enabled delivery incidents require") {
		t.Fatalf("enabled preprod feature without evidence viewing was accepted: %v", err)
	}
}

func TestDeliveryIncidentConfigDependenciesAndAllowlists(t *testing.T) {
	cfg := Load()
	cfg.DeliveryIncident.Enabled = false
	cfg.DeliveryIncident.AutoResolveEnabled = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "AUTO_RESOLVE_ENABLED") {
		t.Fatalf("expected auto-resolve dependency error, got %v", err)
	}
	cfg.DeliveryIncident.AutoResolveEnabled = false
	cfg.DeliveryIncident.RiderAllowlist = []string{"42", "42"}
	cfg.DeliveryIncident.ShopAllowlist = []string{"invalid"}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "RIDER_ALLOWLIST") || !strings.Contains(err.Error(), "SHOP_ALLOWLIST") {
		t.Fatalf("expected invalid allowlists to fail, got %v", err)
	}
}

func TestDeliveryIncidentFullRolloutAllowlist(t *testing.T) {
	cfg := Load()
	cfg.DeliveryIncident.RiderAllowlist = []string{"*"}
	cfg.DeliveryIncident.ShopAllowlist = []string{"all"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("full rollout markers rejected: %v", err)
	}
	cfg.DeliveryIncident.RiderAllowlist = []string{"*", "42"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "only value") {
		t.Fatalf("mixed full rollout marker accepted: %v", err)
	}
}
