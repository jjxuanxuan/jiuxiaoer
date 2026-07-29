package redemption

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/delivery"
	"jiuxiaoer-admin/backend-go/internal/modules/dispatch"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
)

// WineTicketFulfillmentSettlement 在调度和配送事务中同步推进权益侧履约事实。
// 酒票已在创建核销单时扣减，因此本处理器不会变更批次数量，
// 也不会写入零数量流水。
type WineTicketFulfillmentSettlement struct {
	core *serviceCore
	repo *fulfillmentSettlementRepository
}

func NewWineTicketFulfillmentSettlement(
	db *gorm.DB,
	ids IDGenerator,
) *WineTicketFulfillmentSettlement {
	if db == nil || ids == nil {
		return &WineTicketFulfillmentSettlement{}
	}
	return &WineTicketFulfillmentSettlement{
		core: newSettlementCore(db, ids),
		repo: newFulfillmentSettlementRepository(),
	}
}

func (h *WineTicketFulfillmentSettlement) RoutingKey() delivery.FulfillmentSettlementKey {
	return delivery.FulfillmentSettlementKey{
		OrderType:      redemptionOrderType,
		SettlementMode: redemptionSettlementMode,
	}
}

func (h *WineTicketFulfillmentSettlement) AssignmentRoutingKey() dispatch.AssignmentSettlementKey {
	return dispatch.AssignmentSettlementKey{
		OrderType:      redemptionOrderType,
		SettlementMode: redemptionSettlementMode,
	}
}

func (h *WineTicketFulfillmentSettlement) ApplyFulfillment(
	ctx context.Context,
	tx *gorm.DB,
	fact delivery.FulfillmentSettlementFact,
) error {
	return h.apply(ctx, tx, fulfillmentFact{
		Action:          fact.Action,
		DeliveryID:      fact.Delivery.ID,
		DeliveryOrderID: fact.Delivery.OrderID,
		OrderID:         fact.Order.ID,
		OrderType:       fact.Order.OrderType,
		SettlementMode:  fact.Order.SettlementMode,
		PayStatus:       fact.Order.PayStatus,
		PaidAmount:      fact.Order.PaidAmount,
		OccurredAt:      fact.OccurredAt,
		ActorType:       fact.ActorType,
		ActorID:         fact.ActorID,
	})
}

func (h *WineTicketFulfillmentSettlement) ApplyAssignment(
	ctx context.Context,
	tx *gorm.DB,
	fact dispatch.AssignmentSettlementFact,
) error {
	return h.apply(ctx, tx, fulfillmentFact{
		Action:          delivery.FulfillmentAssigned,
		DeliveryID:      fact.Delivery.ID,
		DeliveryOrderID: fact.Delivery.OrderID,
		OrderID:         fact.OrderID,
		OrderType:       fact.OrderType,
		SettlementMode:  fact.SettlementMode,
		PayStatus:       fact.PayStatus,
		PaidAmount:      fact.PaidAmount,
		OccurredAt:      fact.OccurredAt,
		ActorType:       fact.ActorType,
		ActorID:         fact.ActorID,
	})
}

type fulfillmentFact struct {
	Action          string
	DeliveryID      uint64
	DeliveryOrderID uint64
	OrderID         uint64
	OrderType       string
	SettlementMode  string
	PayStatus       string
	PaidAmount      int64
	OccurredAt      time.Time
	ActorType       string
	ActorID         uint64
}

