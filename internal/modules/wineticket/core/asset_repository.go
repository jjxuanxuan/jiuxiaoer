package core

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type transactionAssetRepository struct {
	tx *gorm.DB
}

// NewTransactionAssetRepository 将核心资产变更绑定到已开启的业务事务。
// 调用方仍负责把子域状态与权益变更一并提交。
func NewTransactionAssetRepository(tx *gorm.DB) AssetIssuanceRepository {
	if tx == nil {
		return nil
	}
	return &transactionAssetRepository{tx: tx}
}

func (r *transactionAssetRepository) TransactionByActionKey(
	ctx context.Context,
	actionKey string,
) (Transaction, error) {
	var row Transaction
	err := r.tx.WithContext(ctx).
		Where("action_key = ?", actionKey).
		Take(&row).Error
	return row, assetRepositoryError(err)
}

func (r *transactionAssetRepository) LockLot(
	ctx context.Context,
	lotID uint64,
) (Lot, error) {
	var row Lot
	err := r.tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", lotID).
		Take(&row).Error
	return row, assetRepositoryError(err)
}

func (r *transactionAssetRepository) CreateLot(
	ctx context.Context,
	lot *Lot,
) error {
	return r.tx.WithContext(ctx).Create(lot).Error
}

func (r *transactionAssetRepository) UpdateLot(
	ctx context.Context,
	lotID uint64,
	expectedVersion uint,
	change LotAssetChange,
) (bool, error) {
	values := map[string]any{
		"available_quantity": change.AvailableQuantity,
		"status":             change.Status,
		"version":            gorm.Expr("version + 1"),
		"updated_at":         change.UpdatedAt,
	}
	if change.SetEverUsed {
		values["ever_used"] = change.EverUsed
	}
	result := r.tx.WithContext(ctx).
		Model(&Lot{}).
		Where("id = ? AND version = ?", lotID, expectedVersion).
		Updates(values)
	return result.RowsAffected == 1, result.Error
}

func (r *transactionAssetRepository) CreateTransaction(
	ctx context.Context,
	transaction *Transaction,
) error {
	return r.tx.WithContext(ctx).Create(transaction).Error
}

func assetRepositoryError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrAssetRecordNotFound
	}
	return err
}
