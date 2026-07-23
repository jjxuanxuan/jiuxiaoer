package cart

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestListItemsUsesDistinctCartAndCategoryAliases(t *testing.T) {
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE carts (id INTEGER PRIMARY KEY, customer_id INTEGER NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE cart_items (id INTEGER PRIMARY KEY, cart_id INTEGER NOT NULL, shop_product_id INTEGER NOT NULL, shop_id INTEGER NOT NULL, product_id INTEGER NOT NULL, quantity INTEGER NOT NULL, selected BOOLEAN NOT NULL, updated_at DATETIME, deleted_at DATETIME)`,
		`CREATE TABLE shop_products (id INTEGER PRIMARY KEY, shop_id INTEGER NOT NULL, product_id INTEGER NOT NULL, sale_price_amount INTEGER NOT NULL, status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE products (id INTEGER PRIMARY KEY, category_id INTEGER NOT NULL, name TEXT, brand_name TEXT, spec TEXT, image_url TEXT, status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE categories (id INTEGER PRIMARY KEY, status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE shops (id INTEGER PRIMARY KEY, status TEXT NOT NULL, business_status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE product_stocks (shop_product_id INTEGER NOT NULL, available_qty INTEGER NOT NULL, deleted_at DATETIME)`,
		`INSERT INTO carts (id, customer_id) VALUES (1, 100)`,
		`INSERT INTO categories (id, status) VALUES (2, 'active')`,
		`INSERT INTO products (id, category_id, name, status) VALUES (3, 2, 'test product', 'on_sale')`,
		`INSERT INTO shops (id, status, business_status) VALUES (4, 'active', 'open')`,
		`INSERT INTO shop_products (id, shop_id, product_id, sale_price_amount, status) VALUES (5, 4, 3, 9900, 'on_sale')`,
		`INSERT INTO product_stocks (shop_product_id, available_qty) VALUES (5, 8)`,
		`INSERT INTO cart_items (id, cart_id, shop_product_id, shop_id, product_id, quantity, selected) VALUES (6, 1, 5, 4, 3, 2, 1)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("prepare cart repository fixture: %v", err)
		}
	}

	rows, err := NewRepository(db).ListItems(context.Background(), db, 100)
	if err != nil {
		t.Fatalf("list cart items: %v", err)
	}
	if len(rows) != 1 || rows[0].CategoryStatus != "active" || rows[0].AvailableQty != 8 {
		t.Fatalf("unexpected cart item rows: %#v", rows)
	}
}

func TestCartRespRetainsSoftDeletedReferencesAndRedactsDeletedPayload(t *testing.T) {
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE carts (id INTEGER PRIMARY KEY, customer_id INTEGER NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE cart_items (id INTEGER PRIMARY KEY, cart_id INTEGER NOT NULL, shop_product_id INTEGER NOT NULL, shop_id INTEGER NOT NULL, product_id INTEGER NOT NULL, quantity INTEGER NOT NULL, selected BOOLEAN NOT NULL, updated_at DATETIME, deleted_at DATETIME)`,
		`CREATE TABLE shop_products (id INTEGER PRIMARY KEY, shop_id INTEGER NOT NULL, product_id INTEGER NOT NULL, sale_price_amount INTEGER NOT NULL, status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE products (id INTEGER PRIMARY KEY, category_id INTEGER NOT NULL, name TEXT, brand_name TEXT, spec TEXT, image_url TEXT, status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE categories (id INTEGER PRIMARY KEY, status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE shops (id INTEGER PRIMARY KEY, status TEXT NOT NULL, business_status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE product_stocks (shop_product_id INTEGER NOT NULL, available_qty INTEGER NOT NULL, deleted_at DATETIME)`,
		`INSERT INTO carts (id, customer_id) VALUES (1, 200)`,
		`INSERT INTO categories (id, status) VALUES (20, 'active')`,
		`INSERT INTO products (id, category_id, name, status) VALUES (21, 20, 'visible product', 'on_sale')`,
		`INSERT INTO products (id, category_id, name, brand_name, spec, image_url, status, deleted_at) VALUES (22, 20, 'secret deleted product', 'secret brand', 'secret spec', 'secret image', 'on_sale', CURRENT_TIMESTAMP)`,
		`INSERT INTO shops (id, status, business_status) VALUES (31, 'active', 'open')`,
		`INSERT INTO shops (id, status, business_status, deleted_at) VALUES (32, 'active', 'open', CURRENT_TIMESTAMP)`,
		`INSERT INTO shop_products (id, shop_id, product_id, sale_price_amount, status) VALUES (41, 31, 22, 9900, 'on_sale')`,
		`INSERT INTO shop_products (id, shop_id, product_id, sale_price_amount, status, deleted_at) VALUES (42, 31, 21, 8800, 'on_sale', CURRENT_TIMESTAMP)`,
		`INSERT INTO shop_products (id, shop_id, product_id, sale_price_amount, status) VALUES (43, 32, 21, 7700, 'on_sale')`,
		`INSERT INTO shop_products (id, shop_id, product_id, sale_price_amount, status) VALUES (44, 31, 21, 6600, 'off_sale')`,
		`INSERT INTO shop_products (id, shop_id, product_id, sale_price_amount, status) VALUES (45, 31, 21, 5500, 'on_sale')`,
		`INSERT INTO shop_products (id, shop_id, product_id, sale_price_amount, status) VALUES (46, 31, 21, 4500, 'on_sale')`,
		`INSERT INTO product_stocks (shop_product_id, available_qty) VALUES (41, 10), (42, 10), (43, 10), (44, 10), (45, 1), (46, 3)`,
		`INSERT INTO cart_items (id, cart_id, shop_product_id, shop_id, product_id, quantity, selected) VALUES
			(51, 1, 41, 31, 22, 1, 1),
			(52, 1, 42, 31, 21, 1, 1),
			(53, 1, 43, 32, 21, 1, 1),
			(54, 1, 44, 31, 21, 1, 1),
			(55, 1, 45, 31, 21, 2, 1),
			(56, 1, 46, 31, 21, 1, 1)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("prepare soft-delete cart fixture: %v", err)
		}
	}

	service := &Service{repo: NewRepository(db)}
	resp, err := service.cartResp(context.Background(), db, 200)
	if err != nil {
		t.Fatalf("read cart with soft-deleted references: %v", err)
	}
	if len(resp.Items) != 6 {
		t.Fatalf("cart item facts disappeared: got %d items, want 6: %#v", len(resp.Items), resp.Items)
	}
	if resp.TotalQuantity != 1 || resp.TotalAmount != 4500 || resp.UnavailableCount != 5 {
		t.Fatalf("unexpected safe cart totals: %#v", resp)
	}

	items := make(map[string]CartItemDTO, len(resp.Items))
	for _, item := range resp.Items {
		items[item.ID] = item
	}
	assertUnavailable := func(id, reason string) {
		t.Helper()
		item := items[id]
		if item.Available || item.AvailabilityStatus != reason || item.UnavailableReason == nil || *item.UnavailableReason != reason {
			t.Fatalf("item %s availability=%#v, want reason %q", id, item, reason)
		}
	}
	assertUnavailable("51", "not_on_sale")
	assertUnavailable("52", "not_on_sale")
	assertUnavailable("53", "shop_closed")
	assertUnavailable("54", "not_on_sale")
	assertUnavailable("55", "out_of_stock")
	if item := items["56"]; !item.Available || item.AvailabilityStatus != "available" || item.UnavailableReason != nil {
		t.Fatalf("available item contract mismatch: %#v", item)
	}
	for _, id := range []string{"51", "52", "53"} {
		if item := items[id]; item.SalePriceAmount != 0 || item.TotalAmount != 0 {
			t.Fatalf("soft-deleted reference leaked price for item %s: %#v", id, item)
		}
	}
	if item := items["51"]; item.Name != "" || item.BrandName != "" || item.Spec != "" || item.ImageURL != "" {
		t.Fatalf("deleted product payload was not redacted: %#v", item)
	}
	payload, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "secret") {
		t.Fatalf("deleted product data leaked in response: %s", payload)
	}
}
