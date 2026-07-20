package order_test

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
	"os"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/infra/wechatpay"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestPaymentCallbackAndExpiryRace1000 验证支付回调 And 过期 Race 1000的预期行为。
func TestPaymentCallbackAndExpiryRace1000(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run payment and expiry race test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cfg := config.Load()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := mysql.Open(ctx, cfg.MySQL, log)
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	idGen := snowflake.New(997)
	shopProductID := idGen.Next()
	stockID := idGen.Next()
	orderID := idGen.Next()
	paymentID := idGen.Next()
	paymentNo := fmt.Sprintf("PAY-RACE-%d", paymentID)
	expiresAt := time.Now().Add(-time.Second)

	if err := db.Create(&order.ProductStock{ID: stockID, ShopProductID: shopProductID, ShopID: 4201, ProductID: 5001, AvailableQty: 9, ReservedQty: 1}).Error; err != nil {
		t.Fatalf("create stock fixture: %v", err)
	}
	if err := db.Create(&order.Order{ID: orderID, OrderNo: fmt.Sprintf("ORDER-RACE-%d", orderID), CustomerID: 1, MerchantID: 4001, ShopID: 4201, Status: "pending_payment", PayStatus: "pending", DeliveryStatus: "pending", GoodsAmount: 100, PayableAmount: 100, ExpiresAt: &expiresAt}).Error; err != nil {
		t.Fatalf("create order fixture: %v", err)
	}
	if err := db.Create(&order.OrderItem{ID: idGen.Next(), OrderID: orderID, ShopProductID: shopProductID, ProductID: 5001, ProductSnapshot: []byte(`{"name":"race"}`), Quantity: 1, SalePriceAmount: 100, TotalAmount: 100}).Error; err != nil {
		t.Fatalf("create item fixture: %v", err)
	}
	if err := db.Create(&order.Payment{ID: paymentID, PaymentNo: paymentNo, OrderID: orderID, CustomerID: 1, Channel: "miniapp", Provider: "wechat", Status: "pending", ProviderStatus: stringPointer("NOTPAY"), Amount: 100, Currency: "CNY", ExpiresAt: &expiresAt}).Error; err != nil {
		t.Fatalf("create payment fixture: %v", err)
	}
	defer cleanupRaceFixture(db, orderID, paymentID, shopProductID, stockID)

	provider, err := wechatpay.New(ctx, cfg.WeChat)
	if err != nil {
		t.Fatalf("new payment provider: %v", err)
	}
	defer provider.Shutdown()
	registry := metrics.New("race", "")
	service := order.NewService(cfg, db, idGen).WithPaymentProvider(provider, registry)
	worker := order.NewExpiryWorker(cfg, db, idGen, registry, log)

	errorsCh := make(chan error, 1000)
	var wg sync.WaitGroup
	for index := 0; index < 500; index++ {
		wg.Add(1)
		go func(eventIndex int) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]any{
				"event_id": fmt.Sprintf("race-event-%03d", eventIndex), "provider_trade_no": "race-trade", "payment_no": paymentNo,
				"status": "SUCCESS", "amount": 100, "currency": "CNY", "paid_at": time.Now().UTC().Format(time.RFC3339),
				"app_id": "local-miniapp", "mch_id": "local-mch",
			})
			mac := hmac.New(sha256.New, []byte(wechatpay.FakeCallbackSecret))
			_, _ = mac.Write(body)
			request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/callback", bytes.NewReader(body))
			request.Header.Set("X-JXE-Fake-Signature", hex.EncodeToString(mac.Sum(nil)))
			if err := service.ProcessPaymentCallback(ctx, "wechat", request, body); err != nil {
				errorsCh <- err
			}
		}(index)
	}
	for index := 0; index < 500; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := worker.ExpireBatch(ctx, time.Now(), 1); err != nil {
				errorsCh <- err
			}
		}()
	}
	wg.Wait()
	close(errorsCh)
	for raceErr := range errorsCh {
		t.Fatalf("concurrent callback/expiry failed: %v", raceErr)
	}

	var finalOrder order.Order
	var finalStock order.ProductStock
	if err := db.Where("id = ?", orderID).First(&finalOrder).Error; err != nil {
		t.Fatalf("load final order: %v", err)
	}
	if err := db.Where("id = ?", stockID).First(&finalStock).Error; err != nil {
		t.Fatalf("load final stock: %v", err)
	}
	if finalStock.ReservedQty != 0 || finalStock.AvailableQty < 9 || finalStock.AvailableQty > 10 {
		t.Fatalf("stock invariant violated: %+v", finalStock)
	}
	if finalOrder.Status != "paid" && finalOrder.Status != "payment_exception" {
		t.Fatalf("unexpected final order state: %s", finalOrder.Status)
	}
}

// cleanupRaceFixture 清理Race 测试夹具。
func cleanupRaceFixture(db *gorm.DB, orderID uint64, paymentID uint64, shopProductID uint64, stockID uint64) {
	db.Exec("DELETE FROM payment_callbacks WHERE payment_id = ?", paymentID)
	db.Exec("DELETE FROM stock_records WHERE shop_product_id = ?", shopProductID)
	db.Exec(`DELETE FROM outbox_events
		WHERE (aggregate_type = 'order' AND aggregate_id = ?)
		   OR (aggregate_type = 'payment' AND aggregate_id = ?)
		   OR JSON_UNQUOTE(JSON_EXTRACT(payload, '$.order_id')) = ?`, orderID, paymentID, fmt.Sprint(orderID))
	db.Exec("DELETE FROM audit_logs WHERE resource_type = 'order' AND resource_id = ?", orderID)
	db.Exec("DELETE FROM order_logs WHERE order_id = ?", orderID)
	db.Exec("DELETE FROM order_items WHERE order_id = ?", orderID)
	db.Exec("DELETE FROM payments WHERE id = ?", paymentID)
	db.Exec("DELETE FROM orders WHERE id = ?", orderID)
	db.Exec("DELETE FROM product_stocks WHERE id = ?", stockID)
}

// stringPointer 返回字符串 Pointer。
func stringPointer(value string) *string { return &value }
