package redemption

import (
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/modules/delivery"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
	"jiuxiaoer-admin/backend-go/internal/testutil"
)

type fulfillmentOutboxTestRow struct {
	ID            uint64
	EventID       string
	EventType     string
	EventVersion  uint
	SpecVersion   string
	AggregateType string
	AggregateID   uint64
	Producer      *string
	Payload       datatypes.JSON
	Status        string
	RetryCount    int
	RequestID     *string
	CreatedAt     time.Time
}

func (fulfillmentOutboxTestRow) TableName() string { return "outbox_events" }

func TestWineTicketFulfillmentAdvancesWithoutSecondLotDeduction(t *testing.T) {
	t.Parallel()
	db, err := gorm.Open(
		sqlite.Open(testutil.UniqueSQLiteMemoryDSN(t)),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&core.Lot{},
		&Redemption{},
		&RedemptionAllocation{},
		&core.Transaction{},
		&delivery.AuditLog{},
		&fulfillmentOutboxTestRow{},
	); err != nil {
		t.Fatal(err)
	}

	expiresAt := time.Date(
		2027,
		time.July,
		27,
		23,
		59,
		0,
		0,
		shanghaiLocation,
	)
	lot := core.Lot{
		ID: 101, LotNo: "WTL101", OwnerCustomerID: 11, PurchaseID: 21,
		SourceType: LotSourcePurchase, IssuerMerchantID: 31, ProductID: 41,
		RedeemCityCode: "310100", TotalQuantity: 5, AvailableQuantity: 3,
		OriginalExpiresAt: expiresAt, ExpiresAt: expiresAt,
		ExpiryChangedAt: expiresAt.AddDate(-1, 0, 0), Status: LotStatusActive,
		Version: 2,
	}
	redemption := Redemption{
		ID: 201, RedemptionNo: "WTR201", CustomerID: 11,
		IssuerMerchantID: 31, ProductID: 41, ShopID: 51, ShopProductID: 61,
		DeliveryTimeSlotID: 71, OrderID: 81, Quantity: 2, AddressID: 91,
		AddressVersion: 1, Status: RedemptionStatusScheduled, Version: 1,
		ScheduledStartAt: expiresAt.AddDate(-1, 0, 0),
		ScheduledEndAt:   expiresAt.AddDate(-1, 0, 0).Add(time.Hour),
		NotBeforeAt:      expiresAt.AddDate(-1, 0, 0).Add(-time.Hour),
	}
	allocation := RedemptionAllocation{
		ID: 301, RedemptionID: redemption.ID, LotID: lot.ID, Quantity: 2,
		SourceExpiresAt: expiresAt, Status: RedemptionAllocationStatusHeld,
	}
	if err := db.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&redemption).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&allocation).Error; err != nil {
		t.Fatal(err)
	}

	handler := NewWineTicketFulfillmentSettlement(db, snowflake.New(44))
	orderRow := delivery.Order{
		ID: redemption.OrderID, OrderType: redemptionOrderType,
		SettlementMode: redemptionSettlementMode, PayStatus: redemptionPayStatus,
		PaidAmount: 0,
	}
	deliveryRow := delivery.DeliveryOrder{ID: 401, OrderID: redemption.OrderID}
	baseTime := time.Date(
		2026,
		time.July,
		27,
		10,
		0,
		0,
		123000000,
		shanghaiLocation,
	)
	for index, step := range []struct {
		action string
		status string
	}{
		{delivery.FulfillmentAssigned, RedemptionStatusAssigned},
		{delivery.FulfillmentPickedUp, RedemptionStatusPickedUp},
		{delivery.FulfillmentDelivered, RedemptionStatusDelivered},
	} {
		err := db.Transaction(func(tx *gorm.DB) error {
			return handler.ApplyFulfillment(t.Context(), tx, delivery.FulfillmentSettlementFact{
				Action: step.action, Delivery: deliveryRow, Order: orderRow,
				OccurredAt: baseTime.Add(time.Duration(index) * time.Minute),
				ActorType:  "rider", ActorID: 501,
			})
		})
		if err != nil {
			t.Fatalf("%s: %v", step.action, err)
		}
		var current Redemption
		if err := db.First(&current, redemption.ID).Error; err != nil {
			t.Fatal(err)
		}
		if current.Status != step.status {
			t.Fatalf("%s status=%s", step.action, current.Status)
		}
	}

	var currentLot core.Lot
	if err := db.First(&currentLot, lot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if currentLot.AvailableQuantity != lot.AvailableQuantity ||
		currentLot.Version != lot.Version {
		t.Fatalf("fulfillment changed lot quantity/version: %+v", currentLot)
	}
	var currentAllocation RedemptionAllocation
	if err := db.First(&currentAllocation, allocation.ID).Error; err != nil {
		t.Fatal(err)
	}
	if currentAllocation.Status != RedemptionAllocationStatusConsumed {
		t.Fatalf("allocation status=%s", currentAllocation.Status)
	}
	var transactionCount int64
	if err := db.Model(&core.Transaction{}).Count(&transactionCount).Error; err != nil {
		t.Fatal(err)
	}
	if transactionCount != 0 {
		t.Fatalf("fulfillment created %d quantity transactions", transactionCount)
	}
	var eventCount int64
	if err := db.Model(&fulfillmentOutboxTestRow{}).
		Where("event_type IN ?", []string{
			"wine_ticket.redemption_assigned",
			"wine_ticket.redemption_picked_up",
			"wine_ticket.redemption_delivered",
		}).
		Count(&eventCount).Error; err != nil {
		t.Fatal(err)
	}
	if eventCount != 3 {
		t.Fatalf("fulfillment event count=%d", eventCount)
	}
}
