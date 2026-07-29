package redemption

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type mysqlRedemptionConcurrencyFixture struct {
	customerID    uint64
	addressID     uint64
	merchantID    uint64
	shopID        uint64
	categoryID    uint64
	productID     uint64
	shopProductID uint64
	stockID       uint64
	slotID        uint64
	lotID         uint64
	now           time.Time
}

func seedMySQLRedemptionConcurrencyFixture(
	t *testing.T,
	db *gorm.DB,
	ids *snowflake.Generator,
	quantity uint,
	capacity uint,
) mysqlRedemptionConcurrencyFixture {
	t.Helper()
	now := time.Now().In(shanghaiLocation).Truncate(time.Millisecond)
	fixture := mysqlRedemptionConcurrencyFixture{
		customerID: ids.Next(), addressID: ids.Next(), merchantID: ids.Next(),
		shopID: ids.Next(), categoryID: ids.Next(), productID: ids.Next(),
		shopProductID: ids.Next(), stockID: ids.Next(), slotID: ids.Next(),
		lotID: ids.Next(), now: now,
	}
	mustCreate := func(table string, values map[string]any) {
		t.Helper()
		if err := db.Table(table).Create(values).Error; err != nil {
			t.Fatalf("seed mysql redemption %s: %v", table, err)
		}
	}
	mustCreate("customers", map[string]any{
		"id": fixture.customerID, "account_id": ids.Next(),
		"nickname": "MySQL 提酒用户",
		"phone":    "13" + strconv.FormatUint(fixture.customerID%1_000_000_000, 10),
		"status":   "active",
	})
	mustCreate("customer_realname_verifications", map[string]any{
		"customer_id": fixture.customerID, "request_id": ids.Next(),
		"status": "verified", "provider": "mysql_acceptance",
		"masked_name": "R**", "masked_document_no": "3***************1",
		"adult_result": "adult", "verified_at": now,
		"expires_at": now.AddDate(1, 0, 0), "version": 1,
	})
	mustCreate("customer_addresses", map[string]any{
		"id": fixture.addressID, "customer_id": fixture.customerID,
		"contact_name": "验收用户", "contact_phone": "13800000000",
		"province": "上海市", "city": "上海市", "city_code": "310100",
		"district": "浦东新区", "address_detail": "验收路 1 号",
		"is_default": false, "version": 1, "coordinate_system": "gcj02",
		"location_source": "legacy", "geocode_status": "verified",
	})
	mustCreate("merchants", map[string]any{
		"id":   fixture.merchantID,
		"code": "MYSQL-MERCHANT-" + idString(fixture.merchantID),
		"name": "MySQL P0 发行商", "status": "active", "review_status": "approved",
	})
	mustCreate("shops", map[string]any{
		"id": fixture.shopID, "merchant_id": fixture.merchantID,
		"name": "MySQL P0 履约店", "city": "上海市", "city_code": "310100",
		"district": "浦东新区", "address": "验收路 2 号",
		"status": "active", "business_status": "open",
	})
	mustCreate("categories", map[string]any{
		"id": fixture.categoryID, "name": "MySQL P0 酒类",
		"status": "active", "age_restricted": true,
	})
	mustCreate("products", map[string]any{
		"id": fixture.productID, "category_id": fixture.categoryID,
		"name": "MySQL P0 提酒商品", "status": "on_sale",
		"age_restricted": true,
	})
	mustCreate("shop_products", map[string]any{
		"id": fixture.shopProductID, "merchant_id": fixture.merchantID,
		"shop_id": fixture.shopID, "product_id": fixture.productID,
		"status": "on_sale",
	})
	mustCreate("product_stocks", map[string]any{
		"id": fixture.stockID, "shop_product_id": fixture.shopProductID,
		"shop_id": fixture.shopID, "product_id": fixture.productID,
		"available_qty": int(quantity), "reserved_qty": 0, "locked_qty": 0,
		"version": 1,
	})
	slotStart := time.Date(
		now.Year(),
		now.Month(),
		now.Day(),
		14,
		0,
		0,
		0,
		shanghaiLocation,
	).AddDate(0, 0, 2)
	slot := DeliveryTimeSlot{
		ID: fixture.slotID, ShopID: fixture.shopID, ServiceDate: slotStart,
		StartTime: "14:00:00", EndTime: "16:00:00",
		CutoffAt:       slotStart.Add(-2 * time.Hour),
		CapacityOrders: capacity, Status: "open", Version: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&slot).Error; err != nil {
		t.Fatalf("seed mysql redemption slot: %v", err)
	}
	expiresAt := slotStart.AddDate(1, 0, 0)
	lot := core.Lot{
		ID: fixture.lotID, LotNo: "MYSQL-REDEEM-LOT-" + idString(fixture.lotID),
		OwnerCustomerID: fixture.customerID, PurchaseID: ids.Next(),
		SourceType: LotSourcePurchase, IssuerMerchantID: fixture.merchantID,
		ProductID: fixture.productID, RedeemCityCode: "310100",
		TotalQuantity: quantity, AvailableQuantity: quantity,
		OriginalExpiresAt: expiresAt, ExpiresAt: expiresAt,
		ExpiryChangedAt: now, Status: LotStatusActive, Version: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&lot).Error; err != nil {
		t.Fatalf("seed mysql redemption lot: %v", err)
	}
	return fixture
}

