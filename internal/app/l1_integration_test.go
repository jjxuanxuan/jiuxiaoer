package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/infra/wechat"
	"jiuxiaoer-admin/backend-go/internal/infra/wechatpay"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestL1IdentityPaymentAndExpiryIntegration 验证L 1 身份支付 And 过期集成的预期行为。
func TestL1IdentityPaymentAndExpiryIntegration(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run L1 integration test")
	}
	ctx := context.Background()
	cfg := config.Load()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := mysql.Open(ctx, cfg.MySQL, log)
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatalf("begin transaction: %v", tx.Error)
	}
	defer tx.Rollback()

	redisClient := goredis.NewClient(&goredis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: 14})
	if err := redisClient.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flush redis: %v", err)
	}
	defer redisClient.Close()
	defer redisClient.FlushDB(ctx)

	paymentProvider, err := wechatpay.New(ctx, cfg.WeChat)
	if err != nil {
		t.Fatalf("create payment provider: %v", err)
	}
	defer paymentProvider.Shutdown()
	idGen := snowflake.New(998)
	registry := metrics.New("l1-integration", "")
	router := NewRouter(Dependencies{
		Config: cfg, Log: log, DB: tx, Redis: redisClient, IDGen: idGen, Metrics: registry,
		WeChatAuth: wechat.NewIdentityProvider(cfg.WeChat), PaymentProvider: paymentProvider,
	})

	login := performOK(t, router, http.MethodPost, "/api/v1/auth/customer/wechat-login", "", "", map[string]any{
		"code": "test-code-l1-integration", "device_id": "device-l1",
	})
	loginData := object(t, login["data"])
	accessToken := stringValue(t, loginData["access_token"])
	if bound, _ := loginData["phone_bound"].(bool); bound {
		t.Fatal("new WeChat customer must start without a bound phone")
	}
	phone := fmt.Sprintf("137%08d", time.Now().UnixNano()%100000000)
	performOK(t, router, http.MethodPost, "/api/v1/auth/customer/phone-bind", accessToken, "phone-bind-l1-001", map[string]any{"phone_code": "test-phone-" + phone})

	address := performOK(t, router, http.MethodPost, "/api/v1/addresses", accessToken, "address-l1-00001", map[string]any{
		"contact_name": "L1测试用户", "contact_phone": phone,
		"province": "广东省", "city": "深圳市", "city_code": "440300", "district": "南山区", "district_code": "440305",
		"address_detail": "L1集成测试地址", "location_source": "map_pin",
		"latitude": 22.54, "longitude": 113.93, "coordinate_system": "gcj02",
	})
	addressID := stringValue(t, object(t, address["data"])["id"])

	paidOrderID, paymentID, paymentNo, amount := createL1OrderAndPayment(t, router, accessToken, addressID, "8001", "paid")
	callbackBody, _ := json.Marshal(map[string]any{
		"event_id": "l1-callback-001", "provider_trade_no": "wx-trade-l1-001", "payment_no": paymentNo,
		"status": "SUCCESS", "amount": amount, "currency": "CNY", "paid_at": time.Now().UTC().Format(time.RFC3339),
		"app_id": "local-miniapp", "mch_id": "local-mch",
	})
	for index := 0; index < 10; index++ {
		performFakePaymentCallback(t, router, callbackBody, http.StatusOK)
	}
	paymentResult := performOK(t, router, http.MethodGet, "/api/v1/orders/"+paidOrderID+"/payment", accessToken, "", nil)
	if got := stringValue(t, object(t, paymentResult["data"])["status"]); got != "succeeded" {
		t.Fatalf("expected succeeded payment, got %s", got)
	}
	var callbackCount, deductionCount int64
	if err := tx.Table("payment_callbacks").Where("provider = 'wechat' AND provider_event_id = 'l1-callback-001'").Count(&callbackCount).Error; err != nil {
		t.Fatalf("count callbacks: %v", err)
	}
	if err := tx.Table("stock_records").Where("source_type = 'payment' AND source_id = ?", paymentID).Count(&deductionCount).Error; err != nil {
		t.Fatalf("count deductions: %v", err)
	}
	if callbackCount != 1 || deductionCount != 1 {
		t.Fatalf("callback idempotency failed: callbacks=%d deductions=%d", callbackCount, deductionCount)
	}

	tamperedOrderID, _, tamperedPaymentNo, tamperedAmount := createL1OrderAndPayment(t, router, accessToken, addressID, "8002", "tampered")
	tamperedBody, _ := json.Marshal(map[string]any{
		"event_id": "l1-callback-tampered", "provider_trade_no": "wx-trade-tampered", "payment_no": tamperedPaymentNo,
		"status": "SUCCESS", "amount": tamperedAmount + 1, "currency": "CNY", "paid_at": time.Now().UTC().Format(time.RFC3339),
		"app_id": "local-miniapp", "mch_id": "local-mch",
	})
	performFakePaymentCallback(t, router, tamperedBody, http.StatusUnauthorized)
	var tamperedOrder order.Order
	if err := tx.Where("id = ?", tamperedOrderID).First(&tamperedOrder).Error; err != nil {
		t.Fatalf("load tampered callback order: %v", err)
	}
	if tamperedOrder.Status != "pending_payment" {
		t.Fatalf("amount mismatch changed order state to %s", tamperedOrder.Status)
	}
	var rejectedCallbacks int64
	if err := tx.Table("payment_callbacks").Where("provider_event_id = 'l1-callback-tampered' AND process_status = 'failed' AND error_code = 'PAYMENT_AMOUNT_MISMATCH'").Count(&rejectedCallbacks).Error; err != nil || rejectedCallbacks != 1 {
		t.Fatalf("expected persisted rejected callback: count=%d err=%v", rejectedCallbacks, err)
	}

	expiringOrderID := createL1Order(t, router, accessToken, addressID, "8003", "expiry")
	past := time.Now().Add(-time.Minute)
	if err := tx.Model(&order.Order{}).Where("id = ?", expiringOrderID).Update("expires_at", past).Error; err != nil {
		t.Fatalf("expire order fixture: %v", err)
	}
	worker := order.NewExpiryWorker(cfg, tx, idGen, registry, log)
	processed, err := worker.ExpireBatch(ctx, time.Now(), 10)
	if err != nil || processed != 1 {
		t.Fatalf("expire unpaid order: processed=%d err=%v", processed, err)
	}
	var expired order.Order
	if err := tx.Where("id = ?", expiringOrderID).First(&expired).Error; err != nil {
		t.Fatalf("load expired order: %v", err)
	}
	if expired.Status != "cancelled" || expired.PayStatus != "closed" || stringValuePtr(expired.CancelReasonCode) != "PAYMENT_TIMEOUT" {
		t.Fatalf("unexpected expired order state: %+v", expired)
	}

	reconciledOrderID, reconciledPaymentID, reconciledPaymentNo, reconciledAmount := createL1OrderAndPayment(t, router, accessToken, addressID, "8004", "reconciled")
	if err := tx.Model(&order.Order{}).Where("id = ?", reconciledOrderID).Update("expires_at", past).Error; err != nil {
		t.Fatalf("expire reconciled order fixture: %v", err)
	}
	paidAt := time.Now().UTC()
	reconciliationProvider := &fixedPaymentProvider{state: order.ProviderPaymentState{
		ProviderTradeNo: "wx-query-trade-l1", PaymentNo: reconciledPaymentNo, Status: "SUCCESS",
		Amount: reconciledAmount, Currency: "CNY", PaidAt: &paidAt,
	}}
	worker = order.NewExpiryWorker(cfg, tx, idGen, registry, log, reconciliationProvider)
	processed, err = worker.ExpireBatch(ctx, time.Now(), 10)
	if err != nil || processed != 1 {
		t.Fatalf("reconcile paid expired order: processed=%d err=%v", processed, err)
	}
	var reconciled order.Order
	if err := tx.Where("id = ?", reconciledOrderID).First(&reconciled).Error; err != nil {
		t.Fatalf("load reconciled order: %v", err)
	}
	var reconciledDeductions int64
	if err := tx.Table("stock_records").Where("source_type = 'payment' AND source_id = ?", reconciledPaymentID).Count(&reconciledDeductions).Error; err != nil {
		t.Fatalf("count reconciled deductions: %v", err)
	}
	if reconciled.Status != "paid" || reconciled.PayStatus != "succeeded" || reconciledDeductions != 1 || reconciliationProvider.closed {
		t.Fatalf("provider query reconciliation failed: order=%+v deductions=%d closed=%t", reconciled, reconciledDeductions, reconciliationProvider.closed)
	}

	mismatchOrderID, mismatchPaymentID, mismatchPaymentNo, mismatchAmount := createL1OrderAndPayment(t, router, accessToken, addressID, "8005", "query-mismatch")
	if err := tx.Model(&order.Order{}).Where("id = ?", mismatchOrderID).Update("expires_at", past).Error; err != nil {
		t.Fatalf("expire query mismatch fixture: %v", err)
	}
	mismatchProvider := &fixedPaymentProvider{state: order.ProviderPaymentState{
		ProviderTradeNo: "wx-query-mismatch-l1", PaymentNo: mismatchPaymentNo, Status: "SUCCESS",
		Amount: mismatchAmount + 1, Currency: "CNY", PaidAt: &paidAt,
	}}
	worker = order.NewExpiryWorker(cfg, tx, idGen, registry, log, mismatchProvider)
	if processed, err = worker.ExpireBatch(ctx, time.Now(), 10); err == nil || processed != 0 {
		t.Fatalf("expected provider query mismatch to block reconciliation: processed=%d err=%v", processed, err)
	}
	var mismatchOrder order.Order
	if err := tx.Where("id = ?", mismatchOrderID).First(&mismatchOrder).Error; err != nil {
		t.Fatalf("load query mismatch order: %v", err)
	}
	var mismatchDeductions int64
	if err := tx.Table("stock_records").Where("source_type = 'payment' AND source_id = ?", mismatchPaymentID).Count(&mismatchDeductions).Error; err != nil {
		t.Fatalf("count query mismatch deductions: %v", err)
	}
	if mismatchOrder.Status != "pending_payment" || mismatchDeductions != 0 || mismatchProvider.closed {
		t.Fatalf("query mismatch changed business state: order=%+v deductions=%d closed=%t", mismatchOrder, mismatchDeductions, mismatchProvider.closed)
	}
}

