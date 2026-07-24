package asset

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

// Freeze 返回Freeze。
func (s *Service) Freeze(ctx context.Context, cmd FreezeCommand) (HoldDTO, error) {
	if !s.cfg.Asset.WriteEnabled {
		return HoldDTO{}, problem.New(503, "ASSET_WRITE_DISABLED", "Service Unavailable", "asset writes are disabled")
	}
	cmd.Action = "freeze"
	if cmd.AssetType == TypeGrowth {
		return HoldDTO{}, problem.New(422, "ASSET_TYPE_INVALID", "Unprocessable Entity", "growth value cannot be frozen")
	}
	if err := s.validateCommand(cmd.Command, -1); err != nil {
		return HoldDTO{}, err
	}
	if cmd.ReservationKey == "" || len(cmd.ReservationKey) > 128 {
		return HoldDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "reservation_key is required and must not exceed 128 characters")
	}
	var out HoldDTO
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing Hold
		err := tx.Where("reservation_key=?", cmd.ReservationKey).Take(&existing).Error
		if err == nil {
			if existing.OriginalAmount != cmd.Amount || existing.AssetType != cmd.AssetType || existing.SourceType != cmd.SourceType || existing.SourceID != cmd.SourceID {
				return problem.Conflict("ASSET_IDEMPOTENCY_CONFLICT", "reservation key was reused with different data")
			}
			out = holdDTO(existing)
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := s.requireCustomer(ctx, tx, cmd.CustomerID); err != nil {
			return err
		}
		customer, _, err := s.ensureAccounts(ctx, tx, cmd.CustomerID, cmd.AssetType, cmd.Unit)
		if err != nil {
			return err
		}
		available, err := s.lockBalance(ctx, tx, customer.ID, "available")
		if err != nil {
			return err
		}
		frozen, err := s.lockBalance(ctx, tx, customer.ID, "frozen")
		if err != nil {
			return err
		}
		if available.Amount < cmd.Amount {
			return problem.Conflict("ASSET_INSUFFICIENT_AVAILABLE", "available asset balance is insufficient")
		}
		var lots []Lot
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("account_id=? AND available_amount>0", customer.ID).Order("CASE WHEN expires_at IS NULL THEN 1 ELSE 0 END, expires_at, id").Find(&lots).Error; err != nil {
			return err
		}
		hold := Hold{ID: s.ids.Next(), HoldNo: "AH" + fmt.Sprint(s.ids.Next()), ReservationKey: cmd.ReservationKey, AccountID: customer.ID, AssetType: cmd.AssetType, Unit: cmd.Unit, OriginalAmount: cmd.Amount, Status: "active", SourceType: cmd.SourceType, SourceID: cmd.SourceID, ExpiresAt: cmd.HoldExpiresAt, Version: 1}
		remaining := cmd.Amount
		holdLots := make([]HoldLot, 0)
		for _, lot := range lots {
			use := min64(remaining, lot.AvailableAmount)
			if use == 0 {
				continue
			}
			if err := tx.Model(&Lot{}).Where("id=?", lot.ID).Updates(map[string]any{"available_amount": gorm.Expr("available_amount-?", use), "frozen_amount": gorm.Expr("frozen_amount+?", use), "version": gorm.Expr("version+1")}).Error; err != nil {
				return err
			}
			holdLots = append(holdLots, HoldLot{ID: s.ids.Next(), HoldID: hold.ID, LotID: lot.ID, Amount: use})
			remaining -= use
			if remaining == 0 {
				break
			}
		}
		if remaining != 0 {
			return problem.Internal("asset lots do not match available balance")
		}
		availableAfter, frozenAfter := available.Amount-cmd.Amount, frozen.Amount+cmd.Amount
		if err := s.setBalance(ctx, tx, available.ID, availableAfter); err != nil {
			return err
		}
		if err := s.setBalance(ctx, tx, frozen.ID, frozenAfter); err != nil {
			return err
		}
		now := s.now().UTC()
		row := Transaction{ID: s.ids.Next(), TransactionNo: "AT" + fmt.Sprint(s.ids.Next()), AssetType: cmd.AssetType, Unit: cmd.Unit, Action: "freeze", Status: "posted", SourceType: cmd.SourceType, SourceID: cmd.SourceID, IdempotencyKeyHash: keyHash(cmd.IdempotencyKey), RequestHash: requestHash(cmd), Amount: cmd.Amount, ActorType: defaultString(cmd.ActorType, "system"), ActorID: cmd.ActorID, Metadata: jsonData(cmd.Metadata), OccurredAt: now, PostedAt: now}
		entries := []Entry{{ID: s.ids.Next(), TransactionID: row.ID, EntrySeq: 1, AccountID: customer.ID, Bucket: "available", Delta: -cmd.Amount, BalanceAfter: availableAfter}, {ID: s.ids.Next(), TransactionID: row.ID, EntrySeq: 2, AccountID: customer.ID, Bucket: "frozen", Delta: cmd.Amount, BalanceAfter: frozenAfter}}
		if err := tx.Create(&row).Error; err != nil {
			return mapDuplicate(err)
		}
		if err := tx.Create(&entries).Error; err != nil {
			return err
		}
		if err := tx.Create(&hold).Error; err != nil {
			return mapDuplicate(err)
		}
		if err := tx.Create(&holdLots).Error; err != nil {
			return err
		}
		if err := tx.Create(s.audit(ctx, row.ActorType, row.ActorID, "asset.freeze", hold.ID, nil, map[string]any{"hold_no": hold.HoldNo, "amount": hold.OriginalAmount})).Error; err != nil {
			return err
		}
		if err := tx.Create(s.outbox(ctx, "asset.hold.created", hold.ID, map[string]any{"hold_id": idString(hold.ID), "customer_id": idString(cmd.CustomerID), "amount": hold.OriginalAmount})).Error; err != nil {
			return err
		}
		out = holdDTO(hold)
		return nil
	})
	return out, err
}

