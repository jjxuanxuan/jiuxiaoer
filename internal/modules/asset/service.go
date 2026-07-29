package asset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type Service struct {
	cfg  config.Config
	db   *gorm.DB
	ids  *snowflake.Generator
	idem *idempotency.Store
	now  func() time.Time
}

// NewService 创建并初始化服务。
func NewService(cfg config.Config, db *gorm.DB, ids *snowflake.Generator) *Service {
	return &Service{cfg: cfg, db: db, ids: ids, idem: idempotency.NewStore(db), now: time.Now}
}

// UnitFor 返回资产类型对应的单位。
func UnitFor(assetType string) (string, error) {
	switch assetType {
	case TypeGrowth, TypeWineCoin:
		return UnitPoint, nil
	case TypeBalance:
		return UnitCNY, nil
	default:
		return "", problem.New(422, "ASSET_TYPE_INVALID", "Unprocessable Entity", "asset type is invalid")
	}
}

// Credit 为交易DTO执行入账。
func (s *Service) Credit(ctx context.Context, cmd Command) (TransactionDTO, error) {
	cmd.Action = "credit"
	return s.postTransfer(ctx, cmd, 1, nil)
}

// Debit 为交易DTO执行扣账。
func (s *Service) Debit(ctx context.Context, cmd Command) (TransactionDTO, error) {
	cmd.Action = "debit"
	return s.postTransfer(ctx, cmd, -1, nil)
}

// postTransfer 执行转账后的处理。
func (s *Service) postTransfer(ctx context.Context, cmd Command, direction int64, reversalOf *uint64) (TransactionDTO, error) {
	return s.postTransferWithDB(ctx, s.db, cmd, direction, reversalOf)
}

