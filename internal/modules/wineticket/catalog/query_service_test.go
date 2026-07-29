package catalog

import (
	"context"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/testutil"
)

type queryTestMerchant struct {
	ID           uint64 `gorm:"primaryKey"`
	Status       string
	ReviewStatus string
	DeletedAt    *time.Time
}

func (queryTestMerchant) TableName() string { return "merchants" }

type queryTestShop struct {
	ID             uint64 `gorm:"primaryKey"`
	MerchantID     uint64
	Status         string
	BusinessStatus string
	DeletedAt      *time.Time
}

func (queryTestShop) TableName() string { return "shops" }

type queryTestCategory struct {
	ID            uint64 `gorm:"primaryKey"`
	Status        string
	AgeRestricted bool
	DeletedAt     *time.Time
}

func (queryTestCategory) TableName() string { return "categories" }

type queryTestProduct struct {
	ID            uint64 `gorm:"primaryKey"`
	CategoryID    uint64
	Name          string
	BrandName     *string
	Spec          *string
	ImageURL      *string
	Status        string
	AgeRestricted bool
	DeletedAt     *time.Time
}

func (queryTestProduct) TableName() string { return "products" }

type queryTestShopProduct struct {
	ID         uint64 `gorm:"primaryKey"`
	MerchantID uint64
	ShopID     uint64
	ProductID  uint64
	Status     string
	DeletedAt  *time.Time
}

func (queryTestShopProduct) TableName() string { return "shop_products" }

func TestQueryServiceListsOnlyCurrentlyPublishedEligiblePackages(t *testing.T) {
	db, err := gorm.Open(
		sqlite.Open(testutil.UniqueSQLiteMemoryDSN(t)),
		&gorm.Config{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&Package{},
		&queryTestMerchant{},
		&queryTestShop{},
		&queryTestCategory{},
		&queryTestProduct{},
		&queryTestShopProduct{},
	); err != nil {
		t.Fatal(err)
	}

	now := time.Date(
		2026,
		7,
		28,
		12,
		0,
		0,
		0,
		time.FixedZone("UTC+8", 8*60*60),
	)
	seedQueryCatalog(t, db, now)
	service := NewQueryService(db)
	service.now = func() time.Time { return now }

	items, next, err := service.ListPublicPackages(
		context.Background(),
		pagination.Query{PageSize: 20, TokenHash: "catalog-query-test"},
		PackageTypeStockpile,
	)
	if err != nil {
		t.Fatal(err)
	}
	if next != "" || len(items) != 1 || items[0].PackageNo != "WTP1" {
		t.Fatalf("unexpected package page: items=%+v next=%q", items, next)
	}

	item, err := service.PublicPackage(context.Background(), "WTP1")
	if err != nil {
		t.Fatal(err)
	}
	if item.Product.Name != "测试葡萄酒" || item.Status != PackageStatusPublished {
		t.Fatalf("unexpected package detail: %+v", item)
	}
}

func seedQueryCatalog(t *testing.T, db *gorm.DB, now time.Time) {
	t.Helper()
	rows := []any{
		&queryTestMerchant{
			ID:           101,
			Status:       "active",
			ReviewStatus: "approved",
		},
		&queryTestShop{
			ID:             201,
			MerchantID:     101,
			Status:         "active",
			BusinessStatus: "open",
		},
		&queryTestCategory{
			ID:            501,
			Status:        "active",
			AgeRestricted: true,
		},
		&queryTestProduct{
			ID:         401,
			CategoryID: 501,
			Name:       "测试葡萄酒",
			Status:     "on_sale",
		},
		&queryTestShopProduct{
			ID:         301,
			MerchantID: 101,
			ShopID:     201,
			ProductID:  401,
			Status:     "on_sale",
		},
		&Package{
			ID:                      1,
			PackageNo:               "WTP1",
			PackageCode:             "STOCKPILE_2026",
			PackageVersion:          1,
			IssuerMerchantID:        101,
			SettlementShopID:        201,
			SettlementShopProductID: 301,
			ProductID:               401,
			RedeemCityCode:          "310100",
			PackageType:             PackageTypeStockpile,
			Name:                    "年度囤酒套餐",
			BottleQuantity:          6,
			SalePriceAmount:         59900,
			MinPurchaseQuantity:     1,
			MaxPurchaseQuantity:     10,
			ValidityDays:            365,
			RefundPolicy: datatypes.JSON(
				`{"schema_version":1,"enabled":true,"window_hours":168,"require_never_used":true,"fee_amount":0}`,
			),
			RenewalPolicy: datatypes.JSON(
				`{"schema_version":1,"enabled":true,"extension_days":30,"max_count":1,"grace_days":0,"fee_amount":990}`,
			),
			DeliveryPolicy: datatypes.JSON(
				`{"schema_version":1,"delivery_fee_included":true,"dispatch_lead_minutes":120}`,
			),
			Status:      PackageStatusPublished,
			SaleStartAt: timePointer(now.Add(-time.Hour)),
			SaleEndAt:   timePointer(now.Add(time.Hour)),
			Version:     1,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
	}
	for _, row := range rows {
		if err := db.Create(row).Error; err != nil {
			t.Fatalf("seed catalog query row %T: %v", row, err)
		}
	}
}

func timePointer(value time.Time) *time.Time { return &value }
