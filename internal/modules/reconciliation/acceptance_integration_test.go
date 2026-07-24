package reconciliation

import (
	"context"
	"crypto/sha1" // #nosec G505 -- 测试夹具模拟微信要求的摘要。
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/paygateway"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type billFixtureProvider struct {
	body          string
	hash          string
	err           error
	delay         time.Duration
	calls         atomic.Int64
	requestID     string
	downloadReqID string
}

type billCall struct {
	date     string
	billType string
}

type recordingNoStatementProvider struct {
	mu    sync.Mutex
	calls []billCall
}

func (p *recordingNoStatementProvider) OpenBill(_ context.Context, date time.Time, billType string) (BillFile, error) {
	p.mu.Lock()
	p.calls = append(p.calls, billCall{date: date.Format("2006-01-02"), billType: billType})
	p.mu.Unlock()
	return BillFile{}, &paygateway.ProviderError{Operation: "bill.apply", Code: "NO_STATEMENT_EXIST", HTTPStatus: 404, Retryable: false}
}

func (p *recordingNoStatementProvider) recordedCalls() []billCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]billCall(nil), p.calls...)
}

func (p *billFixtureProvider) OpenBill(ctx context.Context, _ time.Time, _ string) (BillFile, error) {
	p.calls.Add(1)
	if p.delay > 0 {
		select {
		case <-ctx.Done():
			return BillFile{}, ctx.Err()
		case <-time.After(p.delay):
		}
	}
	if p.err != nil {
		return BillFile{}, p.err
	}
	hash := p.hash
	if hash == "" {
		sum := sha1.Sum([]byte(p.body)) // #nosec G401 -- 账单夹具强制使用该摘要算法。
		hash = hex.EncodeToString(sum[:])
	}
	return BillFile{Body: io.NopCloser(strings.NewReader(p.body)), HashType: "SHA1", ExpectedHash: hash, ProviderRequestID: p.requestID, DownloadRequestID: p.downloadReqID}, nil
}

