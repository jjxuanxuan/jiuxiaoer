package redemption

import (
	"context"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// RedemptionDispatchCoordinator 是酒票权益结算与调度之间的原子边界。
// 实现只能使用传入的 tx：不得自行提交、开启其他事务，
// 也不得在提交前投递外部消息。
//
// LockCancellationPrefixTx 是取消流程中的第一个业务锁。
// 它必须按确定的 ID 顺序锁定 delivery_order 及后续取消所需的
// 所有有效调度任务和邀约。ApplyCancellationTx 只能变更这些已锁记录，
// 不得再获取酒票、订单、配送时段或库存锁。
type RedemptionDispatchCoordinator interface {
	EnsureRedemptionTaskTx(context.Context, *gorm.DB, RedemptionDispatchCreateInput) (RedemptionDispatchState, error)
	LockCancellationPrefixTx(context.Context, *gorm.DB, uint64) (RedemptionDispatchState, error)
	ApplyCancellationTx(context.Context, *gorm.DB, RedemptionDispatchCancelInput) error
}

type RedemptionDispatchCreateInput struct {
	OrderID          uint64
	ShopID           uint64
	AddressSnapshot  datatypes.JSON
	ScheduledStartAt time.Time
	ScheduledEndAt   time.Time
	NotBeforeAt      time.Time
}

type RedemptionDispatchState struct {
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

type RedemptionDispatchCancelInput struct {
	State       RedemptionDispatchState
	CustomerID  uint64
	ReasonCode  string
	CancelledAt time.Time
}
