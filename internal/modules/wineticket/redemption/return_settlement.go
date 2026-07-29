package redemption

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/aftersale"
	"jiuxiaoer-admin/backend-go/internal/modules/deliveryreturn"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
)

// WineTicketReturnSettlement 在应用启动时注册到 deliveryreturn.Service。
// 它只负责酒票业务记录；物流、收货、库存和闭环事实归共享配送退回模块负责。
type WineTicketReturnSettlement struct {
	core       *serviceCore
	afterSales *aftersale.Service
	repo       *returnSettlementRepository
}

var (
	_ deliveryreturn.ReturnSettlementHandler         = (*WineTicketReturnSettlement)(nil)
	_ deliveryreturn.ReturnSettlementReceivePreparer = (*WineTicketReturnSettlement)(nil)
	_ deliveryreturn.ReturnSettlementReceivePlan     = (*wineTicketReturnReceivePlan)(nil)
)

type wineTicketReturnReceivePlan struct {
	handler     *WineTicketReturnSettlement
	tx          *gorm.DB
	returnID    uint64
	afterSaleID uint64
	orderID     uint64
	redemption  Redemption
	allocations []RedemptionAllocation
	lots        []core.Lot
}

func NewWineTicketReturnSettlement(
	db *gorm.DB,
	ids IDGenerator,
	afterSales *aftersale.Service,
) *WineTicketReturnSettlement {
	if db == nil || ids == nil {
		return &WineTicketReturnSettlement{afterSales: afterSales}
	}
	return &WineTicketReturnSettlement{
		core:       newSettlementCore(db, ids),
		afterSales: afterSales,
		repo:       newReturnSettlementRepository(),
	}
}

func (h *WineTicketReturnSettlement) RoutingKey() deliveryreturn.ReturnSettlementKey {
	return deliveryreturn.ReturnSettlementKey{
		OrderType:      "wine_ticket_redemption",
		SettlementMode: "wine_ticket",
	}
}

func (h *WineTicketReturnSettlement) SettlementType() string {
	return deliveryreturn.SettlementWineTicketRestore
}

func (h *WineTicketReturnSettlement) InitialBinding(
	ctx context.Context,
	tx *gorm.DB,
	order deliveryreturn.OrderRef,
) (deliveryreturn.ReturnSettlementBinding, error) {
	if err := h.validateCoreDependencies(tx); err != nil {
		return deliveryreturn.ReturnSettlementBinding{}, err
	}
	if order.OrderType != "wine_ticket_redemption" ||
		order.SettlementMode != "wine_ticket" ||
		order.PayStatus != "not_required" ||
		order.PaidAmount != 0 {
		return deliveryreturn.ReturnSettlementBinding{}, problem.Conflict(
			"WT_REDEMPTION_RETURN_INVALID",
			"delivery order is not a zero-cash wine-ticket redemption",
		)
	}
	redemption, err := h.repo.redemptionByOrderID(ctx, tx, order.ID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return deliveryreturn.ReturnSettlementBinding{}, problem.Conflict(
				"WT_REDEMPTION_RETURN_INVALID",
				"wine-ticket redemption link is missing",
			)
		}
		return deliveryreturn.ReturnSettlementBinding{}, err
	}
	if redemption.Status != RedemptionStatusPickedUp &&
		redemption.Status != RedemptionStatusReturnInProgress {
		return deliveryreturn.ReturnSettlementBinding{}, problem.Conflict(
			"WT_REDEMPTION_RETURN_INVALID",
			"wine-ticket redemption is not returnable after pickup",
		)
	}
	redemptionID := redemption.ID
	return deliveryreturn.ReturnSettlementBinding{
		SettlementType:  deliveryreturn.SettlementWineTicketRestore,
		SettlementBizID: &redemptionID,
	}, nil
}

