package redemption

import (
	"context"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// returnSettlementRepository 将酒票退回持久化封装在单一边界内，
// 并始终沿用 deliveryreturn 传入的事务。
type returnSettlementRepository struct{}

func newReturnSettlementRepository() *returnSettlementRepository {
	return &returnSettlementRepository{}
}

func (r *returnSettlementRepository) redemptionByOrderID(
	ctx context.Context,
	tx *gorm.DB,
	orderID uint64,
) (Redemption, error) {
	var row Redemption
	err := tx.WithContext(ctx).
		Where("order_id = ?", orderID).
		Take(&row).Error
	return row, err
}

func (r *returnSettlementRepository) lockRedemptionByID(
	ctx context.Context,
	tx *gorm.DB,
	id uint64,
) (Redemption, error) {
	var row Redemption
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", id).
		Take(&row).Error
	return row, err
}

func (r *returnSettlementRepository) lockAllocations(
	ctx context.Context,
	tx *gorm.DB,
	redemptionID uint64,
) ([]RedemptionAllocation, error) {
	var rows []RedemptionAllocation
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("redemption_id = ?", redemptionID).
		Order("id ASC").
		Find(&rows).Error
	return rows, err
}

func (r *returnSettlementRepository) lockLots(
	ctx context.Context,
	tx *gorm.DB,
	ids []uint64,
) ([]core.Lot, error) {
	var rows []core.Lot
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id IN ?", ids).
		Order("id ASC").
		Find(&rows).Error
	return rows, err
}

func (r *returnSettlementRepository) markReturnInProgress(
	ctx context.Context,
	tx *gorm.DB,
	redemptionID uint64,
	updatedAt time.Time,
) error {
	return tx.WithContext(ctx).Model(&Redemption{}).
		Where("id = ? AND status = ?", redemptionID, RedemptionStatusPickedUp).
		Updates(map[string]any{
			"status":     RedemptionStatusReturnInProgress,
			"version":    gorm.Expr("version + 1"),
			"updated_at": updatedAt,
		}).Error
}

func (r *returnSettlementRepository) markAllocationRestored(
	ctx context.Context,
	tx *gorm.DB,
	allocationID uint64,
	redemptionID uint64,
	updatedAt time.Time,
) error {
	return tx.WithContext(ctx).Model(&RedemptionAllocation{}).
		Where(
			"id = ? AND redemption_id = ? AND status IN ?",
			allocationID,
			redemptionID,
			[]string{
				RedemptionAllocationStatusHeld,
				RedemptionAllocationStatusConsumed,
			},
		).
		Updates(map[string]any{
			"status":     RedemptionAllocationStatusRestored,
			"updated_at": updatedAt,
		}).Error
}

func (r *returnSettlementRepository) markRedemptionRestored(
	ctx context.Context,
	tx *gorm.DB,
	redemptionID uint64,
	updatedAt time.Time,
) error {
	return tx.WithContext(ctx).Model(&Redemption{}).
		Where(
			"id = ? AND status = ?",
			redemptionID,
			RedemptionStatusReturnInProgress,
		).
		Updates(map[string]any{
			"status":      RedemptionStatusRestored,
			"restored_at": updatedAt,
			"version":     gorm.Expr("version + 1"),
			"updated_at":  updatedAt,
		}).Error
}

func (r *returnSettlementRepository) closeAfterSale(
	ctx context.Context,
	tx *gorm.DB,
	afterSaleID uint64,
	deliveryReturnID uint64,
	closedAt time.Time,
) error {
	return tx.WithContext(ctx).Table("after_sales").
		Where(
			"id = ? AND source_type = 'delivery_return' AND source_id = ?",
			afterSaleID,
			deliveryReturnID,
		).
		Updates(map[string]any{
			"status":    "closed",
			"closed_at": closedAt,
			"version":   gorm.Expr("version + 1"),
		}).Error
}

func (r *returnSettlementRepository) markOrderReturned(
	ctx context.Context,
	tx *gorm.DB,
	orderID uint64,
) error {
	return tx.WithContext(ctx).Table("orders").
		Where(
			"id = ? AND order_type = 'wine_ticket_redemption' AND settlement_mode = 'wine_ticket'",
			orderID,
		).
		Updates(map[string]any{
			"status":            "cancelled",
			"delivery_status":   "returned",
			"after_sale_status": "completed",
			"version":           gorm.Expr("version + 1"),
		}).Error
}

func (r *returnSettlementRepository) createOutbox(
	ctx context.Context,
	tx *gorm.DB,
	values map[string]any,
) error {
	return tx.WithContext(ctx).Table("outbox_events").Create(values).Error
}
