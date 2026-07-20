package refund

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type refundAcceptanceFixture struct {
	orderID, paymentID, afterSaleID, itemID, refundID uint64
	orderNo, paymentNo, afterSaleNo, refundNo         string
	amount, total                                     int64
}

type acceptanceProvider struct {
	refundState, queryState, callbackState State
	refundErr, queryErr                    error
	refundCalls, queryCalls                atomic.Int64
	delay                                  time.Duration
}

// Code 返回代码。
func (p *acceptanceProvider) Code() string { return "wechat" }

// Refund 返回退款。
func (p *acceptanceProvider) Refund(_ context.Context, _ Input) (State, error) {
	p.refundCalls.Add(1)
	if p.delay > 0 {
		time.Sleep(p.delay)
	}
	return p.refundState, p.refundErr
}

// QueryRefund 查询退款。
func (p *acceptanceProvider) QueryRefund(_ context.Context, _ string) (State, error) {
	p.queryCalls.Add(1)
	if p.delay > 0 {
		time.Sleep(p.delay)
	}
	return p.queryState, p.queryErr
}

// ParseRefundCallback 解析退款回调。
func (p *acceptanceProvider) ParseRefundCallback(_ context.Context, request *http.Request) (CallbackEvent, error) {
	return CallbackEvent{EventID: request.Header.Get("X-Event-ID"), MchID: "local-mch", State: p.callbackState}, nil
}

