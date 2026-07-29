package dispatch

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

// ScheduledCancellationState 仅在调度服务按确定顺序锁定 delivery_order
// 及全部可变调度子记录后返回，随后业务协调器可以锁定 order 及自身记录。
type ScheduledCancellationState struct {
	DeliveryOrderID uint64
	OrderID         uint64
	Status          string
	DispatchStatus  string
	RiderID         *uint64
	AcceptedAt      *time.Time
	PickedUpAt      *time.Time
	CompletedAt     *time.Time
	CancelledAt     *time.Time
}

type ScheduledCancellationInput struct {
	State       ScheduledCancellationState
	ActorType   string
	ActorID     uint64
	ReasonCode  string
	CancelledAt time.Time
}

// LockScheduledCancellationPrefixTx 执行必需的配送优先锁前缀。
// 初始 ID 查询有意不加锁；锁定配送记录后会重新读取全部业务事实。
func (s *Service) LockScheduledCancellationPrefixTx(
	ctx context.Context,
	tx *gorm.DB,
	orderID uint64,
) (ScheduledCancellationState, error) {
	if s == nil || tx == nil || orderID == 0 {
		return ScheduledCancellationState{}, problem.New(
			503,
			"DISPATCH_DEPENDENCY_UNAVAILABLE",
			"Service Unavailable",
			"scheduled dispatch coordinator is unavailable",
		)
	}
	var probe struct {
		ID uint64
	}
	err := tx.WithContext(ctx).Table("delivery_orders").
		Select("id").
		Where("order_id = ? AND deleted_at IS NULL", orderID).
		Take(&probe).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ScheduledCancellationState{}, problem.Conflict(
			"DELIVERY_NOT_FOUND",
			"scheduled delivery is unavailable",
		)
	}
	if err != nil {
		return ScheduledCancellationState{}, err
	}

	var row DeliveryOrder
	if err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND deleted_at IS NULL", probe.ID).
		Take(&row).Error; err != nil {
		return ScheduledCancellationState{}, err
	}
	if row.OrderID != orderID {
		return ScheduledCancellationState{}, problem.Conflict(
			"DELIVERY_INVALID_STATUS",
			"scheduled delivery relation changed",
		)
	}
	if err := lockDispatchChildrenForCancellation(ctx, tx, row.ID); err != nil {
		return ScheduledCancellationState{}, err
	}
	return scheduledCancellationState(row), nil
}

func lockDispatchChildrenForCancellation(
	ctx context.Context,
	tx *gorm.DB,
	deliveryID uint64,
) error {
	var jobs []Job
	if err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("delivery_order_id = ?", deliveryID).
		Order("id ASC").
		Find(&jobs).Error; err != nil {
		return err
	}
	var offers []Offer
	if err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("delivery_order_id = ?", deliveryID).
		Order("id ASC").
		Find(&offers).Error; err != nil {
		return err
	}
	var assignments []Assignment
	return tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("delivery_order_id = ?", deliveryID).
		Order("id ASC").
		Find(&assignments).Error
}

// ApplyScheduledCancellationTx 只变更由 LockScheduledCancellationPrefixTx
// 锁定的记录。所属业务完成全部状态校验后，必须在同一事务中调用本方法。
func (s *Service) ApplyScheduledCancellationTx(
	ctx context.Context,
	tx *gorm.DB,
	input ScheduledCancellationInput,
) error {
	if s == nil || tx == nil || input.State.DeliveryOrderID == 0 ||
		input.State.OrderID == 0 || input.CancelledAt.IsZero() {
		return problem.Internal("invalid scheduled dispatch cancellation input")
	}
	if input.State.PickedUpAt != nil ||
		input.State.CompletedAt != nil ||
		input.State.CancelledAt != nil {
		return problem.Conflict(
			"DELIVERY_INVALID_STATUS",
			"scheduled delivery is no longer cancellable",
		)
	}
	switch input.State.Status {
	case "pending_assign", "accepted":
	default:
		return problem.Conflict(
			"DELIVERY_INVALID_STATUS",
			"scheduled delivery is no longer cancellable",
		)
	}

	now := input.CancelledAt
	if err := tx.WithContext(ctx).Model(&Assignment{}).
		Where(
			"delivery_order_id = ? AND status = 'active'",
			input.State.DeliveryOrderID,
		).
		Update("status", "cancelled").Error; err != nil {
		return err
	}
	if err := tx.WithContext(ctx).Model(&Offer{}).
		Where(
			"delivery_order_id = ? AND status = 'pending'",
			input.State.DeliveryOrderID,
		).
		Updates(map[string]any{
			"status": "cancelled", "responded_at": now,
			"version": gorm.Expr("version + 1"),
		}).Error; err != nil {
		return err
	}
	if err := tx.WithContext(ctx).Model(&Job{}).
		Where(
			"delivery_order_id = ? AND status IN ?",
			input.State.DeliveryOrderID,
			[]string{
				"pending", "scoring", "offering", "grab_open",
				"manual_required", "assigned",
			},
		).
		Updates(map[string]any{
			"status": "cancelled", "status_reason_code": input.ReasonCode,
			"locked_by": nil, "locked_until": nil,
			"version": gorm.Expr("version + 1"),
		}).Error; err != nil {
		return err
	}
	result := tx.WithContext(ctx).Model(&DeliveryOrder{}).
		Where(
			"id = ? AND order_id = ? AND status IN ? AND picked_up_at IS NULL",
			input.State.DeliveryOrderID,
			input.State.OrderID,
			[]string{"pending_assign", "accepted"},
		).
		Updates(map[string]any{
			"status":              "cancelled",
			"dispatch_status":     "cancelled",
			"pickup_ready_status": "cancelled",
			"cancelled_at":        now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return problem.Conflict(
			"DELIVERY_INVALID_STATUS",
			"scheduled delivery state changed",
		)
	}
	return s.createEvent(
		ctx,
		tx,
		"delivery.cancelled",
		"delivery_order",
		input.State.DeliveryOrderID,
		map[string]any{
			"delivery_order_id": idString(input.State.DeliveryOrderID),
			"order_id":          idString(input.State.OrderID),
			"reason_code":       input.ReasonCode,
		},
	)
}

func scheduledCancellationState(row DeliveryOrder) ScheduledCancellationState {
	return ScheduledCancellationState{
		DeliveryOrderID: row.ID,
		OrderID:         row.OrderID,
		Status:          row.Status,
		DispatchStatus:  row.DispatchStatus,
		RiderID:         row.RiderID,
		AcceptedAt:      row.AcceptedAt,
		PickedUpAt:      row.PickedUpAt,
		CompletedAt:     row.CompletedAt,
		CancelledAt:     row.CancelledAt,
	}
}
