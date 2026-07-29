package core

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type expiryRepository struct {
	db *gorm.DB
}

type expiryRenewalGuard struct {
	ID     uint64
	LotID  uint64
	Status string
}

func (expiryRenewalGuard) TableName() string { return "wine_ticket_renewals" }

type expiryRefundAllocationGuard struct {
	ID     uint64
	LotID  uint64
	Status string
}

func (expiryRefundAllocationGuard) TableName() string {
	return "wine_ticket_refund_allocations"
}

func (r *expiryRepository) withTransaction(
	ctx context.Context,
	fn func(*gorm.DB) error,
) error {
	return r.db.WithContext(ctx).Transaction(fn)
}

func (r *expiryRepository) dueLots(
	ctx context.Context,
	tx *gorm.DB,
	now time.Time,
	batch int,
) ([]Lot, error) {
	var rows []Lot
	err := r.dueLotsQuery(tx.WithContext(ctx), now, batch).
		Find(&rows).Error
	return rows, err
}

func (r *expiryRepository) dueLotsQuery(
	tx *gorm.DB,
	now time.Time,
	batch int,
) *gorm.DB {
	return tx.
		Model(&Lot{}).
		Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
		Where("status = ? AND expires_at <= ?", LotStatusActive, now).
		Where(`NOT EXISTS (
			SELECT 1 FROM wine_ticket_renewals renewal
			WHERE renewal.lot_id = wine_ticket_lots.id
			  AND renewal.status IN ?
		)`, activeExpiryGuardRenewalStatuses).
		Where(`NOT EXISTS (
			SELECT 1 FROM wine_ticket_refund_allocations refund_allocation
			WHERE refund_allocation.lot_id = wine_ticket_lots.id
			  AND refund_allocation.status = 'held'
		)`).
		Order("expires_at ASC, id ASC").
		Limit(batch)
}

func (r *expiryRepository) expiryGuarded(
	ctx context.Context,
	tx *gorm.DB,
	lotID uint64,
) (bool, error) {
	var renewalCount int64
	if err := tx.WithContext(ctx).
		Model(&expiryRenewalGuard{}).
		Where("lot_id = ? AND status IN ?", lotID, activeExpiryGuardRenewalStatuses).
		Count(&renewalCount).Error; err != nil {
		return false, err
	}
	if renewalCount > 0 {
		return true, nil
	}
	var refundCount int64
	if err := tx.WithContext(ctx).
		Model(&expiryRefundAllocationGuard{}).
		Where("lot_id = ? AND status = 'held'", lotID).
		Count(&refundCount).Error; err != nil {
		return false, err
	}
	return refundCount > 0, nil
}