// TestL3RefundAcceptance 验证L 3 退款验收的预期行为。
func TestL3RefundAcceptance(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run L3 refund acceptance tests")
	}
	db := openRefundAcceptanceDB(t)
	ids := snowflake.New(991)
	cfg := config.Load()
	cfg.AfterSale.WorkerBatchSize = 1
	cfg.AfterSale.RefundExecutionEnabled = true
	cfg.AfterSale.Enabled = true

	t.Run("ACC-L3-022-provider-timeout-keeps-same-refund", func(t *testing.T) {
		fx := insertRefundAcceptanceFixture(t, db, ids, "creating", 400, 1000)
		defer cleanupRefundAcceptance(t, db, fx)
		provider := &acceptanceProvider{refundErr: context.DeadlineExceeded}
		service := NewService(cfg, db, ids, provider)
		worker := NewWorker(cfg, service, provider, slog.New(slog.NewTextHandler(io.Discard, nil)))
		worker.runBatch(context.Background())
		var row Row
		if err := db.First(&row, fx.refundID).Error; err != nil {
			t.Fatal(err)
		}
		var count int64
		db.Table("refunds").Where("after_sale_id=?", fx.afterSaleID).Count(&count)
		if row.RefundNo != fx.refundNo || row.Status != "pending" || count != 1 || provider.refundCalls.Load() != 1 {
			t.Fatalf("ACC-022 row=%+v count=%d calls=%d", row, count, provider.refundCalls.Load())
		}
	})

	t.Run("ACC-L3-023-query-success-is-idempotent", func(t *testing.T) {
		fx := insertRefundAcceptanceFixture(t, db, ids, "pending", 400, 1000)
		defer cleanupRefundAcceptance(t, db, fx)
		state := successState(fx)
		provider := &acceptanceProvider{queryState: state}
		service := NewService(cfg, db, ids, provider)
		worker := NewWorker(cfg, service, provider, slog.New(slog.NewTextHandler(io.Discard, nil)))
		worker.runBatch(context.Background())
		if err := service.ApplyProviderState(context.Background(), fx.refundID, state); err != nil {
			t.Fatal(err)
		}
		assertRefundLedger(t, db, fx, "succeeded", 400)
		if provider.queryCalls.Load() != 1 {
			t.Fatalf("ACC-023 query calls=%d", provider.queryCalls.Load())
		}
	})

	t.Run("ACC-L3-024-025-provider-failure-and-retry", func(t *testing.T) {
		fx := insertRefundAcceptanceFixture(t, db, ids, "creating", 400, 1000)
		defer cleanupRefundAcceptance(t, db, fx)
		closed := successState(fx)
		closed.Status = "CLOSED"
		closed.SucceededAt = nil
		provider := &acceptanceProvider{refundState: closed}
		service := NewService(cfg, db, ids, provider)
		NewWorker(cfg, service, provider, slog.New(slog.NewTextHandler(io.Discard, nil))).runBatch(context.Background())
		var row Row
		db.First(&row, fx.refundID)
		if row.Status != "failed" {
			t.Fatalf("ACC-024 status=%s", row.Status)
		}
		admin := refundAdmin(ids.Next())
		competingID := ids.Next()
		refundMustExec(t, db, "INSERT INTO refunds (id,refund_no,after_sale_id,order_id,payment_id,provider,status,amount,total_amount,currency,requested_at,next_retry_at) VALUES (?,?,?,?,?,'wechat','pending',700,1000,'CNY',?,?)", competingID, "RF-COMPETING-"+refundID(competingID), fx.afterSaleID, fx.orderID, fx.paymentID, time.Now(), time.Now().Add(time.Hour))
		if err := service.Retry(context.Background(), &admin, "POST", "/api/v1/admin/refunds/:id/retry", "retry-capacity-acceptance", fx.refundNo); refundErrorCode(err) != "REFUND_AMOUNT_EXCEEDED" {
			t.Fatalf("ACC-024 capacity recheck got %v", err)
		}
		if err := db.Delete(&Row{}, competingID).Error; err != nil {
			t.Fatal(err)
		}
		if err := service.Retry(context.Background(), &admin, "POST", "/api/v1/admin/refunds/:id/retry", "retry-failed-acceptance", fx.refundNo); err != nil {
			t.Fatalf("ACC-024 retry: %v", err)
		}
		db.First(&row, fx.refundID)
		if row.Status != "pending" || row.RefundNo != fx.refundNo {
			t.Fatalf("ACC-024 retry row=%+v", row)
		}
		state := successState(fx)
		if err := service.ApplyProviderState(context.Background(), fx.refundID, state); err != nil {
			t.Fatal(err)
		}
		if err := service.Retry(context.Background(), &admin, "POST", "/api/v1/admin/refunds/:id/retry", "retry-success-acceptance", fx.refundNo); refundErrorCode(err) != "REFUND_RETRY_NOT_ALLOWED" {
			t.Fatalf("ACC-025 got %v", err)
		}
	})

	t.Run("ACC-L3-026-027-valid-callback-and-100-replays", func(t *testing.T) {
		fx := insertRefundAcceptanceFixture(t, db, ids, "pending", 400, 1000)
		defer cleanupRefundAcceptance(t, db, fx)
		provider := &acceptanceProvider{callbackState: successState(fx)}
		service := NewService(cfg, db, ids, provider)
		for index := 0; index < 100; index++ {
			request, _ := http.NewRequest(http.MethodPost, "/callback", nil)
			request.Header.Set("X-Event-ID", "repeat-event")
			if err := service.ProcessCallback(context.Background(), "wechat", request, []byte(`{"event":"same"}`)); err != nil {
				t.Fatalf("callback %d: %v", index, err)
			}
		}
		assertRefundLedger(t, db, fx, "succeeded", 400)
		var callbacks int64
		db.Table("refund_callbacks").Where("provider='wechat' AND provider_event_id='repeat-event'").Count(&callbacks)
		if callbacks != 1 {
			t.Fatalf("ACC-027 callbacks=%d", callbacks)
		}
	})

	t.Run("ACC-L3-028-mismatch-enters-exception-without-ledger-change", func(t *testing.T) {
		fx := insertRefundAcceptanceFixture(t, db, ids, "pending", 400, 1000)
		defer cleanupRefundAcceptance(t, db, fx)
		state := successState(fx)
		state.Amount++
		provider := &acceptanceProvider{callbackState: state}
		service := NewService(cfg, db, ids, provider)
		request, _ := http.NewRequest(http.MethodPost, "/callback", nil)
		request.Header.Set("X-Event-ID", "mismatch-"+fx.refundNo)
		if err := service.ProcessCallback(context.Background(), "wechat", request, []byte(`{"event":"mismatch"}`)); refundErrorCode(err) != "REFUND_AMOUNT_MISMATCH" {
			t.Fatalf("ACC-028 got %v", err)
		}
		assertRefundLedger(t, db, fx, "exception", 0)
		var failed int64
		db.Table("refund_callbacks").Where("provider_event_id=? AND process_status='failed'", "mismatch-"+fx.refundNo).Count(&failed)
		if failed != 1 {
			t.Fatalf("ACC-028 failed callbacks=%d", failed)
		}
	})

	t.Run("ACC-L3-029-500-callback-plus-500-query", func(t *testing.T) {
		fx := insertRefundAcceptanceFixture(t, db, ids, "pending", 400, 1000)
		defer cleanupRefundAcceptance(t, db, fx)
		state := successState(fx)
		provider := &acceptanceProvider{callbackState: state}
		service := NewService(cfg, db, ids, provider)
		errorsCh := make(chan error, 1000)
		var wg sync.WaitGroup
		for index := 0; index < 500; index++ {
			wg.Add(2)
			go func(index int) {
				defer wg.Done()
				request, _ := http.NewRequest(http.MethodPost, "/callback", nil)
				request.Header.Set("X-Event-ID", fmt.Sprintf("race-%s-%d", fx.refundNo, index))
				if err := service.ProcessCallback(context.Background(), "wechat", request, []byte(fmt.Sprintf(`{"event":%d}`, index))); err != nil {
					errorsCh <- err
				}
			}(index)
			go func() {
				defer wg.Done()
				if err := service.ApplyProviderState(context.Background(), fx.refundID, state); err != nil {
					errorsCh <- err
				}
			}()
		}
		wg.Wait()
		close(errorsCh)
		for err := range errorsCh {
			t.Fatalf("ACC-029 concurrent error: %v", err)
		}
		assertRefundLedger(t, db, fx, "succeeded", 400)
		var callbacks int64
		db.Table("refund_callbacks").Where("refund_id=?", fx.refundID).Count(&callbacks)
		if callbacks != 500 {
			t.Fatalf("ACC-029 callbacks=%d", callbacks)
		}
	})

	t.Run("ACC-L3-030-db-result-survives-no-redis-or-mq", func(t *testing.T) {
		fx := insertRefundAcceptanceFixture(t, db, ids, "pending", 400, 1000)
		defer cleanupRefundAcceptance(t, db, fx)
		service := NewService(cfg, db, ids, nil)
		if err := service.ApplyProviderState(context.Background(), fx.refundID, successState(fx)); err != nil {
			t.Fatal(err)
		}
		assertRefundLedger(t, db, fx, "succeeded", 400)
		var outbox int64
		db.Table("outbox_events").Where("aggregate_type='refund' AND aggregate_id=? AND event_type='refund.succeeded'", fx.refundID).Count(&outbox)
		if outbox != 1 {
			t.Fatalf("ACC-030 outbox=%d", outbox)
		}
	})

	t.Run("ACC-L3-031-two-workers-single-provider-call", func(t *testing.T) {
		fx := insertRefundAcceptanceFixture(t, db, ids, "creating", 400, 1000)
		defer cleanupRefundAcceptance(t, db, fx)
		provider := &acceptanceProvider{refundState: successState(fx), delay: 100 * time.Millisecond}
		service := NewService(cfg, db, ids, provider)
		first := NewWorker(cfg, service, provider, slog.New(slog.NewTextHandler(io.Discard, nil)))
		first.instance = "worker-a"
		second := NewWorker(cfg, service, provider, slog.New(slog.NewTextHandler(io.Discard, nil)))
		second.instance = "worker-b"
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); first.runBatch(context.Background()) }()
		go func() { defer wg.Done(); second.runBatch(context.Background()) }()
		wg.Wait()
		if provider.refundCalls.Load() != 1 {
			t.Fatalf("ACC-031 provider calls=%d", provider.refundCalls.Load())
		}
		assertRefundLedger(t, db, fx, "succeeded", 400)
	})
}

