package cabinet

import (
	"context"
	"strconv"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/gift"
	purchasedomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/purchase"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/redemption"
	refunddomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/renewal"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/testutil"
)

type cabinetTestProduct struct {
	ID        uint64 `gorm:"primaryKey"`
	Name      string
	BrandName *string
	Spec      *string
	ImageURL  *string
}

func (cabinetTestProduct) TableName() string { return "products" }

type cabinetTestMerchant struct {
	ID   uint64 `gorm:"primaryKey"`
	Name string
}

func (cabinetTestMerchant) TableName() string { return "merchants" }

func TestCabinetUsesAllocationFactsAndExcludesExpiredLots(t *testing.T) {
	service, db, now := newCabinetTestService(t)
	seedCabinetPurchase(t, db, now, 301, 42)
	activeExpiry := now.AddDate(0, 0, 30)
	expiredAt := now.Add(-time.Millisecond)
	lots := []core.Lot{
		{
			ID:                2001,
			LotNo:             "WTL_ACTIVE",
			OwnerCustomerID:   42,
			PurchaseID:        301,
			SourceType:        core.LotSourcePurchase,
			IssuerMerchantID:  1,
			ProductID:         401,
			RedeemCityCode:    "310100",
			TotalQuantity:     10,
			AvailableQuantity: 2,
			OriginalExpiresAt: activeExpiry,
			ExpiresAt:         activeExpiry,
			ExpiryChangedAt:   now,
			Status:            core.LotStatusActive,
			Version:           1,
			CreatedAt:         now,
			UpdatedAt:         now,
		},
		{
			ID:                2002,
			LotNo:             "WTL_EXPIRED_NOT_RETURNED",
			OwnerCustomerID:   42,
			PurchaseID:        301,
			SourceType:        core.LotSourcePurchase,
			IssuerMerchantID:  1,
			ProductID:         401,
			RedeemCityCode:    "310100",
			TotalQuantity:     5,
			AvailableQuantity: 5,
			OriginalExpiresAt: expiredAt,
			ExpiresAt:         expiredAt,
			ExpiryChangedAt:   now,
			Status:            core.LotStatusActive,
			Version:           1,
			CreatedAt:         now,
			UpdatedAt:         now,
		},
	}
	if err := db.Create(&lots).Error; err != nil {
		t.Fatal(err)
	}
	rows := []any{
		&redemption.RedemptionAllocation{
			ID: 1, RedemptionID: 11, LotID: 2001, Quantity: 3,
			SourceExpiresAt: activeExpiry, Status: "held",
			CreatedAt: now, UpdatedAt: now,
		},
		&redemption.RedemptionAllocation{
			ID: 2, RedemptionID: 12, LotID: 2001, Quantity: 4,
			SourceExpiresAt: activeExpiry, Status: "consumed",
			CreatedAt: now, UpdatedAt: now,
		},
		&gift.GiftAllocation{
			ID: 3, GiftID: 13, SourceLotID: 2001, Quantity: 2,
			SourceExpiresAt: activeExpiry, Status: "held",
			CreatedAt: now, UpdatedAt: now,
		},
		&refunddomain.RefundAllocation{
			ID: 4, WineTicketRefundID: 14, LotID: 2001, Quantity: 1,
			SourceExpiresAt: activeExpiry, Status: "held",
			CreatedAt: now, UpdatedAt: now,
		},
	}
	for _, row := range rows {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}

	dto, err := service.Cabinet(
		context.Background(),
		cabinetClaims(42),
		pagination.Query{PageSize: 20},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(dto.Items) != 1 {
		t.Fatalf("cabinet items=%+v", dto.Items)
	}
	item := dto.Items[0]
	if item.AvailableQuantity != 2 ||
		item.HeldQuantity != 6 ||
		item.ExtractedQuantity != 4 ||
		item.LotCount != 1 ||
		item.GiftSourceLotNo != "WTL_ACTIVE" {
		t.Fatalf("cabinet allocation projection=%+v", item)
	}
	if !containsString(item.Actions, "redeem") ||
		!containsString(item.Actions, "gift") ||
		containsString(item.Actions, "renew") {
		t.Fatalf("available lot actions=%v", item.Actions)
	}
}

func TestCabinetShowsFullyHeldLotsWithoutGrantingActions(t *testing.T) {
	service, db, now := newCabinetTestService(t)
	seedCabinetPurchase(t, db, now, 302, 42)
	expiresAt := now.AddDate(0, 0, 30)
	lots := []core.Lot{
		cabinetLot(2101, "WTL_REDEMPTION_HELD", 302, 42, expiresAt, now),
		cabinetLot(2102, "WTL_GIFT_HELD", 302, 42, expiresAt, now),
		cabinetLot(2103, "WTL_REFUND_HELD", 302, 42, expiresAt, now),
		cabinetLot(2104, "WTL_HISTORY_ONLY", 302, 42, expiresAt, now),
	}
	if err := db.Create(&lots).Error; err != nil {
		t.Fatal(err)
	}
	rows := []any{
		&redemption.RedemptionAllocation{
			ID: 21, RedemptionID: 31, LotID: 2101, Quantity: 3,
			SourceExpiresAt: expiresAt, Status: "held",
			CreatedAt: now, UpdatedAt: now,
		},
		&redemption.RedemptionAllocation{
			ID: 22, RedemptionID: 32, LotID: 2101, Quantity: 1,
			SourceExpiresAt: expiresAt, Status: "consumed",
			CreatedAt: now, UpdatedAt: now,
		},
		&redemption.RedemptionAllocation{
			ID: 23, RedemptionID: 33, LotID: 2101, Quantity: 2,
			SourceExpiresAt: expiresAt, Status: "restored",
			CreatedAt: now, UpdatedAt: now,
		},
		&gift.GiftAllocation{
			ID: 24, GiftID: 34, SourceLotID: 2102, Quantity: 3,
			SourceExpiresAt: expiresAt, Status: "held",
			CreatedAt: now, UpdatedAt: now,
		},
		&gift.GiftAllocation{
			ID: 25, GiftID: 35, SourceLotID: 2102, Quantity: 2,
			SourceExpiresAt: expiresAt, Status: "restored",
			CreatedAt: now, UpdatedAt: now,
		},
		&refunddomain.RefundAllocation{
			ID: 26, WineTicketRefundID: 36, LotID: 2103, Quantity: 3,
			SourceExpiresAt: expiresAt, Status: "held",
			CreatedAt: now, UpdatedAt: now,
		},
		&refunddomain.RefundAllocation{
			ID: 27, WineTicketRefundID: 37, LotID: 2103, Quantity: 2,
			SourceExpiresAt: expiresAt, Status: "consumed",
			CreatedAt: now, UpdatedAt: now,
		},
		&redemption.RedemptionAllocation{
			ID: 28, RedemptionID: 38, LotID: 2104, Quantity: 3,
			SourceExpiresAt: expiresAt, Status: "consumed",
			CreatedAt: now, UpdatedAt: now,
		},
		&refunddomain.RefundAllocation{
			ID: 29, WineTicketRefundID: 39, LotID: 2104, Quantity: 3,
			SourceExpiresAt: expiresAt, Status: "restored",
			CreatedAt: now, UpdatedAt: now,
		},
	}
	for _, row := range rows {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}

	dto, err := service.Cabinet(
		context.Background(),
		cabinetClaims(42),
		pagination.Query{PageSize: 20},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(dto.Items) != 1 {
		t.Fatalf("cabinet items=%+v", dto.Items)
	}
	item := dto.Items[0]
	if item.AvailableQuantity != 0 ||
		item.HeldQuantity != 9 ||
		item.ExtractedQuantity != 1 ||
		item.LotCount != 3 {
		t.Fatalf("fully held cabinet projection=%+v", item)
	}
	if len(item.Actions) != 0 {
		t.Fatalf("fully held lots must remain non-actionable, actions=%v", item.Actions)
	}
}

func newCabinetTestService(t *testing.T) (*Service, *gorm.DB, time.Time) {
	t.Helper()
	db, err := gorm.Open(
		sqlite.Open(testutil.UniqueSQLiteMemoryDSN(t, "_busy_timeout=5000")),
		&gorm.Config{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&purchasedomain.Purchase{},
		&core.Lot{},
		&redemption.RedemptionAllocation{},
		&gift.Gift{},
		&gift.GiftAllocation{},
		&refunddomain.RefundAllocation{},
		&renewal.Renewal{},
		&cabinetTestProduct{},
		&cabinetTestMerchant{},
	); err != nil {
		t.Fatal(err)
	}
	now := time.Date(
		2026,
		7,
		27,
		15,
		30,
		0,
		123000000,
		core.ShanghaiLocation,
	)
	if err := db.Create(&cabinetTestProduct{
		ID:   401,
		Name: "典藏干红",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&cabinetTestMerchant{
		ID:   1,
		Name: "酒票发行商",
	}).Error; err != nil {
		t.Fatal(err)
	}
	return NewService(db).WithClock(func() time.Time { return now }), db, now
}

func seedCabinetPurchase(
	t *testing.T,
	db *gorm.DB,
	now time.Time,
	purchaseID uint64,
	customerID uint64,
) {
	t.Helper()
	if err := db.Create(&purchasedomain.Purchase{
		ID:                    purchaseID,
		PurchaseNo:            "WTPU" + strconv.FormatUint(purchaseID, 10),
		CustomerID:            customerID,
		ProductID:             401,
		RenewalPolicySnapshot: datatypes.JSON(`{"schema_version":1,"enabled":true,"extension_days":30,"max_count":2,"grace_days":0,"fee_amount":990}`),
		Status:                purchasedomain.PurchaseStatusIssued,
		Version:               1,
		CreatedAt:             now,
		UpdatedAt:             now,
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func cabinetLot(
	id uint64,
	lotNo string,
	purchaseID uint64,
	customerID uint64,
	expiresAt time.Time,
	now time.Time,
) core.Lot {
	return core.Lot{
		ID:                id,
		LotNo:             lotNo,
		OwnerCustomerID:   customerID,
		PurchaseID:        purchaseID,
		SourceType:        core.LotSourcePurchase,
		IssuerMerchantID:  1,
		ProductID:         401,
		RedeemCityCode:    "310100",
		TotalQuantity:     3,
		AvailableQuantity: 0,
		OriginalExpiresAt: expiresAt,
		ExpiresAt:         expiresAt,
		ExpiryChangedAt:   now,
		Status:            core.LotStatusDepleted,
		Version:           1,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
}

func cabinetClaims(customerID uint64) *auth.Claims {
	return &auth.Claims{
		AccountType: "customer",
		CustomerID:  strconv.FormatUint(customerID, 10),
		Permissions: []string{"wine_ticket_cabinet:view"},
	}
}
