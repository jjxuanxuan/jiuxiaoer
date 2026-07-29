package redemption

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/modules/aftersale"
	"jiuxiaoer-admin/backend-go/internal/modules/deliveryreturn"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
	"jiuxiaoer-admin/backend-go/internal/testutil"
)

type returnSettlementTestOrder struct {
	ID              uint64
	OrderType       string
	SettlementMode  string
	PayStatus       string
	PaidAmount      int64
	Status          string
	DeliveryStatus  string
	AfterSaleStatus string
	Version         int
}

func (returnSettlementTestOrder) TableName() string { return "orders" }

func TestWineTicketReturnInitialBindingUsesRedemptionID(t *testing.T) {
	t.Parallel()
	db, err := gorm.Open(
		sqlite.Open(testutil.UniqueSQLiteMemoryDSN(t)),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Redemption{}); err != nil {
		t.Fatal(err)
	}
	row := Redemption{
		ID: 91, RedemptionNo: "WTR91", CustomerID: 10, IssuerMerchantID: 20,
		ProductID: 30, ShopID: 40, ShopProductID: 50, DeliveryTimeSlotID: 60,
		OrderID: 71, Quantity: 2, AddressID: 80, AddressVersion: 1,
		Status: RedemptionStatusPickedUp, Version: 1,
		ScheduledStartAt: time.Now(), ScheduledEndAt: time.Now().Add(time.Hour),
		NotBeforeAt: time.Now().Add(-time.Hour),
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	handler := NewWineTicketReturnSettlement(db, snowflake.New(63), nil)
	binding, err := handler.InitialBinding(t.Context(), db, deliveryreturn.OrderRef{
		ID: 71, OrderType: "wine_ticket_redemption", SettlementMode: "wine_ticket",
		PayStatus: "not_required", PaidAmount: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if binding.SettlementType != deliveryreturn.SettlementWineTicketRestore ||
		binding.SettlementBizID == nil ||
		*binding.SettlementBizID != row.ID {
		t.Fatalf("unexpected binding: %+v", binding)
	}
}

func TestWineTicketReturnReceivedRestoresThroughAssetServiceWithExpiryEvidence(t *testing.T) {
	t.Parallel()
	db, err := gorm.Open(
		sqlite.Open(testutil.UniqueSQLiteMemoryDSN(t)),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&Redemption{},
		&RedemptionAllocation{},
		&core.Lot{},
		&core.Transaction{},
		&aftersale.AfterSale{},
		&returnSettlementTestOrder{},
		&redemptionTestAudit{},
		&redemptionTestOutbox{},
	); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 28, 16, 0, 0, 123000000, shanghaiLocation)
	expiresAt := now.Add(-time.Hour)
	redemption := Redemption{
		ID: 91, RedemptionNo: "WTR91", CustomerID: 10, IssuerMerchantID: 20,
		ProductID: 30, ShopID: 40, ShopProductID: 50, DeliveryTimeSlotID: 60,
		OrderID: 71, Quantity: 2, AddressID: 80, AddressVersion: 1,
		Status: RedemptionStatusReturnInProgress, Version: 2,
		ScheduledStartAt: now.Add(-4 * time.Hour),
		ScheduledEndAt:   now.Add(-2 * time.Hour),
		NotBeforeAt:      now.Add(-5 * time.Hour),
	}
	lot := core.Lot{
		ID: 501, LotNo: "WTL501", OwnerCustomerID: redemption.CustomerID,
		PurchaseID: 1, SourceType: LotSourcePurchase, IssuerMerchantID: 20,
		ProductID: 30, RedeemCityCode: "310100", TotalQuantity: 2,
		AvailableQuantity: 0, OriginalExpiresAt: expiresAt,
		ExpiresAt: expiresAt, ExpiryChangedAt: expiresAt.AddDate(-1, 0, 0),
		Status: LotStatusDepleted, EverUsed: true, Version: 3,
	}
	allocation := RedemptionAllocation{
		ID: 301, RedemptionID: redemption.ID, LotID: lot.ID,
		Quantity: 2, SourceExpiresAt: expiresAt,
		Status: RedemptionAllocationStatusConsumed,
	}
	returnID, afterSaleID := uint64(81), uint64(82)
	afterSaleRow := aftersale.AfterSale{
		ID: afterSaleID, OrderID: redemption.OrderID, ShopID: redemption.ShopID,
		SourceType: "delivery_return", SourceID: &returnID,
		Status: "processing", Version: 1,
	}
	orderRow := returnSettlementTestOrder{
		ID: redemption.OrderID, OrderType: redemptionOrderType,
		SettlementMode: redemptionSettlementMode, PayStatus: redemptionPayStatus,
		Status: "returning", DeliveryStatus: "returning",
		AfterSaleStatus: "processing", Version: 3,
	}
	for _, row := range []any{&redemption, &lot, &allocation, &afterSaleRow, &orderRow} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}

	settlementType := deliveryreturn.SettlementWineTicketRestore
	returnRow := deliveryreturn.Return{
		ID: returnID, OrderID: redemption.OrderID, AfterSaleID: &afterSaleID,
		SettlementType: &settlementType, SettlementBizID: &redemption.ID,
	}
	afterSaleFact := deliveryreturn.AfterSale{
		ID: afterSaleID, OrderID: redemption.OrderID,
		SourceType: "delivery_return", SourceID: &returnID,
	}
	orderFact := deliveryreturn.OrderRef{
		ID: redemption.OrderID, OrderType: redemptionOrderType,
		SettlementMode: redemptionSettlementMode, PayStatus: redemptionPayStatus,
	}
	handler := NewWineTicketReturnSettlement(
		db,
		snowflake.New(65),
		new(aftersale.Service),
	)
	handler.core.setClock(func() time.Time { return now })
	err = db.Transaction(func(tx *gorm.DB) error {
		prepared, prepareErr := handler.PrepareReceived(
			t.Context(), tx, returnRow, afterSaleFact, orderFact,
		)
		if prepareErr != nil {
			return prepareErr
		}
		applied, applyErr := prepared.ApplyReceived(
			t.Context(), tx, returnRow, afterSaleFact, orderFact,
		)
		if applyErr != nil {
			return applyErr
		}
		if !applied {
			t.Fatal("received return was not applied")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var currentLot core.Lot
	if err := db.First(&currentLot, lot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if currentLot.AvailableQuantity != 0 ||
		currentLot.Status != LotStatusExpired ||
		currentLot.Version != lot.Version+2 {
		t.Fatalf("returned expired lot = %+v", currentLot)
	}
	var transactions []core.Transaction
	if err := db.Where("lot_id = ?", lot.ID).Order("id ASC").
		Find(&transactions).Error; err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 2 {
		t.Fatalf("return transaction count = %d, want 2", len(transactions))
	}
	restoreActionKey := "redemption_return_restore:81:301"
	if transactions[0].TransactionType != core.TransactionTypeRedemptionReturnRestore ||
		transactions[0].QuantityDelta != 2 ||
		transactions[0].BeforeAvailableQuantity != 0 ||
		transactions[0].AfterAvailableQuantity != 2 ||
		transactions[0].BizType != "wine_ticket_redemption_return" ||
		transactions[0].BizID != returnID ||
		transactions[0].ActionKey != restoreActionKey ||
		!transactions[0].CreatedAt.Equal(now) {
		t.Fatalf("return restore evidence = %+v", transactions[0])
	}
	assertRedemptionTransactionMetadata(t, transactions[0], map[string]string{
		"delivery_return_id": "81",
		"redemption_id":      "91",
		"allocation_id":      "301",
	})
	if transactions[1].TransactionType != core.TransactionTypeRedemptionReturnExpire ||
		transactions[1].QuantityDelta != -2 ||
		transactions[1].BeforeAvailableQuantity != 2 ||
		transactions[1].AfterAvailableQuantity != 0 ||
		transactions[1].BizType != "wine_ticket_redemption_return" ||
		transactions[1].BizID != returnID ||
		transactions[1].ActionKey != "redemption_return_expire:81:301" ||
		!transactions[1].CreatedAt.Equal(now) {
		t.Fatalf("return expiry evidence = %+v", transactions[1])
	}
	assertRedemptionTransactionMetadata(t, transactions[1], map[string]string{
		"reason":             "restored_after_expiry",
		"restore_action_key": restoreActionKey,
		"restore_biz_type":   "wine_ticket_redemption_return",
		"restore_biz_id":     "81",
	})
}

func assertRedemptionTransactionMetadata(
	t *testing.T,
	transaction core.Transaction,
	expected map[string]string,
) {
	t.Helper()
	var actual map[string]string
	if err := json.Unmarshal(transaction.MetadataJSON, &actual); err != nil {
		t.Fatalf("decode transaction metadata: %v", err)
	}
	if len(actual) != len(expected) {
		t.Fatalf("transaction metadata = %#v, want %#v", actual, expected)
	}
	for key, value := range expected {
		if actual[key] != value {
			t.Fatalf("transaction metadata[%q] = %q, want %q", key, actual[key], value)
		}
	}
}

func TestWineTicketReturnPreparationLocksCanonicalPrefixAndBindsPlan(t *testing.T) {
	db, err := gorm.Open(
		sqlite.Open(testutil.UniqueSQLiteMemoryDSN(t)),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Redemption{}, &RedemptionAllocation{}, &core.Lot{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(shanghaiLocation).Truncate(time.Millisecond)
	redemption := Redemption{
		ID: 91, RedemptionNo: "WTR91", CustomerID: 10, IssuerMerchantID: 20,
		ProductID: 30, ShopID: 40, ShopProductID: 50, DeliveryTimeSlotID: 60,
		OrderID: 71, Quantity: 3, AddressID: 80, AddressVersion: 1,
		Status: RedemptionStatusReturnInProgress, Version: 2,
		ScheduledStartAt: now, ScheduledEndAt: now.Add(time.Hour),
		NotBeforeAt: now.Add(-time.Hour),
	}
	lots := []core.Lot{
		{
			ID: 501, LotNo: "WTL501", OwnerCustomerID: redemption.CustomerID,
			PurchaseID: 1, SourceType: "purchase", IssuerMerchantID: 20,
			ProductID: 30, RedeemCityCode: "310100", TotalQuantity: 2,
			OriginalExpiresAt: now.AddDate(1, 0, 0), ExpiresAt: now.AddDate(1, 0, 0),
			ExpiryChangedAt: now, Status: LotStatusDepleted, Version: 1,
		},
		{
			ID: 502, LotNo: "WTL502", OwnerCustomerID: redemption.CustomerID,
			PurchaseID: 1, SourceType: "purchase", IssuerMerchantID: 20,
			ProductID: 30, RedeemCityCode: "310100", TotalQuantity: 1,
			OriginalExpiresAt: now.AddDate(1, 0, 0), ExpiresAt: now.AddDate(1, 0, 0),
			ExpiryChangedAt: now, Status: LotStatusDepleted, Version: 1,
		},
	}
	allocations := []RedemptionAllocation{
		{
			ID: 302, RedemptionID: redemption.ID, LotID: 501,
			Quantity: 2, SourceExpiresAt: lots[0].ExpiresAt,
			Status: RedemptionAllocationStatusConsumed,
		},
		{
			ID: 301, RedemptionID: redemption.ID, LotID: 502,
			Quantity: 1, SourceExpiresAt: lots[1].ExpiresAt,
			Status: RedemptionAllocationStatusConsumed,
		},
	}
	if err := db.Create(&redemption).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&lots).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&allocations).Error; err != nil {
		t.Fatal(err)
	}

	lockStages := make([]string, 0, 3)
	callbackName := "test:wine-ticket-return-lock-prefix"
	if err := db.Callback().Query().Before("gorm:query").Register(
		callbackName,
		func(callbackTx *gorm.DB) {
			switch callbackTx.Statement.Table {
			case "wine_ticket_redemptions":
				lockStages = append(lockStages, "redemption")
			case "wine_ticket_redemption_allocations":
				lockStages = append(lockStages, "allocations")
			case "wine_ticket_lots":
				lockStages = append(lockStages, "lots")
			}
		},
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Callback().Query().Remove(callbackName)
	})

	returnID, afterSaleID := uint64(81), uint64(82)
	settlementType := deliveryreturn.SettlementWineTicketRestore
	row := deliveryreturn.Return{
		ID: returnID, OrderID: redemption.OrderID, AfterSaleID: &afterSaleID,
		SettlementType: &settlementType, SettlementBizID: &redemption.ID,
	}
	afterSale := deliveryreturn.AfterSale{
		ID: afterSaleID, OrderID: redemption.OrderID, SourceType: "delivery_return",
		SourceID: &returnID,
	}
	order := deliveryreturn.OrderRef{
		ID: redemption.OrderID, OrderType: "wine_ticket_redemption",
		SettlementMode: "wine_ticket", PayStatus: "not_required",
	}
	handler := NewWineTicketReturnSettlement(
		db,
		snowflake.New(64),
		new(aftersale.Service),
	)
	err = db.Transaction(func(tx *gorm.DB) error {
		prepared, prepareErr := handler.PrepareReceived(
			t.Context(), tx, row, afterSale, order,
		)
		if prepareErr != nil {
			return prepareErr
		}
		plan, ok := prepared.(*wineTicketReturnReceivePlan)
		if !ok {
			t.Fatalf("prepared plan type = %T", prepared)
		}
		if got, want := strings.Join(lockStages, ","), "redemption,allocations,lots"; got != want {
			t.Fatalf("wine-ticket lock prefix = %q, want %q", got, want)
		}
		if got := []uint64{plan.allocations[0].ID, plan.allocations[1].ID}; got[0] != 301 || got[1] != 302 {
			t.Fatalf("allocation lock order = %v, want [301 302]", got)
		}
		if got := []uint64{plan.lots[0].ID, plan.lots[1].ID}; got[0] != 501 || got[1] != 502 {
			t.Fatalf("lot lock order = %v, want [501 502]", got)
		}
		changed := row
		changed.ID++
		if err := plan.validate(tx, changed, afterSale, order); err == nil {
			t.Fatal("prepared plan accepted a different delivery return")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
