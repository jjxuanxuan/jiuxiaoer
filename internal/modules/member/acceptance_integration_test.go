package member

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
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestL4MemberFormalAcceptance 验证L 4 会员正式验收的预期行为。
func TestL4MemberFormalAcceptance(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run L4 member acceptance tests")
	}
	ctx := context.Background()
	cfg := config.Load()
	cfg.Asset.MemberEnabled = true
	cfg.Asset.ReadEnabled = true
	db, err := mysql.Open(ctx, cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ids := snowflake.New(975)
	service := NewService(cfg, db, ids)
	accountID, customerID := ids.Next(), ids.Next()
	phone := fmt.Sprintf("138%08d", time.Now().UnixNano()%100000000)
	if err := db.Exec("INSERT INTO accounts (id,account_type,phone,status) VALUES (?,'customer',?,'active')", accountID, phone).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO customers (id,account_id,phone,status) VALUES (?,?,?,'active')", customerID, accountID, phone).Error; err != nil {
		t.Fatal(err)
	}
	createdRuleIDs := make([]uint64, 0, 2)
	t.Cleanup(func() {
		db.Exec("UPDATE member_tier_rule_sets SET status='active' WHERE id=9100")
		if len(createdRuleIDs) > 0 {
			db.Exec("DELETE FROM member_tier_rules WHERE rule_set_id IN ?", createdRuleIDs)
			db.Exec("DELETE FROM member_tier_rule_sets WHERE id IN ?", createdRuleIDs)
		}
		db.Exec("DELETE FROM idempotency_keys WHERE actor_type='admin' AND actor_id IN (87001,87002)")
		db.Exec("DELETE FROM member_tier_histories WHERE customer_id=?", customerID)
		db.Exec("DELETE FROM member_profiles WHERE customer_id=?", customerID)
		db.Exec("DELETE FROM asset_balances WHERE account_id IN (SELECT id FROM asset_accounts WHERE owner_type='customer' AND owner_id=?)", customerID)
		db.Exec("DELETE FROM asset_accounts WHERE owner_type='customer' AND owner_id=?", customerID)
		db.Exec("DELETE FROM customers WHERE id=?", customerID)
		db.Exec("DELETE FROM accounts WHERE id=?", accountID)
		if sqlDB, openErr := db.DB(); openErr == nil {
			_ = sqlDB.Close()
		}
	})

	customer := &auth.Claims{AccountType: "customer", CustomerID: idString(customerID)}
	creator := &auth.Claims{AccountType: "admin", AdminUserID: "87001", Permissions: []string{"member_rule:create", "member_rule:activate"}}
	activator := &auth.Claims{AccountType: "admin", AdminUserID: "87002", Permissions: []string{"member_rule:activate"}}

	t.Run("ACC-L4-001-new-customer-profile", func(t *testing.T) {
		profile, err := service.Profile(ctx, customer)
		if err != nil {
			t.Fatal(err)
		}
		if profile.CustomerID != idString(customerID) || profile.TierCode != "normal" || profile.GrowthValue != 0 || profile.RuleVersion == "" {
			t.Fatalf("unexpected initial profile: %#v", profile)
		}
	})

	validRequest := func(version string, silver, gold int64) RuleSetCreateReq {
		return RuleSetCreateReq{
			Version: version, EffectiveAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), Reason: "formal L4 acceptance rule",
			Tiers: []TierReq{{TierCode: "normal", TierName: "Normal", MinGrowth: 0}, {TierCode: "silver", TierName: "Silver", MinGrowth: silver}, {TierCode: "gold", TierName: "Gold", MinGrowth: gold}},
		}
	}

	var candidate RuleSetDTO
	t.Run("ACC-L4-010-create-draft-and-unique-version", func(t *testing.T) {
		req := validRequest(fmt.Sprintf("acc-l4-%d", ids.Next()), 2000, 6000)
		candidate, err = service.CreateRuleSet(ctx, creator, "POST", "/api/v1/admin/member-tier-rules", "member-create-010", req)
		if err != nil {
			t.Fatal(err)
		}
		candidateID, _ := parseID(candidate.ID)
		createdRuleIDs = append(createdRuleIDs, candidateID)
		if candidate.Status != "draft" || len(candidate.Tiers) != 3 {
			t.Fatalf("unexpected candidate: %#v", candidate)
		}
		_, err = service.CreateRuleSet(ctx, creator, "POST", "/api/v1/admin/member-tier-rules", "member-create-dup", req)
		if problem.FromError(err).ErrorCode != "MEMBER_RULE_CONFLICT" {
			t.Fatalf("duplicate version error=%v", err)
		}
	})

	t.Run("ACC-L4-011-invalid-thresholds-have-no-write", func(t *testing.T) {
		before := countRows(t, db, "member_tier_rule_sets")
		invalid := validRequest(fmt.Sprintf("invalid-%d", ids.Next()), 3000, 2000)
		_, err := service.CreateRuleSet(ctx, creator, "POST", "/api/v1/admin/member-tier-rules", "member-invalid-011", invalid)
		if problem.FromError(err).ErrorCode != "MEMBER_RULE_INVALID" {
			t.Fatalf("invalid rules error=%v", err)
		}
		if after := countRows(t, db, "member_tier_rule_sets"); after != before {
			t.Fatalf("invalid rules wrote rows: before=%d after=%d", before, after)
		}
	})

	t.Run("ACC-L4-012-concurrent-activation-has-one-winner", func(t *testing.T) {
		var successes, conflicts atomic.Int64
		var wg sync.WaitGroup
		for i, actor := range []*auth.Claims{creator, activator} {
			i, actor := i, actor
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, callErr := service.ActivateRuleSet(ctx, actor, "POST", "/api/v1/admin/member-tier-rules/:id/activate", candidate.ID, fmt.Sprintf("member-activate-%d", i), ActivateReq{ExpectedStatus: "draft"})
				if callErr == nil {
					successes.Add(1)
				} else if problem.FromError(callErr).ErrorCode == "MEMBER_RULE_CONFLICT" {
					conflicts.Add(1)
				} else {
					t.Errorf("unexpected activation error: %v", callErr)
				}
			}()
		}
		wg.Wait()
		if successes.Load() != 1 || conflicts.Load() != 1 {
			t.Fatalf("successes=%d conflicts=%d", successes.Load(), conflicts.Load())
		}
		var active int64
		if err := db.Table("member_tier_rule_sets").Where("status='active' AND effective_at<=?", time.Now().UTC()).Count(&active).Error; err != nil || active != 1 {
			t.Fatalf("effective active rules=%d err=%v", active, err)
		}
	})

	t.Run("ACC-L4-013-new-rule-recalculates-profile-only", func(t *testing.T) {
		assetAccountID := ids.Next()
		if err := db.Exec("INSERT INTO asset_accounts (id,account_no,owner_type,owner_id,asset_type,unit,status,allow_negative) VALUES (?,?,'customer',?,'growth_value','POINT','active',0)", assetAccountID, fmt.Sprintf("ACC-%d", assetAccountID), customerID).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("INSERT INTO asset_balances (id,account_id,bucket,amount,version) VALUES (?,?, 'available',1500,1),(?,?, 'frozen',0,1)", ids.Next(), assetAccountID, ids.Next(), assetAccountID).Error; err != nil {
			t.Fatal(err)
		}
		beforeTransactions := countRows(t, db, "asset_transactions")
		profile, err := service.Profile(ctx, customer)
		if err != nil {
			t.Fatal(err)
		}
		if profile.RuleVersion != candidate.Version || profile.TierCode != "normal" || profile.GrowthValue != 1500 {
			t.Fatalf("profile did not use activated rule: %#v", profile)
		}
		if afterTransactions := countRows(t, db, "asset_transactions"); afterTransactions != beforeTransactions {
			t.Fatalf("rule evaluation changed transaction history: before=%d after=%d", beforeTransactions, afterTransactions)
		}
	})
}

// countRows 统计Rows的数量。
func countRows(t *testing.T, db *gorm.DB, table string) int64 {
	t.Helper()
	var count int64
	if err := db.Table(table).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}
