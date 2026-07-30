package cart

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/customerlocation"
	"jiuxiaoer-admin/backend-go/internal/modules/servicearea"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

type repurchaseLocationStub struct {
	value     customerlocation.LocationContext
	err       error
	actor     customerlocation.Actor
	contextID string
}

func (s *repurchaseLocationStub) GetContext(_ context.Context, actor customerlocation.Actor, contextID string) (customerlocation.LocationContext, error) {
	s.actor, s.contextID = actor, contextID
	return s.value, s.err
}

func TestListFrequentPurchasesAggregatesHistoryAndUsesCurrentShopFacts(t *testing.T) {
	db := repurchaseSQLite(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO categories (id, status) VALUES (1, 'active')`, nil},
		{`INSERT INTO shops (id, status, business_status) VALUES (50, 'active', 'open')`, nil},
		{`INSERT INTO products (id, category_id, name, status) VALUES (10, 1, '常购啤酒', 'on_sale'), (11, 1, '历史商品', 'on_sale')`, nil},
		{`INSERT INTO shop_products (id, shop_id, product_id, sale_price_amount, status) VALUES (100, 50, 10, 1500, 'on_sale')`, nil},
		{`INSERT INTO product_stocks (shop_product_id, available_qty) VALUES (100, 3)`, nil},
		{`INSERT INTO orders (id, customer_id, status, completed_at) VALUES
			(1, 900, 'completed', ?),
			(2, 900, 'completed', ?),
			(3, 900, 'completed', ?),
			(4, 900, 'pending_payment', ?),
			(5, 901, 'completed', ?)`,
			[]any{now.AddDate(0, 0, -10), now.AddDate(0, 0, -2), now.AddDate(0, 0, -200), now.AddDate(0, 0, -1), now.AddDate(0, 0, -1)}},
		{`INSERT INTO order_items (id, order_id, product_id, quantity, sale_price_amount) VALUES
			(101, 1, 10, 2, 1000),
			(102, 2, 10, 4, 1200),
			(103, 2, 11, 1, 800),
			(104, 3, 10, 20, 700),
			(105, 4, 10, 30, 600),
			(106, 5, 10, 40, 500)`, nil},
	}
	for _, statement := range statements {
		if err := db.Exec(statement.query, statement.args...).Error; err != nil {
			t.Fatalf("prepare repurchase fixture: %v", err)
		}
	}

	location := &repurchaseLocationStub{value: customerlocation.LocationContext{
		LocationLevel:  "exact",
		Serviceability: customerlocation.ServiceabilityDTO{Serviceable: true},
		ServiceShop:    &servicearea.ShopDTO{ID: "50", Selectable: true},
	}}
	service := NewService(db, nil).WithRepurchase(config.RepurchaseConfig{
		Enabled: true, LookbackDays: 180, MaxItems: 20,
	}, location)
	resp, err := service.ListFrequentPurchases(context.Background(), &auth.Claims{
		AccountType: "customer",
		CustomerID:  "900",
		Permissions: []string{"order:list"},
	}, "loc_01234567890123456789012345678901")
	if err != nil {
		t.Fatalf("list frequent purchases: %v", err)
	}
	if location.actor.Type != "customer" || location.actor.ID != "900" {
		t.Fatalf("location context was not customer-bound: %#v", location.actor)
	}
	if resp.ShopID != "50" || resp.LookbackDays != 180 || len(resp.Items) != 2 {
		t.Fatalf("unexpected frequent purchase response: %#v", resp)
	}
	first := resp.Items[0]
	if first.ProductID != "10" || first.ShopProductID != "100" || first.PurchaseCount != 2 ||
		first.PurchasedQuantity != 6 || first.LastQuantity != 4 || first.RecommendedQuantity != 3 ||
		first.LastSalePriceAmount != 1200 || first.SalePriceAmount != 1500 ||
		!first.Available || first.AvailabilityStatus != "available" {
		t.Fatalf("unexpected primary frequent item: %#v", first)
	}
	second := resp.Items[1]
	if second.ProductID != "11" || second.ShopProductID != "" || second.Available ||
		second.AvailabilityStatus != "not_sold_by_shop" || second.UnavailableReason == nil ||
		*second.UnavailableReason != "not_sold_by_shop" {
		t.Fatalf("unexpected unavailable frequent item: %#v", second)
	}
}

func TestRepurchaseValidationAndLocationFailClosed(t *testing.T) {
	if _, err := parseRepurchaseItems([]RepurchaseItemReq{
		{ProductID: "10", Quantity: 1},
		{ProductID: "010", Quantity: 2},
	}, 20); problem.FromError(err).ErrorCode != "REPURCHASE_DUPLICATE_PRODUCT" {
		t.Fatalf("duplicate numeric product ids must fail: %v", err)
	}
	if _, err := parseRepurchaseItems([]RepurchaseItemReq{{ProductID: "10", Quantity: 100}}, 20); problem.FromError(err).ErrorCode != "REPURCHASE_ITEM_INVALID" {
		t.Fatalf("quantity above cart limit must fail: %v", err)
	}

	service := NewService(nil, nil)
	if _, err := service.resolveRepurchaseShop(context.Background(), 9, ""); problem.FromError(err).ErrorCode != "LOCATION_CONTEXT_REQUIRED" {
		t.Fatalf("missing location context must fail: %v", err)
	}
	service.locations = &repurchaseLocationStub{value: customerlocation.LocationContext{
		LocationLevel:  "city",
		Serviceability: customerlocation.ServiceabilityDTO{Serviceable: false},
	}}
	if _, err := service.resolveRepurchaseShop(context.Background(), 9, "loc_01234567890123456789012345678901"); problem.FromError(err).ErrorCode != "PRECISE_LOCATION_REQUIRED" {
		t.Fatalf("city-only location must fail: %v", err)
	}
	service.locations = &repurchaseLocationStub{err: errors.New("store failed")}
	if _, err := service.resolveRepurchaseShop(context.Background(), 9, "loc_01234567890123456789012345678901"); err == nil || err.Error() != "store failed" {
		t.Fatalf("location store error must be preserved: %v", err)
	}
}

func repurchaseSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, customer_id INTEGER NOT NULL, status TEXT NOT NULL, completed_at DATETIME, deleted_at DATETIME)`,
		`CREATE TABLE order_items (id INTEGER PRIMARY KEY, order_id INTEGER NOT NULL, product_id INTEGER NOT NULL, quantity INTEGER NOT NULL, sale_price_amount INTEGER NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE products (id INTEGER PRIMARY KEY, category_id INTEGER NOT NULL, name TEXT NOT NULL, brand_name TEXT, spec TEXT, image_url TEXT, status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE categories (id INTEGER PRIMARY KEY, status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE shops (id INTEGER PRIMARY KEY, status TEXT NOT NULL, business_status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE shop_products (id INTEGER PRIMARY KEY, shop_id INTEGER NOT NULL, product_id INTEGER NOT NULL, sale_price_amount INTEGER NOT NULL, status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE product_stocks (shop_product_id INTEGER NOT NULL, available_qty INTEGER NOT NULL, deleted_at DATETIME)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("create repurchase table: %v", err)
		}
	}
	return db
}
