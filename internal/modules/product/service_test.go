package product

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/modules/servicearea"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestProductDTOServiceShopAvailabilityContract(t *testing.T) {
	base := ProductRow{
		ID: 101, CategoryID: 11, ShopID: 201, ShopProductID: 301,
		Name: "catalog bottle", SalePriceAmount: 0, OriginalPriceAmount: 0,
		Status: "on_sale", ShopProductStatus: "on_sale", ShopStatus: "active", BusinessStatus: "open", AvailableQty: 5,
	}
	tests := []struct {
		name        string
		mutate      func(*ProductRow)
		purchasable bool
		reason      string
	}{
		{name: "available", purchasable: true},
		{name: "global product off sale", mutate: func(row *ProductRow) { row.Status = "off_sale" }, reason: reasonNotOnSale},
		{name: "shop product off sale", mutate: func(row *ProductRow) { row.ShopProductStatus = "off_sale" }, reason: reasonNotOnSale},
		{name: "shop disabled", mutate: func(row *ProductRow) { row.ShopStatus = "inactive" }, reason: reasonShopClosed},
		{name: "shop resting", mutate: func(row *ProductRow) { row.BusinessStatus = "resting" }, reason: reasonShopClosed},
		{name: "out of stock", mutate: func(row *ProductRow) { row.AvailableQty = 0 }, reason: reasonOutOfStock},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := base
			if tt.mutate != nil {
				tt.mutate(&row)
			}
			item := productDTO(row)
			if item.ContextType != contextServiceShop || item.Purchasable != tt.purchasable {
				t.Fatalf("unexpected availability contract: %#v", item)
			}
			if tt.reason == "" {
				if item.UnavailableReason != nil {
					t.Fatalf("purchasable product must use a null reason: %#v", item)
				}
			} else if item.UnavailableReason == nil || *item.UnavailableReason != tt.reason {
				t.Fatalf("expected reason %q, got %#v", tt.reason, item.UnavailableReason)
			}

			payload := productJSONMap(t, item)
			for _, required := range []string{"shop_id", "shop_product_id", "sale_price_amount", "available_qty", "purchasable", "unavailable_reason"} {
				if _, ok := payload[required]; !ok {
					t.Fatalf("service-shop response omitted required field %q: %s", required, mustJSON(t, item))
				}
			}
		})
	}
}

func TestProductDTOLocationlessContractOmitsShopFields(t *testing.T) {
	item := productResponse(productDTO(ProductRow{
		ID: 101, CategoryID: 11, Name: "catalog bottle", OriginalPriceAmount: 999,
		Status: "on_sale", AgeRestricted: true,
	}), ListQuery{}, nil)

	if item.ContextType != contextNoServiceShop || item.Purchasable || item.UnavailableReason == nil || *item.UnavailableReason != reasonLocationRequired {
		t.Fatalf("unexpected locationless contract: %#v", item)
	}
	payload := productJSONMap(t, item)
	for _, forbidden := range []string{"shop_id", "shop_product_id", "sale_price_amount", "original_price_amount", "available_qty", "delivery_promise"} {
		if _, ok := payload[forbidden]; ok {
			t.Fatalf("locationless response exposed forbidden field %q: %s", forbidden, mustJSON(t, item))
		}
	}

	outOfService := productResponse(item, ListQuery{locationlessReason: reasonOutOfService}, nil)
	if outOfService.UnavailableReason == nil || *outOfService.UnavailableReason != reasonOutOfService {
		t.Fatalf("expected out_of_service reason, got %#v", outOfService.UnavailableReason)
	}
}

func TestGetPublicProductRejectsInvalidShopID(t *testing.T) {
	_, err := NewService(nil, nil).GetPublicProduct(context.Background(), "101", ListQuery{ShopID: "invalid"})
	if err == nil || problem.FromError(err).ErrorCode != "VALIDATION_INVALID_QUERY" {
		t.Fatalf("expected invalid shop_id error, got %v", err)
	}
}

func TestGetPublicProductMergesCategoryRestrictionAndDoesNotCachePromise(t *testing.T) {
	db := newProductTestDB(t)
	mini := miniredis.RunT(t)
	redisClient := goredis.NewClient(&goredis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })

	service := NewService(db, redisClient)
	service.resolveShopForTest = func(_ context.Context, query *ListQuery) (*servicearea.DeliveryPromiseDTO, error) {
		query.ShopID = "201"
		distance := uint64(111)
		if query.Latitude != nil && *query.Latitude > 1 {
			distance = 222
		}
		return &servicearea.DeliveryPromiseDTO{
			DeliveryFeeAmount: 500, ETAMinMinutes: 15, ETAMaxMinutes: 25,
			RouteDistanceM: &distance, RouteSource: "amap", Confirmed: true,
		}, nil
	}

	firstLatitude := 1.0
	first, err := service.GetPublicProduct(context.Background(), "101", ListQuery{Latitude: &firstLatitude})
	if err != nil {
		t.Fatalf("first product detail: %v", err)
	}
	if !first.AgeRestricted {
		t.Fatal("category restriction was not merged into product detail")
	}
	assertPromiseDistance(t, first, 111)

	if err := db.Exec("UPDATE products SET name = 'database changed' WHERE id = 101").Error; err != nil {
		t.Fatalf("change database after cache fill: %v", err)
	}
	secondLatitude := 2.0
	second, err := service.GetPublicProduct(context.Background(), "101", ListQuery{Latitude: &secondLatitude})
	if err != nil {
		t.Fatalf("cached product detail: %v", err)
	}
	if second.Name != first.Name {
		t.Fatalf("expected second request to use static cache, got name %q", second.Name)
	}
	assertPromiseDistance(t, second, 222)

	cached, err := redisClient.Get(context.Background(), "product:detail:101:201").Result()
	if err != nil {
		t.Fatalf("read static product cache: %v", err)
	}
	if strings.Contains(cached, "delivery_promise") || strings.Contains(cached, "route_distance_m") {
		t.Fatalf("location-specific promise leaked into product cache: %s", cached)
	}
}

