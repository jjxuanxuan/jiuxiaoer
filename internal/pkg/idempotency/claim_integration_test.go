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

	drivermysql "github.com/go-sql-driver/mysql"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestClientFoundRowsDoesNotTurnNoopDuplicateIntoClaim 验证认领协议
// 不依赖 MySQL 的受影响行数报告模式。
// 启用 clientFoundRows 时，无变化的重复更新也会报告一条匹配记录。
func TestClientFoundRowsDoesNotTurnNoopDuplicateIntoClaim(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run idempotency clientFoundRows test")
	}
	ctx := context.Background()
	db := openIntegrationMySQL(t, true)

	ids := snowflake.New(992)
	actorID := ids.Next()
	now := time.Now()
	activeLease := now.Add(time.Minute)
	requestHash := RequestHash(map[string]string{"operation": "client-found-rows"})
	key := fmt.Sprintf("client-found-rows-%d", ids.Next())
	record := Record{
		ID: ids.Next(), ActorType: "customer", ActorID: actorID, Method: "POST",
		Path: "/integration/client-found-rows", KeyHash: KeyHash(key),
		RequestHash: requestHash, Status: "processing", LockedUntil: &activeLease,
		ExpiredAt: now.Add(time.Hour),
	}
	if err := db.Create(&record).Error; err != nil {
		t.Fatalf("insert active idempotency record: %v", err)
	}
	defer db.Where("actor_type = ? AND actor_id = ?", record.ActorType, actorID).Delete(&Record{})

	store := NewStore(db)
	started, err := store.Start(
		ctx,
		db,
		ids.Next(),
		record.ActorType,
		record.ActorID,
		record.Method,
		record.Path,
		key,
		requestHash,
	)
	if started {
		t.Fatal("active duplicate must not be reported as a new claim with clientFoundRows=true")
	}
	if details := problem.FromError(err); details == nil || details.ErrorCode != errorCodeInProgress {
		t.Fatalf("active duplicate error=%v, want %s", err, errorCodeInProgress)
	}

	started, err = store.Start(
		ctx, db, ids.Next(), record.ActorType, record.ActorID, record.Method, record.Path,
		key, "different-request-hash",
	)
	if started {
		t.Fatal("different request hash must not claim an existing key with clientFoundRows=true")
	}
	if details := problem.FromError(err); details == nil || details.ErrorCode != errorCodeKeyReused {
		t.Fatalf("different request error=%v, want %s", err, errorCodeKeyReused)
	}

	expired := now.Add(-time.Minute)
	if err := db.Model(&Record{}).
		Where("id = ?", record.ID).
		Update("locked_until", expired).Error; err != nil {
		t.Fatalf("expire idempotency lease: %v", err)
	}
	claimID := ids.Next()
	started, err = store.Start(
		ctx, db, claimID, record.ActorType, record.ActorID, record.Method, record.Path,
		key, requestHash,
	)
	if err != nil || !started {
		t.Fatalf("expired lease claim: started=%v err=%v", started, err)
	}
	var claimed Record
	if err := db.Where("actor_type = ? AND actor_id = ? AND path = ? AND key_hash = ?",
		record.ActorType, record.ActorID, record.Path, record.KeyHash,
	).First(&claimed).Error; err != nil {
		t.Fatalf("load claimed record: %v", err)
	}
	if claimed.ID != claimID {
		t.Fatalf("claimed record id=%d, want claim token %d", claimed.ID, claimID)
	}

	response := map[string]string{"order_id": "1001"}
	if err := store.Succeed(ctx, db, record.ActorType, record.ActorID, record.Path, key, response); err != nil {
		t.Fatalf("complete idempotency claim: %v", err)
	}
	started, err = store.Start(
		ctx, db, ids.Next(), record.ActorType, record.ActorID, record.Method, record.Path,
		key, requestHash,
	)
	if err != nil || started {
		t.Fatalf("completed duplicate must replay: started=%v err=%v", started, err)
	}
	var replay map[string]string
	found, err := store.CachedResponse(ctx, db, record.ActorType, record.ActorID, record.Path, key, &replay)
	if err != nil || !found || replay["order_id"] != response["order_id"] {
		t.Fatalf("cached replay found=%v response=%v err=%v", found, replay, err)
	}
}

