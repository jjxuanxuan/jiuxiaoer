package asset

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

// TestL4AssetFormalAcceptanceGaps 验证L 4 资产正式验收 Gaps的预期行为。
func TestL4AssetFormalAcceptanceGaps(t *testing.T) {
	f := newAcceptanceFixture(t)
	customer := &auth.Claims{AccountType: "customer", CustomerID: idString(f.customerID)}

	t.Run("ACC-L4-002-new-customer-has-three-zero-assets", func(t *testing.T) {
		items, err := f.service.Summaries(f.ctx, customer)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 3 {
			t.Fatalf("asset summaries=%#v", items)
		}
		for _, item := range items {
			if item.AvailableAmount != 0 || item.FrozenAmount != 0 {
				t.Fatalf("new customer asset is not zero: %#v", item)
			}
		}
	})

	t.Run("ACC-L4-003-005-customer-owner-isolation", func(t *testing.T) {
		if _, err := f.service.Credit(f.ctx, f.command(TypeBalance, UnitCNY, 10, "owner-a")); err != nil {
			t.Fatal(err)
		}
		otherAccount, otherCustomer := f.createCustomer(t, "137")
		defer f.deleteCustomer(t, otherAccount, otherCustomer)
		otherClaims := &auth.Claims{AccountType: "customer", CustomerID: idString(otherCustomer)}
		items, _, err := f.service.ListCustomerTransactions(f.ctx, otherClaims, TypeBalance, pagination.Query{PageSize: 20})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 0 {
			t.Fatalf("customer B observed customer A transactions: %#v", items)
		}
		_, err = f.service.AdminTransaction(f.ctx, otherClaims, "1")
		if problem.FromError(err).Status != 403 {
			t.Fatalf("customer accessed admin transaction detail: %v", err)
		}
	})

	t.Run("ACC-L4-006-invalid-query-input-is-rejected", func(t *testing.T) {
		_, _, err := f.service.ListCustomerTransactions(f.ctx, customer, "unknown", pagination.Query{PageSize: 20})
		p := problem.FromError(err)
		if p.Status != 422 || p.ErrorCode != "ASSET_TYPE_INVALID" {
			t.Fatalf("invalid filter error=%#v", p)
		}
	})

	t.Run("ACC-L4-016-same-source-different-payload-conflicts", func(t *testing.T) {
		cmd := f.command(TypeBalance, UnitCNY, 15, "payload-conflict")
		if _, err := f.service.Credit(f.ctx, cmd); err != nil {
			t.Fatal(err)
		}
		cmd.Amount = 16
		_, err := f.service.Credit(f.ctx, cmd)
		if problem.FromError(err).ErrorCode != "ASSET_IDEMPOTENCY_CONFLICT" {
			t.Fatalf("payload conflict error=%v", err)
		}
		var count int64
		if err := f.db.Model(&Transaction{}).Where("source_type=? AND source_id=? AND action='credit'", cmd.SourceType, cmd.SourceID).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("transactions=%d err=%v", count, err)
		}
	})

	t.Run("ACC-L4-023-two-checkers-execute-adjustment-once", func(t *testing.T) {
		req := AdjustmentCreateReq{CustomerID: idString(f.customerID), AssetType: TypeBalance, Direction: "credit", Amount: 30, ReasonCode: "ACC023", Reason: "concurrent checker acceptance"}
		adjustment, err := f.service.CreateAdjustment(f.ctx, &f.maker, "POST", "/api/v1/admin/asset-adjustments", f.key("acc023-create"), req)
		if err != nil {
			t.Fatal(err)
		}
		checkers := []auth.Claims{adminClaims(88002, "asset_adjustment:approve"), adminClaims(88003, "asset_adjustment:approve")}
		var successes, conflicts atomic.Int64
		var wg sync.WaitGroup
		for i := range checkers {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, callErr := f.service.ApproveAdjustment(f.ctx, &checkers[i], adjustment.ID, f.key(fmt.Sprintf("acc023-approve-%d", i)), AdjustmentReviewReq{Version: adjustment.Version})
				if callErr == nil {
					successes.Add(1)
				} else if problem.FromError(callErr).ErrorCode == "ASSET_ADJUSTMENT_STATE_CONFLICT" {
					conflicts.Add(1)
				} else {
					t.Errorf("approve error: %v", callErr)
				}
			}()
		}
		wg.Wait()
		if successes.Load() != 1 || conflicts.Load() != 1 {
			t.Fatalf("successes=%d conflicts=%d", successes.Load(), conflicts.Load())
		}
		var txCount int64
		if err := f.db.Model(&Transaction{}).Where("source_type='manual_adjustment' AND source_id=?", adjustment.ID).Count(&txCount).Error; err != nil || txCount != 1 {
			t.Fatalf("adjustment transactions=%d err=%v", txCount, err)
		}
	})

	t.Run("ACC-L4-024-insufficient-debit-has-no-entry", func(t *testing.T) {
		req := AdjustmentCreateReq{CustomerID: idString(f.customerID), AssetType: TypeBalance, Direction: "debit", Amount: 50000, ReasonCode: "ACC024", Reason: "insufficient balance acceptance"}
		adjustment, err := f.service.CreateAdjustment(f.ctx, &f.maker, "POST", "/api/v1/admin/asset-adjustments", f.key("acc024-create"), req)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.service.ApproveAdjustment(f.ctx, &f.checker, adjustment.ID, f.key("acc024-approve"), AdjustmentReviewReq{Version: adjustment.Version})
		if problem.FromError(err).ErrorCode != "ASSET_INSUFFICIENT_AVAILABLE" {
			t.Fatalf("insufficient debit error=%v", err)
		}
		var txCount int64
		if err := f.db.Model(&Transaction{}).Where("source_type='manual_adjustment' AND source_id=?", adjustment.ID).Count(&txCount).Error; err != nil || txCount != 0 {
			t.Fatalf("failed debit transactions=%d err=%v", txCount, err)
		}
		var stored Adjustment
		if err := f.db.Where("id=?", adjustment.ID).Take(&stored).Error; err != nil || stored.Status != "failed" {
			t.Fatalf("adjustment=%#v err=%v", stored, err)
		}
	})

	t.Run("ACC-L4-028-expiry-freeze-race-1000", func(t *testing.T) {
		past := time.Now().Add(-time.Minute)
		cmd := f.command(TypeWineCoin, UnitPoint, 1000, "acc028-grant")
		cmd.ExpiresAt = &past
		if _, err := f.service.Credit(f.ctx, cmd); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var unexpected atomic.Int64
		for i := 0; i < 1000; i++ {
			i := i
			wg.Add(2)
			go func() {
				defer wg.Done()
				freeze := f.command(TypeWineCoin, UnitPoint, 1, fmt.Sprintf("acc028-freeze-%d", i))
				_, err := f.service.Freeze(f.ctx, FreezeCommand{Command: freeze, ReservationKey: f.key(fmt.Sprintf("acc028-reservation-%d", i)), HoldExpiresAt: &past})
				if err != nil && problem.FromError(err).ErrorCode != "ASSET_INSUFFICIENT_AVAILABLE" {
					unexpected.Add(1)
				}
			}()
			go func() {
				defer wg.Done()
				if _, err := f.service.ExpireDueLots(f.ctx, 1); err != nil {
					unexpected.Add(1)
				}
			}()
		}
		wg.Wait()
		if unexpected.Load() != 0 {
			t.Fatalf("unexpected concurrent errors=%d", unexpected.Load())
		}
		if _, err := f.service.ExpireDueHolds(f.ctx, 2000); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.ExpireDueLots(f.ctx, 2000); err != nil {
			t.Fatal(err)
		}
		f.assertLotInvariant(t)
		f.assertLedger(t)
		f.assertCustomerBalancesNonnegative(t)
	})

	t.Run("ACC-L4-032-multiple-workers-claim-compensation-once", func(t *testing.T) {
		comp := f.insertCompensation(t, TypeBalance, 20, "approved")
		workers := []*Worker{NewWorker(f.cfg, f.service, slog.New(slog.NewTextHandler(io.Discard, nil))), NewWorker(f.cfg, f.service, slog.New(slog.NewTextHandler(io.Discard, nil)))}
		workers[0].instance, workers[1].instance = "acc032-a", "acc032-b"
		var claims atomic.Int64
		var wg sync.WaitGroup
		for _, worker := range workers {
			worker := worker
			wg.Add(1)
			go func() {
				defer wg.Done()
				row, err := worker.claimCompensation(f.ctx)
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return
				}
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				claims.Add(1)
				if err := worker.issueCompensation(f.ctx, row); err != nil {
					t.Errorf("issue: %v", err)
				}
			}()
		}
		wg.Wait()
		if claims.Load() != 1 {
			t.Fatalf("claims=%d", claims.Load())
		}
		var stored Compensation
		if err := f.db.Where("id=?", comp.ID).Take(&stored).Error; err != nil || stored.Status != "issued" || stored.AssetTransactionID == nil {
			t.Fatalf("compensation=%#v err=%v", stored, err)
		}
	})

	t.Run("ACC-L4-033-invalid-compensation-fails-without-transaction", func(t *testing.T) {
		comp := f.insertCompensation(t, TypeWineCoin, 10, "issuing")
		worker := NewWorker(f.cfg, f.service, slog.New(slog.NewTextHandler(io.Discard, nil)))
		err := worker.issueCompensation(f.ctx, comp)
		if problem.FromError(err).ErrorCode != "ASSET_TYPE_INVALID" {
			t.Fatalf("invalid compensation error=%v", err)
		}
		var stored Compensation
		if err := f.db.Where("id=?", comp.ID).Take(&stored).Error; err != nil || stored.Status != "failed" || stored.FailureCode == nil {
			t.Fatalf("compensation=%#v err=%v", stored, err)
		}
		var txCount int64
		if err := f.db.Model(&Transaction{}).Where("source_type='compensation' AND source_id=?", idString(comp.ID)).Count(&txCount).Error; err != nil || txCount != 0 {
			t.Fatalf("invalid compensation transactions=%d err=%v", txCount, err)
		}
	})

	t.Run("ACC-L4-034-compensation-does-not-change-refund-ledgers", func(t *testing.T) {
		before := f.refundedAmountTotal(t)
		comp := f.insertCompensation(t, TypeBalance, 12, "issuing")
		worker := NewWorker(f.cfg, f.service, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err := worker.issueCompensation(f.ctx, comp); err != nil {
			t.Fatal(err)
		}
		if after := f.refundedAmountTotal(t); after != before {
			t.Fatalf("refunded amount changed: before=%d after=%d", before, after)
		}
	})

	t.Run("ACC-L4-035-unregistered-source-is-rejected", func(t *testing.T) {
		cmd := f.command(TypeBalance, UnitCNY, 1, "acc035")
		cmd.SourceType = "unknown_source"
		_, err := f.service.Credit(f.ctx, cmd)
		if problem.FromError(err).ErrorCode != "ASSET_SOURCE_NOT_ALLOWED" {
			t.Fatalf("source error=%v", err)
		}
	})

	t.Run("ACC-L4-036-invalid-amounts-have-no-projection-side-effect", func(t *testing.T) {
		before := f.customerAvailable(t, TypeBalance)
		for i, amount := range []int64{0, -1, math.MaxInt64} {
			cmd := f.command(TypeBalance, UnitCNY, amount, fmt.Sprintf("acc036-%d", i))
			_, err := f.service.Credit(f.ctx, cmd)
			if problem.FromError(err).Status != 422 {
				t.Fatalf("amount=%d error=%v", amount, err)
			}
		}
		if after := f.customerAvailable(t, TypeBalance); after != before {
			t.Fatalf("balance changed: before=%d after=%d", before, after)
		}
	})

	t.Run("ACC-L4-037-credit-is-db-atomic-without-mq-or-redis", func(t *testing.T) {
		cmd := f.command(TypeBalance, UnitCNY, 7, "acc037")
		dto, err := f.service.Credit(f.ctx, cmd)
		if err != nil {
			t.Fatal(err)
		}
		txID, _ := parseID(dto.ID)
		var outboxCount int64
		if err := f.db.Model(&Outbox{}).Where("aggregate_type='asset' AND aggregate_id=? AND status='pending'", txID).Count(&outboxCount).Error; err != nil || outboxCount != 1 {
			t.Fatalf("pending outbox=%d err=%v", outboxCount, err)
		}
		f.assertLedger(t)
	})

	t.Run("ACC-L4-038-transaction-failure-rolls-back-all-side-effects", func(t *testing.T) {
		callback := fmt.Sprintf("acc038:reject-outbox:%d", f.seq.Add(1))
		if err := f.db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
			if tx.Statement.Table == "outbox_events" {
				tx.AddError(errors.New("ACC038 injected outbox failure"))
			}
		}); err != nil {
			t.Fatal(err)
		}
		defer f.db.Callback().Create().Remove(callback)
		cmd := f.command(TypeBalance, UnitCNY, 9, "acc038")
		_, err := f.service.Credit(f.ctx, cmd)
		if err == nil {
			t.Fatal("injected transaction failure unexpectedly succeeded")
		}
		var txCount, entryCount int64
		f.db.Model(&Transaction{}).Where("source_type=? AND source_id=?", cmd.SourceType, cmd.SourceID).Count(&txCount)
		f.db.Model(&Entry{}).Where("transaction_id IN (SELECT id FROM asset_transactions WHERE source_type=? AND source_id=?)", cmd.SourceType, cmd.SourceID).Count(&entryCount)
		if txCount != 0 || entryCount != 0 {
			t.Fatalf("rollback residue tx=%d entries=%d", txCount, entryCount)
		}
	})

	t.Run("ACC-L4-039-runtime-user-cannot-mutate-entries", func(t *testing.T) {
		posted, err := f.service.Credit(f.ctx, f.command(TypeBalance, UnitCNY, 1, "acc039"))
		if err != nil {
			t.Fatal(err)
		}
		transactionID, _ := parseID(posted.ID)
		runtimeDSN := os.Getenv("JXE_MYSQL_RUNTIME_DSN")
		if runtimeDSN == "" {
			runtimeDSN = os.Getenv("JXE_MYSQL_DSN")
		}
		cfg := f.cfg.MySQL
		cfg.DSN = runtimeDSN
		runtimeDB, err := mysql.Open(f.ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if sqlDB, openErr := runtimeDB.DB(); openErr == nil {
				_ = sqlDB.Close()
			}
		}()
		var entry Entry
		if err := f.db.Where("transaction_id=?", transactionID).Order("entry_seq").Take(&entry).Error; err != nil {
			t.Fatal(err)
		}
		for operation, statement := range map[string]string{
			"update": "UPDATE asset_entries SET delta=delta WHERE id=?",
			"delete": "DELETE FROM asset_entries WHERE id=?",
		} {
			err := runtimeDB.Exec(statement, entry.ID).Error
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "denied") {
				t.Fatalf("runtime %s was not denied: %v", operation, err)
			}
		}
	})

	t.Run("ACC-L4-040-normal-ledger-reconciles-with-zero-difference", func(t *testing.T) {
		job, err := f.service.RunReconciliation(f.ctx, &f.maker, f.key("acc040"), ReconcileReq{Scope: "customer", ScopeID: idString(f.customerID)})
		if err != nil {
			t.Fatal(err)
		}
		if job.DifferenceCount != 0 {
			t.Fatalf("reconciliation=%#v", job)
		}
	})

	t.Run("ACC-L4-043-unbalanced-entries-are-critical-and-not-repairable", func(t *testing.T) {
		var account Account
		if err := f.db.Where("owner_type='customer' AND owner_id=? AND asset_type=?", f.customerID, TypeBalance).Take(&account).Error; err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		tx := Transaction{ID: f.ids.Next(), TransactionNo: fmt.Sprintf("AT%d", f.ids.Next()), AssetType: TypeBalance, Unit: UnitCNY, Action: "credit", Status: "posted", SourceType: "reconciliation_test", SourceID: f.key("acc043"), IdempotencyKeyHash: keyHash(f.key("acc043-key")), RequestHash: keyHash("acc043-request"), Amount: 1, ActorType: "system", OccurredAt: now, PostedAt: now}
		if err := f.db.Create(&tx).Error; err != nil {
			t.Fatal(err)
		}
		entry := Entry{ID: f.ids.Next(), TransactionID: tx.ID, EntrySeq: 1, AccountID: account.ID, Bucket: "available", Delta: 1, BalanceAfter: f.customerAvailable(t, TypeBalance) + 1}
		if err := f.db.Create(&entry).Error; err != nil {
			t.Fatal(err)
		}
		defer func() {
			f.db.Delete(&entry)
			f.db.Delete(&tx)
		}()
		job, err := f.service.RunReconciliation(f.ctx, &f.maker, f.key("acc043-reconcile"), ReconcileReq{Scope: "customer", ScopeID: idString(f.customerID)})
		if err != nil {
			t.Fatal(err)
		}
		if job.CriticalCount == 0 {
			t.Fatalf("unbalanced entry not critical: %#v", job)
		}
		_, err = f.service.RepairReconciliation(f.ctx, &f.maker, job.ID)
		if problem.FromError(err).ErrorCode != "RECONCILIATION_REPAIR_FORBIDDEN" {
			t.Fatalf("unbalanced repair error=%v", err)
		}
	})

	t.Run("ACC-L4-044-reconciliation-request-is-idempotent", func(t *testing.T) {
		key := f.key("acc044")
		ids := sync.Map{}
		var failures atomic.Int64
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				job, err := f.service.RunReconciliation(f.ctx, &f.maker, key, ReconcileReq{Scope: "customer", ScopeID: idString(f.customerID)})
				if err != nil {
					failures.Add(1)
					return
				}
				ids.Store(job.ID, true)
			}()
		}
		wg.Wait()
		unique := 0
		ids.Range(func(_, _ any) bool { unique++; return true })
		if failures.Load() != 0 || unique != 1 {
			t.Fatalf("failures=%d unique_jobs=%d", failures.Load(), unique)
		}
	})

	t.Run("ACC-L4-045-non-customer-and-unprivileged-admin-are-forbidden", func(t *testing.T) {
		for _, claims := range []*auth.Claims{{AccountType: "merchant"}, {AccountType: "rider"}, {AccountType: "admin", AdminUserID: "88999"}} {
			_, err := f.service.Summaries(f.ctx, claims)
			if problem.FromError(err).Status != 403 {
				t.Fatalf("account_type=%s summaries error=%v", claims.AccountType, err)
			}
		}
		_, _, err := f.service.ListAdminTransactions(f.ctx, &auth.Claims{AccountType: "admin", AdminUserID: "88999"}, ListQuery{Query: pagination.Query{PageSize: 20}})
		if problem.FromError(err).Status != 403 {
			t.Fatalf("unprivileged admin list error=%v", err)
		}
	})

	t.Run("ACC-L4-046-disabled-write-keeps-read-and-stops-workers", func(t *testing.T) {
		cfg := f.cfg
		cfg.Asset.WriteEnabled = false
		cfg.Asset.CompensationIssueEnabled = false
		cfg.Asset.ExpiryEnabled = false
		service := NewService(cfg, f.db, f.ids)
		if _, err := service.Summaries(f.ctx, customer); err != nil {
			t.Fatalf("read failed while write disabled: %v", err)
		}
		cmd := f.command(TypeBalance, UnitCNY, 1, "acc046")
		_, err := service.Credit(f.ctx, cmd)
		if problem.FromError(err).ErrorCode != "ASSET_WRITE_DISABLED" {
			t.Fatalf("disabled write error=%v", err)
		}
		comp := f.insertCompensation(t, TypeBalance, 1, "approved")
		worker := NewWorker(cfg, service, slog.New(slog.NewTextHandler(io.Discard, nil)))
		worker.runBatch(f.ctx)
		var stored Compensation
		if err := f.db.Where("id=?", comp.ID).Take(&stored).Error; err != nil || stored.Status != "approved" || stored.Attempts != 0 {
			t.Fatalf("disabled worker claimed compensation: %#v err=%v", stored, err)
		}
	})
}

