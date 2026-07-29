package refund

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	mysqlinfra "jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	sharedrefund "jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	purchasedomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/purchase"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

const mysqlRefundConcurrency = 100

type mysqlRefundFixture struct {
	purchase   purchasedomain.Purchase
	payment    order.Payment
	lot        core.Lot
	business   WineTicketRefund
	common     commonRefundRow
	allocation RefundAllocation
}

// TestMySQLRefundConcurrentDuplicateSuccessClosesFundsAndEntitlementOnce 验证
// 100 个重复 SUCCESS 观测会按公共退款优先的标准锁计划串行执行，
// 且资金与权益都只闭环一次。
func TestMySQLRefundConcurrentDuplicateSuccessClosesFundsAndEntitlementOnce(
	t *testing.T,
) {
	ctx, db := openRefundMySQLAcceptance(t, 60*time.Second)
	requireRefundMoneyContract(t, db)
	now := time.Now().In(shanghaiLocation).Truncate(time.Millisecond)
	ids := snowflake.New(947)
	fixture := seedMySQLRefundSuccessFixture(t, db, ids, now)
	t.Cleanup(func() { cleanupMySQLRefundSuccessFixture(t, db, fixture) })

	settlement := NewWineTicketRefundSettlement(db, ids).
		WithNow(func() time.Time { return now.Add(time.Minute) })
	commonService := sharedrefund.NewService(
		config.Config{},
		db,
		ids,
		nil,
	).WithRefundSettlementHandler(settlement)
	succeededAt := now.Add(30 * time.Second)
	state := sharedrefund.State{
		ProviderRefundID: "MYSQL-WX-REFUND-" +
			strconv.FormatUint(fixture.common.ID, 10),
		RefundNo:         fixture.common.RefundNo,
		PaymentNo:        fixture.payment.PaymentNo,
		Status:           "SUCCESS",
		Amount:           fixture.common.Amount,
		TotalAmount:      fixture.common.TotalAmount,
		Currency:         fixture.common.Currency,
		CurrencyRequired: true,
		SucceededAt:      &succeededAt,
	}

	results := runConcurrentRefundSuccess(mysqlRefundConcurrency, func() error {
		return commonService.ApplyProviderState(
			ctx,
			fixture.common.ID,
			state,
		)
	})
	for _, resultErr := range results {
		if resultErr != nil {
			t.Fatalf("duplicate SUCCESS settlement failed: %v", resultErr)
		}
	}

	var payment order.Payment
	if err := db.First(&payment, fixture.payment.ID).Error; err != nil {
		t.Fatal(err)
	}
	var allocation RefundAllocation
	if err := db.First(&allocation, fixture.allocation.ID).Error; err != nil {
		t.Fatal(err)
	}
	var lot core.Lot
	if err := db.First(&lot, fixture.lot.ID).Error; err != nil {
		t.Fatal(err)
	}
	var purchase purchasedomain.Purchase
	if err := db.First(&purchase, fixture.purchase.ID).Error; err != nil {
		t.Fatal(err)
	}
	var business WineTicketRefund
	if err := db.First(&business, fixture.business.ID).Error; err != nil {
		t.Fatal(err)
	}
	var common commonRefundRow
	if err := db.First(&common, fixture.common.ID).Error; err != nil {
		t.Fatal(err)
	}

	if payment.RefundedAmount != fixture.payment.Amount ||
		payment.Version != fixture.payment.Version+1 {
		t.Fatalf(
			"payment money closure applied more than once: before=%+v after=%+v",
			fixture.payment,
			payment,
		)
	}
	if allocation.Status != RefundAllocationConsumed {
		t.Fatalf("refund allocation was not consumed exactly once: %+v", allocation)
	}
	if lot.Status != LotStatusRefunded ||
		lot.AvailableQuantity != 0 ||
		lot.Version != fixture.lot.Version+1 {
		t.Fatalf(
			"lot closure applied more than once: before=%+v after=%+v",
			fixture.lot,
			lot,
		)
	}
	if purchase.Status != PurchaseStatusRefunded ||
		purchase.Version != fixture.purchase.Version+1 {
		t.Fatalf("purchase closure applied more than once: %+v", purchase)
	}
	if business.Status != RefundStatusSucceeded ||
		business.Version != fixture.business.Version+1 {
		t.Fatalf("business refund closure applied more than once: %+v", business)
	}
	if common.Status != "succeeded" ||
		common.Version != fixture.common.Version+1 {
		t.Fatalf("common refund closure applied more than once: %+v", common)
	}

	var allocationCount, entitlementTransactionCount int64
	if err := db.Model(&RefundAllocation{}).
		Where(
			"wine_ticket_refund_id = ? AND status = ?",
			fixture.business.ID,
			RefundAllocationConsumed,
		).
		Count(&allocationCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&core.Transaction{}).
		Where("lot_id = ?", fixture.lot.ID).
		Count(&entitlementTransactionCount).Error; err != nil {
		t.Fatal(err)
	}
	if allocationCount != 1 || entitlementTransactionCount != 2 {
		t.Fatalf(
			"duplicate SUCCESS added entitlement effects: allocations=%d transactions=%d",
			allocationCount,
			entitlementTransactionCount,
		)
	}

	var auditCount, outboxCount int64
	if err := db.Table("audit_logs").
		Where(
			"resource_type = ? AND resource_id = ? AND action = ?",
			"wine_ticket_purchase",
			fixture.purchase.ID,
			"wine_ticket.refund.succeed",
		).
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("outbox_events").
		Where(
			"aggregate_type = ? AND aggregate_id = ? AND event_type = ?",
			"wine_ticket_refund",
			fixture.business.ID,
			"wine_ticket.refund_succeeded",
		).
		Count(&outboxCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 || outboxCount != 1 {
		t.Fatalf(
			"duplicate SUCCESS emitted duplicate facts: audit=%d outbox=%d",
			auditCount,
			outboxCount,
		)
	}
}

func openRefundMySQLAcceptance(
	t *testing.T,
	timeout time.Duration,
) (context.Context, *gorm.DB) {
	t.Helper()
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run wine-ticket MySQL P0 acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	cfg := config.Load()
	cfg.MySQL.Required = true
	cfg.MySQL.RequiredTimeZone = "+08:00"
	cfg.MySQL.RequireWineTicketSchema = true
	cfg.MySQL.RequireWineTicketMoneyContract = false
	if cfg.MySQL.MaxOpenConns < 64 {
		cfg.MySQL.MaxOpenConns = 64
	}
	if cfg.MySQL.MaxIdleConns < 16 {
		cfg.MySQL.MaxIdleConns = 16
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := mysqlinfra.Open(ctx, cfg.MySQL, log)
	if err != nil || db == nil {
		t.Fatalf("open schema-verified mysql: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get mysql pool: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return ctx, db
}

func requireRefundMoneyContract(t *testing.T, db *gorm.DB) {
	t.Helper()
	var facts struct {
		OrderIDNullable string
		ConstraintCount int64
	}
	if err := db.Raw(`
		SELECT
			MAX(CASE
				WHEN column_name = 'order_id' THEN is_nullable
				ELSE ''
			END) AS order_id_nullable,
			(
				SELECT COUNT(*)
				FROM information_schema.table_constraints
				WHERE constraint_schema = DATABASE()
				  AND constraint_name = 'chk_refund_business_link'
				  AND constraint_type = 'CHECK'
			) AS constraint_count
		FROM information_schema.columns
		WHERE table_schema = DATABASE()
		  AND table_name = 'refunds'
		  AND column_name = 'order_id'
	`).Scan(&facts).Error; err != nil {
		t.Fatalf("inspect wine-ticket money Contract schema: %v", err)
	}
	if facts.OrderIDNullable != "YES" || facts.ConstraintCount != 1 {
		t.Skip(
			"refund duplicate-SUCCESS acceptance requires the isolated " +
				"wine-ticket Contract schema",
		)
	}
}

func runConcurrentRefundSuccess(
	concurrency int,
	action func() error,
) []error {
	start := make(chan struct{})
	results := make([]error, concurrency)
	var wait sync.WaitGroup
	wait.Add(concurrency)
	for index := 0; index < concurrency; index++ {
		go func(index int) {
			defer wait.Done()
			<-start
			results[index] = action()
		}(index)
	}
	close(start)
	wait.Wait()
	return results
}

func seedMySQLRefundSuccessFixture(
	t *testing.T,
	db *gorm.DB,
	ids *snowflake.Generator,
	now time.Time,
) mysqlRefundFixture {
	t.Helper()
	purchaseID := ids.Next()
	customerID := ids.Next()
	paymentID := ids.Next()
	lotID := ids.Next()
	businessID := ids.Next()
	commonID := ids.Next()
	quantity := uint(6)
	amount := int64(1000)
	paidAt := now.Add(-time.Hour)
	issuedAt := paidAt.Add(time.Second)
	expiresAt := paidAt.AddDate(0, 0, 365)
	paymentBizType := PurchasePaymentBusiness
	paymentBizID := purchaseID

	purchase := purchasedomain.Purchase{
		ID: purchaseID, PurchaseNo: "MYSQL-WTPU-" + idString(purchaseID),
		CustomerID: customerID, PackageID: ids.Next(), PackageVersion: 1,
		PaymentID: paymentID, IssuerMerchantID: 1, SettlementShopID: 2,
		SettlementShopProductID: 3, ProductID: 4,
		RedeemCityCode: "310100", PackageQuantity: 1,
		BottleQuantityPerPackage: quantity, TotalBottleQuantity: quantity,
		UnitPriceAmount: amount, PayableAmount: amount, PaidAmount: amount,
		Currency: "CNY", PackageSnapshot: datatypes.JSON(`{"schema_version":1}`),
		RefundPolicySnapshot:  datatypes.JSON(`{"schema_version":1}`),
		RenewalPolicySnapshot: datatypes.JSON(`{"schema_version":1}`),
		Status:                PurchaseStatusRefundHolding, Version: 4,
		PaidAt: &paidAt, IssuedAt: &issuedAt,
		CreatedAt: now, UpdatedAt: now,
	}
	payment := order.Payment{
		ID: paymentID, PaymentNo: "MYSQL-PAY-" + idString(paymentID),
		BizType: &paymentBizType, BizID: &paymentBizID,
		CustomerID: customerID, Channel: "wechat_miniapp",
		Provider: "wechat", ProviderStatus: mysqlRefundStringPtr("SUCCESS"),
		Status: "succeeded", Amount: amount, Currency: "CNY",
		ClientPayload: datatypes.JSON(`{}`), PaidAt: &paidAt,
		RefundedAmount: 0, Version: 7, CreatedAt: now, UpdatedAt: now,
	}
	lot := core.Lot{
		ID: lotID, LotNo: "MYSQL-WTL-" + idString(lotID),
		OwnerCustomerID: customerID, PurchaseID: purchaseID,
		SourceType: LotSourcePurchase, IssuerMerchantID: 1, ProductID: 4,
		RedeemCityCode: "310100", TotalQuantity: quantity,
		AvailableQuantity: 0, OriginalExpiresAt: expiresAt,
		ExpiresAt: expiresAt, ExpiryChangedAt: issuedAt,
		Status: LotStatusDepleted, Version: 2,
		CreatedAt: issuedAt, UpdatedAt: now,
	}
	refundBizType := WineTicketPurchaseRefundBusiness
	refundBizID := businessID
	business := WineTicketRefund{
		ID:                 businessID,
		WineTicketRefundNo: "MYSQL-WTRF-" + idString(businessID),
		PurchaseID:         purchaseID, CustomerID: customerID,
		CurrentRefundID: commonID, RefundKind: RefundKindUserUnused,
		Amount: amount, Currency: "CNY", ReasonCode: "duplicate_purchase",
		EligibilitySnapshot: datatypes.JSON(`{"schema_version":1}`),
		Status:              RefundStatusProcessing, Version: 3,
		RequestedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	common := commonRefundRow{
		ID: commonID, PaymentID: paymentID,
		RefundNo: "MYSQL-WTRFC-" + idString(commonID),
		BizType:  &refundBizType, BizID: &refundBizID,
		Provider: "wechat", Status: "pending", Currency: "CNY",
		Reason: "MySQL duplicate SUCCESS acceptance",
		Amount: amount, TotalAmount: amount, RequestedAt: now,
		Version: 5, CreatedAt: now, UpdatedAt: now,
	}
	allocation := RefundAllocation{
		ID: ids.Next(), WineTicketRefundID: businessID, LotID: lotID,
		Quantity: quantity, SourceExpiresAt: expiresAt,
		Status: RefundAllocationHeld, CreatedAt: now, UpdatedAt: now,
	}
	transactions := []core.Transaction{
		{
			ID: ids.Next(), TransactionNo: "MYSQL-WTT-" + idString(ids.Next()),
			LotID: lotID, OwnerCustomerID: customerID,
			TransactionType: TransactionTypePurchaseIssue,
			QuantityDelta:   int(quantity), BeforeAvailableQuantity: 0,
			AfterAvailableQuantity: quantity, BizType: "purchase",
			BizID: purchaseID,
			ActionKey: "purchase_issue:" + idString(purchaseID) + ":" +
				idString(lotID),
			CreatedAt: issuedAt,
		},
		{
			ID: ids.Next(), TransactionNo: "MYSQL-WTT-" + idString(ids.Next()),
			LotID: lotID, OwnerCustomerID: customerID,
			TransactionType:         TransactionTypeRefundHold,
			QuantityDelta:           -int(quantity),
			BeforeAvailableQuantity: quantity, AfterAvailableQuantity: 0,
			BizType: "refund", BizID: businessID,
			ActionKey: "refund_hold:" + idString(businessID) + ":" +
				idString(lotID),
			CreatedAt: now,
		},
	}

	rows := []struct {
		name string
		row  any
	}{
		{name: "payment", row: &payment},
		{name: "purchase", row: &purchase},
		{name: "lot", row: &lot},
		{name: "issue/hold", row: &transactions},
		{name: "business", row: &business},
		{name: "common", row: &common},
		{name: "allocation", row: &allocation},
	}
	for _, item := range rows {
		if err := db.Create(item.row).Error; err != nil {
			t.Fatalf("seed mysql refund %s: %v", item.name, err)
		}
	}
	return mysqlRefundFixture{
		purchase: purchase, payment: payment, lot: lot,
		business: business, common: common, allocation: allocation,
	}
}

func mysqlRefundStringPtr(value string) *string { return &value }

func cleanupMySQLRefundSuccessFixture(
	t *testing.T,
	db *gorm.DB,
	fixture mysqlRefundFixture,
) {
	t.Helper()
	cleanup := []struct {
		name  string
		query *gorm.DB
	}{
		{
			name: "outbox",
			query: db.Where(
				"aggregate_type = ? AND aggregate_id = ?",
				"wine_ticket_refund",
				fixture.business.ID,
			).Delete(&refundTestOutbox{}),
		},
		{
			name: "audit",
			query: db.Where(
				"resource_type = ? AND resource_id = ?",
				"wine_ticket_purchase",
				fixture.purchase.ID,
			).Delete(&refundTestAudit{}),
		},
		{
			name: "allocation",
			query: db.Where(
				"wine_ticket_refund_id = ?",
				fixture.business.ID,
			).Delete(&RefundAllocation{}),
		},
		{
			name:  "business",
			query: db.Where("id = ?", fixture.business.ID).Delete(&WineTicketRefund{}),
		},
		{
			name: "common",
			query: db.Where(
				"biz_type = ? AND biz_id = ?",
				WineTicketPurchaseRefundBusiness,
				fixture.business.ID,
			).Delete(&commonRefundRow{}),
		},
		{
			name:  "transactions",
			query: db.Where("lot_id = ?", fixture.lot.ID).Delete(&core.Transaction{}),
		},
		{
			name:  "lot",
			query: db.Where("id = ?", fixture.lot.ID).Delete(&core.Lot{}),
		},
		{
			name: "purchase",
			query: db.Where("id = ?", fixture.purchase.ID).
				Delete(&purchasedomain.Purchase{}),
		},
		{
			name:  "payment",
			query: db.Where("id = ?", fixture.payment.ID).Delete(&order.Payment{}),
		},
	}
	for _, item := range cleanup {
		if item.query.Error != nil {
			t.Errorf("cleanup mysql refund %s: %v", item.name, item.query.Error)
		}
	}
}