func (h *WineTicketReturnSettlement) Approve(
	ctx context.Context,
	tx *gorm.DB,
	row deliveryreturn.Return,
	_ deliveryreturn.DeliveryOrder,
	order deliveryreturn.OrderRef,
	actorID uint64,
	note string,
) (deliveryreturn.ReturnSettlementApproval, error) {
	if err := h.validateDependencies(tx); err != nil {
		return deliveryreturn.ReturnSettlementApproval{}, err
	}
	if err := validateWineTicketReturnLink(row); err != nil {
		return deliveryreturn.ReturnSettlementApproval{}, err
	}
	redemption, err := h.lockRedemption(ctx, tx, *row.SettlementBizID)
	if err != nil {
		return deliveryreturn.ReturnSettlementApproval{}, err
	}
	if redemption.OrderID != order.ID ||
		redemption.ID != *row.SettlementBizID ||
		(redemption.Status != RedemptionStatusPickedUp &&
			redemption.Status != RedemptionStatusReturnInProgress) {
		return deliveryreturn.ReturnSettlementApproval{}, problem.Conflict(
			"WT_REDEMPTION_RETURN_INVALID",
			"wine-ticket redemption return state changed",
		)
	}
	result, err := h.afterSales.CreateSystemWineTicketReturnWithTx(
		ctx,
		tx,
		aftersale.SystemWineTicketReturnRequest{
			DeliveryReturnID: row.ID,
			OrderID:          row.OrderID,
			ApprovedBy:       actorID,
			ReasonCode:       row.ReasonCode,
			Description:      note,
		},
	)
	if err != nil {
		return deliveryreturn.ReturnSettlementApproval{}, err
	}
	now := h.core.nowShanghai()
	if redemption.Status == RedemptionStatusPickedUp {
		if err := h.repo.markReturnInProgress(
			ctx,
			tx,
			redemption.ID,
			now,
		); err != nil {
			return deliveryreturn.ReturnSettlementApproval{}, err
		}
	}
	if err := h.writeReturnAuditAndOutbox(
		ctx,
		tx,
		actorID,
		"wine_ticket.redemption_return_started",
		redemption,
		row.ID,
		RedemptionStatusReturnInProgress,
		now,
	); err != nil {
		return deliveryreturn.ReturnSettlementApproval{}, err
	}
	redemptionID := redemption.ID
	return deliveryreturn.ReturnSettlementApproval{
		AfterSaleID:     result.AfterSaleID,
		SettlementBizID: &redemptionID,
		OrderStatus:     "returning",
		AfterSaleStatus: "processing",
	}, nil
}

func (h *WineTicketReturnSettlement) SettleReceived(
	ctx context.Context,
	tx *gorm.DB,
	row deliveryreturn.Return,
	afterSale deliveryreturn.AfterSale,
	order deliveryreturn.OrderRef,
) (bool, error) {
	plan, err := h.PrepareReceived(ctx, tx, row, afterSale, order)
	if err != nil {
		return false, err
	}
	return plan.ApplyReceived(ctx, tx, row, afterSale, order)
}

func (h *WineTicketReturnSettlement) PrepareReceived(
	ctx context.Context,
	tx *gorm.DB,
	row deliveryreturn.Return,
	afterSale deliveryreturn.AfterSale,
	order deliveryreturn.OrderRef,
) (deliveryreturn.ReturnSettlementReceivePlan, error) {
	if err := h.validateDependencies(tx); err != nil {
		return nil, err
	}
	if err := validateWineTicketReturnReceiveFacts(row, afterSale, order); err != nil {
		return nil, err
	}
	redemption, err := h.lockRedemption(ctx, tx, *row.SettlementBizID)
	if err != nil {
		return nil, err
	}
	allocations, err := h.lockRedemptionAllocations(ctx, tx, redemption.ID)
	if err != nil {
		return nil, err
	}
	lotIDs := make([]uint64, 0, len(allocations))
	for _, allocation := range allocations {
		lotIDs = append(lotIDs, allocation.LotID)
	}
	lots, err := h.lockLots(ctx, tx, lotIDs)
	if err != nil {
		return nil, err
	}

	plan := &wineTicketReturnReceivePlan{
		handler:     h,
		tx:          tx,
		returnID:    row.ID,
		afterSaleID: afterSale.ID,
		orderID:     order.ID,
		redemption:  redemption,
		allocations: allocations,
		lots:        lots,
	}
	if err := plan.validate(tx, row, afterSale, order); err != nil {
		return nil, err
	}
	return plan, nil
}