// createL1OrderAndPayment 创建L 1 订单 And 支付。
func createL1OrderAndPayment(t *testing.T, router http.Handler, token string, addressID string, shopProductID string, suffix string) (string, uint64, string, int64) {
	t.Helper()
	orderID := createL1Order(t, router, token, addressID, shopProductID, suffix)
	created := performOK(t, router, http.MethodPost, "/api/v1/orders/"+orderID+"/payments", token, "payment-l1-"+suffix, map[string]any{
		"provider": "wechat", "client_type": "miniapp",
	})
	data := object(t, created["data"])
	paymentID, err := parseTestUint(stringValue(t, data["id"]))
	if err != nil {
		t.Fatalf("parse payment id: %v", err)
	}
	amount, ok := data["amount"].(float64)
	if !ok {
		t.Fatalf("invalid payment amount: %T", data["amount"])
	}
	duplicate := performOK(t, router, http.MethodPost, "/api/v1/orders/"+orderID+"/payments", token, "payment-l1-"+suffix, map[string]any{
		"provider": "wechat", "client_type": "miniapp",
	})
	if stringValue(t, object(t, duplicate["data"])["id"]) != stringValue(t, data["id"]) {
		t.Fatal("payment idempotency returned a different payment")
	}
	return orderID, paymentID, stringValue(t, data["payment_no"]), int64(amount)
}

