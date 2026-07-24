package store

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestStorePageTokenIsBoundToMerchantPrincipalAndShopScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	owner := &auth.Claims{
		AccountType: "merchant", MerchantID: "4001", MerchantUserID: "4101",
		AuthorizedShopIDs: []string{"4202", "4201"},
	}
	first := storePaginationTestContext("/api/v1/store/orders?page_size=2")
	query, err := pagination.FromGin(first, storePaginationScope(owner)...)
	if err != nil {
		t.Fatal(err)
	}
	token := pagination.NextPageTokenWithCursor(query, "2026-07-22T10:00:00Z", "9001")

	// 即使声明顺序改变，等价门店集合仍然有效。
	sameOwner := *owner
	sameOwner.AuthorizedShopIDs = []string{"4201", "4202"}
	same := storePaginationTestContext("/api/v1/store/orders?page_size=2&page_token=" + url.QueryEscape(token))
	if _, err := pagination.FromGin(same, storePaginationScope(&sameOwner)...); err != nil {
		t.Fatalf("equivalent sorted shop scope rejected: %v", err)
	}

	otherMerchant := *owner
	otherMerchant.MerchantID = "4999"
	crossMerchant := storePaginationTestContext("/api/v1/store/orders?page_size=2&page_token=" + url.QueryEscape(token))
	if _, err := pagination.FromGin(crossMerchant, storePaginationScope(&otherMerchant)...); problem.FromError(err).ErrorCode != "PAGE_TOKEN_INVALID" {
		t.Fatalf("cross-merchant token reuse was accepted: %v", err)
	}

	otherUser := *owner
	otherUser.MerchantUserID = "4199"
	crossUser := storePaginationTestContext("/api/v1/store/orders?page_size=2&page_token=" + url.QueryEscape(token))
	if _, err := pagination.FromGin(crossUser, storePaginationScope(&otherUser)...); problem.FromError(err).ErrorCode != "PAGE_TOKEN_INVALID" {
		t.Fatalf("cross-user token reuse was accepted: %v", err)
	}
}