// Commit 返回Commit。
func (s *Service) Commit(ctx context.Context, cmd HoldCommand) (TransactionDTO, error) {
	cmd.Action = "commit"
	return s.applyHold(ctx, cmd, true)
}

// Release 释放交易DTO。
func (s *Service) Release(ctx context.Context, cmd HoldCommand) (TransactionDTO, error) {
	cmd.Action = "release"
	return s.applyHold(ctx, cmd, false)
}

// expireHold 将Hold标记为过期。
func (s *Service) expireHold(ctx context.Context, cmd HoldCommand) (TransactionDTO, error) {
	cmd.Action = "expire"
	return s.applyHold(ctx, cmd, false)
}

// applyHold 应用Hold。
func (s *Service) applyHold(ctx context.Context, cmd HoldCommand, commit bool) (TransactionDTO, error) {
	if !s.cfg.Asset.WriteEnabled {
		return TransactionDTO{}, problem.New(503, "ASSET_WRITE_DISABLED", "Service Unavailable", "asset writes are disabled")
	}
	if cmd.HoldID == 0 || cmd.SourceType == "" || cmd.SourceID == "" || len(cmd.IdempotencyKey) < 8 {
		return TransactionDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "hold command is invalid")
	}
	var out TransactionDTO
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing Transaction
		err := tx.Where("source_type=? AND source_id=? AND action=?", cmd.SourceType, cmd.SourceID, cmd.Action).Take(&existing).Error
		if err == nil {
			var account Account
			if err := tx.Table("asset_accounts").Where("id IN (SELECT account_id FROM asset_holds WHERE id=?)", cmd.HoldID).Take(&account).Error; err != nil {
				return err
			}
			out, err = s.transactionDTO(ctx, tx, existing, account.OwnerID, nil)
			return err
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var hold Hold
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=?", cmd.HoldID).Take(&hold).Error; err != nil {
			return problem.NotFound("ASSET_HOLD_NOT_FOUND", "asset hold not found")
		}
		remaining := hold.OriginalAmount - hold.CommittedAmount - hold.ReleasedAmount
		amount := cmd.Amount
		if amount == 0 {
			amount = remaining
		}
		if amount <= 0 || amount > remaining {
			return problem.Conflict("ASSET_HOLD_STATE_CONFLICT", "hold amount exceeds active remainder")
		}
		if hold.Status == "committed" || hold.Status == "released" || hold.Status == "expired" {
			return problem.Conflict("ASSET_HOLD_STATE_CONFLICT", "hold is already terminal")
		}
		var customer Account
		if err := tx.Where("id=? AND owner_type='customer'", hold.AccountID).Take(&customer).Error; err != nil {
			return err
		}
		_, control, err := s.ensureAccounts(ctx, tx, customer.OwnerID, hold.AssetType, hold.Unit)
		if err != nil {
			return err
		}
		accounts := []uint64{customer.ID, control.ID}
		sort.Slice(accounts, func(i, j int) bool { return accounts[i] < accounts[j] })
		for _, id := range accounts {
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=?", id).Take(&Account{}).Error; err != nil {
				return err
			}
		}
		available, err := s.lockBalance(ctx, tx, customer.ID, "available")
		if err != nil {
			return err
		}
		frozen, err := s.lockBalance(ctx, tx, customer.ID, "frozen")
		if err != nil {
			return err
		}
		controlAvailable, err := s.lockBalance(ctx, tx, control.ID, "available")
		if err != nil {
			return err
		}
		if frozen.Amount < amount {
			return problem.Internal("hold exceeds frozen balance")
		}
		var allocations []HoldLot
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("hold_id=? AND committed_amount+released_amount<amount", hold.ID).Order("lot_id").Find(&allocations).Error; err != nil {
			return err
		}
		remainingAction := amount
		var availableReturn, platformTransfer int64
		now := s.now().UTC()
		for _, allocation := range allocations {
			left := allocation.Amount - allocation.CommittedAmount - allocation.ReleasedAmount
			use := min64(left, remainingAction)
			if use == 0 {
				continue
			}
			var lot Lot
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=?", allocation.LotID).Take(&lot).Error; err != nil {
				return err
			}
			if lot.FrozenAmount < use {
				return problem.Internal("hold lot exceeds frozen lot amount")
			}
			if commit {
				if err := tx.Model(&Lot{}).Where("id=?", lot.ID).Updates(map[string]any{"frozen_amount": gorm.Expr("frozen_amount-?", use), "consumed_amount": gorm.Expr("consumed_amount+?", use), "version": gorm.Expr("version+1")}).Error; err != nil {
					return err
				}
				if err := tx.Model(&HoldLot{}).Where("id=?", allocation.ID).Update("committed_amount", gorm.Expr("committed_amount+?", use)).Error; err != nil {
					return err
				}
				platformTransfer += use
			} else if lot.ExpiresAt != nil && !lot.ExpiresAt.After(now) {
				if err := tx.Model(&Lot{}).Where("id=?", lot.ID).Updates(map[string]any{"frozen_amount": gorm.Expr("frozen_amount-?", use), "expired_amount": gorm.Expr("expired_amount+?", use), "status": "expired", "version": gorm.Expr("version+1")}).Error; err != nil {
					return err
				}
				if err := tx.Model(&HoldLot{}).Where("id=?", allocation.ID).Update("released_amount", gorm.Expr("released_amount+?", use)).Error; err != nil {
					return err
				}
				platformTransfer += use
			} else {
				if err := tx.Model(&Lot{}).Where("id=?", lot.ID).Updates(map[string]any{"frozen_amount": gorm.Expr("frozen_amount-?", use), "available_amount": gorm.Expr("available_amount+?", use), "version": gorm.Expr("version+1")}).Error; err != nil {
					return err
				}
				if err := tx.Model(&HoldLot{}).Where("id=?", allocation.ID).Update("released_amount", gorm.Expr("released_amount+?", use)).Error; err != nil {
					return err
				}
				availableReturn += use
			}
			remainingAction -= use
			if remainingAction == 0 {
				break
			}
		}
		if remainingAction != 0 {
			return problem.Internal("hold lot allocation is incomplete")
		}
		availableAfter := available.Amount + availableReturn
		frozenAfter := frozen.Amount - amount
		controlAfter := controlAvailable.Amount + platformTransfer
		if err := s.setBalance(ctx, tx, available.ID, availableAfter); err != nil {
			return err
		}
		if err := s.setBalance(ctx, tx, frozen.ID, frozenAfter); err != nil {
			return err
		}
		if platformTransfer > 0 {
			if err := s.setBalance(ctx, tx, controlAvailable.ID, controlAfter); err != nil {
				return err
			}
		}
		status := "active"
		newCommitted, newReleased := hold.CommittedAmount, hold.ReleasedAmount
		if commit {
			newCommitted += amount
		} else {
			newReleased += amount
		}
		if newCommitted+newReleased == hold.OriginalAmount {
			if newCommitted == hold.OriginalAmount {
				status = "committed"
			} else if cmd.Action == "expire" {
				status = "expired"
			} else {
				status = "released"
			}
		} else if newCommitted > 0 {
			status = "partially_committed"
		}
		if err := tx.Model(&Hold{}).Where("id=?", hold.ID).Updates(map[string]any{"committed_amount": newCommitted, "released_amount": newReleased, "status": status, "version": gorm.Expr("version+1")}).Error; err != nil {
			return err
		}
		row := Transaction{ID: s.ids.Next(), TransactionNo: "AT" + fmt.Sprint(s.ids.Next()), AssetType: hold.AssetType, Unit: hold.Unit, Action: cmd.Action, Status: "posted", SourceType: cmd.SourceType, SourceID: cmd.SourceID, IdempotencyKeyHash: keyHash(cmd.IdempotencyKey), RequestHash: requestHash(cmd), Amount: amount, ActorType: defaultString(cmd.ActorType, "system"), ActorID: cmd.ActorID, Metadata: jsonData(map[string]any{"hold_id": idString(hold.ID)}), OccurredAt: now, PostedAt: now}
		entries := make([]Entry, 0, 3)
		seq := uint32(1)
		entries = append(entries, Entry{ID: s.ids.Next(), TransactionID: row.ID, EntrySeq: seq, AccountID: customer.ID, Bucket: "frozen", Delta: -amount, BalanceAfter: frozenAfter})
		seq++
		if availableReturn > 0 {
			entries = append(entries, Entry{ID: s.ids.Next(), TransactionID: row.ID, EntrySeq: seq, AccountID: customer.ID, Bucket: "available", Delta: availableReturn, BalanceAfter: availableAfter})
			seq++
		}
		if platformTransfer > 0 {
			entries = append(entries, Entry{ID: s.ids.Next(), TransactionID: row.ID, EntrySeq: seq, AccountID: control.ID, Bucket: "available", Delta: platformTransfer, BalanceAfter: controlAfter})
		}
		var sum int64
		for _, entry := range entries {
			sum += entry.Delta
		}
		if sum != 0 {
			return problem.Internal("asset hold transaction is unbalanced")
		}
		if err := tx.Create(&row).Error; err != nil {
			return mapDuplicate(err)
		}
		if err := tx.Create(&entries).Error; err != nil {
			return err
		}
		if err := tx.Create(s.audit(ctx, row.ActorType, row.ActorID, "asset."+cmd.Action, hold.ID, nil, map[string]any{"hold_id": idString(hold.ID), "amount": amount})).Error; err != nil {
			return err
		}
		if err := tx.Create(s.outbox(ctx, "asset.hold."+cmd.Action, hold.ID, map[string]any{"hold_id": idString(hold.ID), "amount": amount, "status": status})).Error; err != nil {
			return err
		}
		out = dtoFromTransaction(row, customer.OwnerID, availableReturn, -amount, availableAfter, nil)
		return nil
	})
	return out, err
}