// postTransferWithDB 在指定数据库事务中执行转账。
// 调整单使用该入口把状态、账本、审计和幂等响应放在同一事务边界内。
func (s *Service) postTransferWithDB(ctx context.Context, db *gorm.DB, cmd Command, direction int64, reversalOf *uint64) (TransactionDTO, error) {
	if !s.cfg.Asset.WriteEnabled {
		return TransactionDTO{}, problem.New(503, "ASSET_WRITE_DISABLED", "Service Unavailable", "asset writes are disabled")
	}
	if err := s.validateCommand(cmd, direction); err != nil {
		return TransactionDTO{}, err
	}
	if cmd.OccurredAt.IsZero() {
		cmd.OccurredAt = s.now().UTC()
	}
	reqHash := requestHash(struct {
		CustomerID                                    uint64
		AssetType, Unit, SourceType, SourceID, Action string
		Amount                                        int64
		ExpiresAt                                     *time.Time
		Metadata                                      map[string]any
	}{cmd.CustomerID, cmd.AssetType, cmd.Unit, cmd.SourceType, cmd.SourceID, cmd.Action, cmd.Amount, cmd.ExpiresAt, cmd.Metadata})
	var out TransactionDTO
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing Transaction
		err := tx.Where("source_type=? AND source_id=? AND action=?", cmd.SourceType, cmd.SourceID, cmd.Action).Take(&existing).Error
		if err == nil {
			if existing.RequestHash != reqHash {
				return problem.Conflict("ASSET_IDEMPOTENCY_CONFLICT", "asset source was reused with a different request")
			}
			out, err = s.transactionDTO(ctx, tx, existing, cmd.CustomerID, cmd.Metadata)
			return err
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		now := s.now().UTC()
		row := Transaction{
			ID: s.ids.Next(), TransactionNo: "AT" + fmt.Sprint(s.ids.Next()), AssetType: cmd.AssetType, Unit: cmd.Unit,
			Action: cmd.Action, Status: "posted", SourceType: cmd.SourceType, SourceID: cmd.SourceID,
			IdempotencyKeyHash: keyHash(cmd.IdempotencyKey), RequestHash: reqHash, ReversalOfTransactionID: reversalOf,
			Amount: cmd.Amount, ActorType: defaultString(cmd.ActorType, "system"), ActorID: cmd.ActorID,
			Metadata: jsonData(cmd.Metadata), OccurredAt: cmd.OccurredAt.UTC(), PostedAt: now,
		}
		claim := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected != 1 {
			return problem.Conflict("ASSET_IDEMPOTENCY_CONFLICT", "asset transaction already exists")
		}
		if err := s.requireCustomer(ctx, tx, cmd.CustomerID); err != nil {
			return err
		}
		customer, control, err := s.ensureAccounts(ctx, tx, cmd.CustomerID, cmd.AssetType, cmd.Unit)
		if err != nil {
			return err
		}
		accounts := []Account{customer, control}
		sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
		for _, account := range accounts {
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=?", account.ID).Take(&Account{}).Error; err != nil {
				return err
			}
		}
		customerBalance, err := s.lockBalance(ctx, tx, customer.ID, "available")
		if err != nil {
			return err
		}
		controlBalance, err := s.lockBalance(ctx, tx, control.ID, "available")
		if err != nil {
			return err
		}
		customerDelta := cmd.Amount * direction
		if direction < 0 && customerBalance.Amount < cmd.Amount {
			return problem.Conflict("ASSET_INSUFFICIENT_AVAILABLE", "available asset balance is insufficient")
		}
		if wouldOverflow(customerBalance.Amount, customerDelta) || wouldOverflow(controlBalance.Amount, -customerDelta) {
			return problem.New(422, "ASSET_AMOUNT_INVALID", "Unprocessable Entity", "asset amount overflows balance")
		}
		customerAfter := customerBalance.Amount + customerDelta
		controlAfter := controlBalance.Amount - customerDelta
		if err := s.setBalance(ctx, tx, customerBalance.ID, customerAfter); err != nil {
			return err
		}
		if err := s.setBalance(ctx, tx, controlBalance.ID, controlAfter); err != nil {
			return err
		}
		entries := []Entry{
			{ID: s.ids.Next(), TransactionID: row.ID, EntrySeq: 1, AccountID: customer.ID, Bucket: "available", Delta: customerDelta, BalanceAfter: customerAfter},
			{ID: s.ids.Next(), TransactionID: row.ID, EntrySeq: 2, AccountID: control.ID, Bucket: "available", Delta: -customerDelta, BalanceAfter: controlAfter},
		}
		if entries[0].Delta+entries[1].Delta != 0 {
			return problem.Internal("asset transaction is unbalanced")
		}
		if err := tx.Create(&entries).Error; err != nil {
			return err
		}
		if direction > 0 {
			lot := Lot{ID: s.ids.Next(), AccountID: customer.ID, GrantTransactionID: row.ID, GrantedAmount: cmd.Amount, AvailableAmount: cmd.Amount, ExpiresAt: cmd.ExpiresAt, Status: "active", Version: 1}
			if err := tx.Create(&lot).Error; err != nil {
				return err
			}
		} else if err := s.consumeLots(ctx, tx, customer.ID, cmd.Amount); err != nil {
			return err
		}
		if cmd.AssetType == TypeGrowth {
			if err := s.refreshMemberProfile(ctx, tx, cmd.CustomerID, customerAfter, row.ID); err != nil {
				return err
			}
		}
		if err := tx.Create(s.audit(ctx, row.ActorType, row.ActorID, "asset."+cmd.Action, row.ID, nil, map[string]any{"transaction_no": row.TransactionNo, "asset_type": row.AssetType, "amount": row.Amount})).Error; err != nil {
			return err
		}
		if err := tx.Create(s.outbox(ctx, "asset.transaction.posted", row.ID, map[string]any{"transaction_id": idString(row.ID), "customer_id": idString(cmd.CustomerID), "asset_type": row.AssetType, "action": row.Action, "amount": row.Amount})).Error; err != nil {
			return err
		}
		out = dtoFromTransaction(row, cmd.CustomerID, customerDelta, 0, customerAfter, cmd.Metadata)
		return nil
	})
	if err != nil && problem.FromError(err).ErrorCode == "ASSET_IDEMPOTENCY_CONFLICT" {
		var existing Transaction
		if lookupErr := db.WithContext(ctx).Where("source_type=? AND source_id=? AND action=?", cmd.SourceType, cmd.SourceID, cmd.Action).Take(&existing).Error; lookupErr == nil && existing.RequestHash == reqHash {
			if recovered, dtoErr := s.transactionDTO(ctx, db, existing, cmd.CustomerID, cmd.Metadata); dtoErr == nil {
				return recovered, nil
			}
		}
	}
	return out, err
}

