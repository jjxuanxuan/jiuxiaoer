package redemption

import (
	"context"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/dispatch"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

// FulfillmentDispatch 将共享调度模块适配为酒票协调器契约，
// 无需让调度模块导入酒票领域。
type FulfillmentDispatch struct {
	dispatch *dispatch.Service
}

func NewFulfillmentDispatch(service *dispatch.Service) *FulfillmentDispatch {
	return &FulfillmentDispatch{dispatch: service}
}

func (a *FulfillmentDispatch) EnsureRedemptionTaskTx(
	ctx context.Context,
	tx *gorm.DB,
	input RedemptionDispatchCreateInput,
) (RedemptionDispatchState, error) {
	if a == nil || a.dispatch == nil || tx == nil {
		return RedemptionDispatchState{}, redemptionDispatchUnavailable()
	}
	if input.OrderID == 0 ||
		input.ShopID == 0 ||
		input.ScheduledStartAt.IsZero() ||
		!input.ScheduledStartAt.Before(input.ScheduledEndAt) ||
		input.NotBeforeAt.After(input.ScheduledStartAt) {
		return RedemptionDispatchState{}, problem.Internal(
			"invalid wine-ticket scheduled dispatch input",
		)
	}
	deliveryRow, _, err := a.dispatch.EnsurePaidOrderTask(
		ctx,
		tx,
		dispatch.PaidOrderInput{
			OrderID:          input.OrderID,
			ShopID:           input.ShopID,
			AddressSnapshot:  core.CloneJSON(input.AddressSnapshot),
			ScheduledStartAt: &input.ScheduledStartAt,
			ScheduledEndAt:   &input.ScheduledEndAt,
			NotBeforeAt:      &input.NotBeforeAt,
		},
	)
	if err != nil {
		return RedemptionDispatchState{}, err
	}
	return redemptionDispatchStateFromDelivery(deliveryRow), nil
}

func (a *FulfillmentDispatch) LockCancellationPrefixTx(
	ctx context.Context,
	tx *gorm.DB,
	orderID uint64,
) (RedemptionDispatchState, error) {
	if a == nil || a.dispatch == nil || tx == nil {
		return RedemptionDispatchState{}, redemptionDispatchUnavailable()
	}
	state, err := a.dispatch.LockScheduledCancellationPrefixTx(ctx, tx, orderID)
	if err != nil {
		return RedemptionDispatchState{}, err
	}
	return RedemptionDispatchState{
		DeliveryOrderID: state.DeliveryOrderID,
		OrderID:         state.OrderID,
		Status:          state.Status,
		DispatchStatus:  state.DispatchStatus,
		RiderID:         state.RiderID,
		AcceptedAt:      state.AcceptedAt,
		PickedUpAt:      state.PickedUpAt,
		CompletedAt:     state.CompletedAt,
		CancelledAt:     state.CancelledAt,
	}, nil
}

func (a *FulfillmentDispatch) ApplyCancellationTx(
	ctx context.Context,
	tx *gorm.DB,
	input RedemptionDispatchCancelInput,
) error {
	if a == nil || a.dispatch == nil || tx == nil {
		return redemptionDispatchUnavailable()
	}
	return a.dispatch.ApplyScheduledCancellationTx(
		ctx,
		tx,
		dispatch.ScheduledCancellationInput{
			State: dispatch.ScheduledCancellationState{
				DeliveryOrderID: input.State.DeliveryOrderID,
				OrderID:         input.State.OrderID,
				Status:          input.State.Status,
				DispatchStatus:  input.State.DispatchStatus,
				RiderID:         input.State.RiderID,
				AcceptedAt:      input.State.AcceptedAt,
				PickedUpAt:      input.State.PickedUpAt,
				CompletedAt:     input.State.CompletedAt,
				CancelledAt:     input.State.CancelledAt,
			},
			ActorType:   "customer",
			ActorID:     input.CustomerID,
			ReasonCode:  input.ReasonCode,
			CancelledAt: input.CancelledAt,
		},
	)
}

func redemptionDispatchStateFromDelivery(
	row dispatch.DeliveryOrder,
) RedemptionDispatchState {
	return RedemptionDispatchState{
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
