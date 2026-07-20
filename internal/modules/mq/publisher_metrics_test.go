package mq

import (
	"database/sql"
	"testing"
	"time"
)

// TestPendingAgeSecondsHandlesEmptyOutbox 验证待处理 Age Seconds Handles 空值发件箱事件的预期行为。
func TestPendingAgeSecondsHandlesEmptyOutbox(t *testing.T) {
	if got := pendingAgeSeconds(time.Now(), sql.NullTime{}); got != 0 {
		t.Fatalf("expected zero age for empty outbox, got %f", got)
	}
}

// TestPendingAgeSecondsClampsFutureTimestamp 验证待处理 Age Seconds Clamps Future Timestamp的预期行为。
func TestPendingAgeSecondsClampsFutureTimestamp(t *testing.T) {
	now := time.Now()
	oldest := sql.NullTime{Time: now.Add(time.Minute), Valid: true}
	if got := pendingAgeSeconds(now, oldest); got != 0 {
		t.Fatalf("expected future timestamp to clamp to zero, got %f", got)
	}
}
