package refund

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestApplyProviderStateIsAtomicAndIdempotent 验证Apply 提供器状态 Is Atomic And Idempotent的预期行为。
func TestApplyProviderStateIsAtomicAndIdempotent(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run refund ledger integration test")
	}
	ctx := context.Background()
	cfg := config.Load()
	db, err := mysql.Open(ctx, cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	ids := snowflake.New(994)
	orderID, paymentID, afterSaleID, itemID, refundID := ids.Next(), ids.Next(), ids.Next(), ids.Next(), ids.Next()
	orderNo := "ORDER-RF-" + strconv.FormatUint(orderID, 10)
	paymentNo := "PAY-RF-" + strconv.FormatUint(paymentID, 10)
	afterSaleNo := "AS-RF-" + strconv.FormatUint(afterSaleID, 10)
	refundNo := "RF-" + strconv.FormatUint(refundID, 10)
	now := time.Now().UTC()

	mustExec := func(query string, args ...any) {
		t.Helper()
		if err := db.Exec(query, args...).Error; err != nil {
			t.Fatalf("fixture query failed: %v", err)
		}
	}
	mustExec("INSERT INTO orders (id,order_no,customer_id,merchant_id,shop_id,status,pay_status,delivery_status,goods_amount,payable_amount,paid_amount,refunded_amount,after_sale_status) VALUES (?,?,?,?,?,'completed','succeeded','completed',1000,1000,1000,0,'processing')", orderID, orderNo, 1, 1, 1)
	mustExec("INSERT INTO payments (id,payment_no,order_id,customer_id,channel,provider,status,amount,refunded_amount,currency) VALUES (?,?,?,1,'miniapp','wechat','succeeded',1000,0,'CNY')", paymentID, paymentNo, orderID)
	mustExec("INSERT INTO after_sales (id,after_sale_no,order_id,customer_id,merchant_id,shop_id,type,requested_resolution,approved_resolution,status,requested_amount,approved_amount,description,submitted_at) VALUES (?,?,?,1,1,1,'damaged','refund_only','refund_only','refund_processing',400,400,'integration fixture',?)", afterSaleID, afterSaleNo, orderID, now)
	mustExec("INSERT INTO after_sale_items (id,after_sale_id,order_id,order_item_id,shop_product_id,product_id,requested_quantity,approved_quantity,requested_amount,approved_amount) VALUES (?,?,?,?,?,?,1,1,400,400)", itemID, afterSaleID, orderID, ids.Next(), ids.Next(), ids.Next())
	mustExec("INSERT INTO refunds (id,refund_no,after_sale_id,order_id,payment_id,provider,status,amount,total_amount,currency,requested_at) VALUES (?,?,?,?,?,'wechat','pending',400,1000,'CNY',?)", refundID, refundNo, afterSaleID, orderID, paymentID, now)
	mustExec("INSERT INTO refund_items (id,refund_id,after_sale_item_id,amount,quantity) VALUES (?,?,?,?,1)", ids.Next(), refundID, itemID, 400)
	t.Cleanup(func() {
		db.Exec("DELETE FROM outbox_events WHERE aggregate_type='refund' AND aggregate_id=?", refundID)
		db.Exec("DELETE FROM refund_items WHERE refund_id=?", refundID)
		db.Exec("DELETE FROM refunds WHERE id=?", refundID)
		db.Exec("DELETE FROM after_sale_items WHERE after_sale_id=?", afterSaleID)
		db.Exec("DELETE FROM after_sales WHERE id=?", afterSaleID)
		db.Exec("DELETE FROM payments WHERE id=?", paymentID)
		db.Exec("DELETE FROM orders WHERE id=?", orderID)
	})

	service := NewService(cfg, db, ids, nil)
	state := State{ProviderRefundID: "wx-refund-" + refundNo, RefundNo: refundNo, PaymentNo: paymentNo, Status: "SUCCESS", Currency: "CNY", Amount: 400, TotalAmount: 1000, SucceededAt: &now}
	if err := service.ApplyProviderState(ctx, refundID, state); err != nil {
		t.Fatalf("apply success: %v", err)
	}
	if err := service.ApplyProviderState(ctx, refundID, state); err != nil {
		t.Fatalf("apply duplicate: %v", err)
	}

	var payment Payment
	var order Order
	var afterSale AfterSale
	var item AfterSaleItem
	var row Row
	if err := db.First(&payment, paymentID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&order, orderID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&afterSale, afterSaleID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&item, itemID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&row, refundID).Error; err != nil {
		t.Fatal(err)
	}
	if payment.RefundedAmount != 400 || order.RefundedAmount != 400 || afterSale.RefundedAmount != 400 || item.RefundedAmount != 400 {
		t.Fatalf("ledger mismatch payment=%d order=%d after_sale=%d item=%d", payment.RefundedAmount, order.RefundedAmount, afterSale.RefundedAmount, item.RefundedAmount)
	}
	if row.Status != "succeeded" || afterSale.Status != "completed" || order.AfterSaleStatus != "partial_refunded" {
		t.Fatalf("unexpected terminal states refund=%s after_sale=%s order=%s", row.Status, afterSale.Status, order.AfterSaleStatus)
	}
}