func (p *wineTicketReturnReceivePlan) ApplyReceived(
	ctx context.Context,
	tx *gorm.DB,
	row deliveryreturn.Return,
	afterSale deliveryreturn.AfterSale,
	order deliveryreturn.OrderRef,
) (bool, error) {
	if err := p.validate(tx, row, afterSale, order); err != nil {
		return false, err
	}
	h := p.handler
	redemption := p.redemption
	lotByID := make(map[uint64]core.Lot, len(p.lots))
	for _, lot := range p.lots {
		lotByID[lot.ID] = lot
	}

	now := h.core.nowShanghai()
	for _, allocation := range p.allocations {
		lot, ok := lotByID[allocation.LotID]
		if !ok {
			return false, problem.Internal("wine-ticket redemption source lot is missing")
		}
		if lot.Status == core.LotStatusRefunded {
			return false, problem.Conflict(
				"WT_LOT_INVALID_STATUS",
				"refunded wine ticket lot cannot be restored",
			)
		}
		actionKey := fmt.Sprintf(
			"redemption_return_restore:%d:%d",
			row.ID,
			allocation.ID,
		)
		if _, err := h.core.assets.Restore(
			ctx,
			core.NewTransactionAssetRepository(tx),
			core.AssetCommand{
				LotID:           lot.ID,
				OwnerCustomerID: lot.OwnerCustomerID,
				Quantity:        allocation.Quantity,
				TransactionType: core.TransactionTypeRedemptionReturnRestore,
				BizType:         "wine_ticket_redemption_return",
				BizID:           row.ID,
				ActionKey:       actionKey,
				Metadata: map[string]any{
					"delivery_return_id": idString(row.ID),
					"redemption_id":      idString(redemption.ID),
					"allocation_id":      idString(allocation.ID),
				},
				OccurredAt: now,
				ExpiryEvidence: &core.AssetEvidence{
					TransactionType: core.TransactionTypeRedemptionReturnExpire,
					BizType:         "wine_ticket_redemption_return",
					BizID:           row.ID,
					ActionKey: fmt.Sprintf(
						"redemption_return_expire:%d:%d",
						row.ID,
						allocation.ID,
					),
					Metadata: map[string]any{
						"reason":             "restored_after_expiry",
						"restore_action_key": actionKey,
						"restore_biz_type":   "wine_ticket_redemption_return",
						"restore_biz_id":     idString(row.ID),
					},
				},
			},
		); err != nil {
			return false, err
		}
		if allocation.Status != RedemptionAllocationStatusRestored {
			if err := h.repo.markAllocationRestored(
				ctx,
				tx,
				allocation.ID,
				redemption.ID,
				now,
			); err != nil {
				return false, err
			}
		}
	}

	if redemption.Status != RedemptionStatusRestored {
		if err := h.repo.markRedemptionRestored(
			ctx,
			tx,
			redemption.ID,
			now,
		); err != nil {
			return false, err
		}
	}
	if err := h.repo.closeAfterSale(
		ctx,
		tx,
		afterSale.ID,
		row.ID,
		now,
	); err != nil {
		return false, err
	}
	if err := h.repo.markOrderReturned(ctx, tx, order.ID); err != nil {
		return false, err
	}
	if err := h.writeReturnAuditAndOutbox(
		ctx,
		tx,
		0,
		"wine_ticket.redemption_return_restored",
		redemption,
		row.ID,
		RedemptionStatusRestored,
		now,
	); err != nil {
		return false, err
	}
	return true, nil
}

