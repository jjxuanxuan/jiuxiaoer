package config

import (
	"strings"
	"testing"
)

func TestDeliveryReturnConfigIsClosedByDefault(t *testing.T) {
	t.Parallel()
	cfg := Load()
	if cfg.DeliveryReturn.Enabled || cfg.DeliveryReturn.RiderWriteEnabled || cfg.DeliveryReturn.ApprovalEnabled ||
		cfg.DeliveryReturn.ReceiptEnabled || cfg.DeliveryReturn.SystemAfterSaleEnabled || cfg.DeliveryReturn.NotificationEnabled {
		t.Fatalf("delivery return side effects must default to disabled: %+v", cfg.DeliveryReturn)
	}
}

func TestDeliveryReturnBranchRequiresMasterSwitch(t *testing.T) {
	t.Parallel()
	cfg := Load()
	cfg.DeliveryReturn.RiderWriteEnabled = true
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "branch switches require JXE_DELIVERY_RETURN_ENABLED=true") {
		t.Fatalf("expected master switch validation, got %v", err)
	}
}

func TestDeliveryReturnApprovalRequiresRefundDependencies(t *testing.T) {
	t.Parallel()
	cfg := Load()
	cfg.DeliveryReturn.Enabled = true
	cfg.DeliveryReturn.ApprovalEnabled = true
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "approval requires system after-sale and refund execution") {
		t.Fatalf("expected approval dependency validation, got %v", err)
	}
}

func TestDeliveryReturnAllowlistRejectsMixedFullRolloutMarker(t *testing.T) {
	t.Parallel()
	cfg := Load()
	cfg.DeliveryReturn.RiderAllowlist = []string{"*", "42"}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "full rollout marker '*' or 'all' must be the only value") {
		t.Fatalf("expected allowlist validation, got %v", err)
	}
}