// holdDTO 返回冻结记录 DTO。
func holdDTO(row Hold) HoldDTO {
	return HoldDTO{ID: idString(row.ID), HoldNo: row.HoldNo, ReservationKey: row.ReservationKey, AssetType: row.AssetType, Unit: row.Unit, Status: row.Status, OriginalAmount: row.OriginalAmount, CommittedAmount: row.CommittedAmount, ReleasedAmount: row.ReleasedAmount, Version: row.Version}
}

// ExpireDueLots 将Due 批次余额标记为过期。
func (s *Service) ExpireDueLots(ctx context.Context, limit int) (int, error) {
	if !s.cfg.Asset.WriteEnabled || !s.cfg.Asset.ExpiryEnabled {
		return 0, nil
	}
	if limit <= 0 {
		limit = s.cfg.Asset.WorkerBatchSize
	}
	var ids []uint64
	if err := s.db.WithContext(ctx).Table("asset_lots").Where("expires_at<=? AND available_amount>0 AND status='active'", s.now().UTC()).Order("expires_at,id").Limit(limit).Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	processed := 0
	for _, id := range ids {
		ok, err := s.expireLot(ctx, id)
		if err != nil {
			return processed, err
		}
		if ok {
			processed++
		}
	}
	return processed, nil
}

// ExpireDueHolds 将Due Holds标记为过期。
func (s *Service) ExpireDueHolds(ctx context.Context, limit int) (int, error) {
	if !s.cfg.Asset.WriteEnabled || !s.cfg.Asset.ExpiryEnabled {
		return 0, nil
	}
	if limit <= 0 {
		limit = s.cfg.Asset.WorkerBatchSize
	}
	var ids []uint64
	if err := s.db.WithContext(ctx).Table("asset_holds").Where("status IN ('active','partially_committed') AND expires_at IS NOT NULL AND expires_at<=?", s.now().UTC()).Order("expires_at,id").Limit(limit).Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	processed := 0
	for _, id := range ids {
		sourceID := "hold-" + idString(id)
		_, err := s.expireHold(ctx, HoldCommand{HoldID: id, SourceType: "expiry", SourceID: sourceID, IdempotencyKey: "expire-" + sourceID, ActorType: "system"})
		if err != nil {
			if problem.FromError(err).ErrorCode == "ASSET_HOLD_STATE_CONFLICT" {
				continue
			}
			return processed, err
		}
		processed++
	}
	return processed, nil
}