func cleanupMySQLRedemptionConcurrencyFixture(
	t *testing.T,
	db *gorm.DB,
	fixture mysqlRedemptionConcurrencyFixture,
) {
	t.Helper()
	var redemptionIDs, orderIDs []uint64
	if err := db.Model(&Redemption{}).Where(
		"customer_id = ? AND delivery_time_slot_id = ?",
		fixture.customerID,
		fixture.slotID,
	).Pluck("id", &redemptionIDs).Error; err != nil {
		t.Errorf("find redemptions for cleanup: %v", err)
	}
	if err := db.Model(&Redemption{}).Where("id IN ?", redemptionIDs).
		Pluck("order_id", &orderIDs).Error; err != nil {
		t.Errorf("find redemption orders for cleanup: %v", err)
	}
	steps := []struct {
		name  string
		query *gorm.DB
	}{
		{
			name: "outbox",
			query: db.Where(
				"aggregate_type = ? AND aggregate_id IN ?",
				"wine_ticket_redemption",
				redemptionIDs,
			).Delete(&redemptionTestOutbox{}),
		},
		{
			name:  "audit",
			query: db.Where("actor_type = ? AND actor_id = ?", "customer", fixture.customerID).Delete(&redemptionTestAudit{}),
		},
		{
			name:  "idempotency",
			query: db.Where("actor_type = ? AND actor_id = ?", "customer", fixture.customerID).Delete(&idempotency.Record{}),
		},
		{
			name:  "delivery",
			query: db.Where("order_id IN ?", orderIDs).Delete(&redemptionTestDelivery{}),
		},
		{
			name:  "order logs",
			query: db.Where("order_id IN ?", orderIDs).Delete(&redemptionTestOrderLog{}),
		},
		{
			name:  "order items",
			query: db.Where("order_id IN ?", orderIDs).Delete(&redemptionTestOrderItem{}),
		},
		{
			name:  "orders",
			query: db.Where("id IN ?", orderIDs).Delete(&redemptionTestOrder{}),
		},
		{
			name:  "stock records",
			query: db.Where("shop_product_id = ?", fixture.shopProductID).Delete(&redemptionTestStockRecord{}),
		},
		{
			name:  "transactions",
			query: db.Where("owner_customer_id = ?", fixture.customerID).Delete(&core.Transaction{}),
		},
		{
			name:  "allocations",
			query: db.Where("redemption_id IN ?", redemptionIDs).Delete(&RedemptionAllocation{}),
		},
		{
			name:  "redemptions",
			query: db.Where("id IN ?", redemptionIDs).Delete(&Redemption{}),
		},
		{
			name:  "lot",
			query: db.Where("id = ?", fixture.lotID).Delete(&core.Lot{}),
		},
		{
			name:  "slot",
			query: db.Where("id = ?", fixture.slotID).Delete(&DeliveryTimeSlot{}),
		},
		{
			name:  "stock",
			query: db.Where("id = ?", fixture.stockID).Delete(&PhysicalStock{}),
		},
		{
			name:  "shop product",
			query: db.Table("shop_products").Where("id = ?", fixture.shopProductID).Delete(nil),
		},
		{
			name:  "product",
			query: db.Table("products").Where("id = ?", fixture.productID).Delete(nil),
		},
		{
			name:  "category",
			query: db.Table("categories").Where("id = ?", fixture.categoryID).Delete(nil),
		},
		{
			name:  "address",
			query: db.Table("customer_addresses").Where("id = ?", fixture.addressID).Delete(nil),
		},
		{
			name:  "realname",
			query: db.Table("customer_realname_verifications").Where("customer_id = ?", fixture.customerID).Delete(nil),
		},
		{
			name:  "customer",
			query: db.Table("customers").Where("id = ?", fixture.customerID).Delete(nil),
		},
		{
			name:  "shop",
			query: db.Table("shops").Where("id = ?", fixture.shopID).Delete(nil),
		},
		{
			name:  "merchant",
			query: db.Table("merchants").Where("id = ?", fixture.merchantID).Delete(nil),
		},
	}
	for _, step := range steps {
		if step.query.Error != nil {
			t.Errorf("cleanup mysql redemption %s: %v", step.name, step.query.Error)
		}
	}
}