func TestCategoryCacheSwitchesRevisionAfterEveryCategoryMutation(t *testing.T) {
	dsn := "file:" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()) + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	statements := []string{
		`CREATE TABLE categories (
			id INTEGER PRIMARY KEY, parent_id INTEGER, name TEXT NOT NULL, sort_order INTEGER NOT NULL,
			status TEXT NOT NULL, age_restricted BOOLEAN NOT NULL, created_at DATETIME, updated_at DATETIME, deleted_at DATETIME
		)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("prepare category cache fixture with %q: %v", statement, err)
		}
	}

	mini := miniredis.RunT(t)
	redisClient := goredis.NewClient(&goredis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	service := NewService(db, redisClient)

	revision := categoryRevision(t, db)
	assertCategoryNames(t, service, nil)
	assertRedisKeyExists(t, redisClient, categoryCacheKey(revision))
	initialRevision := revision

	if err := db.Exec(`INSERT INTO categories (id, name, sort_order, status, age_restricted) VALUES (10, 'wine', 20, 'active', 1)`).Error; err != nil {
		t.Fatalf("create category: %v", err)
	}
	revision = changedCategoryRevision(t, db, revision)
	assertCategoryNames(t, service, []string{"wine"})
	assertRedisKeyExists(t, redisClient, categoryCacheKey(revision))

	if err := db.Exec(`UPDATE categories SET name = 'premium wine' WHERE id = 10`).Error; err != nil {
		t.Fatalf("update category: %v", err)
	}
	revision = changedCategoryRevision(t, db, revision)
	assertCategoryNames(t, service, []string{"premium wine"})
	assertRedisKeyExists(t, redisClient, categoryCacheKey(revision))

	if err := db.Exec(`UPDATE categories SET status = 'inactive' WHERE id = 10`).Error; err != nil {
		t.Fatalf("change category status: %v", err)
	}
	revision = changedCategoryRevision(t, db, revision)
	assertCategoryNames(t, service, nil)
	assertRedisKeyExists(t, redisClient, categoryCacheKey(revision))

	if err := db.Exec(`UPDATE categories SET status = 'active' WHERE id = 10`).Error; err != nil {
		t.Fatalf("reactivate category: %v", err)
	}
	revision = changedCategoryRevision(t, db, revision)
	assertCategoryNames(t, service, []string{"premium wine"})
	assertRedisKeyExists(t, redisClient, categoryCacheKey(revision))

	if err := db.Exec(`UPDATE categories SET deleted_at = CURRENT_TIMESTAMP WHERE id = 10`).Error; err != nil {
		t.Fatalf("soft delete category: %v", err)
	}
	revision = changedCategoryRevision(t, db, revision)
	assertCategoryNames(t, service, nil)
	assertRedisKeyExists(t, redisClient, categoryCacheKey(revision))

	if err := db.Exec(`DELETE FROM categories WHERE id = 10`).Error; err != nil {
		t.Fatalf("hard delete category: %v", err)
	}
	revision = changedCategoryRevision(t, db, revision)
	assertCategoryNames(t, service, nil)
	assertRedisKeyExists(t, redisClient, categoryCacheKey(revision))

	// 版本切换刻意让旧记录自然过期；它们仍然存在可证明修改后的请求没有复用它们。
	assertRedisKeyExists(t, redisClient, categoryCacheKey(initialRevision))
}

func TestCategoryRevisionRollsBackWithCategoryMutation(t *testing.T) {
	dsn := "file:" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()) + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	for _, statement := range []string{
		`CREATE TABLE categories (
			id INTEGER PRIMARY KEY, parent_id INTEGER, name TEXT NOT NULL, sort_order INTEGER NOT NULL,
			status TEXT NOT NULL, age_restricted BOOLEAN NOT NULL, deleted_at DATETIME
		)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("prepare rollback fixture with %q: %v", statement, err)
		}
	}

	beforeRevision := categoryRevision(t, db)
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`INSERT INTO categories (id, name, sort_order, status, age_restricted) VALUES (1, 'rolled back', 1, 'active', 0)`).Error; err != nil {
			return err
		}
		return context.Canceled
	})
	if err != context.Canceled {
		t.Fatalf("expected rollback sentinel, got %v", err)
	}

	afterRevision := categoryRevision(t, db)
	if afterRevision != beforeRevision {
		t.Fatalf("rolled-back category mutation changed revision from %s to %s", beforeRevision, afterRevision)
	}
}

