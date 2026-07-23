package idempotency

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

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

func TestExistingClaimResultUsesStableConflictCodes(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	activeLease := now.Add(time.Minute)

	tests := []struct {
		name        string
		record      Record
		requestHash string
		started     bool
		code        string
	}{
		{
			name:        "different request",
			record:      Record{RequestHash: "first", Status: "succeeded"},
			requestHash: "second",
			code:        errorCodeKeyReused,
		},
		{
			name:        "different request takes precedence over active lease",
			record:      Record{RequestHash: "first", Status: "processing", LockedUntil: &activeLease},
			requestHash: "second",
			code:        errorCodeKeyReused,
		},
		{
			name:        "active processing lease",
			record:      Record{RequestHash: "same", Status: "processing", LockedUntil: &activeLease},
			requestHash: "same",
			code:        errorCodeInProgress,
		},
		{
			name:        "completed same request",
			record:      Record{RequestHash: "same", Status: "succeeded"},
			requestHash: "same",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			started, err := existingClaimResult(tt.record, tt.requestHash, now)
			if started != tt.started {
				t.Fatalf("started=%v, want %v", started, tt.started)
			}
			if got := problem.FromError(err); tt.code == "" {
				if err != nil {
					t.Fatalf("expected replayable completed request, got %v", err)
				}
			} else if got == nil || got.Status != 409 || got.ErrorCode != tt.code {
				t.Fatalf("problem=%+v, want HTTP 409 %s", got, tt.code)
			}
		})
	}
}

func TestResourceRequestHashBindsActionResourceAndBody(t *testing.T) {
	body := map[string]any{"expected_version": 3, "reason": "customer_request"}
	want := ResourceRequestHash("order.cancel", uint64(1001), body)
	if want != "f667433729082ae7dd5ec2172f78a92ec7db07b50236cc8facbf76f419b6bcac" {
		t.Fatalf("resource request hash protocol changed: %q", want)
	}
	if got := ResourceRequestHash("  ORDER.CANCEL  ", uint64(1001), body); got != want {
		t.Fatalf("normalized action hash=%q, want %q", got, want)
	}

	variants := map[string]string{
		"resource": ResourceRequestHash("order.cancel", uint64(1002), body),
		"action":   ResourceRequestHash("order.force_cancel", uint64(1001), body),
		"body":     ResourceRequestHash("order.cancel", uint64(1001), map[string]any{"expected_version": 4, "reason": "customer_request"}),
	}
	for dimension, got := range variants {
		if got == want {
			t.Errorf("%s must participate in the resource request hash", dimension)
		}
	}
}

func TestReplayCompletedClassifiesAndReplays(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:idempotency-replay-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&Record{}); err != nil {
		t.Fatalf("migrate idempotency records: %v", err)
	}

	now := time.Now()
	responseBody, _ := json.Marshal(map[string]string{"order_id": "1001"})
	records := []Record{
		{
			ID: 1, ActorType: "customer", ActorID: 7, Method: "POST", Path: "/orders", KeyHash: KeyHash("completed-key"),
			RequestHash: "same", Status: "succeeded", ResponseBody: datatypes.JSON(responseBody), ExpiredAt: now.Add(time.Hour),
		},
		{
			ID: 2, ActorType: "customer", ActorID: 7, Method: "POST", Path: "/payments", KeyHash: KeyHash("processing-key"),
			RequestHash: "same", Status: "processing", LockedUntil: timePointer(now.Add(time.Minute)), ExpiredAt: now.Add(time.Hour),
		},
		{
			ID: 3, ActorType: "customer", ActorID: 7, Method: "POST", Path: "/retry", KeyHash: KeyHash("expired-key"),
			RequestHash: "same", Status: "processing", LockedUntil: timePointer(now.Add(-time.Minute)), ExpiredAt: now.Add(time.Hour),
		},
	}
	if err := db.Create(&records).Error; err != nil {
		t.Fatalf("seed idempotency records: %v", err)
	}

	store := NewStore(db)
	var replay map[string]string
	found, err := store.ReplayCompleted(context.Background(), db, "customer", 7, "/orders", "completed-key", "same", &replay)
	if err != nil || !found || replay["order_id"] != "1001" {
		t.Fatalf("completed replay found=%v response=%v err=%v", found, replay, err)
	}

	if found, err = store.ReplayCompleted(context.Background(), db, "customer", 7, "/orders", "completed-key", "different", &replay); found || problem.FromError(err).ErrorCode != errorCodeKeyReused {
		t.Fatalf("different request found=%v err=%v", found, err)
	}
	if found, err = store.ReplayCompleted(context.Background(), db, "customer", 7, "/payments", "processing-key", "same", &replay); found || problem.FromError(err).ErrorCode != errorCodeInProgress {
		t.Fatalf("active processing request found=%v err=%v", found, err)
	}
	if found, err = store.ReplayCompleted(context.Background(), db, "customer", 7, "/retry", "expired-key", "same", &replay); err != nil || found {
		t.Fatalf("expired processing request must proceed to Start: found=%v err=%v", found, err)
	}
}

func timePointer(value time.Time) *time.Time { return &value }
