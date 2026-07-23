package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/dispatch"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestP0Integration 在外层事务中验证 P0 主链，测试结束后不会保留业务数据。
func TestP0Integration(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run local P0 integration test")
	}
	ctx := context.Background()
	cfg := config.Load()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := mysql.Open(ctx, cfg.MySQL, log)
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get mysql connection: %v", err)
	}
	defer sqlDB.Close()

	tx := db.Begin()
	if tx.Error != nil {
		t.Fatalf("begin outer transaction: %v", tx.Error)
	}
	defer tx.Rollback()

	redisClient := goredis.NewClient(&goredis.Options{
		Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: 15,
	})
	if err := redisClient.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping redis test db: %v", err)
	}
	defer redisClient.Close()
	if err := redisClient.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flush redis test db: %v", err)
	}
	defer redisClient.FlushDB(ctx)

	router := NewRouter(Dependencies{Config: cfg, Log: log, DB: tx, Redis: redisClient})

	// 公共分页必须连续，且商品缓存响应不能携带强一致库存数量。
	firstPage := performOK(t, router, http.MethodGet, "/api/v1/products?page_size=2", "", "", nil)
	firstData := object(t, firstPage["data"])
	firstItems := array(t, firstData["items"])
	pageToken := stringValue(t, firstData["next_page_token"])
	secondPage := performOK(t, router, http.MethodGet, "/api/v1/products?page_size=2&page_token="+pageToken, "", "", nil)
	secondItems := array(t, object(t, secondPage["data"])["items"])
	if stringValue(t, object(t, firstItems[1])["id"]) == stringValue(t, object(t, secondItems[0])["id"]) {
		t.Fatal("pagination returned a duplicate boundary item")
	}
	product := performOK(t, router, http.MethodGet, "/api/v1/products/7001", "", "", nil)
	if _, exists := object(t, product["data"])["available_qty"]; exists {
		t.Fatal("public product cache must not expose strong-consistency inventory")
	}

	phone := fmt.Sprintf("139%08d", time.Now().UnixNano()%100000000)
	performOK(t, router, http.MethodPost, "/api/v1/auth/customer/send-code", "", "", map[string]any{"phone": phone})
	login := performOK(t, router, http.MethodPost, "/api/v1/auth/customer/sms-login", "", "", map[string]any{"phone": phone, "code": "123456"})
	loginData := object(t, login["data"])
	accessToken := stringValue(t, loginData["access_token"])
	oldRefreshToken := stringValue(t, loginData["refresh_token"])

	// 刷新成功后旧 refresh token 必须立即失效。
	refreshed := performOK(t, router, http.MethodPost, "/api/v1/auth/refresh", "", "", map[string]any{"refresh_token": oldRefreshToken})
	refreshedData := object(t, refreshed["data"])
	accessToken = stringValue(t, refreshedData["access_token"])
	if status, _ := perform(t, router, http.MethodPost, "/api/v1/auth/refresh", "", "", map[string]any{"refresh_token": oldRefreshToken}); status != http.StatusUnauthorized {
		t.Fatalf("expected rotated refresh token to fail with 401, got %d", status)
	}

	address := performOK(t, router, http.MethodPost, "/api/v1/addresses", accessToken, "address-key-0001", map[string]any{
		"contact_name": "P0测试用户", "contact_phone": phone,
		"province": "广东省", "city": "深圳市", "city_code": "440300", "district": "南山区", "district_code": "440305",
		"address_detail": "集成测试地址 1 号", "location_source": "map_pin",
		"latitude": 22.54, "longitude": 113.93, "coordinate_system": "gcj02", "is_default": true,
	})
	addressID := stringValue(t, object(t, address["data"])["id"])

	orderBody := map[string]any{
		"shop_id": "4201", "address_id": addressID,
		"items": []map[string]any{{"shop_product_id": "8001", "quantity": 1}},
	}
	created := performOK(t, router, http.MethodPost, "/api/v1/orders", accessToken, "order-key-000001", orderBody)
	orderID := stringValue(t, object(t, created["data"])["order_id"])
	duplicate := performOK(t, router, http.MethodPost, "/api/v1/orders", accessToken, "order-key-000001", orderBody)
	if stringValue(t, object(t, duplicate["data"])["order_id"]) != orderID {
		t.Fatal("duplicate order request returned a different order")
	}

	paid := performOK(t, router, http.MethodPost, "/api/v1/orders/"+orderID+"/pay/mock", accessToken, "payment-key-001", map[string]any{"channel": "mock"})
	paymentID := stringValue(t, object(t, paid["data"])["id"])
	paidAgain := performOK(t, router, http.MethodPost, "/api/v1/orders/"+orderID+"/pay/mock", accessToken, "payment-key-001", map[string]any{"channel": "mock"})
	if stringValue(t, object(t, paidAgain["data"])["id"]) != paymentID {
		t.Fatal("duplicate payment request returned a different payment")
	}
	var paidDelivery struct {
		ID                uint64
		PickupReadyStatus string
	}
	if err := tx.Table("delivery_orders").Select("id,pickup_ready_status").Where("order_id=?", orderID).First(&paidDelivery).Error; err != nil {
		t.Fatalf("payment did not create delivery synchronously: %v", err)
	}
	if paidDelivery.PickupReadyStatus != "waiting_store" {
		t.Fatalf("new paid delivery unexpectedly pickup-ready: %s", paidDelivery.PickupReadyStatus)
	}
	var paidJobCount int64
	if err := tx.Table("dispatch_jobs").Where("order_id=? AND status='pending'", orderID).Count(&paidJobCount).Error; err != nil || paidJobCount != 1 {
		t.Fatalf("payment did not create exactly one pending dispatch job: count=%d err=%v", paidJobCount, err)
	}

	// Confirm the agreed phase-one rule: once payment has produced the order,
	// the rider may accept it before the merchant finishes preparation.
	openOrderGrab(t, cfg, tx, redisClient, log, orderID)
	performOK(t, router, http.MethodPost, "/api/v1/auth/rider/send-code", "", "", map[string]any{"phone": "13800000003"})
	rider := performOK(t, router, http.MethodPost, "/api/v1/auth/rider/sms-login", "", "", map[string]any{"phone": "13800000003", "code": "123456"})
	riderToken := stringValue(t, object(t, rider["data"])["access_token"])
	performOK(t, router, http.MethodPost, "/api/v1/delivery/riders/me/heartbeat", riderToken, "", map[string]any{
		"device_id": "p0-rider-device", "sequence": uint64(time.Now().UnixNano()), "captured_at": time.Now().Format(time.RFC3339Nano),
		"latitude": 22.541, "longitude": 113.931, "coordinate_system": "gcj02", "accuracy_m": 20,
	})
	deliveries := performOK(t, router, http.MethodGet, "/api/v1/delivery/orders?page_size=100", riderToken, "", nil)
	deliveryID := findDeliveryID(t, array(t, object(t, deliveries["data"])["items"]), orderID)
	status, invalidGrab := perform(t, router, http.MethodPost, "/api/v1/delivery/orders/"+deliveryID+"/accept", riderToken, "delivery-accept-invalid", nil)
	if status != http.StatusBadRequest || invalidGrab["error_code"] != "VALIDATION_FAILED" {
		t.Fatalf("grab without assignment version was accepted: status=%d body=%#v", status, invalidGrab)
	}
	performOK(t, router, http.MethodPost, "/api/v1/delivery/orders/"+deliveryID+"/accept", riderToken, "delivery-accept-01", map[string]any{"expected_assignment_version": 1})
	status, pickupBeforeReady := perform(t, router, http.MethodPost, "/api/v1/delivery/orders/"+deliveryID+"/pickup", riderToken, "delivery-pickup-before-ready", nil)
	if status != http.StatusConflict || pickupBeforeReady["error_code"] != "DELIVERY_PICKUP_NOT_READY" {
		t.Fatalf("pickup gate did not block pre-prepare pickup: status=%d body=%#v", status, pickupBeforeReady)
	}

	merchant := performOK(t, router, http.MethodPost, "/api/v1/auth/merchant/login", "", "", map[string]any{"username": "merchant_demo", "password": "merchant123"})
	merchantToken := stringValue(t, object(t, merchant["data"])["access_token"])
	performStoreOrderAction(t, router, tx, orderID, "accept", merchantToken, "store-accept-001")
	performStoreOrderAction(t, router, tx, orderID, "start-preparing", merchantToken, "store-prep-start")
	performStoreOrderAction(t, router, tx, orderID, "prepare", merchantToken, "store-prepared01")

	for _, action := range []string{"pickup", "start", "complete"} {
		performOK(t, router, http.MethodPost, "/api/v1/delivery/orders/"+deliveryID+"/"+action, riderToken, "delivery-"+action+"-01", nil)
	}

	admin := performOK(t, router, http.MethodPost, "/api/v1/auth/admin/login", "", "", map[string]any{"username": "admin", "password": "admin123"})
	adminData := object(t, admin["data"])
	adminToken := stringValue(t, adminData["access_token"])
	adminRefresh := stringValue(t, adminData["refresh_token"])
	var beforeQty int
	if err := tx.Raw("SELECT available_qty FROM product_stocks WHERE shop_product_id = 8002").Scan(&beforeQty).Error; err != nil {
		t.Fatalf("query stock before adjust: %v", err)
	}
	adjustBody := map[string]any{"shop_product_id": "8002", "quantity_delta": 1, "reason": "P0幂等测试"}
	performOK(t, router, http.MethodPost, "/api/v1/admin/stocks/adjust", adminToken, "admin-stock-key1", adjustBody)
	performOK(t, router, http.MethodPost, "/api/v1/admin/stocks/adjust", adminToken, "admin-stock-key1", adjustBody)
	var afterQty int
	if err := tx.Raw("SELECT available_qty FROM product_stocks WHERE shop_product_id = 8002").Scan(&afterQty).Error; err != nil {
		t.Fatalf("query stock after adjust: %v", err)
	}
	if afterQty != beforeQty+1 {
		t.Fatalf("admin stock idempotency failed: before=%d after=%d", beforeQty, afterQty)
	}

	performOK(t, router, http.MethodPost, "/api/v1/auth/logout", adminToken, "", nil)
	if status, _ := perform(t, router, http.MethodPost, "/api/v1/auth/refresh", "", "", map[string]any{"refresh_token": adminRefresh}); status != http.StatusUnauthorized {
		t.Fatalf("expected refresh after logout to fail with 401, got %d", status)
	}

	assertOrderEvents(t, tx, orderID, []string{
		"order.created", "stock.reserved", "order.paid", "stock.deducted",
		"store.order.accepted", "store.order.preparing", "store.order.prepared",
		"delivery.completed", "order.completed",
	})
}