func (h *WineTicketFulfillmentSettlement) apply(
	ctx context.Context,
	tx *gorm.DB,
	fact fulfillmentFact,
) error {
	if h == nil ||
		h.core == nil ||
		h.core.db == nil ||
		h.core.ids == nil ||
		h.repo == nil ||
		tx == nil {
		return problem.New(
			503,
			"WT_REDEMPTION_FULFILLMENT_UNAVAILABLE",
			"Service Unavailable",
			"wine-ticket fulfillment settlement dependencies are unavailable",
		)
	}
	if fact.OrderID == 0 ||
		fact.DeliveryID == 0 ||
		fact.DeliveryOrderID != fact.OrderID ||
		fact.OrderType != redemptionOrderType ||
		fact.SettlementMode != redemptionSettlementMode ||
		fact.PayStatus != redemptionPayStatus ||
		fact.PaidAmount != 0 {
		return problem.Conflict(
			"WT_REDEMPTION_FULFILLMENT_INVALID",
			"delivery is not linked to a zero-cash wine-ticket redemption",
		)
	}

	redemption, err := h.lockFulfillmentRedemption(ctx, tx, fact.OrderID)
	if err != nil {
		return err
	}
	allocations, err := h.lockFulfillmentAllocations(ctx, tx, redemption.ID)
	if err != nil {
		return err
	}
	if len(allocations) == 0 {
		return problem.Internal("wine-ticket redemption allocations are missing")
	}
	lots, err := h.lockFulfillmentLots(ctx, tx, allocations)
	if err != nil {
		return err
	}
	if err := validateFulfillmentEvidence(redemption, allocations, lots); err != nil {
		return err
	}

	now := fact.OccurredAt.In(core.ShanghaiLocation).Truncate(time.Millisecond)
	if now.IsZero() {
		now = h.core.nowShanghai()
	}
	beforeStatus := redemption.Status
	targetStatus := ""
	switch fact.Action {
	case delivery.FulfillmentAssigned:
		targetStatus = RedemptionStatusAssigned
		if redemption.Status == RedemptionStatusAssigned {
			return nil
		}
		if redemption.Status != RedemptionStatusScheduled {
			return fulfillmentStateConflict(redemption.Status, fact.Action)
		}
		if err := requireAllocationStatus(
			allocations,
			RedemptionAllocationStatusHeld,
		); err != nil {
			return err
		}
	case delivery.FulfillmentPickedUp:
		targetStatus = RedemptionStatusPickedUp
		if redemption.Status == RedemptionStatusPickedUp {
			return nil
		}
		if redemption.Status != RedemptionStatusAssigned &&
			redemption.Status != RedemptionStatusScheduled {
			return fulfillmentStateConflict(redemption.Status, fact.Action)
		}
		if err := requireAllocationStatus(
			allocations,
			RedemptionAllocationStatusHeld,
		); err != nil {
			return err
		}
	case delivery.FulfillmentDelivered:
		targetStatus = RedemptionStatusDelivered
		if redemption.Status == RedemptionStatusDelivered {
			return requireAllocationStatus(
				allocations,
				RedemptionAllocationStatusConsumed,
			)
		}
		if redemption.Status != RedemptionStatusPickedUp {
			return fulfillmentStateConflict(redemption.Status, fact.Action)
		}
		if err := requireAllocationStatus(
			allocations,
			RedemptionAllocationStatusHeld,
		); err != nil {
			return err
		}
	default:
		return problem.Internal("unsupported wine-ticket fulfillment action")
	}

	if targetStatus == RedemptionStatusDelivered {
		affected, err := h.repo.consumeHeldAllocations(
			ctx,
			tx,
			redemption.ID,
			now,
		)
		if err != nil {
			return err
		}
		if affected != int64(len(allocations)) {
			return problem.Conflict(
				"WT_REDEMPTION_FULFILLMENT_INVALID",
				"wine-ticket redemption allocation state changed",
			)
		}
	}

	affected, err := h.repo.transitionRedemption(
		ctx,
		tx,
		redemption.ID,
		beforeStatus,
		targetStatus,
		now,
	)
	if err != nil {
		return err
	}
	if affected != 1 {
		return problem.Conflict(
			"WT_REDEMPTION_FULFILLMENT_INVALID",
			"wine-ticket redemption state changed",
		)
	}
	return h.writeFulfillmentAuditAndOutbox(
		ctx,
		tx,
		fact,
		redemption,
		beforeStatus,
		targetStatus,
		now,
	)
}

func (h *WineTicketFulfillmentSettlement) lockFulfillmentRedemption(
	ctx context.Context,
	tx *gorm.DB,
	orderID uint64,
) (Redemption, error) {
	row, err := h.repo.lockRedemptionByOrderID(ctx, tx, orderID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Redemption{}, problem.Conflict(
			"WT_REDEMPTION_FULFILLMENT_INVALID",
			"wine-ticket redemption link is missing",
		)
	}
	return row, err
}

func (h *WineTicketFulfillmentSettlement) lockFulfillmentAllocations(
	ctx context.Context,
	tx *gorm.DB,
	redemptionID uint64,
) ([]RedemptionAllocation, error) {
	return h.repo.lockAllocations(ctx, tx, redemptionID)
}

