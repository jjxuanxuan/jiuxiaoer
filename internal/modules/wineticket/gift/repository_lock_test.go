package gift

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
)

func TestLockGiftLotsUsesCanonicalIDOrderForExistingGiftActions(t *testing.T) {
	_, db, now := newGiftTestService(t)
	lots := []core.Lot{
		{
			ID: 902, LotNo: "LOT_ID_902_EARLY", OwnerCustomerID: 101,
			PurchaseID: 1902, SourceType: LotSourcePurchase,
			IssuerMerchantID: 801, ProductID: 301, RedeemCityCode: "310100",
			TotalQuantity: 1, AvailableQuantity: 1,
			OriginalExpiresAt: now.Add(24 * time.Hour), ExpiresAt: now.Add(24 * time.Hour),
			ExpiryChangedAt: now, Status: LotStatusActive, Version: 1,
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: 901, LotNo: "LOT_ID_901_LATE", OwnerCustomerID: 101,
			PurchaseID: 1901, SourceType: LotSourcePurchase,
			IssuerMerchantID: 801, ProductID: 301, RedeemCityCode: "310100",
			TotalQuantity: 1, AvailableQuantity: 1,
			OriginalExpiresAt: now.Add(48 * time.Hour), ExpiresAt: now.Add(48 * time.Hour),
			ExpiryChangedAt: now, Status: LotStatusActive, Version: 1,
			CreatedAt: now, UpdatedAt: now,
		},
	}
	if err := db.Create(&lots).Error; err != nil {
		t.Fatal(err)
	}
	var locked []core.Lot
	err := db.Transaction(func(tx *gorm.DB) error {
		var lockErr error
		locked, lockErr = newGiftRepository(db).lockGiftLots(
			context.Background(), tx, []uint64{902, 901, 902},
		)
		return lockErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(locked) != 2 || locked[0].ID != 901 || locked[1].ID != 902 {
		t.Fatalf("existing gift lots locked out of canonical ID order: %+v", locked)
	}
}