// createCustomer 创建用户。
func (f *acceptanceFixture) createCustomer(t *testing.T, prefix string) (uint64, uint64) {
	t.Helper()
	accountID, customerID := f.ids.Next(), f.ids.Next()
	phone := fmt.Sprintf("%s%08d", prefix, time.Now().UnixNano()%100000000)
	if err := f.db.Exec("INSERT INTO accounts (id,account_type,phone,status) VALUES (?,'customer',?,'active')", accountID, phone).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Exec("INSERT INTO customers (id,account_id,phone,status) VALUES (?,?,?,'active')", customerID, accountID, phone).Error; err != nil {
		t.Fatal(err)
	}
	return accountID, customerID
}

// deleteCustomer 删除用户。
func (f *acceptanceFixture) deleteCustomer(t *testing.T, accountID, customerID uint64) {
	t.Helper()
	if err := f.db.Exec("DELETE FROM member_profiles WHERE customer_id=?", customerID).Error; err != nil {
		t.Error(err)
	}
	f.db.Exec("DELETE FROM customers WHERE id=?", customerID)
	f.db.Exec("DELETE FROM accounts WHERE id=?", accountID)
}

// insertCompensation 插入Compensation。
func (f *acceptanceFixture) insertCompensation(t *testing.T, assetType string, amount int64, status string) Compensation {
	t.Helper()
	id := f.ids.Next()
	row := Compensation{ID: id, AfterSaleID: f.ids.Next(), CustomerID: f.customerID, CompensationNo: fmt.Sprintf("CP%d", id), Type: "late_delivery", AssetType: assetType, Status: status, Amount: amount}
	if err := f.db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

// customerAvailable 返回用户可用。
func (f *acceptanceFixture) customerAvailable(t *testing.T, assetType string) int64 {
	t.Helper()
	var amount int64
	if err := f.db.Table("asset_balances b").Select("COALESCE(MAX(b.amount),0)").Joins("JOIN asset_accounts a ON a.id=b.account_id").Where("a.owner_type='customer' AND a.owner_id=? AND a.asset_type=? AND b.bucket='available'", f.customerID, assetType).Scan(&amount).Error; err != nil {
		t.Fatal(err)
	}
	return amount
}

// refundedAmountTotal 返回refunded 金额 Total。
func (f *acceptanceFixture) refundedAmountTotal(t *testing.T) int64 {
	t.Helper()
	var total int64
	if err := f.db.Table("orders").Select("COALESCE(SUM(refunded_amount),0)").Scan(&total).Error; err != nil {
		t.Fatal(err)
	}
	return total
}

// assertCustomerBalancesNonnegative 断言用户 Balances Nonnegative符合预期。
func (f *acceptanceFixture) assertCustomerBalancesNonnegative(t *testing.T) {
	t.Helper()
	var count int64
	if err := f.db.Table("asset_balances b").Joins("JOIN asset_accounts a ON a.id=b.account_id").Where("a.owner_type='customer' AND a.owner_id=? AND b.amount<0", f.customerID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("negative customer balances=%d", count)
	}
}