// openOrderGrab 解密并返回订单 Grab。
func openOrderGrab(t *testing.T, cfg config.Config, db *gorm.DB, redisClient *goredis.Client, log *slog.Logger, orderID string) {
	t.Helper()
	var jobID uint64
	if err := db.Table("dispatch_jobs").Select("id").Where("order_id=?", orderID).Order("dispatch_seq DESC").Scan(&jobID).Error; err != nil || jobID == 0 {
		t.Fatalf("find dispatch job for order %s: id=%d err=%v", orderID, jobID, err)
	}
	cfg.Dispatch.Enabled = true
	cfg.Dispatch.ModeOverride = "grab"
	service := dispatch.NewService(cfg, db, redisClient, snowflake.New(990), nil, log)
	if err := service.ProcessJobID(context.Background(), jobID); err != nil {
		t.Fatalf("open grab for order %s: %v", orderID, err)
	}
}

// TestP0ConcurrentOrdersDoNotOversell 验证P 0 Concurrent 订单 Do 不 Oversell的预期行为。
func TestP0ConcurrentOrdersDoNotOversell(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run local P0 integration test")
	}
	ctx := context.Background()
	cfg := config.Load()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := mysql.Open(ctx, cfg.MySQL, log)
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get mysql connection: %v", err)
	}
	defer sqlDB.Close()
	redisClient := goredis.NewClient(&goredis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: 15})
	if err := redisClient.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flush redis test db: %v", err)
	}
	defer redisClient.Close()
	defer redisClient.FlushDB(ctx)

	idGen := snowflake.New(999)
	productID := idGen.Next()
	shopProductID := idGen.Next()
	stockID := idGen.Next()
	phone := fmt.Sprintf("138%08d", time.Now().UnixNano()%100000000)
	var customerID uint64
	defer func() {
		cleanupConcurrentOrderData(db, customerID, productID, shopProductID, stockID)
	}()
	if err := db.Exec(`
		INSERT INTO products (id, category_id, name, sale_price_amount, original_price_amount, status)
		VALUES (?, 6001, 'P0并发测试商品', 100, 100, 'on_sale')
	`, productID).Error; err != nil {
		t.Fatalf("create concurrency product: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO shop_products (id, merchant_id, shop_id, product_id, sale_price_amount, status, sort_order)
		VALUES (?, 4001, 4201, ?, 100, 'on_sale', 9999)
	`, shopProductID, productID).Error; err != nil {
		t.Fatalf("create concurrency shop product: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO product_stocks (id, shop_product_id, shop_id, product_id, available_qty, reserved_qty, locked_qty, low_stock_threshold, version)
		VALUES (?, ?, 4201, ?, 10, 0, 0, 0, 0)
	`, stockID, shopProductID, productID).Error; err != nil {
		t.Fatalf("create concurrency stock: %v", err)
	}

	router := NewRouter(Dependencies{Config: cfg, Log: log, DB: db, Redis: redisClient})
	performOK(t, router, http.MethodPost, "/api/v1/auth/customer/send-code", "", "", map[string]any{"phone": phone})
	login := performOK(t, router, http.MethodPost, "/api/v1/auth/customer/sms-login", "", "", map[string]any{"phone": phone, "code": "123456"})
	accessToken := stringValue(t, object(t, login["data"])["access_token"])
	if err := db.Raw("SELECT id FROM customers WHERE phone = ?", phone).Scan(&customerID).Error; err != nil || customerID == 0 {
		t.Fatalf("query concurrency customer: id=%d err=%v", customerID, err)
	}
	address := performOK(t, router, http.MethodPost, "/api/v1/addresses", accessToken, "race-address-01", map[string]any{
		"contact_name": "并发测试", "contact_phone": phone,
		"province": "广东省", "city": "深圳市", "city_code": "440300", "district": "南山区", "district_code": "440305",
		"address_detail": "并发测试地址", "location_source": "map_pin",
		"latitude": 22.54, "longitude": 113.93, "coordinate_system": "gcj02",
	})
	addressID := stringValue(t, object(t, address["data"])["id"])
	body, err := json.Marshal(map[string]any{
		"shop_id": "4201", "address_id": addressID,
		"items": []map[string]any{{"shop_product_id": fmt.Sprintf("%d", shopProductID), "quantity": 1}},
	})
	if err != nil {
		t.Fatalf("marshal concurrent order body: %v", err)
	}

	statuses := make(chan int, 20)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/orders", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+accessToken)
			req.Header.Set("Idempotency-Key", fmt.Sprintf("race-order-key-%02d", index))
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			statuses <- recorder.Code
		}(i)
	}
	wg.Wait()
	close(statuses)
	successes, conflicts := 0, 0
	for status := range statuses {
		switch status {
		case http.StatusOK:
			successes++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected concurrent order status: %d", status)
		}
	}
	if successes != 10 || conflicts != 10 {
		t.Fatalf("expected 10 successes and 10 conflicts, got successes=%d conflicts=%d", successes, conflicts)
	}
	var stock struct {
		AvailableQty int
		ReservedQty  int
	}
	if err := db.Raw("SELECT available_qty, reserved_qty FROM product_stocks WHERE id = ?", stockID).Scan(&stock).Error; err != nil {
		t.Fatalf("query final concurrent stock: %v", err)
	}
	if stock.AvailableQty != 0 || stock.ReservedQty != 10 {
		t.Fatalf("oversell protection failed: available=%d reserved=%d", stock.AvailableQty, stock.ReservedQty)
	}
}