func TestRepositoryGetPublicProductMergesCategoryRestriction(t *testing.T) {
	repo := NewRepository(newProductTestDB(t))

	shopProduct, err := repo.GetPublicProduct(context.Background(), 101, 201)
	if err != nil {
		t.Fatalf("get shop product: %v", err)
	}
	if !shopProduct.AgeRestricted || shopProduct.Status != "on_sale" || shopProduct.ShopProductStatus != "on_sale" {
		t.Fatalf("unexpected shop product projection: %#v", shopProduct)
	}

	genericProduct, err := repo.GetPublicProduct(context.Background(), 101, 0)
	if err != nil {
		t.Fatalf("get generic product: %v", err)
	}
	if !genericProduct.AgeRestricted {
		t.Fatalf("generic detail did not merge category restriction: %#v", genericProduct)
	}
}

func newProductTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()) + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	statements := []string{
		`CREATE TABLE categories (id INTEGER PRIMARY KEY, status TEXT NOT NULL, age_restricted BOOLEAN NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE products (id INTEGER PRIMARY KEY, category_id INTEGER NOT NULL, name TEXT NOT NULL, brand_name TEXT, spec TEXT, image_url TEXT, description TEXT, original_price_amount INTEGER NOT NULL, status TEXT NOT NULL, age_restricted BOOLEAN NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE shops (id INTEGER PRIMARY KEY, status TEXT NOT NULL, business_status TEXT NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE shop_products (id INTEGER PRIMARY KEY, shop_id INTEGER NOT NULL, product_id INTEGER NOT NULL, sale_price_amount INTEGER NOT NULL, status TEXT NOT NULL, sort_order INTEGER NOT NULL, deleted_at DATETIME)`,
		`CREATE TABLE product_stocks (id INTEGER PRIMARY KEY, shop_product_id INTEGER NOT NULL, available_qty INTEGER NOT NULL, deleted_at DATETIME)`,
		`INSERT INTO categories (id, status, age_restricted) VALUES (11, 'active', 1)`,
		`INSERT INTO products (id, category_id, name, original_price_amount, status, age_restricted) VALUES (101, 11, 'catalog bottle', 1200, 'on_sale', 0)`,
		`INSERT INTO shops (id, status, business_status) VALUES (201, 'active', 'open')`,
		`INSERT INTO shop_products (id, shop_id, product_id, sale_price_amount, status, sort_order) VALUES (301, 201, 101, 1000, 'on_sale', 1)`,
		`INSERT INTO product_stocks (id, shop_product_id, available_qty) VALUES (401, 301, 5)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("prepare product fixture with %q: %v", statement, err)
		}
	}
	return db
}

func assertPromiseDistance(t *testing.T, item ProductDTO, expected uint64) {
	t.Helper()
	promise := item.DeliveryPromise
	if promise == nil || promise.RouteDistanceM == nil || *promise.RouteDistanceM != expected {
		t.Fatalf("expected route distance %d, got %#v", expected, item.DeliveryPromise)
	}
}

func assertCategoryNames(t *testing.T, service *Service, expected []string) {
	t.Helper()
	items, err := service.ListCategories(context.Background())
	if err != nil {
		t.Fatalf("list categories: %v", err)
	}
	if len(items) != len(expected) {
		t.Fatalf("expected category names %v, got %#v", expected, items)
	}
	for index, name := range expected {
		if items[index].Name != name {
			t.Fatalf("expected category names %v, got %#v", expected, items)
		}
	}
}

func assertRedisKeyExists(t *testing.T, redisClient *goredis.Client, key string) {
	t.Helper()
	exists, err := redisClient.Exists(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("check Redis key %q: %v", key, err)
	}
	if exists != 1 {
		t.Fatalf("expected Redis key %q to exist", key)
	}
}

func categoryRevision(t *testing.T, db *gorm.DB) string {
	t.Helper()
	revision, err := NewRepository(db).CategoryCatalogRevision(context.Background())
	if err != nil {
		t.Fatalf("read category revision: %v", err)
	}
	if revision == "" {
		t.Fatal("category revision must not be empty")
	}
	return revision
}

func changedCategoryRevision(t *testing.T, db *gorm.DB, previous string) string {
	t.Helper()
	revision := categoryRevision(t, db)
	if revision == previous {
		t.Fatalf("category mutation did not switch revision %s", previous)
	}
	return revision
}

func productJSONMap(t *testing.T, item ProductDTO) map[string]any {
	t.Helper()
	payload := make(map[string]any)
	if err := json.Unmarshal([]byte(mustJSON(t, item)), &payload); err != nil {
		t.Fatalf("decode product JSON: %v", err)
	}
	return payload
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(payload)
}
