package deliveryverification

import (
	"testing"

	"jiuxiaoer-admin/backend-go/internal/config"
)

// TestGeneratedCodesAndDomainSeparatedHashes 验证Generated Codes And Domain Separated Hashes的预期行为。
func TestGeneratedCodesAndDomainSeparatedHashes(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		code, err := newCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != 6 {
			t.Fatalf("code %q is not six digits", code)
		}
		for _, r := range code {
			if r < '0' || r > '9' {
				t.Fatalf("code %q is not numeric", code)
			}
		}
		seen[code] = true
	}
	if len(seen) < 95 {
		t.Fatalf("unexpectedly low code diversity: %d", len(seen))
	}
	cfg := config.CP1Config{VerificationPepper: "unit-test-pepper"}
	pickup := hashCode(cfg, 1, "pickup", "123456")
	delivery := hashCode(cfg, 1, "delivery", "123456")
	otherOrder := hashCode(cfg, 2, "pickup", "123456")
	if pickup == delivery || pickup == otherOrder {
		t.Fatal("verification hashes are not domain separated")
	}
}

// TestVerificationDTOAttemptBudget 验证核验DTO尝试 Budget的预期行为。
func TestVerificationDTOAttemptBudget(t *testing.T) {
	got := dto(Verification{MaxAttempts: 5, FailedAttempts: 2}, "")
	if got.RemainingAttempts != 3 {
		t.Fatalf("remaining attempts = %d", got.RemainingAttempts)
	}
}