func TestBillReconciliationAcceptance(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run bill reconciliation acceptance tests")
	}
	db := openReconciliationDB(t)
	ids := snowflake.New(987)
	cfg := config.Load()
	cfg.Reconciliation.Enabled = true
	cfg.Reconciliation.InsertBatchSize = 2
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("ACC-BILL-001-exact-match-and-repeat-are-idempotent", func(t *testing.T) {
		date := reconciliationDate(10)
		cleanupReconciliationDate(t, db, date)
		paymentID, refundID := insertReconciliationPaymentAndRefund(t, db, ids, date, "PAY-BILL-1", "420000001", 1, "RF-BILL-1", "503000001", 1)
		defer cleanupReconciliationFixture(t, db, date, paymentID, refundID)
		provider := &billFixtureProvider{body: tradeBill(date, "PAY-BILL-1", "420000001", "0.01", "RF-BILL-1", "503000001", "0.01"), requestID: "apply-1", downloadReqID: "download-1"}
		service := NewService(cfg, db, ids, provider, log)
		result, err := service.RunBill(context.Background(), date, BillTypeTradeAll)
		if err != nil || result.Status != "succeeded" || result.Rows != 2 || result.Discrepancies != 0 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		repeat, err := service.RunBill(context.Background(), date, BillTypeTradeAll)
		if err != nil || !repeat.AlreadyCompleted || provider.calls.Load() != 1 {
			t.Fatalf("repeat=%+v calls=%d err=%v", repeat, provider.calls.Load(), err)
		}
		var observations int64
		db.Model(&Observation{}).Where("run_id=?", result.RunID).Count(&observations)
		if observations != 0 {
			t.Fatalf("observations=%d", observations)
		}
	})

	t.Run("ACC-BILL-002-differences-never-rewrite-local-money", func(t *testing.T) {
		date := reconciliationDate(11)
		cleanupReconciliationDate(t, db, date)
		paymentID, refundID := insertReconciliationPaymentAndRefund(t, db, ids, date, "PAY-BILL-2", "local-trade-2", 2, "RF-BILL-2", "local-refund-2", 2)
		defer cleanupReconciliationFixture(t, db, date, paymentID, refundID)
		provider := &billFixtureProvider{body: tradeBill(date, "PAY-BILL-2", "wechat-trade-2", "0.01", "RF-BILL-2", "wechat-refund-2", "0.01")}
		result, err := NewService(cfg, db, ids, provider, log).RunBill(context.Background(), date, BillTypeTradeAll)
		if err != nil || result.Discrepancies < 3 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		var types []string
		if err := db.Model(&Discrepancy{}).Where("run_id=?", result.RunID).Distinct("discrepancy_type").Pluck("discrepancy_type", &types).Error; err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{DiscrepancyAmountMismatch, DiscrepancyTransactionIDMismatch, DiscrepancyRefundMismatch} {
			if !containsString(types, want) {
				t.Fatalf("missing discrepancy %s in %v", want, types)
			}
		}
		var payment struct {
			Amount          int64
			Status          string
			ProviderTradeNo string
		}
		db.Table("payments").Select("amount,status,provider_trade_no").Where("id=?", paymentID).Scan(&payment)
		var refund struct {
			Amount           int64
			Status           string
			ProviderRefundID string
		}
		db.Table("refunds").Select("amount,status,provider_refund_id").Where("id=?", refundID).Scan(&refund)
		if payment.Amount != 2 || payment.Status != "succeeded" || payment.ProviderTradeNo != "local-trade-2" || refund.Amount != 2 || refund.Status != "succeeded" || refund.ProviderRefundID != "local-refund-2" {
			t.Fatalf("local money changed: payment=%+v refund=%+v", payment, refund)
		}
	})

	t.Run("ACC-BILL-003-digest-mismatch-rolls-back-observations", func(t *testing.T) {
		date := reconciliationDate(12)
		cleanupReconciliationDate(t, db, date)
		provider := &billFixtureProvider{body: tradeBill(date, "PAY-NONE", "420-none", "0.01", "RF-NONE", "503-none", "0.01"), hash: strings.Repeat("0", 40), requestID: "apply-digest-3", downloadReqID: "download-digest-3"}
		_, err := NewService(cfg, db, ids, provider, log).RunBill(context.Background(), date, BillTypeTradeAll)
		if !errors.Is(err, errDigestMismatch) {
			t.Fatalf("expected digest mismatch, got %v", err)
		}
		var run Run
		if err := db.Where("bill_date=? AND bill_type=?", date, BillTypeTradeAll).Take(&run).Error; err != nil {
			t.Fatal(err)
		}
		var observations int64
		db.Model(&Observation{}).Where("run_id=?", run.ID).Count(&observations)
		if run.Status != "failed" || observations != 0 || deref(run.ProviderRequestID) != "apply-digest-3" || deref(run.DownloadRequestID) != "download-digest-3" {
			t.Fatalf("run=%+v observations=%d", run, observations)
		}
		cleanupReconciliationDate(t, db, date)
	})

	t.Run("ACC-BILL-004-no-statement-still-detects-local-payment", func(t *testing.T) {
		date := reconciliationDate(13)
		cleanupReconciliationDate(t, db, date)
		paymentID := insertReconciliationPayment(t, db, ids, date, "PAY-BILL-4", "420000004", 1)
		defer cleanupReconciliationFixture(t, db, date, paymentID, 0)
		provider := &billFixtureProvider{err: &paygateway.ProviderError{Operation: "bill.trade.apply", Code: "NO_STATEMENT_EXIST", HTTPStatus: 404, Retryable: false}}
		result, err := NewService(cfg, db, ids, provider, log).RunBill(context.Background(), date, BillTypeTradeAll)
		if err != nil || result.Status != "no_statement" || result.Discrepancies != 1 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		var row Discrepancy
		if err := db.Where("run_id=?", result.RunID).Take(&row).Error; err != nil || row.DiscrepancyType != DiscrepancyMissingWeChat {
			t.Fatalf("row=%+v err=%v", row, err)
		}
	})

	t.Run("ACC-BILL-005-fund-flow-missing-local-is-recorded", func(t *testing.T) {
		date := reconciliationDate(14)
		cleanupReconciliationDate(t, db, date)
		defer cleanupReconciliationDate(t, db, date)
		provider := &billFixtureProvider{body: fundBill(date, "PAY-ABSENT", "420-absent", "0.01")}
		result, err := NewService(cfg, db, ids, provider, log).RunBill(context.Background(), date, BillTypeFundflowBase)
		if err != nil || result.Discrepancies != 1 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})

	t.Run("ACC-BILL-006-concurrent-run-downloads-once", func(t *testing.T) {
		date := reconciliationDate(15)
		cleanupReconciliationDate(t, db, date)
		defer cleanupReconciliationDate(t, db, date)
		provider := &billFixtureProvider{body: tradeBill(date, "PAY-CONCURRENT", "420-concurrent", "0.01", "RF-CONCURRENT", "503-concurrent", "0.01"), delay: 150 * time.Millisecond}
		service := NewService(cfg, db, ids, provider, log)
		firstDone := make(chan error, 1)
		go func() {
			_, err := service.RunBill(context.Background(), date, BillTypeTradeAll)
			firstDone <- err
		}()
		deadline := time.Now().Add(time.Second)
		for provider.calls.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		second, secondErr := service.RunBill(context.Background(), date, BillTypeTradeAll)
		firstErr := <-firstDone
		if firstErr != nil || secondErr != nil || second.Status != "running" || provider.calls.Load() != 1 {
			t.Fatalf("first_err=%v second=%+v second_err=%v calls=%d", firstErr, second, secondErr, provider.calls.Load())
		}
	})

	t.Run("ACC-BILL-007-refund-is-attributed-to-provider-acceptance-date", func(t *testing.T) {
		requestDate, acceptedDate := reconciliationDate(16), reconciliationDate(17)
		cleanupReconciliationDate(t, db, requestDate)
		cleanupReconciliationDate(t, db, acceptedDate)
		paymentID := insertReconciliationPayment(t, db, ids, reconciliationDate(1), "PAY-BILL-7", "420000007", 1)
		refundID := ids.Next()
		if err := db.Exec("INSERT INTO refunds (id,refund_no,after_sale_id,order_id,payment_id,provider,provider_refund_id,status,amount,total_amount,currency,provider_status,requested_at,provider_accepted_at) VALUES (?,?,?,?,?,'wechat','503000007','succeeded',1,1,'CNY','SUCCESS',?,?)", refundID, "RF-BILL-7", ids.Next(), ids.Next(), paymentID, requestDate.Add(23*time.Hour), acceptedDate.Add(time.Minute)).Error; err != nil {
			t.Fatal(err)
		}
		defer func() {
			db.Exec("DELETE FROM refunds WHERE id=?", refundID)
			db.Exec("DELETE FROM payments WHERE id=?", paymentID)
			cleanupReconciliationDate(t, db, requestDate)
			cleanupReconciliationDate(t, db, acceptedDate)
		}()
		noStatement := func() *billFixtureProvider {
			return &billFixtureProvider{err: &paygateway.ProviderError{Operation: "bill.trade.apply", Code: "NO_STATEMENT_EXIST", HTTPStatus: 404, Retryable: false}}
		}
		requestedResult, err := NewService(cfg, db, ids, noStatement(), log).RunBill(context.Background(), requestDate, BillTypeTradeAll)
		if err != nil || requestedResult.Discrepancies != 0 {
			t.Fatalf("request date result=%+v err=%v", requestedResult, err)
		}
		acceptedResult, err := NewService(cfg, db, ids, noStatement(), log).RunBill(context.Background(), acceptedDate, BillTypeTradeAll)
		if err != nil || acceptedResult.Discrepancies != 1 {
			t.Fatalf("accepted date result=%+v err=%v", acceptedResult, err)
		}
	})

	t.Run("ACC-BILL-008-operational-metrics-are-queryable", func(t *testing.T) {
		service := NewService(cfg, db, ids, &billFixtureProvider{}, log)
		worker := NewWorker(cfg, service, metrics.New("bill-metrics", ""), log)
		samples := worker.collectMetrics()
		for _, name := range []string{"jxe_wechat_bill_reconciliation_last_completed_bill_unixtime", "jxe_wechat_bill_reconciliation_missing_dates", "jxe_wechat_bill_reconciliation_oldest_missing_bill_unixtime", "jxe_wechat_bill_discrepancies_open", "jxe_wechat_bill_discrepancy_oldest_open_seconds"} {
			found := false
			for _, sample := range samples {
				if sample.Name == name {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("missing metric %s in %+v", name, samples)
			}
		}
	})

	t.Run("ACC-BILL-009-database-time-precision-does-not-invalidate-lease", func(t *testing.T) {
		date := reconciliationDate(18)
		cleanupReconciliationDate(t, db, date)
		defer cleanupReconciliationDate(t, db, date)
		provider := &billFixtureProvider{body: fundBill(date, "PAY-PRECISION", "420-precision", "0.01")}
		service := NewService(cfg, db, ids, provider, log)
		fixedNow := time.Date(2026, 7, 20, 13, 7, 32, 285678901, chinaLocation())
		service.now = func() time.Time { return fixedNow }
		result, err := service.RunBill(context.Background(), date, BillTypeFundflowBase)
		if err != nil || result.Status != "succeeded" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})

	t.Run("ACC-BILL-010-stale-worker-cannot-finish-reclaimed-lease", func(t *testing.T) {
		date := reconciliationDate(19)
		cleanupReconciliationDate(t, db, date)
		defer cleanupReconciliationDate(t, db, date)
		repo := newRepository(db)
		firstStartedAt := time.Date(2026, 7, 20, 13, 7, 32, 285678901, chinaLocation())
		first, acquired, err := repo.acquireRun(context.Background(), ids.Next(), date, BillTypeTradeAll, firstStartedAt, time.Minute)
		if err != nil || !acquired {
			t.Fatalf("first=%+v acquired=%v err=%v", first, acquired, err)
		}
		secondStartedAt := firstStartedAt.Add(2 * time.Minute)
		second, acquired, err := repo.acquireRun(context.Background(), ids.Next(), date, BillTypeTradeAll, secondStartedAt, time.Minute)
		if err != nil || !acquired || second.ID != first.ID || second.Version <= first.Version {
			t.Fatalf("first=%+v second=%+v acquired=%v err=%v", first, second, acquired, err)
		}
		if err := repo.markFailed(context.Background(), first, "STALE", "must not win", "", "", secondStartedAt); !errors.Is(err, errRunLeaseLost) {
			t.Fatalf("stale worker error=%v", err)
		}
		var current Run
		if err := db.Where("id=?", second.ID).Take(&current).Error; err != nil {
			t.Fatal(err)
		}
		if current.Status != "running" || current.Version != second.Version {
			t.Fatalf("stale worker changed current lease: %+v", current)
		}
		if err := repo.markFailed(context.Background(), second, "EXPECTED", "current lease", "", "", secondStartedAt); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ACC-BILL-011-worker-backfills-oldest-gaps-in-bounded-batches", func(t *testing.T) {
		startDate := reconciliationDate(20)
		for offset := 0; offset < 3; offset++ {
			date := startDate.AddDate(0, 0, offset)
			cleanupReconciliationDate(t, db, date)
			defer cleanupReconciliationDate(t, db, date)
		}
		backfillCfg := cfg
		backfillCfg.Reconciliation.StartDate = startDate.Format("2006-01-02")
		backfillCfg.Reconciliation.BackfillDaysPerCycle = 2
		backfillCfg.Reconciliation.RunHour = 10
		backfillCfg.Reconciliation.LagDays = 1
		provider := &recordingNoStatementProvider{}
		service := NewService(backfillCfg, db, ids, provider, log)
		worker := NewWorker(backfillCfg, service, nil, log)
		now := startDate.AddDate(0, 0, 3).Add(11 * time.Hour)
		if errs := worker.RunDue(context.Background(), now); len(errs) != 0 {
			t.Fatalf("first cycle errors=%v", errs)
		}
		if errs := worker.RunDue(context.Background(), now); len(errs) != 0 {
			t.Fatalf("second cycle errors=%v", errs)
		}
		calls := provider.recordedCalls()
		want := []billCall{
			{date: startDate.Format("2006-01-02"), billType: BillTypeTradeAll},
			{date: startDate.Format("2006-01-02"), billType: BillTypeFundflowBase},
			{date: startDate.AddDate(0, 0, 1).Format("2006-01-02"), billType: BillTypeTradeAll},
			{date: startDate.AddDate(0, 0, 1).Format("2006-01-02"), billType: BillTypeFundflowBase},
			{date: startDate.AddDate(0, 0, 2).Format("2006-01-02"), billType: BillTypeTradeAll},
			{date: startDate.AddDate(0, 0, 2).Format("2006-01-02"), billType: BillTypeFundflowBase},
		}
		if len(calls) != len(want) {
			t.Fatalf("calls=%+v want=%+v", calls, want)
		}
		for index := range want {
			if calls[index] != want[index] {
				t.Fatalf("call[%d]=%+v want=%+v", index, calls[index], want[index])
			}
		}
	})

	t.Run("ACC-BILL-012-latest-completion-does-not-mask-middle-gap", func(t *testing.T) {
		startDate := reconciliationDate(23)
		endDate := startDate.AddDate(0, 0, 2)
		for date := startDate; !date.After(endDate); date = date.AddDate(0, 0, 1) {
			cleanupReconciliationDate(t, db, date)
			defer cleanupReconciliationDate(t, db, date)
		}
		provider := &recordingNoStatementProvider{}
		service := NewService(cfg, db, ids, provider, log)
		for _, date := range []time.Time{startDate, endDate} {
			for _, billType := range []string{BillTypeTradeAll, BillTypeFundflowBase} {
				if _, err := service.RunBill(context.Background(), date, billType); err != nil {
					t.Fatalf("seed completed run date=%s type=%s: %v", date.Format("2006-01-02"), billType, err)
				}
			}
		}
		worker := NewWorker(cfg, service, nil, log)
		missing, err := worker.missingBillDates(context.Background(), startDate, endDate, 10)
		if err != nil || len(missing) != 1 || !missing[0].Equal(startDate.AddDate(0, 0, 1)) {
			t.Fatalf("missing=%v err=%v", missing, err)
		}
	})

	t.Run("ACC-BILL-013-manual-backfill-is-idempotent-and-window-bounded", func(t *testing.T) {
		date := reconciliationDate(26)
		cleanupReconciliationDate(t, db, date)
		defer cleanupReconciliationDate(t, db, date)
		provider := &recordingNoStatementProvider{}
		service := NewService(cfg, db, ids, provider, log)
		service.now = func() time.Time { return date.AddDate(0, 0, 1).Add(11 * time.Hour) }
		claims := &auth.Claims{AccountType: "admin", AdminUserID: "900987", Permissions: []string{"refund:exception"}}
		const path = "/api/v1/admin/reconciliation/runs"
		const key = "acc-bill-manual-013"
		defer db.Where("actor_type=? AND actor_id=? AND path=? AND key_hash=?", "admin", uint64(900987), path, idempotency.KeyHash(key)).Delete(&idempotency.Record{})
		first, err := service.RunBillManual(context.Background(), claims, "POST", path, key, date.Format("2006-01-02"), BillTypeTradeAll)
		if err != nil {
			t.Fatalf("first manual run: %v", err)
		}
		repeat, err := service.RunBillManual(context.Background(), claims, "POST", path, key, date.Format("2006-01-02"), BillTypeTradeAll)
		if err != nil || repeat.RunID != first.RunID || len(provider.recordedCalls()) != 1 {
			t.Fatalf("first=%+v repeat=%+v calls=%+v err=%v", first, repeat, provider.recordedCalls(), err)
		}
		tooOld := date.AddDate(0, 0, -90)
		if _, err := service.RunBillManual(context.Background(), claims, "POST", path, "acc-bill-manual-old-013", tooOld.Format("2006-01-02"), BillTypeTradeAll); err == nil {
			t.Fatal("expected a date older than the official 90-day window to fail")
		}
		if len(provider.recordedCalls()) != 1 {
			t.Fatalf("out-of-window request reached provider: %+v", provider.recordedCalls())
		}
	})

	t.Run("ACC-BILL-014-provider-id-fallback-cannot-hide-business-id-mismatch", func(t *testing.T) {
		date := reconciliationDate(27)
		cleanupReconciliationDate(t, db, date)
		paymentID, refundID := insertReconciliationPaymentAndRefund(t, db, ids, date, "PAY-BILL-14", "420000014", 1, "RF-BILL-14", "503000014", 1)
		defer cleanupReconciliationFixture(t, db, date, paymentID, refundID)
		provider := &billFixtureProvider{body: tradeBill(date, "PAY-WRONG-14", "420000014", "0.01", "RF-WRONG-14", "503000014", "0.01")}
		result, err := NewService(cfg, db, ids, provider, log).RunBill(context.Background(), date, BillTypeTradeAll)
		if err != nil || result.Discrepancies != 2 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		var types []string
		if err := db.Model(&Discrepancy{}).Where("run_id=?", result.RunID).Distinct("discrepancy_type").Pluck("discrepancy_type", &types).Error; err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{DiscrepancyTransactionIDMismatch, DiscrepancyRefundMismatch} {
			if !containsString(types, want) {
				t.Fatalf("missing discrepancy %s in %v", want, types)
			}
		}
	})

	t.Run("ACC-BILL-015-failed-provider-calls-preserve-request-id", func(t *testing.T) {
		for index, tc := range []struct {
			operation         string
			providerRequestID string
			downloadRequestID string
		}{
			{operation: "bill.trade.apply", providerRequestID: "apply-failed-15"},
			{operation: "bill.download", downloadRequestID: "download-failed-15"},
		} {
			date := reconciliationDate(28).AddDate(0, 0, index)
			cleanupReconciliationDate(t, db, date)
			defer cleanupReconciliationDate(t, db, date)
			requestID := tc.providerRequestID
			if requestID == "" {
				requestID = tc.downloadRequestID
			}
			provider := &billFixtureProvider{err: &paygateway.ProviderError{Operation: tc.operation, HTTPStatus: 500, Code: "SYSTEM_ERROR", RequestID: requestID, Retryable: true}}
			if _, err := NewService(cfg, db, ids, provider, log).RunBill(context.Background(), date, BillTypeTradeAll); err == nil {
				t.Fatalf("expected %s failure", tc.operation)
			}
			var run Run
			if err := db.Where("bill_date=? AND bill_type=?", date, BillTypeTradeAll).Take(&run).Error; err != nil {
				t.Fatal(err)
			}
			if run.Status != "failed" || deref(run.ProviderRequestID) != tc.providerRequestID || deref(run.DownloadRequestID) != tc.downloadRequestID {
				t.Fatalf("operation=%s run=%+v", tc.operation, run)
			}
		}
	})
}

