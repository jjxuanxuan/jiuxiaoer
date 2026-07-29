package order

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
)

func TestGetShopProductForOrderCombinesProductAndCategoryRestrictions(t *testing.T) {
	dsn := uniqueSQLiteMemoryDSN(t)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE categories (id INTEGER PRIMARY KEY, status TEXT NOT NULL, age_restricted BOOLEAN NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE products (id INTEGER PRIMARY KEY, category_id INTEGER NOT NULL, name TEXT NOT NULL, brand_name TEXT, spec TEXT, image_url TEXT, return_eligible BOOLEAN NOT NULL, return_policy_code TEXT NOT NULL, return_policy_version TEXT NOT NULL, sealed_package_required BOOLEAN NOT NULL, age_restricted BOOLEAN NOT NULL, status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE shops (id INTEGER PRIMARY KEY, status TEXT NOT NULL, business_status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE shop_products (id INTEGER PRIMARY KEY, shop_id INTEGER NOT NULL, merchant_id INTEGER NOT NULL, product_id INTEGER NOT NULL, sale_price_amount INTEGER NOT NULL, status TEXT NOT NULL, deleted_at DATETIME)`,
		`INSERT INTO categories (id, status, age_restricted) VALUES (1, 'active', 1)`,
		`INSERT INTO products (id, category_id, name, return_eligible, return_policy_code, return_policy_version, sealed_package_required, age_restricted, status) VALUES (2, 1, 'category restricted wine', 1, 'sealed', 'v1', 1, 0, 'on_sale')`,
		`INSERT INTO shops (id, status, business_status) VALUES (3, 'active', 'open')`,
		`INSERT INTO shop_products (id, shop_id, merchant_id, product_id, sale_price_amount, status) VALUES (4, 3, 5, 2, 9900, 'on_sale')`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("prepare order repository fixture: %v", err)
		}
	}

	row, err := NewRepository(db).GetShopProductForOrder(context.Background(), db, 3, 4)
	if err != nil {
		t.Fatalf("get shop product for order: %v", err)
	}
	if !row.AgeRestricted || row.CategoryStatus != "active" {
		t.Fatalf("expected category restriction and status in order row, got %#v", row)
	}
}

func TestCustomerOrderKeysetDoesNotSkipWhenEarlierRowsChange(t *testing.T) {
	dsn := uniqueSQLiteMemoryDSN(t)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE orders (
		id INTEGER PRIMARY KEY,
		customer_id INTEGER NOT NULL,
		order_type TEXT NOT NULL,
		settlement_mode TEXT NOT NULL,
		status TEXT,
		created_at DATETIME NOT NULL,
		deleted_at DATETIME
	)`).Error; err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	for id, createdAt := range map[uint64]time.Time{1: base, 2: base.Add(time.Minute), 3: base.Add(2 * time.Minute)} {
		if err := db.Exec(
			`INSERT INTO orders (id,customer_id,order_type,settlement_mode,status,created_at)
			 VALUES (?,?,?,?,?,?)`,
			id,
			10,
			retailOrderType,
			cashSettlementMode,
			"paid",
			createdAt,
		).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Exec(
		`INSERT INTO orders (id,customer_id,order_type,settlement_mode,status,created_at)
		 VALUES (?,?,?,?,?,?)`,
		9,
		10,
		"wine_ticket_redemption",
		"wine_ticket",
		"paid",
		base.Add(90*time.Second),
	).Error; err != nil {
		t.Fatal(err)
	}
	repo := NewRepository(db)
	first, err := repo.ListCustomerOrders(context.Background(), 10, CustomerOrderListFilters{}, pagination.Query{PageSize: 2})
	if err != nil || len(first) != 3 || first[0].ID != 3 || first[1].ID != 2 {
		t.Fatalf("unexpected first page rows: %#v err=%v", first, err)
	}
	if _, _, err := repo.GetCustomerOrder(context.Background(), 10, 9); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("wine-ticket order leaked through retail detail: %v", err)
	}
	if _, err := repo.LockCustomerOrder(context.Background(), db, 10, 9); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("wine-ticket order leaked through retail write lock: %v", err)
	}
	cursor := pagination.Query{PageSize: 2, Offset: 2, Cursor: []string{first[1].CreatedAt.UTC().Format(time.RFC3339Nano), idString(first[1].ID)}}
	if err := db.Exec(`DELETE FROM orders WHERE id=3`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(
		`INSERT INTO orders (id,customer_id,order_type,settlement_mode,status,created_at)
		 VALUES (4,10,?,?,?,?)`,
		retailOrderType,
		cashSettlementMode,
		"paid",
		base.Add(3*time.Minute),
	).Error; err != nil {
		t.Fatal(err)
	}
	second, err := repo.ListCustomerOrders(context.Background(), 10, CustomerOrderListFilters{}, cursor)
	if err != nil || len(second) != 1 || second[0].ID != 1 {
		t.Fatalf("keyset page skipped or duplicated rows after changes: %#v err=%v", second, err)
	}
}

func TestCustomerPhoneBoundRequiresNonEmptyActiveRow(t *testing.T) {
	dsn := uniqueSQLiteMemoryDSN(t)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE customers (id INTEGER PRIMARY KEY, phone TEXT NOT NULL, deleted_at DATETIME)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO customers (id, phone) VALUES (1, '')`).Error; err != nil {
		t.Fatal(err)
	}
	repo := NewRepository(db)

	bound, err := repo.CustomerPhoneBound(context.Background(), db, 1)
	if err != nil || bound {
		t.Fatalf("empty phone must be unbound: bound=%t err=%v", bound, err)
	}
	if err := db.Exec(`UPDATE customers SET phone = '13800138000' WHERE id = 1`).Error; err != nil {
		t.Fatal(err)
	}
	bound, err = repo.CustomerPhoneBound(context.Background(), db, 1)
	if err != nil || !bound {
		t.Fatalf("valid phone must be bound: bound=%t err=%v", bound, err)
	}
	if err := db.Exec(`UPDATE customers SET deleted_at = ? WHERE id = 1`, time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	bound, err = repo.CustomerPhoneBound(context.Background(), db, 1)
	if err != nil || bound {
		t.Fatalf("deleted customer must be unbound: bound=%t err=%v", bound, err)
	}
}

func TestClaimNextReconcilablePaymentOnlyRetriesScheduledExceptions(t *testing.T) {
	dsn := uniqueSQLiteMemoryDSN(t)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Order{}, &Payment{}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`ALTER TABLE orders ADD COLUMN deleted_at DATETIME`,
		`ALTER TABLE payments ADD COLUMN deleted_at DATETIME`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}

	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute)
	bizType := "wine_ticket_purchase"
	bizID := uint64(101)
	rows := []Payment{
		{
			ID: 1, PaymentNo: "PAY-SCHEDULED", BizType: &bizType, BizID: &bizID,
			CustomerID: 1, Provider: "wechat", Status: "exception",
			ProviderStatus: stringPointer("SUCCESS"), Amount: 100, Currency: "CNY",
			NextReconcileAt: &past, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
		},
		{
			ID: 2, PaymentNo: "PAY-UNSCHEDULED", BizType: &bizType, BizID: &bizID,
			CustomerID: 1, Provider: "wechat", Status: "exception",
			ProviderStatus: stringPointer("SUCCESS"), Amount: 100, Currency: "CNY",
			CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour),
		},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}

	claimed, err := NewRepository(db).ClaimNextReconcilablePayment(
		context.Background(), "wechat", now, now.Add(-5*time.Minute),
	)
	if err != nil {
		t.Fatalf("claim scheduled exception: %v", err)
	}
	if claimed.ID != 1 || claimed.ReconcileAttempts != 1 ||
		claimed.NextReconcileAt == nil || !claimed.NextReconcileAt.After(now) {
		t.Fatalf("unexpected claimed payment: %+v", claimed)
	}

	_, err = NewRepository(db).ClaimNextReconcilablePayment(
		context.Background(), "wechat", now, now.Add(-5*time.Minute),
	)
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("unscheduled exception must not be claimed: %v", err)
	}
}

func stringPointer(value string) *string { return &value }