// createL1Order 创建L 1 订单。
func createL1Order(t *testing.T, router http.Handler, token string, addressID string, shopProductID string, suffix string) string {
	t.Helper()
	created := performOK(t, router, http.MethodPost, "/api/v1/orders", token, "order-l1-"+suffix+"-001", map[string]any{
		"shop_id": "4201", "address_id": addressID,
		"items": []map[string]any{{"shop_product_id": shopProductID, "quantity": 1}},
	})
	data := object(t, created["data"])
	if stringValue(t, data["expires_at"]) == "" {
		t.Fatal("new L1 order is missing expires_at")
	}
	return stringValue(t, data["order_id"])
}

// performFakePaymentCallback 处理perform Fake 支付回调相关逻辑。
func performFakePaymentCallback(t *testing.T, router http.Handler, body []byte, expectedStatus int) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(wechatpay.FakeCallbackSecret))
	_, _ = mac.Write(body)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/payments/wechat/callbacks", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-JXE-Fake-Signature", hex.EncodeToString(mac.Sum(nil)))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != expectedStatus {
		t.Fatalf("callback status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// parseTestUint 解析Test Uint。
func parseTestUint(value string) (uint64, error) {
	var result uint64
	_, err := fmt.Sscanf(value, "%d", &result)
	return result, err
}

// stringValuePtr 返回字符串值 Ptr。
func stringValuePtr(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

type fixedPaymentProvider struct {
	state  order.ProviderPaymentState
	closed bool
}

// Code 返回代码。
func (p *fixedPaymentProvider) Code() string { return "wechat" }

// Create 创建提供器支付结果。
func (p *fixedPaymentProvider) Create(context.Context, order.CreateProviderPaymentInput) (order.ProviderPaymentResult, error) {
	return order.ProviderPaymentResult{}, nil
}

// Query 查询提供器支付状态。
func (p *fixedPaymentProvider) Query(context.Context, string) (order.ProviderPaymentState, error) {
	return p.state, nil
}

// Close 关闭当前实例并释放相关资源。
func (p *fixedPaymentProvider) Close(context.Context, string) error {
	p.closed = true
	return nil
}

// ParseCallback 解析回调。
func (p *fixedPaymentProvider) ParseCallback(context.Context, *http.Request) (order.PaymentCallbackEvent, error) {
	return order.PaymentCallbackEvent{}, nil
}

// Shutdown 平滑停止当前实例并释放相关资源。
func (p *fixedPaymentProvider) Shutdown() {}