func runMySQLRedemptionConcurrent100(
	t *testing.T,
	ctx context.Context,
	db *gorm.DB,
	fixture mysqlRedemptionConcurrencyFixture,
	wantSuccesses int,
) {
	t.Helper()
	service := NewRedemptionService(db, snowflake.New(945)).
		WithDispatch(&redemptionTestDispatch{}).
		WithNow(func() time.Time { return fixture.now })
	claims := &auth.Claims{
		AccountType: "customer",
		CustomerID:  idString(fixture.customerID),
		Permissions: []string{"wine_ticket_redemption:create"},
	}
	request := RedemptionCreateRequest{
		ProductID: idString(fixture.productID), Quantity: 1,
		AddressID: idString(fixture.addressID), AddressVersion: 1,
		DeliveryTimeSlotID: idString(fixture.slotID),
	}

	results := runMySQLConcurrentErrors(
		mysqlP0Concurrency,
		func(index int) error {
			_, err := service.Create(
				ctx,
				claims,
				http.MethodPost,
				"/api/v1/wine-tickets/redemptions",
				fmt.Sprintf("mysql-redemption-%d-%03d", fixture.slotID, index),
				request,
			)
			return err
		},
	)

	successes := 0
	expectedRejects := 0
	for _, resultErr := range results {
		if resultErr == nil {
			successes++
			continue
		}
		switch problem.FromError(resultErr).ErrorCode {
		case "WT_INSUFFICIENT_QUANTITY", "WT_SLOT_FULL":
			expectedRejects++
		default:
			t.Fatalf("unexpected redemption concurrency error: %v", resultErr)
		}
	}
	if successes != wantSuccesses ||
		expectedRejects != mysqlP0Concurrency-wantSuccesses {
		t.Fatalf(
			"redemption results success=%d rejected=%d want=%d/%d",
			successes,
			expectedRejects,
			wantSuccesses,
			mysqlP0Concurrency-wantSuccesses,
		)
	}
}

// AC-WT-010：100 个请求竞争最后一瓶时，只产生一次冻结。
func TestMySQLRedemptionConcurrent100LastBottleNoOverRedemption(t *testing.T) {
	ctx, db := openWineTicketMySQLAcceptance(t, 60*time.Second)
	fixture := seedMySQLRedemptionConcurrencyFixture(
		t,
		db,
		snowflake.New(944),
		1,
		mysqlP0Concurrency,
	)
	t.Cleanup(func() { cleanupMySQLRedemptionConcurrencyFixture(t, db, fixture) })
	runMySQLRedemptionConcurrent100(t, ctx, db, fixture, 1)

	var lot core.Lot
	if err := db.First(&lot, fixture.lotID).Error; err != nil {
		t.Fatal(err)
	}
	var slot DeliveryTimeSlot
	if err := db.First(&slot, fixture.slotID).Error; err != nil {
		t.Fatal(err)
	}
	var stock PhysicalStock
	if err := db.First(&stock, fixture.stockID).Error; err != nil {
		t.Fatal(err)
	}
	var allocations int64
	if err := db.Model(&RedemptionAllocation{}).Where("lot_id = ?", fixture.lotID).
		Count(&allocations).Error; err != nil {
		t.Fatal(err)
	}
	if lot.AvailableQuantity != 0 || lot.Status != LotStatusDepleted ||
		slot.ReservedOrders != 1 || stock.AvailableQty != 0 || allocations != 1 {
		t.Fatalf(
			"last-bottle invariants lot=%+v slot=%+v stock=%+v allocations=%d",
			lot,
			slot,
			stock,
			allocations,
		)
	}
}

// AC-WT-011：容量为 N 时，100 个并发预约中恰好有 N 个提交成功。
func TestMySQLRedemptionConcurrent100HonorsExactSlotCapacity(t *testing.T) {
	ctx, db := openWineTicketMySQLAcceptance(t, 60*time.Second)
	const capacity = 7
	fixture := seedMySQLRedemptionConcurrencyFixture(
		t,
		db,
		snowflake.New(946),
		mysqlP0Concurrency,
		capacity,
	)
	t.Cleanup(func() { cleanupMySQLRedemptionConcurrencyFixture(t, db, fixture) })
	runMySQLRedemptionConcurrent100(t, ctx, db, fixture, capacity)

	var lot core.Lot
	if err := db.First(&lot, fixture.lotID).Error; err != nil {
		t.Fatal(err)
	}
	var slot DeliveryTimeSlot
	if err := db.First(&slot, fixture.slotID).Error; err != nil {
		t.Fatal(err)
	}
	var stock PhysicalStock
	if err := db.First(&stock, fixture.stockID).Error; err != nil {
		t.Fatal(err)
	}
	var redemptions int64
	if err := db.Model(&Redemption{}).Where(
		"customer_id = ? AND delivery_time_slot_id = ?",
		fixture.customerID,
		fixture.slotID,
	).Count(&redemptions).Error; err != nil {
		t.Fatal(err)
	}
	if slot.ReservedOrders != capacity ||
		lot.AvailableQuantity != mysqlP0Concurrency-capacity ||
		stock.AvailableQty != mysqlP0Concurrency-capacity ||
		redemptions != capacity {
		t.Fatalf(
			"slot capacity invariants slot=%+v lot=%+v stock=%+v redemptions=%d",
			slot,
			lot,
			stock,
			redemptions,
		)
	}
}
