package idempotency

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/datatypes"
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
					if problem.FromError(err).ErrorCode == errorCodeInProgress {
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

func TestStartReturnsStableConflictCodesAndReplaysCompleted(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run idempotency contract test")
	}
	ctx := context.Background()
	cfg := config.Load()
	db, err := mysql.Open(ctx, cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	ids := snowflake.New(993)
	actorID := ids.Next()
	store := NewStore(db)
	now := time.Now()
	activeLease := now.Add(time.Minute)
	processingKey := fmt.Sprintf("processing-contract-%d", ids.Next())
	completedKey := fmt.Sprintf("completed-contract-%d", ids.Next())
	requestHash := RequestHash(map[string]string{"operation": "contract"})
	response := map[string]string{"order_id": "1001"}
	responseBody, _ := json.Marshal(response)
	statusOK := http.StatusOK
	records := []Record{
		{
			ID: ids.Next(), ActorType: "customer", ActorID: actorID, Method: "POST", Path: "/integration/processing",
			KeyHash: KeyHash(processingKey), RequestHash: requestHash, Status: "processing", LockedUntil: &activeLease, ExpiredAt: now.Add(time.Hour),
		},
		{
			ID: ids.Next(), ActorType: "customer", ActorID: actorID, Method: "POST", Path: "/integration/completed",
			KeyHash: KeyHash(completedKey), RequestHash: requestHash, Status: "succeeded", ResponseStatus: &statusOK,
			ResponseBody: datatypes.JSON(responseBody), ExpiredAt: now.Add(time.Hour),
		},
	}
	if err := db.Create(&records).Error; err != nil {
		t.Fatalf("insert idempotency contract records: %v", err)
	}
	defer db.Where("actor_type = ? AND actor_id = ?", "customer", actorID).Delete(&Record{})

	assertCode := func(t *testing.T, err error, want string) {
		t.Helper()
		details := problem.FromError(err)
		if details == nil || details.Status != http.StatusConflict || details.ErrorCode != want {
			t.Fatalf("problem=%+v, want HTTP 409 %s", details, want)
		}
	}

	started, err := store.Start(ctx, db, ids.Next(), "customer", actorID, "POST", "/integration/processing", processingKey, requestHash)
	if started {
		t.Fatal("active processing lease must not be reclaimed")
	}
	assertCode(t, err, errorCodeInProgress)
	_, err = store.Start(ctx, db, ids.Next(), "customer", actorID, "POST", "/integration/processing", processingKey, "different")
	assertCode(t, err, errorCodeKeyReused)

	started, err = store.Start(ctx, db, ids.Next(), "customer", actorID, "POST", "/integration/completed", completedKey, requestHash)
	if err != nil || started {
		t.Fatalf("completed same request must select cached replay: started=%v err=%v", started, err)
	}
	var replay map[string]string
	found, err := store.CachedResponse(ctx, db, "customer", actorID, "/integration/completed", completedKey, &replay)
	if err != nil || !found || replay["order_id"] != response["order_id"] {
		t.Fatalf("cached replay found=%v response=%v err=%v", found, replay, err)
	}
	_, err = store.Start(ctx, db, ids.Next(), "customer", actorID, "POST", "/integration/completed", completedKey, "different")
	assertCode(t, err, errorCodeKeyReused)
}
