package idempotency

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestExpiredKeyHasSingleConcurrentClaimant 验证已过期密钥 Has Single Concurrent Claimant的预期行为。
func TestExpiredKeyHasSingleConcurrentClaimant(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run idempotency claim test")
	}
	ctx := context.Background()
	cfg := config.Load()
	db, err := mysql.Open(ctx, cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	idGen := snowflake.New(994)
	recordID := idGen.Next()
	actorID := idGen.Next()
	key := fmt.Sprintf("expired-claim-%d", recordID)
	requestHash := RequestHash(map[string]string{"operation": "claim"})
	expired := time.Now().Add(-time.Minute)
	record := Record{
		ID: recordID, ActorType: "customer", ActorID: actorID, Method: "POST", Path: "/integration/idempotency",
		KeyHash: KeyHash(key), RequestHash: requestHash, Status: "processing", LockedUntil: &expired, ExpiredAt: time.Now().Add(time.Hour),
	}
	if err := db.Create(&record).Error; err != nil {
		t.Fatalf("insert expired idempotency record: %v", err)
	}
	defer db.Where("id = ?", recordID).Delete(&Record{})

	store := NewStore(db)
	var startedCount atomic.Int64
	errorsCh := make(chan error, 100)
	var wg sync.WaitGroup
	for index := 0; index < 100; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				started, err := store.Start(ctx, tx, idGen.Next(), "customer", actorID, "POST", "/integration/idempotency", key, requestHash)
				if err != nil {
					if problem.FromError(err).ErrorCode == "IDEMPOTENCY_CONFLICT" {
						return nil
					}
					return err
				}
				if !started {
					return nil
				}
				startedCount.Add(1)
				return store.Succeed(ctx, tx, "customer", actorID, "/integration/idempotency", key, map[string]bool{"ok": true})
			})
			if err != nil {
				errorsCh <- err
			}
		}()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Fatalf("claim transaction failed: %v", err)
	}
	if startedCount.Load() != 1 {
		t.Fatalf("expected exactly one claimant, got %d", startedCount.Load())
	}
}