// Reverse 对交易DTO执行冲正。
func (s *Service) Reverse(ctx context.Context, cmd ReverseCommand) (TransactionDTO, error) {
	if !s.cfg.Asset.WriteEnabled {
		return TransactionDTO{}, problem.New(503, "ASSET_WRITE_DISABLED", "Service Unavailable", "asset writes are disabled")
	}
	if cmd.OriginalTransactionID == 0 || cmd.SourceID == "" || len(cmd.IdempotencyKey) < 8 || (cmd.SourceType != "manual_adjustment" && cmd.SourceType != "reconciliation_test") {
		return TransactionDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "reverse command is invalid")
	}
	var out TransactionDTO
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var original Transaction
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=? AND status='posted'", cmd.OriginalTransactionID).Take(&original).Error; err != nil {
			return problem.NotFound("ASSET_TRANSACTION_NOT_FOUND", "asset transaction not found")
		}
		if original.Action != "credit" && original.Action != "debit" {
			return problem.Conflict("ASSET_REVERSAL_UNSUPPORTED", "only direct credit or debit transactions can be reversed")
		}
		var existing Transaction
		err := tx.Where("source_type=? AND source_id=? AND action='reverse'", cmd.SourceType, cmd.SourceID).Take(&existing).Error
		if err == nil {
			var account Account
			if err := tx.Table("asset_accounts a").Select("a.*").Joins("JOIN asset_entries e ON e.account_id=a.id").Where("e.transaction_id=? AND a.owner_type='customer'", original.ID).Take(&account).Error; err != nil {
				return err
			}
			out, err = s.transactionDTO(ctx, tx, existing, account.OwnerID, map[string]any{"reversal_of": idString(original.ID)})
			return err
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var account Account
		if err := tx.Table("asset_accounts a").Select("a.*").Joins("JOIN asset_entries e ON e.account_id=a.id").Where("e.transaction_id=? AND a.owner_type='customer'", original.ID).Take(&account).Error; err != nil {
			return err
		}
		amount := cmd.Amount
		if amount == 0 {
			amount = original.Amount
		}
		var reversed int64
		if err := tx.Table("asset_transactions").Select("COALESCE(SUM(amount),0)").Where("reversal_of_transaction_id=? AND status='posted'", original.ID).Scan(&reversed).Error; err != nil {
			return err
		}
		if amount <= 0 || reversed+amount > original.Amount {
			return problem.Conflict("ASSET_REVERSAL_EXCEEDED", "reversal exceeds original transaction amount")
		}
		customer, control, err := s.ensureAccounts(ctx, tx, account.OwnerID, original.AssetType, original.Unit)
		if err != nil {
			return err
		}
		accounts := []Account{customer, control}
		sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
		for _, a := range accounts {
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=?", a.ID).Take(&Account{}).Error; err != nil {
				return err
			}
		}
		customerBalance, err := s.lockBalance(ctx, tx, customer.ID, "available")
		if err != nil {
			return err
		}
		controlBalance, err := s.lockBalance(ctx, tx, control.ID, "available")
		if err != nil {
			return err
		}
		direction := int64(-1)
		if original.Action == "debit" {
			direction = 1
		}
		delta := amount * direction
		if delta < 0 && customerBalance.Amount < amount {
			return problem.Conflict("ASSET_INSUFFICIENT_AVAILABLE", "available asset balance is insufficient")
		}
		customerAfter, controlAfter := customerBalance.Amount+delta, controlBalance.Amount-delta
		if wouldOverflow(customerBalance.Amount, delta) || wouldOverflow(controlBalance.Amount, -delta) {
			return problem.New(422, "ASSET_AMOUNT_INVALID", "Unprocessable Entity", "asset amount overflows balance")
		}
		if err := s.setBalance(ctx, tx, customerBalance.ID, customerAfter); err != nil {
			return err
		}
		if err := s.setBalance(ctx, tx, controlBalance.ID, controlAfter); err != nil {
			return err
		}
		now := s.now().UTC()
		row := Transaction{ID: s.ids.Next(), TransactionNo: "AT" + fmt.Sprint(s.ids.Next()), AssetType: original.AssetType, Unit: original.Unit, Action: "reverse", Status: "posted", SourceType: cmd.SourceType, SourceID: cmd.SourceID, IdempotencyKeyHash: keyHash(cmd.IdempotencyKey), RequestHash: requestHash(cmd), ReversalOfTransactionID: &original.ID, Amount: amount, ActorType: defaultString(cmd.ActorType, "system"), ActorID: cmd.ActorID, Metadata: jsonData(map[string]any{"reversal_of": idString(original.ID)}), OccurredAt: now, PostedAt: now}
		entries := []Entry{{ID: s.ids.Next(), TransactionID: row.ID, EntrySeq: 1, AccountID: customer.ID, Bucket: "available", Delta: delta, BalanceAfter: customerAfter}, {ID: s.ids.Next(), TransactionID: row.ID, EntrySeq: 2, AccountID: control.ID, Bucket: "available", Delta: -delta, BalanceAfter: controlAfter}}
		if err := tx.Create(&row).Error; err != nil {
			return mapDuplicate(err)
		}
		if err := tx.Create(&entries).Error; err != nil {
			return err
		}
		if delta > 0 {
			lot := Lot{ID: s.ids.Next(), AccountID: customer.ID, GrantTransactionID: row.ID, GrantedAmount: amount, AvailableAmount: amount, Status: "active", Version: 1}
			if err := tx.Create(&lot).Error; err != nil {
				return err
			}
		} else if err := s.consumeLots(ctx, tx, customer.ID, amount); err != nil {
			return err
		}
		if original.AssetType == TypeGrowth {
			if err := s.refreshMemberProfile(ctx, tx, account.OwnerID, customerAfter, row.ID); err != nil {
				return err
			}
		}
		if err := tx.Create(s.audit(ctx, row.ActorType, row.ActorID, "asset.reverse", row.ID, nil, map[string]any{"reversal_of": idString(original.ID), "amount": amount})).Error; err != nil {
			return err
		}
		if err := tx.Create(s.outbox(ctx, "asset.transaction.reversed", row.ID, map[string]any{"transaction_id": idString(row.ID), "reversal_of": idString(original.ID), "amount": amount})).Error; err != nil {
			return err
		}
		out = dtoFromTransaction(row, account.OwnerID, delta, 0, customerAfter, map[string]any{"reversal_of": idString(original.ID)})
		return nil
	})
	return out, err
}