// expireLot 将Lot标记为过期。
func (s *Service) expireLot(ctx context.Context, lotID uint64) (bool, error) {
	processed := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var lot Lot
		if err := tx.Where("id=? AND expires_at<=? AND available_amount>0", lotID, s.now().UTC()).Take(&lot).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		var customer Account
		if err := tx.Where("id=? AND owner_type='customer'", lot.AccountID).Take(&customer).Error; err != nil {
			return err
		}
		_, control, err := s.ensureAccounts(ctx, tx, customer.OwnerID, customer.AssetType, customer.Unit)
		if err != nil {
			return err
		}
		accounts := []uint64{customer.ID, control.ID}
		sort.Slice(accounts, func(i, j int) bool { return accounts[i] < accounts[j] })
		for _, id := range accounts {
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=?", id).Take(&Account{}).Error; err != nil {
				return err
			}
		}
		available, err := s.lockBalance(ctx, tx, customer.ID, "available")
		if err != nil {
			return err
		}
		controlBalance, err := s.lockBalance(ctx, tx, control.ID, "available")
		if err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("id=? AND expires_at<=? AND available_amount>0", lotID, s.now().UTC()).Take(&lot).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		amount := lot.AvailableAmount
		if available.Amount < amount {
			return problem.Internal("expiring lot exceeds available balance")
		}
		availableAfter, controlAfter := available.Amount-amount, controlBalance.Amount+amount
		if err := s.setBalance(ctx, tx, available.ID, availableAfter); err != nil {
			return err
		}
		if err := s.setBalance(ctx, tx, controlBalance.ID, controlAfter); err != nil {
			return err
		}
		if err := tx.Model(&Lot{}).Where("id=?", lot.ID).Updates(map[string]any{"available_amount": 0, "expired_amount": gorm.Expr("expired_amount+?", amount), "status": "expired", "version": gorm.Expr("version+1")}).Error; err != nil {
			return err
		}
		now := s.now().UTC()
		sourceID := idString(lot.ID)
		row := Transaction{ID: s.ids.Next(), TransactionNo: "AT" + fmt.Sprint(s.ids.Next()), AssetType: customer.AssetType, Unit: customer.Unit, Action: "expire", Status: "posted", SourceType: "expiry", SourceID: sourceID, IdempotencyKeyHash: keyHash("expiry-lot-" + sourceID), RequestHash: requestHash(map[string]any{"lot_id": sourceID, "amount": amount}), Amount: amount, ActorType: "system", Metadata: jsonData(map[string]any{"lot_id": sourceID}), OccurredAt: now, PostedAt: now}
		entries := []Entry{{ID: s.ids.Next(), TransactionID: row.ID, EntrySeq: 1, AccountID: customer.ID, Bucket: "available", Delta: -amount, BalanceAfter: availableAfter}, {ID: s.ids.Next(), TransactionID: row.ID, EntrySeq: 2, AccountID: control.ID, Bucket: "available", Delta: amount, BalanceAfter: controlAfter}}
		if err := tx.Create(&row).Error; err != nil {
			return mapDuplicate(err)
		}
		if err := tx.Create(&entries).Error; err != nil {
			return err
		}
		if err := tx.Create(s.outbox(ctx, "asset.lot.expired", lot.ID, map[string]any{"lot_id": sourceID, "amount": amount})).Error; err != nil {
			return err
		}
		processed = true
		return nil
	})
	return processed, err
}

var _ = time.Now
