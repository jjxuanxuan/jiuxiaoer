package order_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/paygateway"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type paymentAcceptanceFixture struct {
	orderID, paymentID, shopProductID, stockID, customerID uint64
	paymentNo                                              string
	amount                                                 int64
}

type paymentAcceptanceProvider struct {
	state      order.ProviderPaymentState
	queryErr   error
	callback   order.PaymentCallbackEvent
	delay      time.Duration
	queryCalls atomic.Int64
	closeCalls atomic.Int64
}

func (p *paymentAcceptanceProvider) Code() string { return "wechat" }
func (p *paymentAcceptanceProvider) Create(context.Context, order.CreateProviderPaymentInput) (order.ProviderPaymentResult, error) {
	return order.ProviderPaymentResult{}, nil
}
func (p *paymentAcceptanceProvider) Query(context.Context, string) (order.ProviderPaymentState, error) {
	p.queryCalls.Add(1)
	if p.delay > 0 {
		time.Sleep(p.delay)
	}
	return p.state, p.queryErr
}
func (p *paymentAcceptanceProvider) Close(context.Context, string) (order.ProviderOperationResult, error) {
	p.closeCalls.Add(1)
	return order.ProviderOperationResult{}, nil
}
func (p *paymentAcceptanceProvider) ParseCallback(context.Context, *http.Request) (order.PaymentCallbackEvent, error) {
	return p.callback, nil
}
func (p *paymentAcceptanceProvider) Shutdown() {}

