package ops

import (
	"encoding/json"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/catalog"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/gift"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/purchase"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/redemption"
	refunddomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/renewal"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type queryTestProduct struct {
	ID        uint64 `gorm:"primaryKey"`
	Name      string
	BrandName *string
	Spec      *string
	ImageURL  *string
}

func (queryTestProduct) TableName() string { return "products" }

type queryTestMerchant struct {
	ID   uint64 `gorm:"primaryKey"`
	Name string
}

func (queryTestMerchant) TableName() string { return "merchants" }

func newCustomerAssetTestService(
	t *testing.T,
) (*Service, *gorm.DB, time.Time) {
	t.Helper()
	db, err := gorm.Open(
		sqlite.Open(uniqueSQLiteMemoryDSN(t, "_busy_timeout=5000")),
		&gorm.Config{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&catalog.Package{},
		&purchase.Purchase{},
		&core.Lot{},
		&redemption.RedemptionAllocation{},
		&gift.Gift{},
		&gift.GiftAllocation{},
		&refunddomain.RefundAllocation{},
		&renewal.Renewal{},
		&refunddomain.WineTicketRefund{},
		&order.Payment{},
		&queryTestProduct{},
		&queryTestMerchant{},
	); err != nil {
		t.Fatal(err)
	}
	now := time.Date(
		2026,
		time.July,
		27,
		15,
		30,
		0,
		123000000,
		shanghaiLocation,
	)
	ids := snowflake.New(87)
	projector := purchase.NewService(db, ids).WithNow(func() time.Time {
		return now
	})
	service := NewService(db, ids, projector).WithClock(func() time.Time {
		return now
	})
	if err := db.Create(&queryTestProduct{
		ID:   401,
		Name: "典藏干红",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&queryTestMerchant{
		ID:   1,
		Name: "酒票发行商",
	}).Error; err != nil {
		t.Fatal(err)
	}
	return service, db, now
}

func seedCustomerAssetPurchase(
	t *testing.T,
	db *gorm.DB,
	now time.Time,
	purchaseID uint64,
	customerID uint64,
	packageQuantity uint,
	bottlesPerPackage uint,
) purchase.Purchase {
	t.Helper()
	refundPolicy := datatypes.JSON(
		`{"schema_version":1,"enabled":true,"window_hours":168,` +
			`"require_never_used":true,"fee_amount":0}`,
	)
	renewalPolicy := datatypes.JSON(
		`{"schema_version":1,"enabled":true,"extension_days":30,` +
			`"max_count":2,"grace_days":0,"fee_amount":990}`,
	)
	pkg := catalog.Package{
		ID:                      purchaseID + 10_000,
		PackageNo:               "WTP" + idString(purchaseID),
		PackageCode:             "STOCKPILE_A",
		PackageVersion:          1,
		IssuerMerchantID:        1,
		SettlementShopID:        2,
		SettlementShopProductID: 3,
		ProductID:               401,
		RedeemCityCode:          "310100",
		PackageType:             catalog.PackageTypeStockpile,
		Name:                    "典藏囤酒套餐",
		BottleQuantity:          bottlesPerPackage,
		SalePriceAmount:         1000,
		MinPurchaseQuantity:     1,
		MaxPurchaseQuantity:     100,
		ValidityDays:            365,
		RefundPolicy:            refundPolicy,
		RenewalPolicy:           renewalPolicy,
		DeliveryPolicy: datatypes.JSON(
			`{"schema_version":1,"delivery_fee_included":true,` +
				`"dispatch_lead_minutes":120}`,
		),
		Status:    catalog.PackageStatusPublished,
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.Create(&pkg).Error; err != nil {
		t.Fatal(err)
	}
	snapshot, err := json.Marshal(map[string]any{
		"schema_version":             1,
		"package_no":                 pkg.PackageNo,
		"package_code":               pkg.PackageCode,
		"package_name":               pkg.Name,
		"package_type":               pkg.PackageType,
		"package_version":            1,
		"validity_days":              365,
		"bottle_quantity":            bottlesPerPackage,
		"unit_price_amount":          1000,
		"issuer_merchant_id":         "1",
		"settlement_shop_id":         "2",
		"settlement_shop_product_id": "3",
		"redeem_city_code":           "310100",
		"product": map[string]any{
			"product_id": "401",
			"name":       "典藏干红",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	row := purchase.Purchase{
		ID:                       purchaseID,
		PurchaseNo:               "WTPU" + idString(purchaseID),
		CustomerID:               customerID,
		PackageID:                pkg.ID,
		PackageVersion:           1,
		PaymentID:                purchaseID + 20_000,
		IssuerMerchantID:         1,
		SettlementShopID:         2,
		SettlementShopProductID:  3,
		ProductID:                401,
		RedeemCityCode:           "310100",
		PackageQuantity:          packageQuantity,
		BottleQuantityPerPackage: bottlesPerPackage,
		TotalBottleQuantity:      packageQuantity * bottlesPerPackage,
		UnitPriceAmount:          1000,
		PayableAmount:            int64(packageQuantity) * 1000,
		Currency:                 "CNY",
		PackageSnapshot:          datatypes.JSON(snapshot),
		RefundPolicySnapshot:     refundPolicy,
		RenewalPolicySnapshot:    renewalPolicy,
		Status:                   purchase.PurchaseStatusPendingPayment,
		Version:                  1,
		CreatedAt:                now,
		UpdatedAt:                now,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	bizType := purchase.PurchasePaymentBusiness
	bizID := row.ID
	if err := db.Create(&order.Payment{
		ID:         row.PaymentID,
		PaymentNo:  "PAY-" + idString(row.ID),
		BizType:    &bizType,
		BizID:      &bizID,
		CustomerID: customerID,
		Channel:    "wechat_miniapp",
		Provider:   "wechat",
		Status:     "pending",
		Amount:     row.PayableAmount,
		Currency:   "CNY",
		ClientPayload: datatypes.JSON(
			`{"timeStamp":"1","nonceStr":"n","package":"prepay_id=x",` +
				`"signType":"RSA","paySign":"s"}`,
		),
		CreatedAt: now,
		UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return row
}
