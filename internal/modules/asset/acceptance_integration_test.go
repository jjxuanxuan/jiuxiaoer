package asset

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/member"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type acceptanceFixture struct {
	ctx                   context.Context
	db                    *gorm.DB
	cfg                   config.Config
	ids                   *snowflake.Generator
	service               *Service
	customerID, accountID uint64
	maker, checker        auth.Claims
	seq                   atomic.Uint64
}

// newAcceptanceFixture 创建并初始化验收测试夹具。
func newAcceptanceFixture(t *testing.T) *acceptanceFixture {
	t.Helper()
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run L4 acceptance tests")
	}
	ctx := context.Background()
	cfg := config.Load()
	cfg.Asset.MemberEnabled = true
	cfg.Asset.ReadEnabled = true
	cfg.Asset.WriteEnabled = true
	cfg.Asset.CompensationIssueEnabled = true
	cfg.Asset.ExpiryEnabled = true
	cfg.Asset.RepairEnabled = true
	cfg.Asset.WorkerEnabled = true
	db, err := mysql.Open(ctx, cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	ids := snowflake.New(976)
	f := &acceptanceFixture{ctx: ctx, db: db, cfg: cfg, ids: ids}
	f.accountID = ids.Next()
	f.customerID = ids.Next()
	phone := fmt.Sprintf("139%08d", time.Now().UnixNano()%100000000)
	if err := db.Exec("INSERT INTO accounts (id,account_type,phone,status) VALUES (?,'customer',?,'active')", f.accountID, phone).Error; err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.Exec("INSERT INTO customers (id,account_id,phone,status) VALUES (?,?,?,'active')", f.customerID, f.accountID, phone).Error; err != nil {
		t.Fatalf("insert customer: %v", err)
	}
	f.service = NewService(cfg, db, ids)
	f.maker = adminClaims(88001, "asset_adjustment:create", "asset_adjustment:approve", "asset_transaction:list", "asset_transaction:view", "asset_reconcile:run", "asset_reconcile:view", "asset_reconcile:repair")
	f.checker = adminClaims(88002, "asset_adjustment:approve")
	t.Cleanup(func() { f.cleanup(t) })
	return f
}

