package redemption

import (
	"context"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// fulfillmentSettlementRepository 负责履约结算适配器执行的持久化操作。
// 每个方法都只使用配送或调度流程传入的事务。
type fulfillmentSettlementRepository struct{}

func newFulfillmentSettlementRepository() *fulfillmentSettlementRepository {
	return &fulfillmentSettlementRepository{}
}

func (r *fulfillmentSettlementRepository) lockRedemptionByOrderID(
	ctx context.Context,
	tx *gorm.DB,
	orderID uint64,
) (Redemption, error) {
	var row Redemption
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("order_id = ?", orderID).
		Take(&row).Error
	return row, err
}

func (r *fulfillmentSettlementRepository) lockAllocations(
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

func (r *fulfillmentSettlementRepository) lockLots(
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

func (r *fulfillmentSettlementRepository) consumeHeldAllocations(
	ctx context.Context,
	tx *gorm.DB,
	redemptionID uint64,
	updatedAt time.Time,
) (int64, error) {
	result := tx.WithContext(ctx).Model(&RedemptionAllocation{}).
		Where(
			"redemption_id = ? AND status = ?",
			redemptionID,
			RedemptionAllocationStatusHeld,
		).
		Updates(map[string]any{
			"status":     RedemptionAllocationStatusConsumed,
			"updated_at": updatedAt,
		})
	return result.RowsAffected, result.Error
}

func (r *fulfillmentSettlementRepository) transitionRedemption(
	ctx context.Context,
	tx *gorm.DB,
	redemptionID uint64,
	beforeStatus string,
	targetStatus string,
	updatedAt time.Time,
) (int64, error) {
	updates := map[string]any{
		"status":     targetStatus,
		"version":    gorm.Expr("version + 1"),
		"updated_at": updatedAt,
	}
	switch targetStatus {
	case RedemptionStatusPickedUp:
		updates["picked_up_at"] = updatedAt
	case RedemptionStatusDelivered:
		updates["completed_at"] = updatedAt
	}
	result := tx.WithContext(ctx).Model(&Redemption{}).
		Where("id = ? AND status = ?", redemptionID, beforeStatus).
		Updates(updates)
	return result.RowsAffected, result.Error
}

func (r *fulfillmentSettlementRepository) createOutbox(
	ctx context.Context,
	tx *gorm.DB,
	values map[string]any,
) error {
	return tx.WithContext(ctx).Table("outbox_events").Create(values).Error
}