func (p *wineTicketReturnReceivePlan) validate(
	tx *gorm.DB,
	row deliveryreturn.Return,
	afterSale deliveryreturn.AfterSale,
	order deliveryreturn.OrderRef,
) error {
	if p == nil || p.handler == nil || p.tx == nil || tx == nil || p.tx != tx {
		return problem.Internal("wine-ticket return settlement preparation transaction changed")
	}
	if err := p.handler.validateDependencies(tx); err != nil {
		return err
	}
	if err := validateWineTicketReturnReceiveFacts(row, afterSale, order); err != nil {
		return err
	}
	if row.ID != p.returnID ||
		afterSale.ID != p.afterSaleID ||
		order.ID != p.orderID ||
		row.SettlementBizID == nil ||
		*row.SettlementBizID != p.redemption.ID {
		return problem.Internal("wine-ticket return settlement preparation link changed")
	}
	if p.redemption.OrderID != order.ID ||
		(p.redemption.Status != RedemptionStatusReturnInProgress &&
			p.redemption.Status != RedemptionStatusRestored) {
		return problem.Conflict(
			"WT_REDEMPTION_RETURN_INVALID",
			"wine-ticket redemption cannot be restored in its current state",
		)
	}
	if len(p.allocations) == 0 {
		return problem.Internal("wine-ticket redemption allocations are missing")
	}
	var quantity uint
	lotIDs := make([]uint64, 0, len(p.allocations))
	var previousAllocationID uint64
	for index, allocation := range p.allocations {
		if allocation.ID == 0 ||
			(index > 0 && allocation.ID <= previousAllocationID) ||
			allocation.RedemptionID != p.redemption.ID {
			return problem.Internal("wine-ticket redemption allocation lock order changed")
		}
		previousAllocationID = allocation.ID
		quantity += allocation.Quantity
		lotIDs = append(lotIDs, allocation.LotID)
		if allocation.Status != RedemptionAllocationStatusHeld &&
			allocation.Status != RedemptionAllocationStatusConsumed &&
			allocation.Status != RedemptionAllocationStatusRestored {
			return problem.Conflict(
				"WT_REDEMPTION_RETURN_INVALID",
				"wine-ticket redemption allocation cannot be restored",
			)
		}
	}
	if quantity != p.redemption.Quantity {
		return problem.Internal("wine-ticket redemption allocation quantity mismatch")
	}

	expectedLotIDs := uniqueSortedIDs(lotIDs)
	if len(p.lots) != len(expectedLotIDs) {
		return problem.Internal("wine-ticket redemption source lots are missing")
	}
	for index, lot := range p.lots {
		if lot.ID != expectedLotIDs[index] {
			return problem.Internal("wine-ticket redemption lot lock order changed")
		}
		if lot.OwnerCustomerID != p.redemption.CustomerID {
			return problem.Internal("wine-ticket redemption source lot owner changed")
		}
	}
	return nil
}

func validateWineTicketReturnReceiveFacts(
	row deliveryreturn.Return,
	afterSale deliveryreturn.AfterSale,
	order deliveryreturn.OrderRef,
) error {
	if err := validateWineTicketReturnLink(row); err != nil {
		return err
	}
	if row.AfterSaleID == nil ||
		*row.AfterSaleID != afterSale.ID ||
		afterSale.SourceType != "delivery_return" ||
		afterSale.SourceID == nil ||
		*afterSale.SourceID != row.ID ||
		afterSale.ApprovedAmount != 0 ||
		afterSale.RefundedAmount != 0 {
		return problem.Internal("wine-ticket return after-sale link is invalid")
	}
	if order.OrderType != "wine_ticket_redemption" ||
		order.SettlementMode != "wine_ticket" ||
		order.PayStatus != "not_required" ||
		order.PaidAmount != 0 {
		return problem.Internal("wine-ticket return order money facts changed")
	}
	return nil
}

