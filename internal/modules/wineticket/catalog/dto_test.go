package catalog

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"
)

func TestPackageDTOFromRecordKeepsSettlementFactsPrivate(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	row := PackageRecord{
		Package: Package{
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
			Status:    PackageStatusPublished,
			Version:   2,
			CreatedAt: now,
			UpdatedAt: now,
		},
		ProductName: "测试葡萄酒",
	}

	dto, err := PackageDTOFromRecord(row)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	body := string(payload)
	for _, forbidden := range []string{
		"package_code",
		"issuer_merchant_id",
		"settlement_shop_id",
		"settlement_shop_product_id",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("public catalog DTO leaked %q: %s", forbidden, body)
		}
	}
	if dto.PackageNo != "WTP1" || dto.Product.Name != "测试葡萄酒" {
		t.Fatalf("unexpected public DTO: %+v", dto)
	}
}