// validateCommand 校验命令是否合法。
func (s *Service) validateCommand(cmd Command, direction int64) error {
	unit, err := UnitFor(cmd.AssetType)
	if err != nil || cmd.Unit != unit {
		return problem.New(422, "ASSET_TYPE_INVALID", "Unprocessable Entity", "asset type and unit do not match")
	}
	if cmd.Amount <= 0 || cmd.Amount > math.MaxInt64/2 || strings.TrimSpace(cmd.SourceID) == "" || strings.TrimSpace(cmd.IdempotencyKey) == "" {
		return problem.New(422, "ASSET_AMOUNT_INVALID", "Unprocessable Entity", "asset amount or source is invalid")
	}
	if len(cmd.IdempotencyKey) < 8 || len(cmd.IdempotencyKey) > 128 {
		return problem.InvalidArgument("IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must be between 8 and 128 characters")
	}
	switch cmd.SourceType {
	case "compensation":
		if cmd.AssetType != TypeBalance || direction != 1 || cmd.Action != "credit" {
			return problem.Forbidden("ASSET_SOURCE_NOT_ALLOWED", "compensation may only credit account balance")
		}
	case "manual_adjustment":
		if direction > 0 && cmd.Action != "credit" ||
			direction < 0 && cmd.Action != "debit" {
			return problem.Forbidden(
				"ASSET_SOURCE_NOT_ALLOWED",
				"asset source action and direction do not match",
			)
		}
	case "reconciliation_test":
		if direction > 0 && cmd.Action != "credit" ||
			direction < 0 && cmd.Action != "debit" && cmd.Action != "freeze" {
			return problem.Forbidden(
				"ASSET_SOURCE_NOT_ALLOWED",
				"asset source action and direction do not match",
			)
		}
	case "expiry":
		if cmd.Action != "expire" {
			return problem.Forbidden("ASSET_SOURCE_NOT_ALLOWED", "expiry source action is invalid")
		}
	default:
		return problem.Forbidden("ASSET_SOURCE_NOT_ALLOWED", "asset source is not registered")
	}
	return nil
}

