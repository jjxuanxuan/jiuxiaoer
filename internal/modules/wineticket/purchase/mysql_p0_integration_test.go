package purchase

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func mysqlPurchaseFixture(
	t *testing.T,
	db *gorm.DB,
	ids *snowflake.Generator,
	now time.Time,
	packageCode string,
) (Purchase, PurchaseQuota, order.PaymentSettlementFact) {
	t.Helper()
	purchaseID := ids.Next()
	customerID := ids.Next()
	paymentID := ids.Next()
	snapshot, err := json.Marshal(purchasePackageSnapshot{
		SchemaVersion:           1,
		PackageNo:               "MYSQL-PACKAGE-" + strconv.FormatUint(purchaseID, 10),
		PackageCode:             packageCode,
		PackageName:             "MySQL P0 资金闭环套餐",
		PackageType:             PackageTypeStockpile,
		PackageVersion:          1,
		ValidityDays:            365,
		BottleQuantity:          1,
		UnitPriceAmount:         1000,
		IssuerMerchantID:        "1",
		SettlementShopID:        "2",
		SettlementShopProductID: "3",
		RedeemCityCode:          "310100",
		Product: core.ProductSummaryDTO{
			ProductID: "4",
			Name:      "MySQL P0 验收酒",
		},
	})
	if err != nil {
		t.Fatalf("marshal purchase snapshot: %v", err)
	}
	purchase := Purchase{
		ID: purchaseID, PurchaseNo: "MYSQL-WTPU-" + strconv.FormatUint(purchaseID, 10),
		CustomerID: customerID, PackageID: ids.Next(), PackageVersion: 1,
		PaymentID: paymentID, IssuerMerchantID: 1, SettlementShopID: 2,
		SettlementShopProductID: 3, ProductID: 4, RedeemCityCode: "310100",
		PackageQuantity: 1, BottleQuantityPerPackage: 1, TotalBottleQuantity: 1,
		UnitPriceAmount: 1000, PayableAmount: 1000, Currency: "CNY",
		PackageSnapshot:       datatypes.JSON(snapshot),
		RefundPolicySnapshot:  datatypes.JSON(`{"schema_version":1}`),
		RenewalPolicySnapshot: datatypes.JSON(`{"schema_version":1}`),
		Status:                PurchaseStatusPendingPayment,
		Version:               1,
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	quota := PurchaseQuota{
		ID: ids.Next(), CustomerID: customerID, PackageCode: packageCode,
		ReservedQuantity: 1, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&purchase).Error; err != nil {
		t.Fatalf("create mysql purchase fixture: %v", err)
	}
	if err := db.Create(&quota).Error; err != nil {
		t.Fatalf("create mysql quota fixture: %v", err)
	}
	paidAt := now.Add(-time.Minute)
	fact := order.PaymentSettlementFact{
		PaymentID: paymentID,
		PaymentNo: "MYSQL-PAY-" + strconv.FormatUint(paymentID, 10),
		BizType:   PurchasePaymentBusiness, BizID: purchaseID,
		CustomerID: customerID, Amount: purchase.PayableAmount,
		Currency: "CNY", Provider: "wechat", ProviderStatus: "SUCCESS",
		PaidAt: &paidAt,
	}
	return purchase, quota, fact
}

func cleanupMySQLPurchaseAcceptance(
	t *testing.T,
	db *gorm.DB,
	purchase Purchase,
	quota PurchaseQuota,
) {
	t.Helper()
	cleanup := []struct {
		name  string
		query *gorm.DB
	}{
		{
			name: "outbox",
			query: db.Where(
				"(aggregate_type = ? AND aggregate_id = ?) OR (aggregate_type = ? AND aggregate_id IN (?))",
				"wine_ticket_purchase", purchase.ID,
				"wine_ticket_refund",
				db.Model(&issuanceCompensationRefund{}).Select("id").Where("purchase_id = ?", purchase.ID),
			).Delete(&customerAssetOutbox{}),
		},
		{
			name: "audit",
			query: db.Where(
				"resource_type = ? AND resource_id = ?",
				"wine_ticket_purchase",
				purchase.ID,
			).Delete(&packageTestAudit{}),
		},
		{
			name: "refund allocations",
			query: db.Where(
				"wine_ticket_refund_id IN (?)",
				db.Model(&issuanceCompensationRefund{}).Select("id").Where("purchase_id = ?", purchase.ID),
			).Delete(&purchaseRefundAllocationGuard{}),
		},
		{
			name:  "refunds",
			query: db.Where("purchase_id = ?", purchase.ID).Delete(&issuanceCompensationRefund{}),
		},
		{
			name:  "transactions",
			query: db.Where("biz_type = ? AND biz_id = ?", "purchase", purchase.ID).Delete(&core.Transaction{}),
		},
		{
			name:  "lots",
			query: db.Where("purchase_id = ?", purchase.ID).Delete(&core.Lot{}),
		},
		{
			name:  "purchase",
			query: db.Where("id = ?", purchase.ID).Delete(&Purchase{}),
		},
		{
			name:  "quota",
			query: db.Where("id = ?", quota.ID).Delete(&PurchaseQuota{}),
		},
	}
	for _, item := range cleanup {
		if item.query.Error != nil {
			t.Errorf("cleanup mysql purchase %s: %v", item.name, item.query.Error)
		}
	}
}

// AC-WT-005E：已持久化失败事实与支付机构成功结果竞争时，
// 必须收敛为一次权益发放，且不能产生重复批次或台账。
func TestMySQLPurchaseSettlementConcurrentFailureAndSuccessIssuesExactlyOnce(
	t *testing.T,
) {
	ctx, db := openWineTicketMySQLAcceptance(t, 45*time.Second)
	now := time.Now().In(shanghaiLocation).Truncate(time.Millisecond)
	ids := snowflake.New(941)
	purchase, quota, fact := mysqlPurchaseFixture(
		t,
		db,
		ids,
		now,
		"MYSQL-RACE-"+strconv.FormatUint(ids.Next(), 10),
	)
	t.Cleanup(func() { cleanupMySQLPurchaseAcceptance(t, db, purchase, quota) })
	service := NewService(db, ids)
	service.now = func() time.Time { return now }

	results := runMySQLConcurrentErrors(
		mysqlP0Concurrency,
		func(index int) error {
			return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if err := service.LockBusiness(ctx, tx, purchase.ID); err != nil {
					return err
				}
				if index%2 == 0 {
					return service.ApplyException(
						ctx,
						tx,
						fact,
						"injected_issuance_write_failure",
					)
				}
				return service.ApplySuccess(ctx, tx, fact)
			})
		},
	)
	for _, resultErr := range results {
		if resultErr != nil {
			t.Fatalf("concurrent payment settlement failed: %v", resultErr)
		}
	}

	var stored Purchase
	if err := db.First(&stored, purchase.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != PurchaseStatusIssued || stored.PaidAt == nil ||
		stored.IssuedAt == nil {
		t.Fatalf("purchase did not converge to issued: %+v", stored)
	}
	var storedQuota PurchaseQuota
	if err := db.First(&storedQuota, quota.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedQuota.ReservedQuantity != 0 || storedQuota.ConsumedQuantity != 1 {
		t.Fatalf("quota was applied more than once: %+v", storedQuota)
	}
	var lotCount, issueCount, refundCount int64
	if err := db.Model(&core.Lot{}).Where("purchase_id = ?", purchase.ID).Count(&lotCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&core.Transaction{}).Where(
		"biz_type = ? AND biz_id = ? AND transaction_type = ?",
		"purchase",
		purchase.ID,
		TransactionTypePurchaseIssue,
	).Count(&issueCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&issuanceCompensationRefund{}).Where("purchase_id = ?", purchase.ID).Count(&refundCount).Error; err != nil {
		t.Fatal(err)
	}
	if lotCount != 1 || issueCount != 1 || refundCount != 0 {
		t.Fatalf(
			"settlement effects lot=%d issue=%d refund=%d, want 1/1/0",
			lotCount,
			issueCount,
			refundCount,
		)
	}
}