// openRefundAcceptanceDB 解密并返回退款验收数据库。
func openRefundAcceptanceDB(t *testing.T) *gorm.DB {
	t.Helper()
	cfg := config.Load()
	db, err := mysql.Open(context.Background(), cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// insertRefundAcceptanceFixture 插入退款验收测试夹具。
func insertRefundAcceptanceFixture(t *testing.T, db *gorm.DB, ids *snowflake.Generator, status string, amount, total int64) refundAcceptanceFixture {
	t.Helper()
	fx := refundAcceptanceFixture{orderID: ids.Next(), paymentID: ids.Next(), afterSaleID: ids.Next(), itemID: ids.Next(), refundID: ids.Next(), amount: amount, total: total}
	fx.orderNo = "RF-ACC-ORDER-" + refundID(fx.orderID)
	fx.paymentNo = "RF-ACC-PAY-" + refundID(fx.paymentID)
	fx.afterSaleNo = "RF-ACC-AS-" + refundID(fx.afterSaleID)
	fx.refundNo = "RF-ACC-" + refundID(fx.refundID)
	now := time.Now().UTC()
	refundMustExec(t, db, "INSERT INTO orders (id,order_no,customer_id,merchant_id,shop_id,status,pay_status,delivery_status,goods_amount,payable_amount,paid_amount,refunded_amount,after_sale_status) VALUES (?,?,?,?,?,'completed','succeeded','completed',?,?,?,0,'processing')", fx.orderID, fx.orderNo, ids.Next(), ids.Next(), ids.Next(), total, total, total)
	refundMustExec(t, db, "INSERT INTO payments (id,payment_no,order_id,customer_id,channel,provider,status,amount,refunded_amount,currency) VALUES (?,?,?,?,'miniapp','wechat','succeeded',?,0,'CNY')", fx.paymentID, fx.paymentNo, fx.orderID, ids.Next(), total)
	refundMustExec(t, db, "INSERT INTO after_sales (id,after_sale_no,order_id,customer_id,merchant_id,shop_id,type,requested_resolution,approved_resolution,status,requested_amount,approved_amount,description,submitted_at) VALUES (?,?,?,1,1,1,'damaged','refund_only','refund_only','refund_processing',?,?,'refund acceptance',?)", fx.afterSaleID, fx.afterSaleNo, fx.orderID, amount, amount, now)
	refundMustExec(t, db, "INSERT INTO after_sale_items (id,after_sale_id,order_id,order_item_id,shop_product_id,product_id,requested_quantity,approved_quantity,requested_amount,approved_amount) VALUES (?,?,?,?,?,?,1,1,?,?)", fx.itemID, fx.afterSaleID, fx.orderID, ids.Next(), ids.Next(), ids.Next(), amount, amount)
	refundMustExec(t, db, "INSERT INTO refunds (id,refund_no,after_sale_id,order_id,payment_id,provider,status,amount,total_amount,currency,requested_at,next_retry_at) VALUES (?,?,?,?,?,'wechat',?,?,?,'CNY',?,?)", fx.refundID, fx.refundNo, fx.afterSaleID, fx.orderID, fx.paymentID, status, amount, total, now, now.Add(-time.Second))
	refundMustExec(t, db, "INSERT INTO refund_items (id,refund_id,after_sale_item_id,amount,quantity) VALUES (?,?,?,?,1)", ids.Next(), fx.refundID, fx.itemID, amount)
	return fx
}

// successState 返回成功状态状态。
func successState(fx refundAcceptanceFixture) State {
	now := time.Now().UTC()
	return State{ProviderRefundID: "wx-" + fx.refundNo, RefundNo: fx.refundNo, PaymentNo: fx.paymentNo, Status: "SUCCESS", Currency: "CNY", Amount: fx.amount, TotalAmount: fx.total, SucceededAt: &now}
}

// assertRefundLedger 断言退款账本符合预期。
func assertRefundLedger(t *testing.T, db *gorm.DB, fx refundAcceptanceFixture, status string, amount int64) {
	t.Helper()
	var row Row
	var payment Payment
	var order Order
	var afterSale AfterSale
	var item AfterSaleItem
	if err := db.First(&row, fx.refundID).Error; err != nil {
		t.Fatal(err)
	}
	db.First(&payment, fx.paymentID)
	db.First(&order, fx.orderID)
	db.First(&afterSale, fx.afterSaleID)
	db.First(&item, fx.itemID)
	if row.Status != status || payment.RefundedAmount != amount || order.RefundedAmount != amount || afterSale.RefundedAmount != amount || item.RefundedAmount != amount {
		t.Fatalf("ledger status=%s/%s payment=%d order=%d aftersale=%d item=%d want=%d", row.Status, status, payment.RefundedAmount, order.RefundedAmount, afterSale.RefundedAmount, item.RefundedAmount, amount)
	}
}

// cleanupRefundAcceptance 清理退款验收。
func cleanupRefundAcceptance(t *testing.T, db *gorm.DB, fx refundAcceptanceFixture) {
	t.Helper()
	queries := []string{"DELETE FROM refund_callbacks WHERE refund_id=?", "DELETE FROM refund_items WHERE refund_id=?", "DELETE FROM refunds WHERE id=?", "DELETE FROM outbox_events WHERE (aggregate_type='refund' AND aggregate_id=?) OR (aggregate_type='after_sale' AND aggregate_id=?)", "DELETE FROM audit_logs WHERE (resource_type='refund' AND resource_id=?) OR (resource_type='after_sale' AND resource_id=?)", "DELETE FROM after_sale_items WHERE after_sale_id=?", "DELETE FROM after_sales WHERE id=?", "DELETE FROM payments WHERE id=?", "DELETE FROM orders WHERE id=?"}
	for _, query := range queries {
		args := []any{fx.refundID}
		if query == queries[3] || query == queries[4] {
			args = []any{fx.refundID, fx.afterSaleID}
		} else if query == queries[5] {
			args = []any{fx.afterSaleID}
		} else if query == queries[6] {
			args = []any{fx.afterSaleID}
		} else if query == queries[7] {
			args = []any{fx.paymentID}
		} else if query == queries[8] {
			args = []any{fx.orderID}
		}
		if err := db.Exec(query, args...).Error; err != nil {
			t.Logf("cleanup: %v", err)
		}
	}
}

// refundAdmin 返回退款管理端。
func refundAdmin(id uint64) auth.Claims {
	return auth.Claims{AccountType: "admin", AdminUserID: refundID(id), Permissions: []string{"refund:list", "refund:view", "refund:retry", "refund:exception"}}
}

// refundErrorCode 返回退款错误代码。
func refundErrorCode(err error) string {
	if err == nil {
		return ""
	}
	return problem.FromError(err).ErrorCode
}

// refundID 返回退款ID。
func refundID(id uint64) string { return strconv.FormatUint(id, 10) }

// refundMustExec 处理退款 Must Exec相关逻辑。
func refundMustExec(t *testing.T, db *gorm.DB, query string, args ...any) {
	t.Helper()
	if err := db.Exec(query, args...).Error; err != nil {
		t.Fatalf("fixture SQL: %v", err)
	}
}