// TestL4LedgerAcceptanceIntegration 验证L 4 账本验收集成的预期行为。
func TestL4LedgerAcceptanceIntegration(t *testing.T) {
	f := newAcceptanceFixture(t)
	t.Run("ACC-L4-014-015-balanced-credit-and-concurrent-idempotency", func(t *testing.T) {
		cmd := f.command(TypeBalance, UnitCNY, 200, "credit-race")
		var wg sync.WaitGroup
		var failures atomic.Int64
		var firstError string
		var firstErrorOnce sync.Once
		ids := sync.Map{}
		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				dto, err := f.service.Credit(f.ctx, cmd)
				if err != nil {
					failures.Add(1)
					firstErrorOnce.Do(func() { firstError = err.Error() })
					return
				}
				ids.Store(dto.ID, true)
			}()
		}
		wg.Wait()
		if failures.Load() != 0 {
			t.Fatalf("concurrent idempotency failures=%d first=%s", failures.Load(), firstError)
		}
		count := 0
		ids.Range(func(_, _ any) bool { count++; return true })
		if count != 1 {
			t.Fatalf("expected one transaction id, got %d", count)
		}
		f.assertBalance(t, TypeBalance, 200, 0)
		f.assertLedger(t)
	})

	t.Run("ACC-L4-018-019-debit-and-concurrent-partial-reversal", func(t *testing.T) {
		debit, err := f.service.Debit(f.ctx, f.command(TypeBalance, UnitCNY, 100, "debit-original"))
		if err != nil {
			t.Fatal(err)
		}
		originalID, _ := parseID(debit.ID)
		var successes atomic.Int64
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := f.service.Reverse(f.ctx, ReverseCommand{OriginalTransactionID: originalID, Amount: 60, SourceType: "reconciliation_test", SourceID: fmt.Sprintf("reverse-%d-%d", originalID, i), IdempotencyKey: fmt.Sprintf("reverse-key-%d-%d", originalID, i), ActorType: "system"})
				if err == nil {
					successes.Add(1)
					return
				}
				if problem.FromError(err).ErrorCode != "ASSET_REVERSAL_EXCEEDED" {
					t.Errorf("unexpected reverse error: %v", err)
				}
			}()
		}
		wg.Wait()
		if successes.Load() != 1 {
			t.Fatalf("expected one partial reversal, got %d", successes.Load())
		}
		f.assertBalance(t, TypeBalance, 160, 0)
		f.assertLedger(t)
	})

	t.Run("ACC-L4-025-026-029-freeze-commit-release-and-expired-release", func(t *testing.T) {
		_, err := f.service.Credit(f.ctx, f.command(TypeWineCoin, UnitPoint, 100, "coin-grant"))
		if err != nil {
			t.Fatal(err)
		}
		hold, err := f.service.Freeze(f.ctx, FreezeCommand{Command: f.command(TypeWineCoin, UnitPoint, 60, "coin-hold"), ReservationKey: f.key("reservation")})
		if err != nil {
			t.Fatal(err)
		}
		holdID, _ := parseID(hold.ID)
		if _, err = f.service.Commit(f.ctx, HoldCommand{HoldID: holdID, Amount: 40, SourceType: "reconciliation_test", SourceID: f.key("commit"), IdempotencyKey: f.key("commit-key"), ActorType: "system"}); err != nil {
			t.Fatal(err)
		}
		if _, err = f.service.Release(f.ctx, HoldCommand{HoldID: holdID, Amount: 20, SourceType: "reconciliation_test", SourceID: f.key("release"), IdempotencyKey: f.key("release-key"), ActorType: "system"}); err != nil {
			t.Fatal(err)
		}
		f.assertBalance(t, TypeWineCoin, 60, 0)
		past := time.Now().Add(-time.Minute)
		exp := f.command(TypeWineCoin, UnitPoint, 30, "expired-grant")
		exp.ExpiresAt = &past
		if _, err = f.service.Credit(f.ctx, exp); err != nil {
			t.Fatal(err)
		}
		expiredHold, err := f.service.Freeze(f.ctx, FreezeCommand{Command: f.command(TypeWineCoin, UnitPoint, 30, "expired-hold"), ReservationKey: f.key("expired-reservation")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.service.ExpireDueLots(f.ctx, 100); err != nil {
			t.Fatal(err)
		}
		expiredHoldID, _ := parseID(expiredHold.ID)
		if _, err = f.service.Release(f.ctx, HoldCommand{HoldID: expiredHoldID, Amount: 30, SourceType: "reconciliation_test", SourceID: f.key("expired-release"), IdempotencyKey: f.key("expired-release-key"), ActorType: "system"}); err != nil {
			t.Fatal(err)
		}
		f.assertBalance(t, TypeWineCoin, 60, 0)
		f.assertLotInvariant(t)
		f.assertLedger(t)
	})

	t.Run("ACC-L4-027-concurrent-commit-release-has-one-terminal-result", func(t *testing.T) {
		if _, err := f.service.Credit(f.ctx, f.command(TypeWineCoin, UnitPoint, 50, "terminal-race-grant")); err != nil {
			t.Fatal(err)
		}
		hold, err := f.service.Freeze(f.ctx, FreezeCommand{Command: f.command(TypeWineCoin, UnitPoint, 50, "terminal-race-hold"), ReservationKey: f.key("terminal-race-reservation")})
		if err != nil {
			t.Fatal(err)
		}
		holdID, _ := parseID(hold.ID)
		var successes, conflicts atomic.Int64
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := f.service.Commit(f.ctx, HoldCommand{HoldID: holdID, Amount: 50, SourceType: "reconciliation_test", SourceID: f.key("terminal-commit"), IdempotencyKey: f.key("terminal-commit-key"), ActorType: "system"})
			if err == nil {
				successes.Add(1)
			} else if problem.FromError(err).ErrorCode == "ASSET_HOLD_STATE_CONFLICT" {
				conflicts.Add(1)
			} else {
				t.Errorf("commit: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			_, err := f.service.Release(f.ctx, HoldCommand{HoldID: holdID, Amount: 50, SourceType: "reconciliation_test", SourceID: f.key("terminal-release"), IdempotencyKey: f.key("terminal-release-key"), ActorType: "system"})
			if err == nil {
				successes.Add(1)
			} else if problem.FromError(err).ErrorCode == "ASSET_HOLD_STATE_CONFLICT" {
				conflicts.Add(1)
			} else {
				t.Errorf("release: %v", err)
			}
		}()
		wg.Wait()
		if successes.Load() != 1 || conflicts.Load() != 1 {
			t.Fatalf("successes=%d conflicts=%d", successes.Load(), conflicts.Load())
		}
		var row Hold
		if err := f.db.Where("id=?", holdID).Take(&row).Error; err != nil {
			t.Fatal(err)
		}
		if row.Status != "committed" && row.Status != "released" {
			t.Fatalf("unexpected hold status %s", row.Status)
		}
		f.assertLotInvariant(t)
		f.assertLedger(t)
	})

	t.Run("ACC-L4-029-expired-hold-worker-releases-remainder", func(t *testing.T) {
		if _, err := f.service.Credit(f.ctx, f.command(TypeWineCoin, UnitPoint, 25, "hold-expiry-grant")); err != nil {
			t.Fatal(err)
		}
		past := time.Now().Add(-time.Minute)
		hold, err := f.service.Freeze(f.ctx, FreezeCommand{Command: f.command(TypeWineCoin, UnitPoint, 25, "hold-expiry-freeze"), ReservationKey: f.key("hold-expiry-reservation"), HoldExpiresAt: &past})
		if err != nil {
			t.Fatal(err)
		}
		processed, err := f.service.ExpireDueHolds(f.ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		if processed < 1 {
			t.Fatal("expected expired hold to be processed")
		}
		holdID, _ := parseID(hold.ID)
		var stored Hold
		if err := f.db.Where("id=?", holdID).Take(&stored).Error; err != nil {
			t.Fatal(err)
		}
		if stored.Status != "expired" || stored.ReleasedAmount != 25 {
			t.Fatalf("expired hold=%#v", stored)
		}
		f.assertLotInvariant(t)
		f.assertLedger(t)
	})

	t.Run("ACC-L4-004-transaction-pagination-never-splits-multi-entry-transaction", func(t *testing.T) {
		claims := &auth.Claims{AccountType: "customer", CustomerID: idString(f.customerID)}
		first, next, err := f.service.ListCustomerTransactions(f.ctx, claims, TypeWineCoin, pagination.Query{PageSize: 1, Offset: 0, TokenHash: "l4-page"})
		if err != nil {
			t.Fatal(err)
		}
		if len(first) != 1 || next == "" {
			t.Fatalf("first page=%#v next=%q", first, next)
		}
		second, _, err := f.service.ListCustomerTransactions(f.ctx, claims, TypeWineCoin, pagination.Query{PageSize: 1, Offset: 1, TokenHash: "l4-page"})
		if err != nil {
			t.Fatal(err)
		}
		if len(second) != 1 || second[0].ID == first[0].ID {
			t.Fatalf("pagination split or duplicated transaction: first=%#v second=%#v", first, second)
		}
		detail, err := f.service.AdminTransaction(f.ctx, &f.maker, first[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if detail.AvailableDelta != first[0].AvailableDelta || detail.FrozenDelta != first[0].FrozenDelta {
			t.Fatalf("admin detail did not aggregate all entries: list=%#v detail=%#v", first[0], detail)
		}
	})

	t.Run("ACC-L4-017-growth-upgrades-member", func(t *testing.T) {
		dto, err := f.service.Credit(f.ctx, f.command(TypeGrowth, UnitPoint, 1000, "growth-silver"))
		if err != nil {
			t.Fatal(err)
		}
		profileService := member.NewService(f.cfg, f.db, f.ids)
		profile, err := profileService.Profile(f.ctx, &auth.Claims{AccountType: "customer", CustomerID: idString(f.customerID)})
		if err != nil {
			t.Fatal(err)
		}
		if profile.TierCode != "silver" || profile.GrowthValue != 1000 {
			t.Fatalf("unexpected member profile: %#v", profile)
		}
		var historyCount int64
		if err := f.db.Table("member_tier_histories").Where("customer_id=? AND from_tier='normal' AND to_tier='silver'", f.customerID).Count(&historyCount).Error; err != nil || historyCount != 1 {
			t.Fatalf("tier history count=%d err=%v", historyCount, err)
		}
		originalID, _ := parseID(dto.ID)
		if _, err = f.service.Reverse(f.ctx, ReverseCommand{OriginalTransactionID: originalID, Amount: 1000, SourceType: "reconciliation_test", SourceID: f.key("growth-reverse"), IdempotencyKey: f.key("growth-reverse-key"), ActorType: "system"}); err != nil {
			t.Fatal(err)
		}
		profile, err = profileService.Profile(f.ctx, &auth.Claims{AccountType: "customer", CustomerID: idString(f.customerID)})
		if err != nil || profile.TierCode != "normal" {
			t.Fatalf("growth reversal did not downgrade: %#v err=%v", profile, err)
		}
	})

	t.Run("ACC-L4-020-021-022-maker-checker-adjustment", func(t *testing.T) {
		req := AdjustmentCreateReq{CustomerID: idString(f.customerID), AssetType: TypeBalance, Direction: "credit", Amount: 50, ReasonCode: "TEST", Reason: "acceptance adjustment"}
		adjustment, err := f.service.CreateAdjustment(f.ctx, &f.maker, "POST", "/api/v1/admin/asset-adjustments", f.key("adjust-create"), req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.service.ApproveAdjustment(f.ctx, &f.maker, adjustment.ID, f.key("self-approve"), AdjustmentReviewReq{Version: adjustment.Version}); err == nil || problem.FromError(err).ErrorCode != "ADJUSTMENT_SELF_APPROVAL_FORBIDDEN" {
			t.Fatalf("self approval should fail: %v", err)
		}
		approved, err := f.service.ApproveAdjustment(f.ctx, &f.checker, adjustment.ID, f.key("checker-approve"), AdjustmentReviewReq{Version: adjustment.Version})
		if err != nil {
			t.Fatal(err)
		}
		if approved.Status != "succeeded" || approved.AssetTransactionID == "" {
			t.Fatalf("adjustment not issued: %#v", approved)
		}
		f.assertBalance(t, TypeBalance, 210, 0)
	})

	t.Run("ACC-L4-030-031-compensation-crash-recovery", func(t *testing.T) {
		compID := f.ids.Next()
		row := Compensation{ID: compID, AfterSaleID: f.ids.Next(), CustomerID: f.customerID, CompensationNo: fmt.Sprintf("CP%d", compID), Type: "late_delivery", AssetType: TypeBalance, Status: "approved", Amount: 25}
		if err := f.db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		pre, err := f.service.Credit(f.ctx, Command{CustomerID: f.customerID, AssetType: TypeBalance, Unit: UnitCNY, Amount: 25, SourceType: "compensation", SourceID: idString(compID), IdempotencyKey: "compensation-" + row.CompensationNo, ActorType: "system", Metadata: map[string]any{"compensation_no": row.CompensationNo, "after_sale_id": idString(row.AfterSaleID)}})
		if err != nil {
			t.Fatal(err)
		}
		worker := NewWorker(f.cfg, f.service, slog.New(slog.NewTextHandler(io.Discard, nil)))
		row.Status = "issuing"
		if err = f.db.Model(&Compensation{}).Where("id=?", compID).Update("status", "issuing").Error; err != nil {
			t.Fatal(err)
		}
		if err = worker.issueCompensation(f.ctx, row); err != nil {
			t.Fatal(err)
		}
		var stored Compensation
		if err = f.db.Where("id=?", compID).Take(&stored).Error; err != nil {
			t.Fatal(err)
		}
		txID, _ := parseID(pre.ID)
		if stored.Status != "issued" || stored.AssetTransactionID == nil || *stored.AssetTransactionID != txID {
			t.Fatalf("compensation did not converge: %#v", stored)
		}
		f.assertBalance(t, TypeBalance, 235, 0)
		f.assertLedger(t)
	})

	t.Run("ACC-L4-041-042-reconciliation-detects-and-repairs-projection", func(t *testing.T) {
		var balance Balance
		if err := f.db.Table("asset_balances b").Select("b.*").Joins("JOIN asset_accounts a ON a.id=b.account_id").Where("a.owner_type='customer' AND a.owner_id=? AND a.asset_type=? AND b.bucket='available'", f.customerID, TypeBalance).Take(&balance).Error; err != nil {
			t.Fatal(err)
		}
		if err := f.db.Model(&Balance{}).Where("id=?", balance.ID).Update("amount", gorm.Expr("amount+7")).Error; err != nil {
			t.Fatal(err)
		}
		job, err := f.service.RunReconciliation(f.ctx, &f.maker, f.key("reconcile"), ReconcileReq{Scope: "customer", ScopeID: idString(f.customerID)})
		if err != nil {
			t.Fatal(err)
		}
		if job.DifferenceCount == 0 {
			t.Fatal("expected projection mismatch")
		}
		if _, err = f.service.RepairReconciliation(f.ctx, &f.maker, job.ID); err != nil {
			t.Fatal(err)
		}
		f.assertBalance(t, TypeBalance, 235, 0)
		f.assertLedger(t)
	})
}

// command 返回命令。
func (f *acceptanceFixture) command(assetType, unit string, amount int64, label string) Command {
	return Command{CustomerID: f.customerID, AssetType: assetType, Unit: unit, Amount: amount, SourceType: "reconciliation_test", SourceID: f.key(label), IdempotencyKey: f.key(label + "-key"), ActorType: "system", OccurredAt: time.Now().UTC()}
}

// key 返回密钥。
func (f *acceptanceFixture) key(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, f.customerID, f.seq.Add(1))
}

// adminClaims 返回管理端认证声明。
func adminClaims(id uint64, permissions ...string) auth.Claims {
	return auth.Claims{AccountType: "admin", AdminUserID: idString(id), Permissions: permissions}
}

// assertBalance 断言余额符合预期。
func (f *acceptanceFixture) assertBalance(t *testing.T, assetType string, available, frozen int64) {
	t.Helper()
	var rows []Balance
	if err := f.db.Table("asset_balances b").Select("b.*").Joins("JOIN asset_accounts a ON a.id=b.account_id").Where("a.owner_type='customer' AND a.owner_id=? AND a.asset_type=?", f.customerID, assetType).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, row := range rows {
		got[row.Bucket] = row.Amount
	}
	if got["available"] != available || got["frozen"] != frozen {
		t.Fatalf("%s balances=%v want available=%d frozen=%d", assetType, got, available, frozen)
	}
}

// assertLedger 断言账本符合预期。
func (f *acceptanceFixture) assertLedger(t *testing.T) {
	t.Helper()
	var count int64
	if err := f.db.Raw("SELECT COUNT(*) FROM (SELECT t.id FROM asset_transactions t JOIN asset_entries e ON e.transaction_id=t.id WHERE t.id IN (SELECT DISTINCT ce.transaction_id FROM asset_entries ce JOIN asset_accounts ca ON ca.id=ce.account_id WHERE ca.owner_type='customer' AND ca.owner_id=?) GROUP BY t.id HAVING SUM(e.delta)<>0) x", f.customerID).Scan(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("unbalanced transactions=%d", count)
	}
}

// assertLotInvariant 断言Lot 不变量符合预期。
func (f *acceptanceFixture) assertLotInvariant(t *testing.T) {
	t.Helper()
	var count int64
	if err := f.db.Table("asset_lots l").Joins("JOIN asset_accounts a ON a.id=l.account_id").Where("a.owner_type='customer' AND a.owner_id=? AND l.granted_amount<>l.available_amount+l.frozen_amount+l.consumed_amount+l.expired_amount", f.customerID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("lot invariant mismatches=%d", count)
	}
}

// cleanup 清理资产。
func (f *acceptanceFixture) cleanup(t *testing.T) {
	t.Helper()
	for _, assetType := range []string{TypeGrowth, TypeWineCoin, TypeBalance} {
		var amount int64
		_ = f.db.Table("asset_balances b").Select("COALESCE(MAX(b.amount),0)").Joins("JOIN asset_accounts a ON a.id=b.account_id").Where("a.owner_type='customer' AND a.owner_id=? AND a.asset_type=? AND b.bucket='available'", f.customerID, assetType).Scan(&amount).Error
		if amount > 0 {
			unit, _ := UnitFor(assetType)
			_, _ = f.service.Debit(f.ctx, Command{CustomerID: f.customerID, AssetType: assetType, Unit: unit, Amount: amount, SourceType: "reconciliation_test", SourceID: f.key("cleanup"), IdempotencyKey: f.key("cleanup-key"), ActorType: "system"})
		}
	}
	var accountIDs, txIDs, holdIDs, lotIDs, adjustmentIDs, jobIDs []uint64
	_ = f.db.Table("asset_accounts").Where("owner_type='customer' AND owner_id=?", f.customerID).Pluck("id", &accountIDs).Error
	if len(accountIDs) > 0 {
		_ = f.db.Table("asset_entries").Where("account_id IN ?", accountIDs).Distinct("transaction_id").Pluck("transaction_id", &txIDs).Error
		_ = f.db.Table("asset_holds").Where("account_id IN ?", accountIDs).Pluck("id", &holdIDs).Error
		_ = f.db.Table("asset_lots").Where("account_id IN ?", accountIDs).Pluck("id", &lotIDs).Error
	}
	_ = f.db.Table("asset_adjustments").Where("customer_id=?", f.customerID).Pluck("id", &adjustmentIDs).Error
	_ = f.db.Table("asset_reconciliation_jobs").Where("requested_by IN ?", []uint64{88001, 88002}).Pluck("id", &jobIDs).Error
	if len(jobIDs) > 0 {
		f.db.Where("job_id IN ?", jobIDs).Delete(&ReconciliationItem{})
		f.db.Where("id IN ?", jobIDs).Delete(&ReconciliationJob{})
	}
	f.db.Where("customer_id=?", f.customerID).Delete(&Compensation{})
	if len(adjustmentIDs) > 0 {
		f.db.Where("id IN ?", adjustmentIDs).Delete(&Adjustment{})
	}
	if len(holdIDs) > 0 {
		f.db.Where("aggregate_type='asset' AND aggregate_id IN ? AND event_type LIKE 'asset.hold.%'", holdIDs).Delete(&Outbox{})
		f.db.Where("hold_id IN ?", holdIDs).Delete(&HoldLot{})
		f.db.Where("id IN ?", holdIDs).Delete(&Hold{})
	}
	if len(lotIDs) > 0 {
		f.db.Where("aggregate_type='asset' AND aggregate_id IN ? AND event_type='asset.lot.expired'", lotIDs).Delete(&Outbox{})
	}
	if len(accountIDs) > 0 {
		f.db.Where("account_id IN ?", accountIDs).Delete(&Lot{})
	}
	if len(txIDs) > 0 {
		f.db.Where("transaction_id IN ?", txIDs).Delete(&Entry{})
		f.db.Where("id IN ?", txIDs).Delete(&Transaction{})
		f.db.Where("resource_type='asset_transaction' AND resource_id IN ?", txIDs).Delete(&AuditLog{})
		f.db.Where("aggregate_type='asset' AND aggregate_id IN ?", txIDs).Delete(&Outbox{})
	}
	f.db.Exec("DELETE FROM member_tier_histories WHERE customer_id=?", f.customerID)
	f.db.Exec("DELETE FROM member_profiles WHERE customer_id=?", f.customerID)
	if len(accountIDs) > 0 {
		f.db.Where("account_id IN ?", accountIDs).Delete(&Balance{})
		f.db.Where("id IN ?", accountIDs).Delete(&Account{})
	}
	f.db.Exec("DELETE FROM idempotency_keys WHERE actor_type='admin' AND actor_id IN (?,?)", 88001, 88002)
	f.db.Exec("DELETE FROM customers WHERE id=?", f.customerID)
	f.db.Exec("DELETE FROM accounts WHERE id=?", f.accountID)
	if sqlDB, err := f.db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}