func openReconciliationDB(t *testing.T) *gorm.DB {
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

func reconciliationDate(day int) time.Time {
	return time.Date(2026, 6, day, 0, 0, 0, 0, chinaLocation())
}

func insertReconciliationPaymentAndRefund(t *testing.T, db *gorm.DB, ids *snowflake.Generator, date time.Time, paymentNo, tradeNo string, paymentAmount int64, refundNo, providerRefundNo string, refundAmount int64) (uint64, uint64) {
	t.Helper()
	paymentID := insertReconciliationPayment(t, db, ids, date, paymentNo, tradeNo, paymentAmount)
	refundID := ids.Next()
	err := db.Exec("INSERT INTO refunds (id,refund_no,after_sale_id,order_id,payment_id,provider,provider_refund_id,status,amount,total_amount,currency,provider_status,requested_at) VALUES (?,?,?,?,?,'wechat',?,'succeeded',?,?,'CNY','SUCCESS',?)", refundID, refundNo, ids.Next(), ids.Next(), paymentID, providerRefundNo, refundAmount, paymentAmount, date.Add(12*time.Hour)).Error
	if err != nil {
		t.Fatal(err)
	}
	return paymentID, refundID
}

func insertReconciliationPayment(t *testing.T, db *gorm.DB, ids *snowflake.Generator, date time.Time, paymentNo, tradeNo string, amount int64) uint64 {
	t.Helper()
	paymentID := ids.Next()
	err := db.Exec("INSERT INTO payments (id,payment_no,order_id,customer_id,channel,provider,provider_trade_no,provider_status,status,amount,currency,paid_at) VALUES (?,?,?,?,'miniapp','wechat',?,'SUCCESS','succeeded',?,'CNY',?)", paymentID, paymentNo, ids.Next(), ids.Next(), tradeNo, amount, date.Add(11*time.Hour)).Error
	if err != nil {
		t.Fatal(err)
	}
	return paymentID
}

func cleanupReconciliationFixture(t *testing.T, db *gorm.DB, date time.Time, paymentID, refundID uint64) {
	t.Helper()
	if refundID != 0 {
		db.Exec("DELETE FROM refunds WHERE id=?", refundID)
	}
	if paymentID != 0 {
		db.Exec("DELETE FROM payments WHERE id=?", paymentID)
	}
	cleanupReconciliationDate(t, db, date)
}

func cleanupReconciliationDate(t *testing.T, db *gorm.DB, date time.Time) {
	t.Helper()
	var runIDs []uint64
	db.Model(&Run{}).Where("bill_date=?", date).Pluck("id", &runIDs)
	if len(runIDs) > 0 {
		db.Where("run_id IN ?", runIDs).Delete(&Discrepancy{})
		db.Where("run_id IN ?", runIDs).Delete(&Observation{})
		db.Model(&Run{}).Where("id IN ?", runIDs).Updates(map[string]any{"status": "failed", "started_at": nil, "completed_at": nil})
	}
}

func tradeBill(date time.Time, paymentNo, tradeNo, paymentAmount, refundNo, providerRefundNo, refundAmount string) string {
	day := date.Format("2006-01-02")
	payment := "`" + day + " 11:00:00,`wxapp,`1900000001,`0,`,`" + tradeNo + ",`" + paymentNo + ",`openid,`JSAPI,`SUCCESS,`OTHERS,`CNY,`" + paymentAmount + ",`0.00,`0,`0,`0.00,`0.00,`,``,`测试,`,`0.00,`0.60%,`" + paymentAmount + ",`0.00,`\n"
	refund := "`" + day + " 12:00:00,`wxapp,`1900000001,`0,`,`" + tradeNo + ",`" + paymentNo + ",`openid,`JSAPI,`REFUND,`OTHERS,`CNY,`0.00,`0.00,`" + providerRefundNo + ",`" + refundNo + ",`" + refundAmount + ",`0.00,`ORIGINAL,`SUCCESS,`测试,`,`0.00,`0.60%,`0.00,`" + refundAmount + ",`\n"
	return tradeAllHeader + payment + refund + "总交易单数,应结订单总金额,退款总金额,充值券退款总金额,手续费总金额,订单总金额,申请退款总金额\n`2,`" + paymentAmount + ",`" + refundAmount + ",`0.00,`0.00,`" + paymentAmount + ",`" + refundAmount + "\n"
}

func fundBill(date time.Time, businessNo, providerNo, amount string) string {
	return "记账时间,微信支付业务单号,资金流水单号,业务名称,业务类型,收支类型,收支金额(元),账户结余(元),资金变更提交申请人,备注,业务凭证号\n" +
		"`" + date.Format("2006-01-02") + " 13:00:00,`" + providerNo + ",`FUND-1,`交易,`交易,`收入,`" + amount + ",`1.00,`system,`测试,`" + businessNo + "\n" +
		"资金流水总笔数,收入笔数,收入金额,支出笔数,支出金额\n`1,`1,`" + amount + ",`0,`0.00\n"
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
