package order_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestExpiryReconcilesCreatingPaymentBeforeClosingOrder 验证订单关闭前先对账创建中的支付。
func TestExpiryReconcilesCreatingPaymentBeforeClosingOrder(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run creating payment reconciliation test")
	}
	ctx := context.Background()
	cfg := config.Load()
	db, err := mysql.Open(ctx, cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })

	idGen := snowflake.New(995)
	productID, shopProductID, stockID, orderID, paymentID := idGen.Next(), idGen.Next(), idGen.Next(), idGen.Next(), idGen.Next()
	paymentNo := "PAY-CREATING-" + strconv.FormatUint(paymentID, 10)
	expiresAt := time.Date(1998, time.January, 1, 0, 0, 0, 0, time.UTC)
	registerRaceFixtureCleanup(t, db, orderID, paymentID, shopProductID, stockID)
	if err := db.Create(&order.ProductStock{ID: stockID, ShopProductID: shopProductID, ShopID: 4201, ProductID: productID, AvailableQty: 9, ReservedQty: 1}).Error; err != nil {
		t.Fatalf("create stock: %v", err)
	}
	if err := db.Create(&order.Order{ID: orderID, OrderNo: "ORDER-CREATING-" + strconv.FormatUint(orderID, 10), CustomerID: 1, MerchantID: 4001, ShopID: 4201, Status: "pending_payment", PayStatus: "pending", DeliveryStatus: "pending", GoodsAmount: 100, PayableAmount: 100, ExpiresAt: &expiresAt}).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.Create(&order.OrderItem{ID: idGen.Next(), OrderID: orderID, ShopProductID: shopProductID, ProductID: productID, ProductSnapshot: []byte(`{"name":"creating"}`), Quantity: 1, SalePriceAmount: 100, TotalAmount: 100}).Error; err != nil {
		t.Fatalf("create order item: %v", err)
	}
	payment := retailOrderPaymentFixture(t, orderID, order.Payment{
		ID: paymentID, PaymentNo: paymentNo, CustomerID: 1, Channel: "miniapp",
		Provider: "wechat", Status: "creating", Amount: 100, Currency: "CNY", ExpiresAt: &expiresAt,
	})
	if err := db.Create(&payment).Error; err != nil {
		t.Fatalf("create payment: %v", err)
	}
	var insertedPayment order.Payment
	if err := db.First(&insertedPayment, paymentID).Error; err != nil || insertedPayment.Status != "creating" || insertedPayment.Provider != "wechat" {
		t.Fatalf("invalid payment fixture: %+v err=%v", insertedPayment, err)
	}

	paidAt := time.Now().Add(-time.Second)
	provider := &successfulQueryProvider{state: order.ProviderPaymentState{
		ProviderTradeNo: "wx-creating-success", PaymentNo: paymentNo, Status: "SUCCESS", Amount: 100, Currency: "CNY", PaidAt: &paidAt,
	}}
	worker := order.NewExpiryWorker(cfg, db, idGen, metrics.New("creating-reconciliation", ""), slog.New(slog.NewTextHandler(io.Discard, nil)), provider)
	processed, err := worker.ExpireBatch(ctx, time.Now(), 1)
	if err != nil {
		t.Fatalf("expire batch: %v (query=%d close=%d)", err, provider.queryCount, provider.closeCount)
	}
	if processed != 1 || provider.queryCount != 1 || provider.closeCount != 0 {
		t.Fatalf("unexpected reconciliation result: processed=%d query=%d close=%d", processed, provider.queryCount, provider.closeCount)
	}
	var finalOrder order.Order
	var finalPayment order.Payment
	var finalStock order.ProductStock
	if err := db.First(&finalOrder, orderID).Error; err != nil {
		t.Fatalf("load order: %v", err)
	}
	if err := db.First(&finalPayment, paymentID).Error; err != nil {
		t.Fatalf("load payment: %v", err)
	}
	if err := db.First(&finalStock, stockID).Error; err != nil {
		t.Fatalf("load stock: %v", err)
	}
	if finalOrder.Status != "paid" || finalPayment.Status != "succeeded" || finalStock.ReservedQty != 0 || finalStock.AvailableQty != 9 {
		t.Fatalf("creating payment was not reconciled safely: order=%s payment=%s stock=%+v", finalOrder.Status, finalPayment.Status, finalStock)
	}
}