func TestStoreOrderDefaultKeysetIsStableAcrossInsertAndDelete(t *testing.T) {
	_, db := storeDetailTestService(t)
	repo := NewRepository(db)
	base := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	rows := []Order{
		{ID: 101, OrderNo: "K101", MerchantID: 1, ShopID: 10, Status: "paid", CreatedAt: base.Add(time.Minute), UpdatedAt: base.Add(time.Minute)},
		{ID: 102, OrderNo: "K102", MerchantID: 1, ShopID: 10, Status: "paid", CreatedAt: base.Add(2 * time.Minute), UpdatedAt: base.Add(2 * time.Minute)},
		{ID: 103, OrderNo: "K103", MerchantID: 1, ShopID: 10, Status: "paid", CreatedAt: base.Add(3 * time.Minute), UpdatedAt: base.Add(3 * time.Minute)},
		{ID: 104, OrderNo: "K104", MerchantID: 1, ShopID: 10, Status: "paid", CreatedAt: base.Add(4 * time.Minute), UpdatedAt: base.Add(4 * time.Minute)},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}

	firstQuery := pagination.Query{PageSize: 2, OrderBy: "created_at desc,id desc"}
	first, err := repo.ListOrders(context.Background(), 1, []uint64{10}, StoreOrderListFilters{}, firstQuery)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 || first[0].ID != 104 || first[1].ID != 103 {
		t.Fatalf("unexpected first page/lookahead: %+v", orderIDsForTest(first))
	}
	cursorRow := first[1]

	// 新增较新记录并删除已消费页面中的记录会移动偏移窗口，
	// 但不得影响订单 103 之后的键集窗口。
	if err := db.Exec("DELETE FROM orders WHERE id=?", uint64(104)).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Order{ID: 105, OrderNo: "K105", MerchantID: 1, ShopID: 10, Status: "paid", CreatedAt: base.Add(5 * time.Minute), UpdatedAt: base.Add(5 * time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	secondQuery := pagination.Query{
		PageSize: 2,
		OrderBy:  "created_at desc,id desc",
		Cursor:   []string{cursorRow.CreatedAt.Format(time.RFC3339Nano), "103"},
	}
	second, err := repo.ListOrders(context.Background(), 1, []uint64{10}, StoreOrderListFilters{}, secondQuery)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 || second[0].ID != 102 || second[1].ID != 101 {
		t.Fatalf("keyset page shifted after insert/delete: %+v", orderIDsForTest(second))
	}
}

func TestStoreInventoryDefaultKeysetIsStableAcrossInsertAndDelete(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE products (
			id INTEGER PRIMARY KEY,
			category_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			brand_name TEXT,
			spec TEXT,
			image_url TEXT,
			original_price_amount INTEGER NOT NULL DEFAULT 0,
			age_restricted INTEGER NOT NULL DEFAULT 0,
			deleted_at DATETIME
		)`,
		`CREATE TABLE shop_products (
			id INTEGER PRIMARY KEY,
			merchant_id INTEGER NOT NULL,
			shop_id INTEGER NOT NULL,
			product_id INTEGER NOT NULL,
			sale_price_amount INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL,
			sort_order INTEGER NOT NULL DEFAULT 0,
			updated_at DATETIME NOT NULL,
			deleted_at DATETIME
		)`,
		`CREATE TABLE product_stocks (
			id INTEGER PRIMARY KEY,
			shop_product_id INTEGER NOT NULL,
			available_qty INTEGER NOT NULL DEFAULT 0,
			reserved_qty INTEGER NOT NULL DEFAULT 0,
			locked_qty INTEGER NOT NULL DEFAULT 0,
			low_stock_threshold INTEGER NOT NULL DEFAULT 0,
			version INTEGER NOT NULL DEFAULT 0,
			updated_at DATETIME,
			deleted_at DATETIME
		)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}

	base := time.Date(2026, 7, 22, 11, 0, 0, 0, time.UTC)
	type inventorySeed struct {
		id        uint64
		productID uint64
		shopTime  time.Time
		stockTime *time.Time
	}
	stockTime202 := base.Add(2 * time.Minute)
	stockTime203 := base.Add(3 * time.Minute)
	seeds := []inventorySeed{
		{id: 201, productID: 1001, shopTime: base.Add(time.Minute)},
		{id: 202, productID: 1002, shopTime: base.Add(time.Minute), stockTime: &stockTime202},
		{id: 203, productID: 1003, shopTime: base.Add(time.Minute), stockTime: &stockTime203},
		{id: 204, productID: 1004, shopTime: base.Add(4 * time.Minute)},
	}
	for _, seed := range seeds {
		if err := db.Exec(`INSERT INTO products (id, category_id, name) VALUES (?, ?, ?)`, seed.productID, 1, "product").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(`INSERT INTO shop_products (id, merchant_id, shop_id, product_id, status, updated_at) VALUES (?, ?, ?, ?, ?, ?)`, seed.id, 1, 10, seed.productID, "on_sale", seed.shopTime).Error; err != nil {
			t.Fatal(err)
		}
		if seed.stockTime != nil {
			if err := db.Exec(`INSERT INTO product_stocks (id, shop_product_id, updated_at) VALUES (?, ?, ?)`, seed.id, seed.id, *seed.stockTime).Error; err != nil {
				t.Fatal(err)
			}
		}
	}

	repo := NewRepository(db)
	first, err := repo.ListShopProducts(context.Background(), 1, []uint64{10}, StoreInventoryFilters{}, pagination.Query{PageSize: 2, OrderBy: "updated_at desc,id desc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 || first[0].ID != 204 || first[1].ID != 203 {
		t.Fatalf("unexpected first inventory page/lookahead: %+v", shopProductIDsForTest(first))
	}
	if !first[1].UpdatedAt.Equal(stockTime203) {
		t.Fatalf("effective stock updated_at not projected: got %s want %s", first[1].UpdatedAt, stockTime203)
	}
	cursorRow := first[1]

	// 新增较新记录并删除已消费页面中的记录，不得移动门店商品 203
	// 之后基于有效更新时间的键集窗口。
	if err := db.Exec(`DELETE FROM shop_products WHERE id = ?`, 204).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO products (id, category_id, name) VALUES (?, ?, ?)`, 1005, 1, "new product").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO shop_products (id, merchant_id, shop_id, product_id, status, updated_at) VALUES (?, ?, ?, ?, ?, ?)`, 205, 1, 10, 1005, "on_sale", base.Add(5*time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	second, err := repo.ListShopProducts(context.Background(), 1, []uint64{10}, StoreInventoryFilters{}, pagination.Query{
		PageSize: 2,
		OrderBy:  "updated_at desc,id desc",
		Cursor:   []string{cursorRow.UpdatedAt.Format(time.RFC3339Nano), "203"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 || second[0].ID != 202 || second[1].ID != 201 {
		t.Fatalf("inventory keyset page shifted after insert/delete: %+v", shopProductIDsForTest(second))
	}
}

func TestStoreInventoryDefaultKeysetBuildsEffectiveUpdatedAtWindow(t *testing.T) {
	capture := &storeSQLCaptureLogger{}
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{DryRun: true, Logger: capture})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewRepository(db)
	query := pagination.Query{
		PageSize: 2,
		Offset:   99,
		Cursor:   []string{"2026-07-22T11:03:00Z", "203"},
	}
	if _, err := repo.ListShopProducts(context.Background(), 1, []uint64{10}, StoreInventoryFilters{}, query); err != nil && !errors.Is(err, gorm.ErrDryRunModeUnsupported) {
		t.Fatal(err)
	}
	sql := capture.SQL
	for _, fragment := range []string{
		"COALESCE(ps.updated_at, sp.updated_at) <",
		"COALESCE(ps.updated_at, sp.updated_at) =",
		"sp.id <",
		"ORDER BY COALESCE(ps.updated_at, sp.updated_at) DESC, sp.id DESC",
		"LIMIT 3",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("inventory keyset SQL missing %q: %s", fragment, sql)
		}
	}
	if strings.Contains(strings.ToUpper(sql), "OFFSET") {
		t.Fatalf("keyset inventory query must ignore legacy offset: %s", sql)
	}
}

func storePaginationTestContext(target string) *gin.Context {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("GET", target, nil)
	return ctx
}

func orderIDsForTest(rows []Order) []uint64 {
	ids := make([]uint64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func shopProductIDsForTest(rows []ShopProductRow) []uint64 {
	ids := make([]uint64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

type storeSQLCaptureLogger struct{ SQL string }

func (l *storeSQLCaptureLogger) LogMode(logger.LogLevel) logger.Interface { return l }
func (l *storeSQLCaptureLogger) Info(context.Context, string, ...any)     {}
func (l *storeSQLCaptureLogger) Warn(context.Context, string, ...any)     {}
func (l *storeSQLCaptureLogger) Error(context.Context, string, ...any)    {}
func (l *storeSQLCaptureLogger) Trace(_ context.Context, _ time.Time, statement func() (string, int64), _ error) {
	l.SQL, _ = statement()
}
