package ops

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestAdminPurchaseAndLotQueriesArePermissionedFilteredAndSafe(t *testing.T) {
	service, db, now := newCustomerAssetTestService(t)
	purchase := seedCustomerAssetPurchase(t, db, now, 31_001, 41_001, 2, 3)
	expiresAt := now.AddDate(0, 0, 365)
	lot := core.Lot{
		ID: 51_001, LotNo: "WTL51001", OwnerCustomerID: purchase.CustomerID,
		PurchaseID: purchase.ID, SourceType: LotSourcePurchase,
		IssuerMerchantID: purchase.IssuerMerchantID, ProductID: purchase.ProductID,
		RedeemCityCode: purchase.RedeemCityCode, TotalQuantity: 6,
		AvailableQuantity: 6, OriginalExpiresAt: expiresAt, ExpiresAt: expiresAt,
		ExpiryChangedAt: now, Status: LotStatusActive, Version: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}
	admin := &auth.Claims{
		AccountType: "admin",
		AdminUserID: "9001",
		Permissions: []string{
			"wine_ticket_purchase:list_all",
			"wine_ticket_lot:list_all",
		},
	}
	purchases, next, err := service.ListAdminPurchases(
		context.Background(),
		admin,
		pagination.Query{PageSize: 20},
		AdminPurchaseFilter{
			CustomerID:       purchase.CustomerID,
			PurchaseNo:       purchase.PurchaseNo,
			Status:           purchase.Status,
			PackageCode:      "STOCKPILE_A",
			IssuerMerchantID: purchase.IssuerMerchantID,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if next != "" || len(purchases) != 1 {
		t.Fatalf("admin purchases=%+v next=%q", purchases, next)
	}
	item := purchases[0]
	if item.CustomerID != "41001" ||
		item.PackageCode != "STOCKPILE_A" ||
		item.IssuerMerchantID != "1" ||
		item.SettlementShopID != "2" ||
		item.SettlementShopProductID != "3" ||
		item.PaymentNo != "PAY-31001" ||
		item.RemainingBottleQuantity != 6 {
		t.Fatalf("unexpected admin purchase projection: %+v", item)
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		`"phone":`, `"openid":`, `"realname":`, `"package_snapshot":`, `"client_payload":`,
		`"provider_trade_no":`,
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("admin purchase leaked %q: %s", forbidden, encoded)
		}
	}

	lots, next, err := service.ListAdminLots(
		context.Background(),
		admin,
		pagination.Query{PageSize: 20},
		AdminLotFilter{
			OwnerCustomerID:  purchase.CustomerID,
			LotNo:            lot.LotNo,
			PurchaseNo:       purchase.PurchaseNo,
			Status:           LotStatusActive,
			ProductID:        purchase.ProductID,
			IssuerMerchantID: purchase.IssuerMerchantID,
			ExpiresBefore:    timePtr(expiresAt.AddDate(0, 0, 1)),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if next != "" || len(lots) != 1 {
		t.Fatalf("admin lots=%+v next=%q", lots, next)
	}
	if lots[0].OwnerCustomerID != "41001" ||
		lots[0].PurchaseNo != purchase.PurchaseNo ||
		lots[0].LotNo != lot.LotNo {
		t.Fatalf("unexpected admin lot projection: %+v", lots[0])
	}

	withoutPermission := &auth.Claims{
		AccountType: "admin",
		AdminUserID: "9002",
	}
	_, _, err = service.ListAdminPurchases(
		context.Background(),
		withoutPermission,
		pagination.Query{PageSize: 20},
		AdminPurchaseFilter{},
	)
	if problem.FromError(err).ErrorCode != "PERM_FORBIDDEN" {
		t.Fatalf("missing permission error=%v", err)
	}
}