// TestWorkerReconcilesStaleCreatingPaymentBeforeExpiry 验证工作进程在过期前对账停滞的创建中支付。
func TestWorkerReconcilesStaleCreatingPaymentBeforeExpiry(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run creating payment reconciliation test")
	}
	ctx := context.Background()
	cfg := config.Load()
	db, err := mysql.Open(ctx, cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	idGen := snowflake.New(993)
	productID, shopProductID, stockID, orderID, paymentID := idGen.Next(), idGen.Next(), idGen.Next(), idGen.Next(), idGen.Next()
	paymentNo := "PAY-CREATING-EARLY-" + strconv.FormatUint(paymentID, 10)
	expiresAt := time.Now().Add(10 * time.Minute)
	staleAt := time.Now().Add(-time.Minute)
	registerRaceFixtureCleanup(t, db, orderID, paymentID, shopProductID, stockID)
	if err := db.Create(&order.ProductStock{ID: stockID, ShopProductID: shopProductID, ShopID: 4201, ProductID: productID, AvailableQty: 9, ReservedQty: 1}).Error; err != nil {
		t.Fatalf("create stock: %v", err)
	}
	if err := db.Create(&order.Order{ID: orderID, OrderNo: "ORDER-CREATING-EARLY-" + strconv.FormatUint(orderID, 10), CustomerID: 1, MerchantID: 4001, ShopID: 4201, Status: "pending_payment", PayStatus: "pending", DeliveryStatus: "pending", GoodsAmount: 100, PayableAmount: 100, ExpiresAt: &expiresAt}).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.Create(&order.OrderItem{ID: idGen.Next(), OrderID: orderID, ShopProductID: shopProductID, ProductID: productID, ProductSnapshot: []byte(`{"name":"creating-early"}`), Quantity: 1, SalePriceAmount: 100, TotalAmount: 100}).Error; err != nil {
		t.Fatalf("create item: %v", err)
	}
	payment := retailOrderPaymentFixture(t, orderID, order.Payment{
		ID: paymentID, PaymentNo: paymentNo, CustomerID: 1, Channel: "miniapp",
		Provider: "wechat", Status: "creating", Amount: 100, Currency: "CNY",
		ExpiresAt: &expiresAt, UpdatedAt: staleAt,
	})
	if err := db.Create(&payment).Error; err != nil {
		t.Fatalf("create payment: %v", err)
	}
	// GORM 可能在 Create 时刷新自动更新时间戳，因此固定测试夹具的时间。
	if err := db.Model(&order.Payment{}).Where("id = ?", paymentID).UpdateColumn("updated_at", staleAt).Error; err != nil {
		t.Fatalf("age payment fixture: %v", err)
	}
	paidAt := time.Now()
	provider := &successfulQueryProvider{state: order.ProviderPaymentState{ProviderTradeNo: "wx-creating-early", PaymentNo: paymentNo, Status: "SUCCESS", Amount: 100, Currency: "CNY", PaidAt: &paidAt}}
	worker := order.NewExpiryWorker(cfg, db, idGen, metrics.New("creating-early", ""), slog.New(slog.NewTextHandler(io.Discard, nil)), provider)
	processed, err := worker.ReconcileCreatingBatch(ctx, time.Now(), 1)
	if err != nil || processed != 1 || provider.queryCount != 1 {
		t.Fatalf("pre-expiry reconciliation failed: processed=%d query=%d err=%v", processed, provider.queryCount, err)
	}
	var finalOrder order.Order
	if err := db.First(&finalOrder, orderID).Error; err != nil || finalOrder.Status != "paid" {
		t.Fatalf("expected paid order before expiry: status=%s err=%v", finalOrder.Status, err)
	}
}

type successfulQueryProvider struct {
	state      order.ProviderPaymentState
	queryCount int
	closeCount int
}

// Code 返回代码。
func (p *successfulQueryProvider) Code() string { return "wechat" }

// Create 创建提供器支付结果。
func (p *successfulQueryProvider) Create(context.Context, order.CreateProviderPaymentInput) (order.ProviderPaymentResult, error) {
	return order.ProviderPaymentResult{}, nil
}

// Query 查询提供器支付状态。
func (p *successfulQueryProvider) Query(context.Context, string) (order.ProviderPaymentState, error) {
	p.queryCount++
	return p.state, nil
}

// Close 关闭当前实例并释放相关资源。
func (p *successfulQueryProvider) Close(context.Context, string) (order.ProviderOperationResult, error) {
	p.closeCount++
	return order.ProviderOperationResult{}, nil
}

// ParseCallback 解析回调。
func (p *successfulQueryProvider) ParseCallback(context.Context, *http.Request) (order.PaymentCallbackEvent, error) {
	return order.PaymentCallbackEvent{}, nil
}

// Shutdown 平滑停止当前实例并释放相关资源。
func (p *successfulQueryProvider) Shutdown() {}