// TestExpiredKeyHasSingleConcurrentClaimant 验证过期键在并发下只有一个认领者。
func TestExpiredKeyHasSingleConcurrentClaimant(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run idempotency claim test")
	}
	ctx := context.Background()
	db := openIntegrationMySQL(t, true)

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
	defer db.Where(
		"actor_type = ? AND actor_id = ? AND path = ? AND key_hash = ?",
		record.ActorType,
		record.ActorID,
		record.Path,
		record.KeyHash,
	).Delete(&Record{})

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
	db := openIntegrationMySQL(t, true)

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

func TestMySQLOwnerFencingRejectsStaleCompletionAndRelease(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run idempotency owner fencing test")
	}
	ctx := context.Background()
	db := openIntegrationMySQL(t, true)
	ids := snowflake.New(991)
	actorID := ids.Next()
	ownerA := ids.Next()
	ownerB := ids.Next()
	key := fmt.Sprintf("owner-fencing-%d", ids.Next())
	const path = "/integration/idempotency-owner-fencing"
	requestHash := RequestHash(map[string]string{"operation": "owner-fencing"})
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	store := NewStore(db)
	defer db.Where(
		"actor_type = ? AND actor_id = ? AND path = ? AND key_hash = ?",
		"customer", actorID, path, KeyHash(key),
	).Delete(&Record{})

	claim := func(owner uint64, claimAt time.Time) {
		t.Helper()
		err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			started, err := store.StartAt(
				ctx, tx, owner, "customer", actorID, "POST", path, key, requestHash, claimAt,
			)
			if err != nil {
				return err
			}
			if !started {
				return fmt.Errorf("owner %d did not acquire claim", owner)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("claim owner %d: %v", owner, err)
		}
	}
	claim(ownerA, now)
	claim(ownerB, now.Add(31*time.Second))

	staleSucceedErr := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return store.SucceedOwned(
			ctx, tx, ownerA, "customer", actorID, path, key, map[string]string{"owner": "A"},
		)
	})
	if !IsClaimLost(staleSucceedErr) {
		t.Fatalf("stale owner A succeed err=%v, want claim lost", staleSucceedErr)
	}
	staleFailErr := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return store.FailOwned(ctx, tx, ownerA, "customer", actorID, path, key)
	})
	if !IsClaimLost(staleFailErr) {
		t.Fatalf("stale owner A fail err=%v, want claim lost", staleFailErr)
	}

	var ownedByB Record
	if err := db.Where(
		"actor_type = ? AND actor_id = ? AND path = ? AND key_hash = ?",
		"customer", actorID, path, KeyHash(key),
	).First(&ownedByB).Error; err != nil {
		t.Fatalf("load owner B claim: %v", err)
	}
	if ownedByB.ID != ownerB || ownedByB.Status != "processing" ||
		ownedByB.LockedUntil == nil || !ownedByB.LockedUntil.After(now.Add(31*time.Second)) {
		t.Fatalf("stale owner modified B claim: %+v", ownedByB)
	}

	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return store.SucceedOwned(
			ctx, tx, ownerB, "customer", actorID, path, key, map[string]string{"owner": "B"},
		)
	}); err != nil {
		t.Fatalf("owner B succeed: %v", err)
	}
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return store.FailOwned(ctx, tx, ownerA, "customer", actorID, path, key)
	}); !IsClaimLost(err) {
		t.Fatalf("stale fail after B success err=%v, want claim lost", err)
	}
	if err := store.Fail(ctx, db, "customer", actorID, path, key); err != nil {
		t.Fatalf("legacy fail after B success: %v", err)
	}
	var replay map[string]string
	found, err := store.CachedResponse(ctx, db, "customer", actorID, path, key, &replay)
	if err != nil || !found || replay["owner"] != "B" {
		t.Fatalf("B response was not preserved: found=%v response=%v err=%v", found, replay, err)
	}
}

func openIntegrationMySQL(t *testing.T, clientFoundRows bool) *gorm.DB {
	t.Helper()
	cfg := config.Load()
	driverCfg, err := drivermysql.ParseDSN(cfg.MySQL.DSN)
	if err != nil {
		t.Fatalf("parse mysql DSN: %v", err)
	}
	driverCfg.ClientFoundRows = clientFoundRows
	cfg.MySQL.DSN = driverCfg.FormatDSN()
	cfg.MySQL.RequireWineTicketSchema = false
	cfg.MySQL.RequireWineTicketMoneyContract = false

	db, err := mysql.Open(
		context.Background(),
		cfg.MySQL,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil || db == nil {
		t.Fatalf("open mysql (clientFoundRows=%v): %v", clientFoundRows, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("access mysql pool: %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close mysql pool: %v", err)
		}
	})
	return db
}
