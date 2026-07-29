package integrity

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"
	"unicode/utf8"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/modules/delivery"
	"jiuxiaoer-admin/backend-go/internal/modules/deliveryreturn"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	sharedrefund "jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	giftdomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/gift"
	purchasedomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/purchase"
	redemptiondomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/redemption"
	refunddomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/refund"
	reminderdomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/reminder"
	renewaldomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/renewal"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func newTestIntegrityService(
	db *gorm.DB,
	ids *snowflake.Generator,
) *IntegrityService {
	service := NewIntegrityService(db, ids)
	service.snapshot = func(
		ctx context.Context,
		db *gorm.DB,
		read func(*gorm.DB) error,
	) error {
		return db.WithContext(ctx).Transaction(read)
	}
	return service
}

func TestIntegrityActiveExceptionUpsertAndNoAutoAdjustment(t *testing.T) {
	db := newIntegrityTestDB(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, shanghaiLocation)
	lot := core.Lot{
		ID: 101, LotNo: "WTL101", OwnerCustomerID: 11, PurchaseID: 21,
		SourceType: LotSourcePurchase, IssuerMerchantID: 31,
		ProductID: 41, RedeemCityCode: "440100",
		TotalQuantity: 5, AvailableQuantity: 4,
		OriginalExpiresAt: now.AddDate(0, 1, 0),
		ExpiresAt:         now.AddDate(0, 1, 0),
		ExpiryChangedAt:   now,
		Status:            LotStatusActive,
		Version:           7,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := db.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}
	service := newTestIntegrityService(db, snowflake.New(410))
	service.now = func() time.Time { return now }
	cursor := IntegrityCursor{Phase: IntegrityPhaseLots}

	first, err := service.ScanBatch(context.Background(), cursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	if first.Checked != 1 || first.Detected != 1 {
		t.Fatalf("unexpected first result: %+v", first)
	}
	var exception Exception
	if err := db.Where(
		"exception_type = ? AND biz_type = ? AND biz_id = ?",
		"REC-WT-003:lot_transaction_replay",
		"wine_ticket_lot",
		lot.ID,
	).Take(&exception).Error; err != nil {
		t.Fatal(err)
	}
	if exception.OccurrenceCount != 1 ||
		exception.Version != 1 ||
		exception.Severity != "P1" ||
		exception.Status != ExceptionStatusInvestigating ||
		exception.FirstDetectedAt != exception.LastDetectedAt {
		t.Fatalf("unexpected created exception: %+v", exception)
	}
	var unchanged core.Lot
	if err := db.First(&unchanged, lot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if unchanged.AvailableQuantity != 4 || unchanged.Version != 7 {
		t.Fatalf("scanner adjusted the lot: %+v", unchanged)
	}

	proposal := "close_without_asset_change"
	if err := db.Model(&Exception{}).Where("id = ?", exception.ID).Updates(map[string]any{
		"status":          ExceptionStatusPendingReview,
		"proposed_action": proposal,
	}).Error; err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now.Add(time.Minute) }
	second, err := service.ScanBatch(context.Background(), cursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	if second.Detected != 1 {
		t.Fatalf("expected the still-present difference, got %+v", second)
	}
	if err := db.First(&exception, exception.ID).Error; err != nil {
		t.Fatal(err)
	}
	if exception.OccurrenceCount != 2 ||
		exception.Version != 2 ||
		exception.Status != ExceptionStatusPendingReview ||
		exception.ProposedAction == nil ||
		*exception.ProposedAction != proposal ||
		!exception.FirstDetectedAt.Equal(now) ||
		!exception.LastDetectedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("active exception was not safely refreshed: %+v", exception)
	}

	// 从外部修复不可变事实。下一次扫描既不能关闭由运营人员负责的异常，
	// 也不能写入虚构的平衡流水。
	if err := db.Create(&core.Transaction{
		ID: 201, TransactionNo: "WTT201",
		LotID: lot.ID, OwnerCustomerID: lot.OwnerCustomerID,
		TransactionType: TransactionTypePurchaseIssue,
		QuantityDelta:   4, BeforeAvailableQuantity: 0,
		AfterAvailableQuantity: 4, BizType: "purchase",
		BizID: lot.PurchaseID, ActionKey: "purchase_issue:21:101",
		CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	third, err := service.ScanBatch(context.Background(), cursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	if third.Detected != 0 {
		t.Fatalf("repaired fact still detected: %+v", third)
	}
	if err := db.First(&exception, exception.ID).Error; err != nil {
		t.Fatal(err)
	}
	if exception.Status != ExceptionStatusPendingReview ||
		exception.OccurrenceCount != 2 ||
		exception.ResolvedAt != nil {
		t.Fatalf("disappearance auto-resolved exception: %+v", exception)
	}
	var transactionCount int64
	if err := db.Model(&core.Transaction{}).Where("lot_id = ?", lot.ID).
		Count(&transactionCount).Error; err != nil {
		t.Fatal(err)
	}
	if transactionCount != 1 {
		t.Fatalf("scanner wrote a balancing transaction, count=%d", transactionCount)
	}
}

func TestIntegrityAllRECRulesProduceBoundedExceptions(t *testing.T) {
	db := newIntegrityTestDB(t)
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, shanghaiLocation)
	purchaseBiz := PurchasePaymentBusiness
	missingPurchaseID := uint64(1002)
	providerSuccess := "SUCCESS"
	fixtures := []any{
		&order.Payment{
			ID: 1001, PaymentNo: "PAY1001",
			BizType: &purchaseBiz, BizID: &missingPurchaseID,
			CustomerID: 1, Status: "succeeded", ProviderStatus: &providerSuccess,
			Amount: 100, Currency: "CNY", UpdatedAt: now,
		},
		&purchasedomain.Purchase{
			ID: 2001, PurchaseNo: "WTP2001", CustomerID: 1,
			PaymentID: 1001, IssuerMerchantID: 2,
			TotalBottleQuantity: 2, Status: PurchaseStatusIssued,
			Currency: "CNY", CreatedAt: now, UpdatedAt: now,
		},
		&core.Lot{
			ID: 3001, LotNo: "WTL3001", OwnerCustomerID: 1,
			PurchaseID: 2001, SourceType: LotSourcePurchase,
			IssuerMerchantID: 2, ProductID: 3,
			TotalQuantity: 2, AvailableQuantity: 2,
			OriginalExpiresAt: now.AddDate(0, 1, 0),
			ExpiresAt:         now.AddDate(0, 1, 0),
			Status:            LotStatusActive,
		},
		&redemptiondomain.Redemption{
			ID: 4001, RedemptionNo: "WTR4001", CustomerID: 1,
			IssuerMerchantID: 2, ProductID: 3, ShopID: 4,
			ShopProductID: 5, DeliveryTimeSlotID: 8001,
			OrderID: 4002, Quantity: 1, Status: RedemptionStatusScheduled,
		},
		&giftdomain.Gift{
			ID: 5001, GiftNo: "WTG5001", GiverCustomerID: 1,
			IssuerMerchantID: 2, ProductID: 3, Quantity: 1,
			Status: GiftStatusPending,
		},
		&renewaldomain.Renewal{
			ID: 6001, RenewalNo: "WTRN6001", LotID: 6999,
			CustomerID: 1, OldExpiresAt: now,
			NewExpiresAt: now.AddDate(0, 1, 0), ExtensionDays: 30,
			FeeAmount: 0, Currency: "CNY", Status: RenewalStatusCompleted,
		},
		&refunddomain.WineTicketRefund{
			ID: 7001, WineTicketRefundNo: "WTRF7001",
			PurchaseID: 7999, CustomerID: 1, CurrentRefundID: 7998,
			RefundKind: RefundKindIssueCompensation,
			Amount:     100, Currency: "CNY", Status: RefundStatusSucceeded,
		},
		&redemptiondomain.DeliveryTimeSlot{
			ID: 8001, ShopID: 4, CapacityOrders: 5,
			ReservedOrders: 0, Status: "open",
		},
		&reminderdomain.Reminder{
			ID: 9001, LotID: 3001, OwnerCustomerID: 1,
			ExpiresAt: now.AddDate(0, 1, 0), RemindDays: 7,
			Channel: "wechat_subscribe", Status: "pending",
		},
		&reminderdomain.Reminder{
			ID: 9002, LotID: 3001, OwnerCustomerID: 1,
			ExpiresAt: now.AddDate(0, 1, 0), RemindDays: 7,
			Channel: "wechat_subscribe", Status: "pending",
		},
	}
	for _, fixture := range fixtures {
		if err := db.Create(fixture).Error; err != nil {
			t.Fatalf("create %T: %v", fixture, err)
		}
	}
	service := newTestIntegrityService(db, snowflake.New(411))
	service.now = func() time.Time { return now.Add(2 * time.Minute) }
	expected := map[IntegrityPhase]string{
		IntegrityPhasePayments:    reconciliationRulePaymentSettlement,
		IntegrityPhasePurchases:   reconciliationRulePurchaseIssue,
		IntegrityPhaseLots:        reconciliationRuleLotReplay,
		IntegrityPhaseRedemptions: reconciliationRuleRedemptionLedger,
		IntegrityPhaseGifts:       reconciliationRuleGift,
		IntegrityPhaseRenewals:    reconciliationRuleRenewal,
		IntegrityPhaseRefunds:     reconciliationRuleRefund,
		IntegrityPhaseSlots:       reconciliationRuleSlot,
		IntegrityPhaseReminders:   reconciliationRuleReminder,
	}
	for phase, rule := range expected {
		result, err := service.ScanBatch(
			context.Background(),
			IntegrityCursor{Phase: phase},
			20,
		)
		if err != nil {
			t.Fatalf("%s scan failed: %v", phase, err)
		}
		if result.Checked < 1 || result.Detected < 1 {
			t.Fatalf("%s did not detect %s: %+v", phase, rule, result)
		}
		var count int64
		if err := db.Model(&Exception{}).
			Where("correlation_id = ?", rule).
			Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count < 1 {
			t.Fatalf("%s did not persist a %s exception", phase, rule)
		}
	}
}

func TestIntegrityDetectsProviderSuccessSettlementExceptionWithoutCursorDelay(
	t *testing.T,
) {
	db := newIntegrityTestDB(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, shanghaiLocation)
	bizType := PurchasePaymentBusiness
	oldBizID := uint64(91)
	recentBizID := uint64(92)
	providerSuccess := "SUCCESS"
	for _, payment := range []order.Payment{
		{
			ID: 81, PaymentNo: "PAY81", BizType: &bizType, BizID: &oldBizID,
			Status: "exception", ProviderStatus: &providerSuccess,
			Amount: 100, Currency: "CNY", UpdatedAt: now.Add(-2 * time.Minute),
		},
		{
			ID: 82, PaymentNo: "PAY82", BizType: &bizType, BizID: &recentBizID,
			Status: "exception", ProviderStatus: &providerSuccess,
			Amount: 100, Currency: "CNY", UpdatedAt: now.Add(-30 * time.Second),
		},
	} {
		if err := db.Create(&payment).Error; err != nil {
			t.Fatal(err)
		}
	}
	service := newTestIntegrityService(db, snowflake.New(414))
	service.now = func() time.Time { return now }
	result, err := service.ScanBatch(
		context.Background(),
		IntegrityCursor{Phase: IntegrityPhasePayments},
		10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Checked != 2 || result.Detected != 2 {
		t.Fatalf("unexpected provider SUCCESS exception result: %+v", result)
	}
	var exception Exception
	if err := db.Where("biz_type = ? AND biz_id = ?", "payment", 81).
		Take(&exception).Error; err != nil {
		t.Fatal(err)
	}
	if exception.Severity != "P1" {
		t.Fatalf("ordinary unsettled payment must default to P1: %+v", exception)
	}
	var recent Exception
	if err := db.
		Where("biz_type = ? AND biz_id = ?", "payment", 82).
		Take(&recent).Error; err != nil {
		t.Fatal(err)
	}
	if recent.Severity != "P1" {
		t.Fatalf("recent provider success was not observed before cursor advance: %+v", recent)
	}
}

func TestIntegritySlotKeepsPostPickupRestoreReserved(t *testing.T) {
	db := newIntegrityTestDB(t)
	slot := redemptiondomain.DeliveryTimeSlot{
		ID: 61, ShopID: 1, CapacityOrders: 2, ReservedOrders: 1,
		Status: "open",
	}
	redemption := redemptiondomain.Redemption{
		ID: 62, RedemptionNo: "WTR62", DeliveryTimeSlotID: slot.ID,
		Quantity: 1, Status: RedemptionStatusRestored,
	}
	if err := db.Create(&slot).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&redemption).Error; err != nil {
		t.Fatal(err)
	}
	service := newTestIntegrityService(db, snowflake.New(415))
	result, err := service.ScanBatch(
		context.Background(),
		IntegrityCursor{Phase: IntegrityPhaseSlots},
		10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Detected != 0 {
		t.Fatalf("restored redemption incorrectly released its slot: %+v", result)
	}
	if err := db.Model(&redemptiondomain.DeliveryTimeSlot{}).Where("id = ?", slot.ID).
		Update("reserved_orders", 0).Error; err != nil {
		t.Fatal(err)
	}
	result, err = service.ScanBatch(
		context.Background(),
		IntegrityCursor{Phase: IntegrityPhaseSlots},
		10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Detected != 1 {
		t.Fatalf("missing restored slot reservation was not detected: %+v", result)
	}
}

func TestIntegrityCursorAndWorkerRunOnceAreBounded(t *testing.T) {
	db := newIntegrityTestDB(t)
	now := time.Now().In(shanghaiLocation)
	for id := uint64(1); id <= 2; id++ {
		if err := db.Create(&core.Lot{
			ID: id, LotNo: fmt.Sprintf("WTL%d", id),
			OwnerCustomerID: 1, PurchaseID: 1,
			SourceType: LotSourcePurchase, IssuerMerchantID: 1,
			ProductID: 1, TotalQuantity: 1, AvailableQuantity: 1,
			OriginalExpiresAt: now.Add(time.Hour),
			ExpiresAt:         now.Add(time.Hour),
			Status:            LotStatusActive,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	service := newTestIntegrityService(db, snowflake.New(412))
	worker := NewIntegrityWorker(
		service,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	).ConfigureBounds(1, time.Millisecond, time.Millisecond)
	worker.cursor = IntegrityCursor{Phase: IntegrityPhaseLots}

	first, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Checked != 1 ||
		first.NextCursor.Phase != IntegrityPhaseLots ||
		first.NextCursor.LastID != 1 ||
		first.PhaseCompleted {
		t.Fatalf("first batch was not bounded: %+v", first)
	}
	second, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.Checked != 1 ||
		second.NextCursor.Phase != IntegrityPhaseRedemptions ||
		second.NextCursor.LastID != 0 ||
		!second.PhaseCompleted {
		t.Fatalf("second batch was not bounded: %+v", second)
	}
	third, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if third.Checked != 0 ||
		!third.PhaseCompleted ||
		third.NextCursor.Phase != IntegrityPhaseGifts ||
		third.NextCursor.LastID != 0 {
		t.Fatalf("phase did not advance safely: %+v", third)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		worker.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop on context cancellation")
	}
}

func TestIntegrityCheckpointResumesAndStopsAtCapturedHighWatermark(
	t *testing.T,
) {
	db := newIntegrityTestDB(t)
	now := time.Date(2026, 7, 28, 1, 0, 0, 0, shanghaiLocation)
	for id := uint64(1); id <= 2; id++ {
		if err := db.Create(&core.Lot{
			ID: id, LotNo: fmt.Sprintf("WTL-HIGH-%d", id),
			OwnerCustomerID: 1, PurchaseID: 1,
			SourceType: LotSourcePurchase, IssuerMerchantID: 1,
			ProductID: 1, TotalQuantity: 1, AvailableQuantity: 1,
			OriginalExpiresAt: now.Add(time.Hour),
			ExpiresAt:         now.Add(time.Hour),
			Status:            LotStatusActive,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	service1 := newTestIntegrityService(db, snowflake.New(420))
	service1.now = func() time.Time { return now }
	worker1 := NewIntegrityWorker(
		service1,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	).WithOwner("api-实例一").ConfigureBounds(1, time.Millisecond, time.Minute)
	worker1.cursor = IntegrityCursor{Phase: IntegrityPhaseLots}
	first, err := worker1.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Checked != 1 || first.NextCursor.LastID != 1 ||
		first.PhaseCompleted {
		t.Fatalf("unexpected first persisted batch: %+v", first)
	}

	if err := db.Create(&core.Lot{
		ID: 3, LotNo: "WTL-HIGH-3",
		OwnerCustomerID: 1, PurchaseID: 1,
		SourceType: LotSourcePurchase, IssuerMerchantID: 1,
		ProductID: 1, TotalQuantity: 1, AvailableQuantity: 1,
		OriginalExpiresAt: now.Add(time.Hour),
		ExpiresAt:         now.Add(time.Hour),
		Status:            LotStatusActive,
	}).Error; err != nil {
		t.Fatal(err)
	}

	service2 := newTestIntegrityService(db, snowflake.New(421))
	service2.now = func() time.Time { return now.Add(time.Second) }
	worker2 := NewIntegrityWorker(
		service2,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	).WithOwner("worker-实例二").ConfigureBounds(1, time.Millisecond, time.Minute)
	second, err := worker2.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.Checked != 1 ||
		second.Phase != IntegrityPhaseLots ||
		!second.PhaseCompleted ||
		second.NextCursor.Phase != IntegrityPhaseRedemptions {
		t.Fatalf("restart did not resume the persisted lot cursor: %+v", second)
	}
	var checkpoint Checkpoint
	if err := db.Take(&checkpoint).Error; err != nil {
		t.Fatal(err)
	}
	if checkpoint.CheckedRows != 2 ||
		checkpoint.Phase != IntegrityPhaseRedemptions ||
		checkpoint.LastID != 0 {
		t.Fatalf("unexpected durable checkpoint: %+v", checkpoint)
	}
	var lateExceptionCount int64
	if err := db.Model(&Exception{}).
		Where("biz_type = ? AND biz_id = ?", "wine_ticket_lot", 3).
		Count(&lateExceptionCount).Error; err != nil {
		t.Fatal(err)
	}
	if lateExceptionCount != 0 {
		t.Fatalf("post-watermark lot leaked into the cycle: %d", lateExceptionCount)
	}
}

func TestIntegrityCheckpointLeaseRejectsStaleOwnerAdvance(t *testing.T) {
	db := newIntegrityTestDB(t)
	service := newTestIntegrityService(db, snowflake.New(422))
	now := time.Date(2026, 7, 28, 1, 0, 0, 0, shanghaiLocation)
	cycleKey, nextStart := reconciliationDueCycle(
		now,
		defaultIntegrityDailyStart,
	)
	first, err := service.repo.claimCheckpoint(
		context.Background(),
		cycleKey,
		"owner-one",
		now,
		time.Minute,
		nextStart,
		IntegrityCursor{Phase: IntegrityPhasePayments},
	)
	if err != nil || first.Claim == nil {
		t.Fatalf("first claim failed: %+v, %v", first, err)
	}
	blocked, err := service.repo.claimCheckpoint(
		context.Background(),
		cycleKey,
		"owner-two",
		now.Add(30*time.Second),
		time.Minute,
		nextStart,
		IntegrityCursor{},
	)
	if err != nil || blocked.Claim != nil ||
		!blocked.WaitUntil.Equal(now.Add(time.Minute)) {
		t.Fatalf("active lease was not respected: %+v, %v", blocked, err)
	}
	takenOver, err := service.repo.claimCheckpoint(
		context.Background(),
		cycleKey,
		"owner-two",
		now.Add(2*time.Minute),
		time.Minute,
		nextStart,
		IntegrityCursor{},
	)
	if err != nil || takenOver.Claim == nil {
		t.Fatalf("expired lease was not reclaimed: %+v, %v", takenOver, err)
	}
	staleResult := IntegrityBatchResult{
		Phase: IntegrityPhasePayments,
		NextCursor: IntegrityCursor{
			Phase:  IntegrityPhasePayments,
			LastID: 99,
		},
		Checked: 1,
	}
	err = service.repo.persistClaimedBatch(
		context.Background(),
		*first.Claim,
		staleResult,
		nil,
		now.Add(2*time.Minute),
	)
	if err == nil {
		t.Fatal("expired owner advanced a newer owner's cursor")
	}
	var checkpoint Checkpoint
	if err := db.Where("cycle_key = ?", cycleKey).
		Take(&checkpoint).Error; err != nil {
		t.Fatal(err)
	}
	if checkpoint.LeaseOwner == nil ||
		*checkpoint.LeaseOwner != takenOver.Claim.Owner ||
		checkpoint.LastID != 0 {
		t.Fatalf("stale owner mutated checkpoint: %+v", checkpoint)
	}
}

func TestIntegrityDailyBoundaryAndCompletedCycleDoNotRescan(t *testing.T) {
	before := time.Date(2026, 7, 28, 0, 4, 59, 0, shanghaiLocation)
	after := time.Date(2026, 7, 28, 0, 5, 0, 0, shanghaiLocation)
	beforeKey, beforeNext := reconciliationDueCycle(
		before,
		defaultIntegrityDailyStart,
	)
	afterKey, afterNext := reconciliationDueCycle(
		after,
		defaultIntegrityDailyStart,
	)
	if beforeKey != "2026-07-26" ||
		!beforeNext.Equal(after) ||
		afterKey != "2026-07-27" ||
		!afterNext.Equal(after.AddDate(0, 0, 1)) {
		t.Fatalf(
			"unexpected daily boundary: before=%s/%s after=%s/%s",
			beforeKey,
			beforeNext,
			afterKey,
			afterNext,
		)
	}

	db := newIntegrityTestDB(t)
	highWatermarks := make(map[IntegrityPhase]uint64)
	for _, phase := range reconciliationPhases {
		highWatermarks[phase] = 0
	}
	raw, err := reconciliationSnapshot(highWatermarks)
	if err != nil {
		t.Fatal(err)
	}
	completedAt := after.Add(time.Hour)
	if err := db.Create(&Checkpoint{
		CycleKey: afterKey, Status: reconciliationCheckpointCompleted,
		Phase: IntegrityPhasePayments, HighWatermarks: raw,
		StartedAt: after, CompletedAt: &completedAt, Version: 3,
	}).Error; err != nil {
		t.Fatal(err)
	}
	service := newTestIntegrityService(db, snowflake.New(423))
	service.now = func() time.Time { return completedAt }
	worker := NewIntegrityWorker(
		service,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	result, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.LeaseAcquired ||
		result.NextRunAt == nil ||
		!result.NextRunAt.Equal(afterNext) {
		t.Fatalf("completed daily cycle was rescanned: %+v", result)
	}
}

func TestNormalizeIntegrityOwnerIsRuneSafeAndStable(t *testing.T) {
	owner := ""
	for range 200 {
		owner += "实"
	}
	normalized := normalizeIntegrityOwner(owner)
	if !utf8.ValidString(normalized) ||
		len([]rune(normalized)) > maxIntegrityCheckpointOwner ||
		normalized != normalizeIntegrityOwner(owner) {
		t.Fatalf("invalid normalized checkpoint owner: %q", normalized)
	}
}

func ruleForPhase(phase IntegrityPhase) string {
	switch phase {
	case IntegrityPhasePayments:
		return reconciliationRulePaymentSettlement
	case IntegrityPhasePurchases:
		return reconciliationRulePurchaseIssue
	case IntegrityPhaseLots:
		return reconciliationRuleLotReplay
	case IntegrityPhaseRedemptions:
		return reconciliationRuleRedemptionLedger
	case IntegrityPhaseGifts:
		return reconciliationRuleGift
	case IntegrityPhaseRenewals:
		return reconciliationRuleRenewal
	case IntegrityPhaseRefunds:
		return reconciliationRuleRefund
	case IntegrityPhaseSlots:
		return reconciliationRuleSlot
	case IntegrityPhaseReminders:
		return reconciliationRuleReminder
	default:
		return ""
	}
}

func newIntegrityTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := uniqueSQLiteMemoryDSN(t, "_busy_timeout=5000")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(
		&purchasedomain.Purchase{},
		&core.Lot{},
		&core.Transaction{},
		&redemptiondomain.DeliveryTimeSlot{},
		&redemptiondomain.Redemption{},
		&redemptiondomain.RedemptionAllocation{},
		&giftdomain.Gift{},
		&giftdomain.GiftAllocation{},
		&renewaldomain.Renewal{},
		&refunddomain.WineTicketRefund{},
		&refunddomain.RefundAllocation{},
		&reminderdomain.Reminder{},
		&Exception{},
		&Checkpoint{},
		&order.Payment{},
		&order.Order{},
		&order.OrderItem{},
		&order.ProductStock{},
		&order.StockRecord{},
		&delivery.DeliveryOrder{},
		&deliveryreturn.Return{},
		&sharedrefund.Row{},
	); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"ALTER TABLE orders ADD COLUMN deleted_at DATETIME",
		"ALTER TABLE order_items ADD COLUMN deleted_at DATETIME",
		"ALTER TABLE product_stocks ADD COLUMN deleted_at DATETIME",
		"ALTER TABLE stock_records ADD COLUMN deleted_at DATETIME",
		"ALTER TABLE delivery_orders ADD COLUMN deleted_at DATETIME",
		`CREATE UNIQUE INDEX uk_wt_reconciliation_exception_no
			ON wine_ticket_exceptions(exception_no)`,
		`CREATE UNIQUE INDEX uk_wt_reconciliation_active_exception
			ON wine_ticket_exceptions(exception_type,biz_type,biz_id)
			WHERE status IN (
				'investigating','awaiting_external_fact','pending_review'
			)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("prepare reconciliation schema: %v", err)
		}
	}
	return db
}