// ensureAccounts 确保相关账户存在且可用。
func (s *Service) ensureAccounts(ctx context.Context, tx *gorm.DB, customerID uint64, assetType, unit string) (Account, Account, error) {
	var customer, control Account
	findExisting := func() error {
		if err := tx.WithContext(ctx).Where("owner_type='customer' AND owner_id=? AND asset_type=? AND unit=?", customerID, assetType, unit).Take(&customer).Error; err != nil {
			return err
		}
		return tx.WithContext(ctx).Where("owner_type='platform' AND owner_id=1 AND asset_type=? AND unit=?", assetType, unit).Take(&control).Error
	}
	if err := findExisting(); err == nil {
		return customer, control, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return Account{}, Account{}, err
	}

	makeAccount := func(ownerType string, ownerID uint64, allowNegative bool) error {
		row := Account{ID: s.ids.Next(), AccountNo: fmt.Sprintf("AA-%s-%d-%s", ownerType, ownerID, assetType), OwnerType: ownerType, OwnerID: ownerID, AssetType: assetType, Unit: unit, Status: "active", AllowNegative: allowNegative}
		if err := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		var stored Account
		// INSERT IGNORE 可能等待并发创建者。在 MySQL REPEATABLE READ 下
		// 必须执行加锁当前读，才能在之后看见竞争获胜方。
		if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("owner_type=? AND owner_id=? AND asset_type=? AND unit=?", ownerType, ownerID, assetType, unit).Take(&stored).Error; err != nil {
			return err
		}
		for _, bucket := range []string{"available", "frozen"} {
			balance := Balance{ID: s.ids.Next(), AccountID: stored.ID, Bucket: bucket, Amount: 0, Version: 1}
			if err := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&balance).Error; err != nil {
				return err
			}
		}
		return nil
	}
	if err := makeAccount("customer", customerID, false); err != nil {
		return Account{}, Account{}, err
	}
	if err := makeAccount("platform", 1, true); err != nil {
		return Account{}, Account{}, err
	}
	if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("owner_type='customer' AND owner_id=? AND asset_type=? AND unit=?", customerID, assetType, unit).Take(&customer).Error; err != nil {
		return Account{}, Account{}, err
	}
	if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("owner_type='platform' AND owner_id=1 AND asset_type=? AND unit=?", assetType, unit).Take(&control).Error; err != nil {
		return Account{}, Account{}, err
	}
	return customer, control, nil
}

// lockBalance 加锁并获取余额。
func (s *Service) lockBalance(ctx context.Context, tx *gorm.DB, accountID uint64, bucket string) (Balance, error) {
	var row Balance
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("account_id=? AND bucket=?", accountID, bucket).Take(&row).Error
	return row, err
}

// setBalance 设置余额。
func (s *Service) setBalance(ctx context.Context, tx *gorm.DB, id uint64, amount int64) error {
	res := tx.WithContext(ctx).Model(&Balance{}).Where("id=?", id).Updates(map[string]any{"amount": amount, "version": gorm.Expr("version+1")})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return gorm.ErrInvalidData
	}
	return nil
}

// consumeLots 消费并处理批次余额。
func (s *Service) consumeLots(ctx context.Context, tx *gorm.DB, accountID uint64, amount int64) error {
	var lots []Lot
	if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("account_id=? AND available_amount>0", accountID).Order("CASE WHEN expires_at IS NULL THEN 1 ELSE 0 END, expires_at, id").Find(&lots).Error; err != nil {
		return err
	}
	remaining := amount
	for _, lot := range lots {
		use := min64(remaining, lot.AvailableAmount)
		if use == 0 {
			continue
		}
		status := lot.Status
		if lot.AvailableAmount-use == 0 && lot.FrozenAmount == 0 {
			status = "consumed"
		}
		if err := tx.Model(&Lot{}).Where("id=?", lot.ID).Updates(map[string]any{"available_amount": gorm.Expr("available_amount-?", use), "consumed_amount": gorm.Expr("consumed_amount+?", use), "status": status, "version": gorm.Expr("version+1")}).Error; err != nil {
			return err
		}
		remaining -= use
		if remaining == 0 {
			return nil
		}
	}
	return problem.Internal("asset lots do not match available balance")
}

