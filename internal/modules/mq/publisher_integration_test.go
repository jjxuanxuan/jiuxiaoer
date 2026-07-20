package mq

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestConcurrentPublishersClaimDifferentEvents 验证Concurrent Publishers 认领 Different Events的预期行为。
func TestConcurrentPublishersClaimDifferentEvents(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run outbox lease integration test")
	}
	ctx := context.Background()
	cfg := config.Load()
	db, err := mysql.Open(ctx, cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	generator := snowflake.New(1001)
	priorityIDs := priorityOutboxIDs(t, db, 2, generator)
	prefix := "l0-claim-" + uuid.NewString()
	createdAt := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	events := []OutboxEvent{
		{ID: priorityIDs[0], EventID: prefix + "-1", EventType: "l0.test", AggregateType: "test", AggregateID: 1, Payload: datatypes.JSON(`{"n":1}`), Status: "pending", CreatedAt: createdAt},
		{ID: priorityIDs[1], EventID: prefix + "-2", EventType: "l0.test", AggregateType: "test", AggregateID: 2, Payload: datatypes.JSON(`{"n":2}`), Status: "pending", CreatedAt: createdAt},
	}
	if err := db.Create(&events).Error; err != nil {
		t.Fatalf("insert outbox events: %v", err)
	}
	defer db.Where("event_id LIKE ?", prefix+"%").Delete(&OutboxEvent{})

	now := time.Now()
	publishers := []*Publisher{
		NewPublisher(db, nil, nil, "worker-a", slog.New(slog.NewTextHandler(io.Discard, nil))),
		NewPublisher(db, nil, nil, "worker-b", slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
	claimed := make(chan OutboxEvent, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, publisher := range publishers {
		wg.Add(1)
		go func(p *Publisher) {
			defer wg.Done()
			items, claimErr := p.claimBatch(ctx, now, 1)
			if claimErr != nil {
				errs <- claimErr
				return
			}
			if len(items) != 1 {
				errs <- fmt.Errorf("expected one claimed event, got %d", len(items))
				return
			}
			claimed <- items[0]
		}(publisher)
	}
	wg.Wait()
	close(errs)
	for claimErr := range errs {
		t.Fatal(claimErr)
	}
	close(claimed)
	ids := map[uint64]bool{}
	for event := range claimed {
		ids[event.ID] = true
	}
	if len(ids) != len(events) || !ids[events[0].ID] || !ids[events[1].ID] {
		t.Fatalf("expected inserted events to be claimed exactly once, got IDs %v", ids)
	}
}

// TestPublisherReclaimsExpiredLease 验证发布器 Reclaims 已过期租约的预期行为。
func TestPublisherReclaimsExpiredLease(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run outbox lease integration test")
	}
	ctx := context.Background()
	cfg := config.Load()
	db, err := mysql.Open(ctx, cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	generator := snowflake.New(1002)
	priorityIDs := priorityOutboxIDs(t, db, 1, generator)
	prefix := "l0-reclaim-" + uuid.NewString()
	staleWorker := "stopped-worker"
	expiredAt := time.Now().Add(-time.Minute)
	event := OutboxEvent{
		ID:            priorityIDs[0],
		EventID:       prefix,
		EventType:     "l0.test",
		AggregateType: "test",
		AggregateID:   1,
		Payload:       datatypes.JSON(`{"reclaim":true}`),
		Status:        "pending",
		LockedBy:      &staleWorker,
		LockedUntil:   &expiredAt,
		CreatedAt:     time.Date(1999, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := db.Create(&event).Error; err != nil {
		t.Fatalf("insert leased outbox event: %v", err)
	}
	defer db.Where("event_id = ?", prefix).Delete(&OutboxEvent{})

	publisher := NewPublisher(db, nil, nil, "replacement-worker", slog.New(slog.NewTextHandler(io.Discard, nil)))
	claimed, err := publisher.claimBatch(ctx, time.Now(), 1)
	if err != nil {
		t.Fatalf("claim expired lease: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != event.ID {
		t.Fatalf("expected expired event %d to be reclaimed, got %+v", event.ID, claimed)
	}
}

// priorityOutboxIDs 返回priority 发件箱事件 I Ds。
func priorityOutboxIDs(t *testing.T, db *gorm.DB, count int, generator *snowflake.Generator) []uint64 {
	t.Helper()
	var minimum uint64
	if err := db.Raw("SELECT COALESCE(MIN(id), 0) FROM outbox_events").Scan(&minimum).Error; err != nil {
		t.Fatalf("query minimum outbox id: %v", err)
	}
	ids := make([]uint64, count)
	if minimum > uint64(count) {
		for index := range ids {
			ids[index] = minimum - uint64(count-index)
		}
		return ids
	}
	for index := range ids {
		ids[index] = generator.Next()
	}
	return ids
}
