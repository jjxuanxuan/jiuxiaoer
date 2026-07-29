package core

import (
	"strings"
	"testing"
)

func TestDecodePolicyJSONIsStrict(t *testing.T) {
	var policy RefundPolicy
	err := DecodePolicyJSON(
		[]byte(`{"schema_version":1,"enabled":true,"window_hours":168,"require_never_used":true,"fee_amount":0}`),
		&policy,
		"schema_version",
		"enabled",
		"window_hours",
		"require_never_used",
		"fee_amount",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Enabled || policy.WindowHours != 168 {
		t.Fatalf("unexpected policy: %+v", policy)
	}

	err = DecodePolicyJSON(
		[]byte(`{"schema_version":1,"enabled":true,"window_hours":168,"require_never_used":true,"fee_amount":0,"unknown":1}`),
		&policy,
		"schema_version",
	)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown policy field must fail, got %v", err)
	}
}