// cleanupConcurrentOrderData 清理Concurrent 订单数据。
func cleanupConcurrentOrderData(db *gorm.DB, customerID uint64, productID uint64, shopProductID uint64, stockID uint64) {
	if db == nil {
		return
	}
	var orderIDs []uint64
	if customerID != 0 {
		_ = db.Table("orders").Where("customer_id = ?", customerID).Pluck("id", &orderIDs).Error
	}
	if len(orderIDs) > 0 {
		db.Exec("DELETE FROM order_items WHERE order_id IN ?", orderIDs)
		db.Exec("DELETE FROM order_logs WHERE order_id IN ?", orderIDs)
		db.Exec("DELETE FROM payments WHERE order_id IN ?", orderIDs)
		db.Exec("DELETE FROM delivery_orders WHERE order_id IN ?", orderIDs)
		db.Exec("DELETE FROM outbox_events WHERE JSON_UNQUOTE(JSON_EXTRACT(payload, '$.order_id')) IN ?", uint64Strings(orderIDs))
		db.Exec("DELETE FROM audit_logs WHERE resource_type = 'order' AND resource_id IN ?", orderIDs)
		db.Exec("DELETE FROM orders WHERE id IN ?", orderIDs)
	}
	db.Exec("DELETE FROM stock_records WHERE shop_product_id = ?", shopProductID)
	if customerID != 0 {
		db.Exec("DELETE FROM idempotency_keys WHERE actor_type = 'customer' AND actor_id = ?", customerID)
		db.Exec("DELETE FROM audit_logs WHERE actor_type = 'customer' AND actor_id = ?", customerID)
		db.Exec("DELETE FROM customer_addresses WHERE customer_id = ?", customerID)
		db.Exec("DELETE FROM cart_items WHERE cart_id IN (SELECT id FROM carts WHERE customer_id = ?)", customerID)
		db.Exec("DELETE FROM carts WHERE customer_id = ?", customerID)
		var accountID uint64
		_ = db.Raw("SELECT account_id FROM customers WHERE id = ?", customerID).Scan(&accountID).Error
		db.Exec("DELETE FROM customers WHERE id = ?", customerID)
		if accountID != 0 {
			db.Exec("DELETE FROM accounts WHERE id = ?", accountID)
		}
	}
	db.Exec("DELETE FROM product_stocks WHERE id = ?", stockID)
	db.Exec("DELETE FROM shop_products WHERE id = ?", shopProductID)
	db.Exec("DELETE FROM products WHERE id = ?", productID)
}