func (h *WineTicketReturnSettlement) validateDependencies(tx *gorm.DB) error {
	if err := h.validateCoreDependencies(tx); err != nil {
		return err
	}
	if h.afterSales == nil {
		return problem.New(
			503,
			"WT_REDEMPTION_RETURN_UNAVAILABLE",
			"Service Unavailable",
			"wine-ticket return settlement dependencies are unavailable",
		)
	}
	return nil
}

func (h *WineTicketReturnSettlement) validateCoreDependencies(tx *gorm.DB) error {
	if h == nil ||
		h.core == nil ||
		h.core.ids == nil ||
		h.core.db == nil ||
		h.core.effects == nil ||
		h.repo == nil ||
		tx == nil {
		return problem.New(
			503,
			"WT_REDEMPTION_RETURN_UNAVAILABLE",
			"Service Unavailable",
			"wine-ticket return settlement dependencies are unavailable",
		)
	}
	return nil
}

func validateWineTicketReturnLink(row deliveryreturn.Return) error {
	if row.SettlementType == nil ||
		*row.SettlementType != deliveryreturn.SettlementWineTicketRestore ||
		row.SettlementBizID == nil ||
		*row.SettlementBizID == 0 {
		return problem.Internal("wine-ticket delivery return settlement link is invalid")
	}
	return nil
}

func (h *WineTicketReturnSettlement) lockRedemption(
	ctx context.Context,
	tx *gorm.DB,
	id uint64,
) (Redemption, error) {
	row, err := h.repo.lockRedemptionByID(ctx, tx, id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Redemption{}, problem.Conflict(
			"WT_REDEMPTION_RETURN_INVALID",
			"wine-ticket redemption link is missing",
		)
	}
	return row, err
}

func (h *WineTicketReturnSettlement) lockRedemptionAllocations(
	ctx context.Context,
	tx *gorm.DB,
	redemptionID uint64,
) ([]RedemptionAllocation, error) {
	return h.repo.lockAllocations(ctx, tx, redemptionID)
}

func (h *WineTicketReturnSettlement) lockLots(
	ctx context.Context,
	tx *gorm.DB,
	ids []uint64,
) ([]core.Lot, error) {
	ids = uniqueSortedIDs(ids)
	return h.repo.lockLots(ctx, tx, ids)
}

func uniqueSortedIDs(values []uint64) []uint64 {
	seen := make(map[uint64]struct{}, len(values))
	out := make([]uint64, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Slice(out, func(left, right int) bool { return out[left] < out[right] })
	return out
}

func (h *WineTicketReturnSettlement) writeReturnAuditAndOutbox(
	ctx context.Context,
	tx *gorm.DB,
	actorID uint64,
	eventType string,
	redemption Redemption,
	deliveryReturnID uint64,
	status string,
	now time.Time,
) error {
	actorType := "system"
	if actorID != 0 {
		actorType = "admin"
	}
	if err := h.core.createAudit(ctx, tx, map[string]any{
		"id":            h.core.ids.Next(),
		"actor_type":    actorType,
		"actor_id":      actorID,
		"action":        eventType,
		"resource_type": "wine_ticket_redemption",
		"resource_id":   redemption.ID,
		"before_data":   jsonData(map[string]any{"status": redemption.Status}),
		"after_data": jsonData(map[string]any{
			"status":             status,
			"delivery_return_id": idString(deliveryReturnID),
		}),
		"result":     "success",
		"request_id": requestctx.RequestIDPtr(ctx),
		"ip_hash":    requestctx.IPHashPtr(ctx),
		"user_agent": requestctx.UserAgentPtr(ctx),
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
		"payload": jsonData(map[string]any{
			"redemption_id":      idString(redemption.ID),
			"redemption_no":      redemption.RedemptionNo,
			"delivery_return_id": idString(deliveryReturnID),
			"status":             status,
		}),
		"status":      "pending",
		"retry_count": 0,
		"request_id":  requestctx.RequestIDPtr(ctx),
		"created_at":  now,
	})
}
