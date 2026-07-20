package refund

import (
	"errors"
	"testing"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

// TestIsStateMismatch 验证Is 状态 Mismatch的预期行为。
func TestIsStateMismatch(t *testing.T) {
	for _, code := range []string{"REFUND_AMOUNT_MISMATCH", "REFUND_PROVIDER_ID_MISMATCH", "REFUND_PAYMENT_MISMATCH", "REFUND_AMOUNT_EXCEEDED", "REFUND_ITEM_AMOUNT_EXCEEDED"} {
		if !isStateMismatch(problem.Conflict(code, "mismatch")) {
			t.Fatalf("expected %s to be terminal", code)
		}
	}
	if isStateMismatch(problem.Conflict("REFUND_RETRY_NOT_ALLOWED", "state")) {
		t.Fatal("retry conflict must not be classified as provider mismatch")
	}
	if isStateMismatch(errors.New("network timeout")) {
		t.Fatal("transport errors must remain retryable")
	}
}