// uint64Strings 返回uint 64 Strings。
func uint64Strings(values []uint64) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, fmt.Sprintf("%d", value))
	}
	return result
}

// perform 返回perform。
func perform(t *testing.T, handler http.Handler, method string, path string, token string, idempotencyKey string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(payload)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode %s %s response (%d): %v; body=%s", method, path, recorder.Code, err, recorder.Body.String())
	}
	return recorder.Code, response
}

// mustOK 返回must OK。
func mustOK(t *testing.T, status int, response map[string]any) map[string]any {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %#v", status, response)
	}
	return response
}

// performOK 返回perform OK。
func performOK(t *testing.T, handler http.Handler, method string, path string, token string, idempotencyKey string, body any) map[string]any {
	t.Helper()
	status, response := perform(t, handler, method, path, token, idempotencyKey, body)
	return mustOK(t, status, response)
}

// object 返回object。
func object(t *testing.T, value any) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %T", value)
	}
	return result
}

// array 返回array。
func array(t *testing.T, value any) []any {
	t.Helper()
	result, ok := value.([]any)
	if !ok {
		t.Fatalf("expected array, got %T", value)
	}
	return result
}

// stringValue 安全读取字符串指针的值。
func stringValue(t *testing.T, value any) string {
	t.Helper()
	result, ok := value.(string)
	if !ok || result == "" {
		t.Fatalf("expected non-empty string, got %#v", value)
	}
	return result
}