func TestPaymentConfirmationAcceptance(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run payment confirmation acceptance tests")
	}
	db := openPaymentAcceptanceDB(t)
	ids := snowflake.New(989)
	cfg := config.Load()
	cfg.WeChat.PayEnabled = true
	cfg.WeChat.PayMockEnabled = true

	t.Run("ACC-PAY-001-success-and-repeat-do-not-double-apply", func(t *testing.T) {
		fx := insertPaymentAcceptanceFixture(t, db, ids, "pending", time.Now().Add(10*time.Minute))
		defer cleanupRaceFixture(db, fx.orderID, fx.paymentID, fx.shopProductID, fx.stockID)
		state := paymentSuccessState(fx)
		state.RequestID = "wechat-confirm-success-request-id"
		provider := &paymentAcceptanceProvider{state: state}
		service := order.NewService(cfg, db, ids).WithPaymentProvider(provider, metrics.New("confirm-success", ""))
		claims := paymentCustomer(fx.customerID)

		result, err := service.ConfirmPayment(context.Background(), &claims, strconv.FormatUint(fx.orderID, 10))
		if err != nil || result.Status != "succeeded" {
			t.Fatalf("confirm result=%+v err=%v", result, err)
		}
		repeat, err := service.ConfirmPayment(context.Background(), &claims, strconv.FormatUint(fx.orderID, 10))
		if err != nil || repeat.Status != "succeeded" || provider.queryCalls.Load() != 1 {
			t.Fatalf("repeat=%+v calls=%d err=%v", repeat, provider.queryCalls.Load(), err)
		}
		assertPaymentAcceptanceLedger(t, db, fx, "paid", "succeeded", 0)
	})

	for _, tc := range []struct {
		providerStatus string
		localStatus    string
	}{
		{providerStatus: "NOTPAY", localStatus: "pending"},
		{providerStatus: "USERPAYING", localStatus: "pending"},
		{providerStatus: "CLOSED", localStatus: "closed"},
	} {
		t.Run("ACC-PAY-002-map-"+tc.providerStatus, func(t *testing.T) {
			fx := insertPaymentAcceptanceFixture(t, db, ids, "pending", time.Now().Add(10*time.Minute))
			defer cleanupRaceFixture(db, fx.orderID, fx.paymentID, fx.shopProductID, fx.stockID)
			state := paymentSuccessState(fx)
			state.Status = tc.providerStatus
			state.PaidAt = nil
			provider := &paymentAcceptanceProvider{state: state}
			service := order.NewService(cfg, db, ids).WithPaymentProvider(provider, metrics.New("confirm-map", ""))
			claims := paymentCustomer(fx.customerID)
			result, err := service.ConfirmPayment(context.Background(), &claims, strconv.FormatUint(fx.orderID, 10))
			if err != nil || result.Status != tc.localStatus || result.ProviderStatus != tc.providerStatus {
				t.Fatalf("mapped result=%+v err=%v", result, err)
			}
			assertPaymentAcceptanceLedger(t, db, fx, "pending_payment", tc.localStatus, 1)
		})
	}

	t.Run("ACC-PAY-003-query-timeout-preserves-local-state", func(t *testing.T) {
		fx := insertPaymentAcceptanceFixture(t, db, ids, "pending", time.Now().Add(10*time.Minute))
		defer cleanupRaceFixture(db, fx.orderID, fx.paymentID, fx.shopProductID, fx.stockID)
		provider := &paymentAcceptanceProvider{queryErr: &paygateway.ProviderError{Operation: "payment.query", HTTPStatus: http.StatusInternalServerError, Code: "SYSTEM_ERROR", RequestID: "wechat-timeout-request-id", Retryable: true}}
		service := order.NewService(cfg, db, ids).WithPaymentProvider(provider, metrics.New("confirm-timeout", ""))
		claims := paymentCustomer(fx.customerID)
		_, err := service.ConfirmPayment(context.Background(), &claims, strconv.FormatUint(fx.orderID, 10))
		if problem.FromError(err).ErrorCode != "PAYMENT_CONFIRM_RETRYABLE" {
			t.Fatalf("unexpected error: %v", err)
		}
		assertPaymentAcceptanceLedger(t, db, fx, "pending_payment", "pending", 1)
	})

	t.Run("ACC-PAY-004-provider-mismatch-enters-exception", func(t *testing.T) {
		fx := insertPaymentAcceptanceFixture(t, db, ids, "pending", time.Now().Add(10*time.Minute))
		defer cleanupRaceFixture(db, fx.orderID, fx.paymentID, fx.shopProductID, fx.stockID)
		state := paymentSuccessState(fx)
		state.Amount++
		state.RequestID = "wechat-mismatch-request-id"
		provider := &paymentAcceptanceProvider{state: state}
		service := order.NewService(cfg, db, ids).WithPaymentProvider(provider, metrics.New("confirm-mismatch", ""))
		claims := paymentCustomer(fx.customerID)
		_, err := service.ConfirmPayment(context.Background(), &claims, strconv.FormatUint(fx.orderID, 10))
		if problem.FromError(err).ErrorCode != "PAYMENT_PROVIDER_DATA_MISMATCH" {
			t.Fatalf("unexpected mismatch error: %v", err)
		}
		assertPaymentAcceptanceLedger(t, db, fx, "payment_exception", "exception", 1)
		var alerts int64
		db.Table("outbox_events").Where("aggregate_type='payment' AND aggregate_id=? AND event_type='payment.exception'", fx.paymentID).Count(&alerts)
		if alerts != 1 {
			t.Fatalf("payment exception alerts=%d", alerts)
		}
	})

	t.Run("ACC-PAY-005-non-owner-is-rejected-before-provider-call", func(t *testing.T) {
		fx := insertPaymentAcceptanceFixture(t, db, ids, "pending", time.Now().Add(10*time.Minute))
		defer cleanupRaceFixture(db, fx.orderID, fx.paymentID, fx.shopProductID, fx.stockID)
		provider := &paymentAcceptanceProvider{state: paymentSuccessState(fx)}
		service := order.NewService(cfg, db, ids).WithPaymentProvider(provider, metrics.New("confirm-owner", ""))
		other := paymentCustomer(ids.Next())
		_, err := service.ConfirmPayment(context.Background(), &other, strconv.FormatUint(fx.orderID, 10))
		if problem.FromError(err).ErrorCode != "PAYMENT_NOT_FOUND" || provider.queryCalls.Load() != 0 {
			t.Fatalf("non-owner err=%v query_calls=%d", err, provider.queryCalls.Load())
		}
	})

	t.Run("ACC-PAY-006-confirm-and-callback-concurrency-apply-once", func(t *testing.T) {
		fx := insertPaymentAcceptanceFixture(t, db, ids, "pending", time.Now().Add(10*time.Minute))
		defer cleanupRaceFixture(db, fx.orderID, fx.paymentID, fx.shopProductID, fx.stockID)
		state := paymentSuccessState(fx)
		provider := &paymentAcceptanceProvider{state: state, delay: 2 * time.Millisecond, callback: order.PaymentCallbackEvent{EventID: "confirm-callback-race-" + fx.paymentNo, ProviderTradeNo: state.ProviderTradeNo, PaymentNo: state.PaymentNo, Status: state.Status, Amount: state.Amount, Currency: state.Currency, PaidAt: state.PaidAt}}
		service := order.NewService(cfg, db, ids).WithPaymentProvider(provider, metrics.New("confirm-callback-race", ""))
		claims := paymentCustomer(fx.customerID)
		var wg sync.WaitGroup
		errs := make(chan error, 40)
		for i := 0; i < 20; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, err := service.ConfirmPayment(context.Background(), &claims, strconv.FormatUint(fx.orderID, 10))
				if err != nil {
					errs <- err
				}
			}()
			go func() {
				defer wg.Done()
				req, _ := http.NewRequest(http.MethodPost, "/callback", bytes.NewReader([]byte(`{"race":true}`)))
				if err := service.ProcessPaymentCallback(context.Background(), "wechat", req, []byte(`{"race":true}`)); err != nil {
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent apply: %v", err)
		}
		assertPaymentAcceptanceLedger(t, db, fx, "paid", "succeeded", 0)
		var deductions int64
		db.Table("stock_records").Where("source_type='payment' AND source_id=? AND change_type='deduct'", fx.paymentID).Count(&deductions)
		if deductions != 1 {
			t.Fatalf("stock deductions=%d", deductions)
		}
	})

	t.Run("ACC-PAY-007-confirm-and-expiry-concurrency-do-not-close-paid", func(t *testing.T) {
		fx := insertPaymentAcceptanceFixture(t, db, ids, "pending", time.Now().Add(-time.Second))
		defer cleanupRaceFixture(db, fx.orderID, fx.paymentID, fx.shopProductID, fx.stockID)
		provider := &paymentAcceptanceProvider{state: paymentSuccessState(fx), delay: time.Millisecond}
		registry := metrics.New("confirm-expiry-race", "")
		service := order.NewService(cfg, db, ids).WithPaymentProvider(provider, registry)
		worker := order.NewExpiryWorker(cfg, db, ids, registry, slog.New(slog.NewTextHandler(io.Discard, nil)), provider)
		claims := paymentCustomer(fx.customerID)
		var wg sync.WaitGroup
		errs := make(chan error, 20)
		for i := 0; i < 10; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				if _, err := service.ConfirmPayment(context.Background(), &claims, strconv.FormatUint(fx.orderID, 10)); err != nil {
					errs <- err
				}
			}()
			go func() {
				defer wg.Done()
				if _, err := worker.ExpireBatch(context.Background(), time.Now(), 1); err != nil {
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("confirm/expiry race: %v", err)
		}
		assertPaymentAcceptanceLedger(t, db, fx, "paid", "succeeded", 0)
		if provider.closeCalls.Load() != 0 {
			t.Fatalf("provider close calls=%d", provider.closeCalls.Load())
		}
	})

	t.Run("ACC-PAY-008-callback-body-boundaries", func(t *testing.T) {
		fx := insertPaymentAcceptanceFixture(t, db, ids, "pending", time.Now().Add(10*time.Minute))
		defer cleanupRaceFixture(db, fx.orderID, fx.paymentID, fx.shopProductID, fx.stockID)
		state := paymentSuccessState(fx)
		provider := &paymentAcceptanceProvider{callback: order.PaymentCallbackEvent{EventID: "callback-boundary-" + fx.paymentNo, ProviderTradeNo: state.ProviderTradeNo, PaymentNo: state.PaymentNo, Status: state.Status, Amount: state.Amount, Currency: state.Currency, PaidAt: state.PaidAt}}
		service := order.NewService(cfg, db, ids).WithPaymentProvider(provider, metrics.New("callback-boundary", ""))
		router := gin.New()
		router.POST("/payments/:provider/callbacks", order.NewHandler(service).PaymentCallback)

		tooLarge := httptest.NewRequest(http.MethodPost, "/payments/wechat/callbacks", bytes.NewReader(bytes.Repeat([]byte{'x'}, int(paygateway.MaxCallbackBodyBytes+1))))
		tooLargeResp := httptest.NewRecorder()
		router.ServeHTTP(tooLargeResp, tooLarge)
		if tooLargeResp.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("too-large status=%d", tooLargeResp.Code)
		}

		readFailure := httptest.NewRequest(http.MethodPost, "/payments/wechat/callbacks", nil)
		readFailure.Body = io.NopCloser(errorReader{})
		readFailureResp := httptest.NewRecorder()
		router.ServeHTTP(readFailureResp, readFailure)
		if readFailureResp.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("read-failure status=%d", readFailureResp.Code)
		}

		maximum := httptest.NewRequest(http.MethodPost, "/payments/wechat/callbacks", bytes.NewReader(bytes.Repeat([]byte{'y'}, int(paygateway.MaxCallbackBodyBytes))))
		maximumResp := httptest.NewRecorder()
		router.ServeHTTP(maximumResp, maximum)
		if maximumResp.Code != http.StatusOK {
			t.Fatalf("maximum legal body status=%d body=%s", maximumResp.Code, maximumResp.Body.String())
		}
		assertPaymentAcceptanceLedger(t, db, fx, "paid", "succeeded", 0)
	})

	t.Run("ACC-PAY-009-pending-worker-uses-explicit-reconcile-schedule", func(t *testing.T) {
		fx := insertPaymentAcceptanceFixture(t, db, ids, "pending", time.Now().Add(10*time.Minute))
		defer cleanupRaceFixture(db, fx.orderID, fx.paymentID, fx.shopProductID, fx.stockID)
		state := paymentSuccessState(fx)
		state.Status = "NOTPAY"
		state.PaidAt = nil
		provider := &paymentAcceptanceProvider{state: state}
		worker := order.NewExpiryWorker(cfg, db, ids, metrics.New("pending-reconcile", ""), slog.New(slog.NewTextHandler(io.Discard, nil)), provider)
		processed, err := worker.ReconcileCreatingBatch(context.Background(), time.Now(), 1)
		if err != nil || processed != 1 || provider.queryCalls.Load() != 1 {
			t.Fatalf("first reconcile processed=%d calls=%d err=%v", processed, provider.queryCalls.Load(), err)
		}
		var payment order.Payment
		if err := db.First(&payment, fx.paymentID).Error; err != nil {
			t.Fatal(err)
		}
		if payment.Status != "pending" || payment.ReconcileAttempts != 1 || payment.NextReconcileAt == nil || !payment.NextReconcileAt.After(time.Now()) {
			t.Fatalf("scheduled payment=%+v", payment)
		}

		provider.state = paymentSuccessState(fx)
		if err := db.Model(&order.Payment{}).Where("id=?", fx.paymentID).Update("next_reconcile_at", time.Now().Add(-time.Second)).Error; err != nil {
			t.Fatal(err)
		}
		processed, err = worker.ReconcileCreatingBatch(context.Background(), time.Now(), 1)
		if err != nil || processed != 1 || provider.queryCalls.Load() != 2 {
			t.Fatalf("success reconcile processed=%d calls=%d err=%v", processed, provider.queryCalls.Load(), err)
		}
		assertPaymentAcceptanceLedger(t, db, fx, "paid", "succeeded", 0)
	})

	t.Run("ACC-PAY-010-two-workers-claim-pending-payment-once", func(t *testing.T) {
		fx := insertPaymentAcceptanceFixture(t, db, ids, "pending", time.Now().Add(10*time.Minute))
		defer cleanupRaceFixture(db, fx.orderID, fx.paymentID, fx.shopProductID, fx.stockID)
		provider := &paymentAcceptanceProvider{state: paymentSuccessState(fx), delay: 50 * time.Millisecond}
		first := order.NewExpiryWorker(cfg, db, ids, metrics.New("pending-worker-a", ""), slog.New(slog.NewTextHandler(io.Discard, nil)), provider)
		second := order.NewExpiryWorker(cfg, db, ids, metrics.New("pending-worker-b", ""), slog.New(slog.NewTextHandler(io.Discard, nil)), provider)
		var wg sync.WaitGroup
		results := make(chan error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := first.ReconcileCreatingBatch(context.Background(), time.Now(), 1)
			results <- err
		}()
		go func() {
			defer wg.Done()
			_, err := second.ReconcileCreatingBatch(context.Background(), time.Now(), 1)
			results <- err
		}()
		wg.Wait()
		close(results)
		for err := range results {
			if err != nil {
				t.Fatal(err)
			}
		}
		if provider.queryCalls.Load() != 1 {
			t.Fatalf("provider query calls=%d", provider.queryCalls.Load())
		}
		assertPaymentAcceptanceLedger(t, db, fx, "paid", "succeeded", 0)
	})

	t.Run("ACC-PAY-011-notpay-without-optional-amount-remains-pending", func(t *testing.T) {
		fx := insertPaymentAcceptanceFixture(t, db, ids, "pending", time.Now().Add(10*time.Minute))
		defer cleanupRaceFixture(db, fx.orderID, fx.paymentID, fx.shopProductID, fx.stockID)
		provider := &paymentAcceptanceProvider{state: order.ProviderPaymentState{PaymentNo: fx.paymentNo, Status: "NOTPAY"}}
		service := order.NewService(cfg, db, ids).WithPaymentProvider(provider, metrics.New("confirm-optional-amount", ""))
		claims := paymentCustomer(fx.customerID)
		result, err := service.ConfirmPayment(context.Background(), &claims, strconv.FormatUint(fx.orderID, 10))
		if err != nil || result.Status != "pending" || result.ProviderStatus != "NOTPAY" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		assertPaymentAcceptanceLedger(t, db, fx, "pending_payment", "pending", 1)
	})

	t.Run("ACC-PAY-012-success-without-amount-is-rejected", func(t *testing.T) {
		fx := insertPaymentAcceptanceFixture(t, db, ids, "pending", time.Now().Add(10*time.Minute))
		defer cleanupRaceFixture(db, fx.orderID, fx.paymentID, fx.shopProductID, fx.stockID)
		provider := &paymentAcceptanceProvider{state: order.ProviderPaymentState{ProviderTradeNo: "wx-" + fx.paymentNo, PaymentNo: fx.paymentNo, Status: "SUCCESS"}}
		service := order.NewService(cfg, db, ids).WithPaymentProvider(provider, metrics.New("confirm-missing-success-amount", ""))
		claims := paymentCustomer(fx.customerID)
		_, err := service.ConfirmPayment(context.Background(), &claims, strconv.FormatUint(fx.orderID, 10))
		if problem.FromError(err).ErrorCode != "PAYMENT_PROVIDER_DATA_MISMATCH" {
			t.Fatalf("unexpected error: %v", err)
		}
		assertPaymentAcceptanceLedger(t, db, fx, "payment_exception", "exception", 1)
	})

	for _, tc := range []struct {
		name, appID, mchID string
	}{
		{name: "appid", appID: "wx-wrong", mchID: "1900000001"},
		{name: "mchid", appID: "wx-expected", mchID: "1900000002"},
	} {
		t.Run("ACC-PAY-013-query-"+tc.name+"-mismatch-is-rejected", func(t *testing.T) {
			fx := insertPaymentAcceptanceFixture(t, db, ids, "pending", time.Now().Add(10*time.Minute))
			defer cleanupRaceFixture(db, fx.orderID, fx.paymentID, fx.shopProductID, fx.stockID)
			strictCfg := cfg
			strictCfg.WeChat.PayMockEnabled = false
			strictCfg.WeChat.MiniAppID = "wx-expected"
			strictCfg.WeChat.PayMchID = "1900000001"
			state := paymentSuccessState(fx)
			state.AppID, state.MchID = tc.appID, tc.mchID
			provider := &paymentAcceptanceProvider{state: state}
			service := order.NewService(strictCfg, db, ids).WithPaymentProvider(provider, metrics.New("confirm-identity-mismatch", ""))
			claims := paymentCustomer(fx.customerID)
			_, err := service.ConfirmPayment(context.Background(), &claims, strconv.FormatUint(fx.orderID, 10))
			if problem.FromError(err).ErrorCode != "PAYMENT_PROVIDER_DATA_MISMATCH" {
				t.Fatalf("unexpected error: %v", err)
			}
			assertPaymentAcceptanceLedger(t, db, fx, "payment_exception", "exception", 1)
			var payment order.Payment
			if err := db.First(&payment, fx.paymentID).Error; err != nil || payment.FailureCode == nil || *payment.FailureCode != "PROVIDER_IDENTITY_MISMATCH" {
				t.Fatalf("payment=%+v err=%v", payment, err)
			}
		})
	}

	t.Run("ACC-PAY-014-stale-notpay-cannot-regress-succeeded-evidence", func(t *testing.T) {
		fx := insertPaymentAcceptanceFixture(t, db, ids, "pending", time.Now().Add(10*time.Minute))
		defer cleanupRaceFixture(db, fx.orderID, fx.paymentID, fx.shopProductID, fx.stockID)
		service := order.NewService(cfg, db, ids).WithPaymentProvider(&paymentAcceptanceProvider{}, metrics.New("confirm-success-terminal", ""))

		result, err := service.ApplyProviderPaymentState(context.Background(), fx.paymentNo, "wechat", paymentSuccessState(fx), "system", 0, "success:"+fx.paymentNo)
		if err != nil || result.Status != "succeeded" || result.ProviderStatus != "SUCCESS" {
			t.Fatalf("success result=%+v err=%v", result, err)
		}

		result, err = service.ApplyProviderPaymentState(context.Background(), fx.paymentNo, "wechat", order.ProviderPaymentState{PaymentNo: fx.paymentNo, Status: "NOTPAY"}, "system", 0, "stale-notpay:"+fx.paymentNo)
		if err != nil || result.Status != "succeeded" || result.ProviderStatus != "SUCCESS" {
			t.Fatalf("stale result=%+v err=%v", result, err)
		}

		var payment order.Payment
		if err := db.First(&payment, fx.paymentID).Error; err != nil {
			t.Fatal(err)
		}
		if payment.Status != "succeeded" || payment.ProviderStatus == nil || *payment.ProviderStatus != "SUCCESS" || payment.NextReconcileAt != nil {
			t.Fatalf("regressed payment=%+v", payment)
		}
		assertPaymentAcceptanceLedger(t, db, fx, "paid", "succeeded", 0)
	})
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func openPaymentAcceptanceDB(t *testing.T) *gorm.DB {
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

func insertPaymentAcceptanceFixture(t *testing.T, db *gorm.DB, ids *snowflake.Generator, paymentStatus string, expiresAt time.Time) paymentAcceptanceFixture {
	t.Helper()
	fx := paymentAcceptanceFixture{orderID: ids.Next(), paymentID: ids.Next(), shopProductID: ids.Next(), stockID: ids.Next(), customerID: ids.Next(), amount: 100}
	fx.paymentNo = "PAY-CONFIRM-" + strconv.FormatUint(fx.paymentID, 10)
	if err := db.Create(&order.ProductStock{ID: fx.stockID, ShopProductID: fx.shopProductID, ShopID: 4201, ProductID: 5004, AvailableQty: 9, ReservedQty: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&order.Order{ID: fx.orderID, OrderNo: "ORDER-CONFIRM-" + strconv.FormatUint(fx.orderID, 10), CustomerID: fx.customerID, MerchantID: 4001, ShopID: 4201, Status: "pending_payment", PayStatus: "pending", DeliveryStatus: "pending", GoodsAmount: fx.amount, PayableAmount: fx.amount, ExpiresAt: &expiresAt}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&order.OrderItem{ID: ids.Next(), OrderID: fx.orderID, ShopProductID: fx.shopProductID, ProductID: 5004, ProductSnapshot: []byte(`{"name":"confirmation"}`), Quantity: 1, SalePriceAmount: fx.amount, TotalAmount: fx.amount}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&order.Payment{ID: fx.paymentID, PaymentNo: fx.paymentNo, OrderID: fx.orderID, CustomerID: fx.customerID, Channel: "miniapp", Provider: "wechat", Status: paymentStatus, ProviderStatus: stringPointer("NOTPAY"), Amount: fx.amount, Currency: "CNY", ExpiresAt: &expiresAt}).Error; err != nil {
		t.Fatal(err)
	}
	return fx
}

func paymentSuccessState(fx paymentAcceptanceFixture) order.ProviderPaymentState {
	now := time.Now().UTC()
	return order.ProviderPaymentState{ProviderTradeNo: "wx-" + fx.paymentNo, PaymentNo: fx.paymentNo, Status: "SUCCESS", Amount: fx.amount, Currency: "CNY", PaidAt: &now}
}

func paymentCustomer(customerID uint64) auth.Claims {
	return auth.Claims{AccountType: "customer", CustomerID: strconv.FormatUint(customerID, 10)}
}

func assertPaymentAcceptanceLedger(t *testing.T, db *gorm.DB, fx paymentAcceptanceFixture, orderStatus, paymentStatus string, reserved int) {
	t.Helper()
	var orderRow order.Order
	var payment order.Payment
	var stock order.ProductStock
	if err := db.First(&orderRow, fx.orderID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&payment, fx.paymentID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&stock, fx.stockID).Error; err != nil {
		t.Fatal(err)
	}
	if orderRow.Status != orderStatus || payment.Status != paymentStatus || stock.ReservedQty != reserved {
		t.Fatalf("ledger order=%s/%s payment=%s/%s reserved=%d/%d", orderRow.Status, orderStatus, payment.Status, paymentStatus, stock.ReservedQty, reserved)
	}
}