// requireCustomer 校验并确保用户满足要求。
func (s *Service) requireCustomer(ctx context.Context, tx *gorm.DB, id uint64) error {
	var count int64
	if err := tx.WithContext(ctx).Table("customers").Where("id=? AND deleted_at IS NULL AND status='active'", id).Count(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return problem.NotFound("MEMBER_NOT_FOUND", "member not found")
	}
	return nil
}

// refreshMemberProfile 刷新会员资料。
func (s *Service) refreshMemberProfile(ctx context.Context, tx *gorm.DB, customerID uint64, growth int64, transactionID uint64) error {
	var ruleSet struct {
		ID      uint64
		Version string
	}
	if err := tx.WithContext(ctx).Table("member_tier_rule_sets").Select("id,version").Where("status='active' AND effective_at<=?", s.now().UTC()).Order("effective_at DESC,id DESC").Take(&ruleSet).Error; err != nil {
		return err
	}
	var rules []struct {
		TierCode  string
		MinGrowth int64
	}
	if err := tx.WithContext(ctx).Table("member_tier_rules").Select("tier_code,min_growth").Where("rule_set_id=?", ruleSet.ID).Order("min_growth").Find(&rules).Error; err != nil {
		return err
	}
	tier := "normal"
	for _, rule := range rules {
		if growth >= rule.MinGrowth {
			tier = rule.TierCode
		}
	}
	now := s.now().UTC()
	seed := struct {
		CustomerID      uint64
		CurrentGrowth   int64
		TierCode        string
		RuleSetID       uint64
		TierEffectiveAt time.Time
		Version         uint32
	}{customerID, 0, "normal", ruleSet.ID, now, 1}
	if err := tx.Table("member_profiles").Clauses(clause.OnConflict{DoNothing: true}).Create(&seed).Error; err != nil {
		return err
	}
	var before struct {
		TierCode      string
		CurrentGrowth int64
	}
	if err := tx.Table("member_profiles").Clauses(clause.Locking{Strength: "UPDATE"}).Select("tier_code,current_growth").Where("customer_id=?", customerID).Take(&before).Error; err != nil {
		return err
	}
	updates := map[string]any{"current_growth": growth, "tier_code": tier, "rule_set_id": ruleSet.ID, "version": gorm.Expr("version+1")}
	if before.TierCode != tier {
		updates["tier_effective_at"] = now
		history := map[string]any{"id": s.ids.Next(), "event_id": uuid.NewString(), "customer_id": customerID, "from_tier": before.TierCode, "to_tier": tier, "growth_value": growth, "rule_set_id": ruleSet.ID, "asset_transaction_id": transactionID, "created_at": now}
		if err := tx.Table("member_tier_histories").Create(history).Error; err != nil {
			return err
		}
		if err := tx.Create(s.outbox(ctx, "member.tier.changed", customerID, map[string]any{"customer_id": idString(customerID), "from_tier": before.TierCode, "to_tier": tier, "rule_version": ruleSet.Version})).Error; err != nil {
			return err
		}
	}
	return tx.Table("member_profiles").Where("customer_id=?", customerID).Updates(updates).Error
}

// audit 返回审计。
func (s *Service) audit(ctx context.Context, actorType string, actorID uint64, action string, resourceID uint64, before, after any) *AuditLog {
	return &AuditLog{ID: s.ids.Next(), ActorType: actorType, ActorID: actorID, Action: action, ResourceType: "asset_transaction", ResourceID: resourceID, BeforeData: jsonData(before), AfterData: jsonData(after), Result: "success", RequestID: requestctx.RequestIDPtr(ctx), IPHash: requestctx.IPHashPtr(ctx), UserAgent: requestctx.UserAgentPtr(ctx)}
}