// performStoreOrderAction reads the latest order version immediately before
// each merchant transition, matching the optimistic-lock HTTP contract.
func performStoreOrderAction(t *testing.T, handler http.Handler, tx *gorm.DB, orderID, action, token, key string) map[string]any {
	t.Helper()
	var version uint
	if err := tx.Table("orders").Select("version").Where("id = ?", orderID).Scan(&version).Error; err != nil {
		t.Fatalf("query store order version before %s: %v", action, err)
	}
	return performOK(t, handler, http.MethodPost, "/api/v1/store/orders/"+orderID+"/"+action, token, key, map[string]any{"expected_version": version})
}

// findDeliveryID 查找配送ID。
func findDeliveryID(t *testing.T, items []any, orderID string) string {
	t.Helper()
	for _, item := range items {
		row := object(t, item)
		if row["order_id"] == orderID {
			return stringValue(t, row["id"])
		}
	}
	t.Fatalf("delivery order not found for order %s", orderID)
	return ""
}

// assertOrderEvents 断言订单 Events符合预期。
func assertOrderEvents(t *testing.T, tx *gorm.DB, orderID string, expected []string) {
	t.Helper()
	var eventTypes []string
	err := tx.Table("outbox_events").
		Where("JSON_UNQUOTE(JSON_EXTRACT(payload, '$.order_id')) = ?", orderID).
		Pluck("event_type", &eventTypes).Error
	if err != nil {
		t.Fatalf("query order events: %v", err)
	}
	found := make(map[string]bool, len(eventTypes))
	for _, eventType := range eventTypes {
		found[eventType] = true
	}
	for _, eventType := range expected {
		if !found[eventType] {
			t.Fatalf("missing P0 event %s; got %v", eventType, eventTypes)
		}
	}
}
