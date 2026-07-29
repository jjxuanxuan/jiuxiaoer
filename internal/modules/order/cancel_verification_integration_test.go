package order_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/deliveryverification"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestCustomerCancelInvalidatesEarlyVerificationMySQL(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run customer cancellation verification acceptance")
	}
	ctx := context.Background()
	cfg := config.Load()
	db, err := mysql.Open(ctx, cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })

	ids := snowflake.New(971)
	customerID, shopProductID, stockID := ids.Next(), ids.Next(), ids.Next()
	orderID, paymentID, deliveryID, verificationID := ids.Next(), ids.Next(), ids.Next(), ids.Next()
	path, key := "/api/v1/orders/:id/cancel", fmt.Sprintf("cancel-verification-%d", orderID)
	t.Cleanup(func() {
		cleanupCustomerCancelVerification(t, db, customerID, orderID, paymentID, deliveryID, verificationID, shopProductID, stockID, path)
	})

	expiresAt := time.Now().UTC().Add(30 * time.Minute)
	if err := db.Create(&order.ProductStock{ID: stockID, ShopProductID: shopProductID, ShopID: 4201, ProductID: 5001, AvailableQty: 9, ReservedQty: 1, Version: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&order.Order{ID: orderID, OrderNo: fmt.Sprintf("ORDER-CANCEL-VERIFY-%d", orderID), CustomerID: customerID, MerchantID: 4001, ShopID: 4201, Status: "pending_payment", PayStatus: "pending", DeliveryStatus: "pending", GoodsAmount: 100, PayableAmount: 100, ExpiresAt: &expiresAt, Version: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&order.OrderItem{ID: ids.Next(), OrderID: orderID, ShopProductID: shopProductID, ProductID: 5001, ProductSnapshot: []byte(`{"name":"legacy early delivery"}`), Quantity: 1, SalePriceAmount: 100, TotalAmount: 100}).Error; err != nil {
		t.Fatal(err)
	}
	payment := retailOrderPaymentFixture(t, orderID, order.Payment{
		ID: paymentID, PaymentNo: fmt.Sprintf("PAY-CANCEL-VERIFY-%d", paymentID),
		CustomerID: customerID, Channel: "miniapp", Provider: "wechat",
		Status: "pending", Amount: 100, Currency: "CNY", ExpiresAt: &expiresAt,
	})
	if err := db.Create(&payment).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("delivery_orders").Create(map[string]any{"id": deliveryID, "order_id": orderID, "shop_id": 4201, "status": "pending_assign", "assignment_version": 1, "dispatch_status": "pending", "pickup_ready_status": "waiting_store"}).Error; err != nil {
		t.Fatal(err)
	}
	verification := deliveryverification.Verification{
		ID: verificationID, DeliveryOrderID: deliveryID, Stage: "pickup", ModeSnapshot: "enforce",
		CodeHash: fmt.Sprintf("%064d", 1), CodeCiphertext: []byte("legacy-code"), CodeMask: "****11",
		PolicyVersion: "cp1-v1", SecretKeyVersion: "v1", Status: "active", MaxAttempts: 5,
		ExpiresAt: time.Now().UTC().Add(time.Hour), ActivatedAt: timePointer(time.Now().UTC()), Version: 1,
	}
	if err := db.Create(&verification).Error; err != nil {
		t.Fatal(err)
	}

	claims := &auth.Claims{AccountType: "customer", CustomerID: fmt.Sprint(customerID), Permissions: []string{"order:cancel"}}
	service := order.NewService(cfg, db, ids)
	expectedVersion := uint(1)
	result, err := service.Cancel(ctx, claims, "POST", path, key, fmt.Sprint(orderID), order.OrderCancelReq{Reason: "customer changed mind", ExpectedVersion: &expectedVersion})
	if err != nil || result.Status != "cancelled" {
		t.Fatalf("customer cancel: result=%+v err=%v", result, err)
	}
	var invalidated deliveryverification.Verification
	if err := db.First(&invalidated, verificationID).Error; err != nil {
		t.Fatal(err)
	}
	if invalidated.Status != "invalidated" || invalidated.InvalidatedAt == nil || invalidated.InvalidationReasonCode == nil || *invalidated.InvalidationReasonCode != "order_cancelled" || invalidated.Version != 2 {
		t.Fatalf("customer cancel left verification usable: %+v", invalidated)
	}

	// HTTP 幂等重放不得改写失效事实。
	if _, err := service.Cancel(ctx, claims, "POST", path, key, fmt.Sprint(orderID), order.OrderCancelReq{Reason: "customer changed mind", ExpectedVersion: &expectedVersion}); err != nil {
		t.Fatalf("cancel replay: %v", err)
	}
	var replayed deliveryverification.Verification
	if err := db.First(&replayed, verificationID).Error; err != nil {
		t.Fatal(err)
	}
	if replayed.Version != 2 || replayed.InvalidationReasonCode == nil || *replayed.InvalidationReasonCode != "order_cancelled" || replayed.InvalidatedAt == nil || !replayed.InvalidatedAt.Equal(*invalidated.InvalidatedAt) {
		t.Fatalf("cancel replay rewrote verification fact: %+v", replayed)
	}
}

func cleanupCustomerCancelVerification(t *testing.T, db *gorm.DB, customerID, orderID, paymentID, deliveryID, verificationID, shopProductID, stockID uint64, path string) {
	t.Helper()
	statements := []struct {
		query string
		args  []any
	}{
		{"DELETE FROM delivery_verification_attempts WHERE verification_id=?", []any{verificationID}},
		{"DELETE FROM delivery_verifications WHERE id=?", []any{verificationID}},
		{"DELETE FROM delivery_orders WHERE id=?", []any{deliveryID}},
		{"DELETE FROM idempotency_keys WHERE actor_type='customer' AND actor_id=? AND path=?", []any{customerID, path}},
		{"DELETE FROM stock_records WHERE shop_product_id=?", []any{shopProductID}},
		{"DELETE FROM outbox_events WHERE aggregate_id=? OR JSON_UNQUOTE(JSON_EXTRACT(payload, '$.order_id'))=?", []any{orderID, fmt.Sprint(orderID)}},
		{"DELETE FROM audit_logs WHERE resource_type='order' AND resource_id=?", []any{orderID}},
		{"DELETE FROM order_logs WHERE order_id=?", []any{orderID}},
		{"DELETE FROM order_items WHERE order_id=?", []any{orderID}},
		{"DELETE FROM payments WHERE id=?", []any{paymentID}},
		{"DELETE FROM orders WHERE id=?", []any{orderID}},
		{"DELETE FROM product_stocks WHERE id=?", []any{stockID}},
	}
	for _, statement := range statements {
		if err := db.Exec(statement.query, statement.args...).Error; err != nil {
			t.Errorf("cleanup customer cancel verification fixture: %v", err)
		}
	}
}

func timePointer(value time.Time) *time.Time { return &value }
