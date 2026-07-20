package idempotency

import (
	"context"
	"testing"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

// TestStartValidatesKeyBeforeUsingDatabase 验证Start Validates 密钥 Before Using Database的预期行为。
func TestStartValidatesKeyBeforeUsingDatabase(t *testing.T) {
	store := NewStore(nil)
	for _, key := range []string{"", "short"} {
		_, err := store.Start(context.Background(), nil, 1, "customer", 1, "POST", "/orders", key, "hash")
		if err == nil {
			t.Fatalf("expected key %q to fail", key)
		}
		if problem.FromError(err).Status != 400 {
			t.Fatalf("expected bad request for key %q", key)
		}
	}
}
