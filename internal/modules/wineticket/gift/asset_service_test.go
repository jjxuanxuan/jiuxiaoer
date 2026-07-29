package gift

import (
	"testing"

	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestNewGiftServiceOwnsIndependentAssetService(t *testing.T) {
	first, db, _ := newGiftTestService(t)
	second := NewGiftService(
		db,
		snowflake.New(92),
		"second-gift-test-pepper-with-at-least-32-bytes",
	)
	if first.assets == nil || second.assets == nil {
		t.Fatal("gift service asset capability is not configured")
	}
	if first.assets == second.assets {
		t.Fatal("gift services unexpectedly share one mutable asset service")
	}
}
