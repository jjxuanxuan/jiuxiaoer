package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
	"jiuxiaoer-admin/backend-go/internal/testutil"
)

func newExpiryWorkerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(
		sqlite.Open(testutil.UniqueSQLiteMemoryDSN(t)),
		&gorm.Config{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&Lot{},
		&Transaction{},
		&expiryRenewalGuard{},
		&expiryRefundAllocationGuard{},
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(
		"CREATE UNIQUE INDEX uk_core_expiry_action ON wine_ticket_transactions(action_key)",
	).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func TestExpiryWorkerExpiresDueLotExactlyOnce(t *testing.T) {
	db := newExpiryWorkerTestDB(t)
	now := time.Date(2026, 7, 28, 10, 0, 0, 0, ShanghaiLocation)
	lot := Lot{
		ID:                101,
		LotNo:             "WTL101",
		OwnerCustomerID:   1001,
		PurchaseID:        2001,
		SourceType:        LotSourcePurchase,
		IssuerMerchantID:  1,
		ProductID:         2,
		RedeemCityCode:    "310100",
		TotalQuantity:     6,
		AvailableQuantity: 6,
		OriginalExpiresAt: now.Add(-time.Hour),
		ExpiresAt:         now.Add(-time.Hour),
		ExpiryChangedAt:   now.Add(-30 * 24 * time.Hour),
		Status:            LotStatusActive,
		Version:           1,
		CreatedAt:         now.Add(-30 * 24 * time.Hour),
		UpdatedAt:         now.Add(-30 * 24 * time.Hour),
	}
	if err := db.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}

	worker := NewExpiryWorker(db, snowflake.New(231)).
		WithNow(func() time.Time { return now })
	count, err := worker.ExpireLotsOnce(context.Background())
	if err != nil || count != 1 {
		t.Fatalf("first expiry count=%d err=%v", count, err)
	}
	count, err = worker.ExpireLotsOnce(context.Background())
	if err != nil || count != 0 {
		t.Fatalf("replayed expiry count=%d err=%v", count, err)
	}

	var stored Lot
	if err := db.First(&stored, lot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != LotStatusExpired || stored.AvailableQuantity != 0 {
		t.Fatalf("unexpected expired lot: %+v", stored)
	}
	var rows []Transaction
	if err := db.Where("lot_id = ?", lot.ID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 ||
		rows[0].TransactionType != TransactionTypeLotExpiry ||
		rows[0].QuantityDelta != -6 ||
		rows[0].AfterAvailableQuantity != 0 {
		t.Fatalf("unexpected expiry ledger: %+v", rows)
	}
}

func TestExpiryWorkerLeavesGuardedLotUntouched(t *testing.T) {
	db := newExpiryWorkerTestDB(t)
	now := time.Date(2026, 7, 28, 10, 0, 0, 0, ShanghaiLocation)
	lot := Lot{
		ID:                102,
		LotNo:             "WTL102",
		OwnerCustomerID:   1002,
		PurchaseID:        2002,
		SourceType:        LotSourcePurchase,
		IssuerMerchantID:  1,
		ProductID:         2,
		RedeemCityCode:    "310100",
		TotalQuantity:     6,
		AvailableQuantity: 6,
		OriginalExpiresAt: now.Add(-time.Hour),
		ExpiresAt:         now.Add(-time.Hour),
		ExpiryChangedAt:   now.Add(-30 * 24 * time.Hour),
		Status:            LotStatusActive,
		Version:           1,
		CreatedAt:         now.Add(-30 * 24 * time.Hour),
		UpdatedAt:         now.Add(-30 * 24 * time.Hour),
	}
	renewal := expiryRenewalGuard{
		ID:     301,
		LotID:  lot.ID,
		Status: "applying",
	}
	if err := db.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&renewal).Error; err != nil {
		t.Fatal(err)
	}

	worker := NewExpiryWorker(db, snowflake.New(232)).
		WithNow(func() time.Time { return now })
	count, err := worker.ExpireLotsOnce(context.Background())
	if err != nil || count != 0 {
		t.Fatalf("guarded expiry count=%d err=%v", count, err)
	}
	var stored Lot
	if err := db.First(&stored, lot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != LotStatusActive || stored.AvailableQuantity != 6 {
		t.Fatalf("guarded lot changed: %+v", stored)
	}
}

func TestExpiryWorkerConcurrentRunsWriteOneNonZeroLedger(t *testing.T) {
	db := newExpiryWorkerTestDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	// SQLite 不支持行级 SKIP LOCKED。
	// 单个连接使该可移植单元测试能够验证回放串行化；
	// 真实锁语义由 MySQL 验收套件负责。
	sqlDB.SetMaxOpenConns(1)
	now := time.Date(2026, 7, 28, 10, 0, 0, 0, ShanghaiLocation)
	lot := expiryTestLot(103, 1003, now.Add(-time.Hour), now)
	lot.AvailableQuantity = 3
	lot.TotalQuantity = 3
	if err := db.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}

	workers := []*ExpiryWorker{
		NewExpiryWorker(db, snowflake.New(233)).
			WithNow(func() time.Time { return now }),
		NewExpiryWorker(db, snowflake.New(234)).
			WithNow(func() time.Time { return now }),
	}
	var wait sync.WaitGroup
	errs := make(chan error, len(workers))
	for _, worker := range workers {
		wait.Add(1)
		go func(current *ExpiryWorker) {
			defer wait.Done()
			_, runErr := current.ExpireLotsOnce(context.Background())
			errs <- runErr
		}(worker)
	}
	wait.Wait()
	close(errs)
	for runErr := range errs {
		if runErr != nil {
			t.Fatal(runErr)
		}
	}

	var rows []Transaction
	if err := db.Where(
		"lot_id = ? AND transaction_type = ?",
		lot.ID,
		TransactionTypeLotExpiry,
	).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 ||
		rows[0].QuantityDelta != -3 ||
		rows[0].BeforeAvailableQuantity != 3 ||
		rows[0].AfterAvailableQuantity != 0 {
		t.Fatalf("expiry ledger rows=%+v", rows)
	}
}

func TestExpiryWorkerRefundGuardDoesNotStarveNextDueLot(t *testing.T) {
	db := newExpiryWorkerTestDB(t)
	now := time.Date(2026, 7, 28, 10, 0, 0, 0, ShanghaiLocation)
	guarded := expiryTestLot(104, 1004, now.Add(-2*time.Hour), now)
	unblocked := expiryTestLot(105, 1005, now.Add(-time.Hour), now)
	if err := db.Create(&[]Lot{guarded, unblocked}).Error; err != nil {
		t.Fatal(err)
	}
	allocation := expiryRefundAllocationGuard{
		ID:     401,
		LotID:  guarded.ID,
		Status: "held",
	}
	if err := db.Create(&allocation).Error; err != nil {
		t.Fatal(err)
	}

	count, err := NewExpiryWorker(db, snowflake.New(235)).
		WithNow(func() time.Time { return now }).
		WithBatchSize(1).
		ExpireLotsOnce(context.Background())
	if err != nil || count != 1 {
		t.Fatalf("expiry count=%d err=%v", count, err)
	}
	if err := db.First(&guarded, guarded.ID).Error; err != nil {
		t.Fatal(err)
	}
	if guarded.Status != LotStatusActive || guarded.AvailableQuantity != 6 {
		t.Fatalf("refund-guarded lot changed: %+v", guarded)
	}
	if err := db.First(&unblocked, unblocked.ID).Error; err != nil {
		t.Fatal(err)
	}
	if unblocked.Status != LotStatusExpired ||
		unblocked.AvailableQuantity != 0 {
		t.Fatalf("guarded first lot starved next due lot: %+v", unblocked)
	}
	if err := db.First(&allocation, allocation.ID).Error; err != nil {
		t.Fatal(err)
	}
	if allocation.Status != "held" {
		t.Fatalf("expiry worker changed refund guard: %+v", allocation)
	}
}

func expiryTestLot(
	id uint64,
	customerID uint64,
	expiresAt time.Time,
	now time.Time,
) Lot {
	return Lot{
		ID:                id,
		LotNo:             "WTL" + IDString(id),
		OwnerCustomerID:   customerID,
		PurchaseID:        id + 10_000,
		SourceType:        LotSourcePurchase,
		IssuerMerchantID:  1,
		ProductID:         2,
		RedeemCityCode:    "310100",
		TotalQuantity:     6,
		AvailableQuantity: 6,
		OriginalExpiresAt: expiresAt,
		ExpiresAt:         expiresAt,
		ExpiryChangedAt:   now.Add(-30 * 24 * time.Hour),
		Status:            LotStatusActive,
		Version:           1,
		CreatedAt:         now.Add(-30 * 24 * time.Hour),
		UpdatedAt:         now.Add(-30 * 24 * time.Hour),
	}
}