// outbox 返回发件箱事件。
func (s *Service) outbox(ctx context.Context, event string, aggregateID uint64, payload any) *Outbox {
	return &Outbox{ID: s.ids.Next(), EventID: uuid.NewString(), EventType: event, AggregateType: "asset", AggregateID: aggregateID, Payload: jsonData(payload), Status: "pending", RequestID: requestctx.RequestIDPtr(ctx)}
}

// dtoFromTransaction 根据交易记录构造 DTO。
func dtoFromTransaction(row Transaction, customerID uint64, availableDelta, frozenDelta, after int64, metadata map[string]any) TransactionDTO {
	return TransactionDTO{ID: idString(row.ID), TransactionNo: row.TransactionNo, CustomerID: idString(customerID), AssetType: row.AssetType, Unit: row.Unit, Action: row.Action, SourceType: row.SourceType, SourceID: row.SourceID, Amount: row.Amount, AvailableDelta: availableDelta, FrozenDelta: frozenDelta, BalanceAfter: after, Metadata: metadata, OccurredAt: row.OccurredAt.UTC().Format(time.RFC3339Nano), PostedAt: row.PostedAt.UTC().Format(time.RFC3339Nano)}
}

// transactionDTO 返回交易DTO。
func (s *Service) transactionDTO(ctx context.Context, tx *gorm.DB, row Transaction, customerID uint64, metadata map[string]any) (TransactionDTO, error) {
	var entry Entry
	err := tx.WithContext(ctx).Table("asset_entries e").Select("e.*").Joins("JOIN asset_accounts a ON a.id=e.account_id").Where("e.transaction_id=? AND a.owner_type='customer' AND a.owner_id=?", row.ID, customerID).Order("e.entry_seq").Take(&entry).Error
	if err != nil {
		return TransactionDTO{}, err
	}
	available, frozen := int64(0), int64(0)
	if entry.Bucket == "available" {
		available = entry.Delta
	} else {
		frozen = entry.Delta
	}
	return dtoFromTransaction(row, customerID, available, frozen, entry.BalanceAfter, metadata), nil
}

// jsonData 将输入值序列化为 JSON 数据。
func jsonData(v any) datatypes.JSON { b, _ := json.Marshal(v); return b }

// requestHash 返回请求哈希。
func requestHash(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// keyHash 返回密钥哈希。
func keyHash(v string) string { sum := sha256.Sum256([]byte(v)); return hex.EncodeToString(sum[:]) }

// idString 将数字 ID 转换为字符串。
func idString(v uint64) string { return strconv.FormatUint(v, 10) }

// defaultString 在字符串为空时返回默认值。
func defaultString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// min64 返回两个 64 位整数中的较小值。
func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// wouldOverflow 判断加法是否会溢出。
func wouldOverflow(a, b int64) bool {
	return (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b)
}

// mapDuplicate 映射并返回重复项。
func mapDuplicate(err error) error {
	if strings.Contains(strings.ToLower(err.Error()), "duplicate") {
		return problem.Conflict("ASSET_IDEMPOTENCY_CONFLICT", "asset transaction already exists")
	}
	return err
}

// parseID 解析并校验字符串形式的 ID。
func parseID(raw string) (uint64, error) {
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

// customerIDFromClaims 从认证声明中解析并返回用户 ID。
func customerIDFromClaims(claims *auth.Claims) (uint64, error) {
	if claims == nil || claims.AccountType != "customer" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "customer account required")
	}
	id, err := parseID(claims.CustomerID)
	if err != nil {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "invalid customer identity")
	}
	return id, nil
}

// adminIDWithPermission 返回具备指定权限的管理员 ID。
func adminIDWithPermission(claims *auth.Claims, permission string) (uint64, error) {
	if claims == nil || claims.AccountType != "admin" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "admin account required")
	}
	for _, p := range claims.Permissions {
		if p == permission {
			id, err := parseID(claims.AdminUserID)
			if err != nil {
				break
			}
			return id, nil
		}
	}
	return 0, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
}

type ListQuery struct {
	pagination.Query
	AssetType, SourceType, Action string
}