func (h *WineTicketFulfillmentSettlement) lockFulfillmentLots(
	ctx context.Context,
	tx *gorm.DB,
	allocations []RedemptionAllocation,
) ([]core.Lot, error) {
	ids := make([]uint64, 0, len(allocations))
	seen := make(map[uint64]struct{}, len(allocations))
	for _, allocation := range allocations {
		if _, ok := seen[allocation.LotID]; ok {
			continue
		}
		seen[allocation.LotID] = struct{}{}
		ids = append(ids, allocation.LotID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return h.repo.lockLots(ctx, tx, ids)
}

func validateFulfillmentEvidence(
	redemption Redemption,
	allocations []RedemptionAllocation,
	lots []core.Lot,
) error {
	lotByID := make(map[uint64]core.Lot, len(lots))
	for _, lot := range lots {
		lotByID[lot.ID] = lot
	}
	var total uint
	for _, allocation := range allocations {
		total += allocation.Quantity
		lot, ok := lotByID[allocation.LotID]
		if !ok ||
			lot.OwnerCustomerID != redemption.CustomerID ||
			lot.ProductID != redemption.ProductID ||
			!lot.ExpiresAt.Equal(allocation.SourceExpiresAt) {
			return problem.Internal("wine-ticket redemption allocation evidence changed")
		}
	}
	if total != redemption.Quantity {
		return problem.Internal("wine-ticket redemption allocation quantity mismatch")
	}
	return nil
}

func requireAllocationStatus(
	allocations []RedemptionAllocation,
	status string,
) error {
	for _, allocation := range allocations {
		if allocation.Status != status {
			return problem.Conflict(
				"WT_REDEMPTION_FULFILLMENT_INVALID",
				"wine-ticket redemption allocation state changed",
			)
		}
	}
	return nil
}

func fulfillmentStateConflict(current, action string) error {
	return problem.Conflict(
		"WT_REDEMPTION_FULFILLMENT_INVALID",
		"wine-ticket redemption cannot apply "+action+" from "+current,
	)
}

func (h *WineTicketFulfillmentSettlement) writeFulfillmentAuditAndOutbox(
	ctx context.Context,
	tx *gorm.DB,
	fact fulfillmentFact,
	redemption Redemption,
	beforeStatus string,
	afterStatus string,
	now time.Time,
) error {
	eventType := "wine_ticket.redemption_" + afterStatus
	actorType := fact.ActorType
	if actorType == "" {
		actorType = "system"
	}
	if err := h.core.createAudit(ctx, tx, map[string]any{
		"id":            h.core.ids.Next(),
		"actor_type":    actorType,
		"actor_id":      fact.ActorID,
		"action":        eventType,
		"resource_type": "wine_ticket_redemption",
		"resource_id":   redemption.ID,
		"order_id":      redemption.OrderID,
		"delivery_id":   fact.DeliveryID,
		"before_data":   core.JSONData(map[string]any{"status": beforeStatus}),
		"after_data": core.JSONData(map[string]any{
			"status":            afterStatus,
			"delivery_order_id": core.IDString(fact.DeliveryID),
		}),
		"result":        "success",
		"before_status": beforeStatus,
		"after_status":  afterStatus,
		"version":       redemption.Version + 1,
		"request_id":    requestctx.RequestIDPtr(ctx),
		"ip_hash":       requestctx.IPHashPtr(ctx),
		"user_agent":    requestctx.UserAgentPtr(ctx),
	}); err != nil {
		return err
	}
	return h.repo.createOutbox(ctx, tx, map[string]any{
		"id":             h.core.ids.Next(),
		"event_id":       uuid.NewString(),
		"event_type":     eventType,
		"event_version":  1,
		"spec_version":   "1.0",
		"aggregate_type": "wine_ticket_redemption",
		"aggregate_id":   redemption.ID,
		"producer":       "wine-ticket",
		"payload": core.JSONData(map[string]any{
			"redemption_id":     core.IDString(redemption.ID),
			"redemption_no":     redemption.RedemptionNo,
			"order_id":          core.IDString(redemption.OrderID),
			"delivery_order_id": core.IDString(fact.DeliveryID),
			"status":            afterStatus,
		}),
		"status":      "pending",
		"retry_count": 0,
		"request_id":  requestctx.RequestIDPtr(ctx),
		"created_at":  now,
	})
}
