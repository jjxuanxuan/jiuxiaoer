package integrity

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	giftdomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/gift"
	redemptiondomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/redemption"
	refunddomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/refund"
)

type reconciliationTransactionAggregate struct {
	Count int
	Delta int64
}

type reconciliationTransactionKey struct {
	LotID           uint64
	TransactionType string
	BizType         string
	BizID           uint64
}

type reconciliationLotAllocations struct {
	Redemptions []redemptiondomain.RedemptionAllocation
	Gifts       []giftdomain.GiftAllocation
	Refunds     []refunddomain.RefundAllocation
}

func (s *IntegrityService) scanLots(
	ctx context.Context,
	tx *gorm.DB,
	afterID uint64,
	upperID *uint64,
	limit int,
) (int, uint64, []reconciliationDiscrepancy, error) {
	lots, err := s.repo.listIntegrityLots(
		ctx,
		tx,
		afterID,
		upperID,
		limit,
	)
	if err != nil {
		return 0, afterID, nil, err
	}
	if len(lots) == 0 {
		return 0, afterID, nil, nil
	}
	lotIDs := make([]uint64, 0, len(lots))
	for _, lot := range lots {
		lotIDs = append(lotIDs, lot.ID)
	}

	transactions, err := s.repo.listIntegrityLotTransactions(
		ctx,
		tx,
		lotIDs,
	)
	if err != nil {
		return 0, afterID, nil, err
	}
	transactionsByLot := make(map[uint64][]core.Transaction, len(lots))
	transactionFacts := make(
		map[reconciliationTransactionKey]reconciliationTransactionAggregate,
		len(transactions),
	)
	for _, entry := range transactions {
		transactionsByLot[entry.LotID] = append(
			transactionsByLot[entry.LotID],
			entry,
		)
		key := reconciliationTransactionKey{
			LotID: entry.LotID, TransactionType: entry.TransactionType,
			BizType: entry.BizType, BizID: entry.BizID,
		}
		aggregate := transactionFacts[key]
		aggregate.Count++
		aggregate.Delta += int64(entry.QuantityDelta)
		transactionFacts[key] = aggregate
	}

	allocations, err := s.repo.loadIntegrityLotAllocations(ctx, tx, lotIDs)
	if err != nil {
		return 0, afterID, nil, err
	}
	redemptionsByLot := make(map[uint64][]redemptiondomain.RedemptionAllocation)
	for _, allocation := range allocations.Redemptions {
		redemptionsByLot[allocation.LotID] = append(
			redemptionsByLot[allocation.LotID],
			allocation,
		)
	}
	giftsByLot := make(map[uint64][]giftdomain.GiftAllocation)
	for _, allocation := range allocations.Gifts {
		giftsByLot[allocation.SourceLotID] = append(
			giftsByLot[allocation.SourceLotID],
			allocation,
		)
	}
	refundsByLot := make(map[uint64][]refunddomain.RefundAllocation)
	for _, allocation := range allocations.Refunds {
		refundsByLot[allocation.LotID] = append(
			refundsByLot[allocation.LotID],
			allocation,
		)
	}

	discrepancies := make([]reconciliationDiscrepancy, 0)
	for _, lot := range lots {
		if fact := checkLotReplay(lot, transactionsByLot[lot.ID]); fact != nil {
			discrepancies = append(discrepancies, *fact)
		}
		if fact := checkLotAllocationProjection(
			lot,
			redemptionsByLot[lot.ID],
			giftsByLot[lot.ID],
			refundsByLot[lot.ID],
			transactionFacts,
		); fact != nil {
			discrepancies = append(discrepancies, *fact)
		}
	}
	return len(lots), lots[len(lots)-1].ID, discrepancies, nil
}

func checkLotReplay(
	lot core.Lot,
	transactions []core.Transaction,
) *reconciliationDiscrepancy {
	balance := int64(0)
	replayValid := len(transactions) > 0
	var firstInvalidTransactionID uint64
	for _, entry := range transactions {
		before := int64(entry.BeforeAvailableQuantity)
		after := int64(entry.AfterAvailableQuantity)
		next := balance + int64(entry.QuantityDelta)
		if entry.OwnerCustomerID != lot.OwnerCustomerID ||
			entry.QuantityDelta == 0 ||
			before != balance ||
			after != next ||
			next < 0 ||
			next > int64(lot.TotalQuantity) {
			replayValid = false
			firstInvalidTransactionID = entry.ID
			break
		}
		balance = next
	}
	statusValid := (lot.Status == LotStatusActive && lot.AvailableQuantity > 0) ||
		((lot.Status == LotStatusDepleted ||
			lot.Status == LotStatusExpired ||
			lot.Status == LotStatusRefunded) &&
			lot.AvailableQuantity == 0)
	if replayValid &&
		balance == int64(lot.AvailableQuantity) &&
		statusValid {
		return nil
	}
	return &reconciliationDiscrepancy{
		Rule:    reconciliationRuleLotReplay,
		Kind:    "lot_transaction_replay",
		BizType: "wine_ticket_lot", BizID: lot.ID,
		BizNo: &lot.LotNo, IssuerMerchantID: &lot.IssuerMerchantID,
		Severity: "P1",
		Expected: map[string]any{
			"replayed_available_quantity": lot.AvailableQuantity,
			"transaction_chain_valid":     true,
			"status_matches_quantity":     true,
		},
		Actual: map[string]any{
			"stored_available_quantity":    lot.AvailableQuantity,
			"replayed_available_quantity":  balance,
			"transaction_chain_valid":      replayValid,
			"status":                       lot.Status,
			"status_matches_quantity":      statusValid,
			"transaction_count":            len(transactions),
			"first_invalid_transaction_id": firstInvalidTransactionID,
		},
	}
}

func checkLotAllocationProjection(
	lot core.Lot,
	redemptions []redemptiondomain.RedemptionAllocation,
	gifts []giftdomain.GiftAllocation,
	refunds []refunddomain.RefundAllocation,
	transactionFacts map[reconciliationTransactionKey]reconciliationTransactionAggregate,
) *reconciliationDiscrepancy {
	var (
		heldQuantity      uint64
		extractedQuantity uint64
		backingValid      = true
		invalidStatus     string
		invalidBizID      uint64
	)
	for _, allocation := range redemptions {
		switch allocation.Status {
		case RedemptionAllocationStatusHeld:
			heldQuantity += uint64(allocation.Quantity)
			backingValid = backingValid && allocationHoldBacked(
				transactionFacts,
				reconciliationTransactionKey{
					LotID:           lot.ID,
					TransactionType: TransactionTypeRedemptionHold,
					BizType:         "wine_ticket_redemption",
					BizID:           allocation.RedemptionID,
				},
				allocation.Quantity,
			)
		case RedemptionAllocationStatusConsumed:
			extractedQuantity += uint64(allocation.Quantity)
			backingValid = backingValid && allocationHoldBacked(
				transactionFacts,
				reconciliationTransactionKey{
					LotID:           lot.ID,
					TransactionType: TransactionTypeRedemptionHold,
					BizType:         "wine_ticket_redemption",
					BizID:           allocation.RedemptionID,
				},
				allocation.Quantity,
			)
		case RedemptionAllocationStatusRestored:
			// 已恢复的分配记录有意不计入冻结量或已提取量投影。
		default:
			invalidStatus = allocation.Status
			invalidBizID = allocation.RedemptionID
		}
	}
	for _, allocation := range gifts {
		switch allocation.Status {
		case GiftAllocationStatusHeld:
			heldQuantity += uint64(allocation.Quantity)
			backingValid = backingValid && allocationHoldBacked(
				transactionFacts,
				reconciliationTransactionKey{
					LotID:           lot.ID,
					TransactionType: TransactionTypeGiftHold,
					BizType:         "gift",
					BizID:           allocation.GiftID,
				},
				allocation.Quantity,
			)
		case GiftAllocationStatusClaimed, GiftAllocationStatusRestored:
		default:
			invalidStatus = allocation.Status
			invalidBizID = allocation.GiftID
		}
	}
	for _, allocation := range refunds {
		switch allocation.Status {
		case RefundAllocationHeld:
			heldQuantity += uint64(allocation.Quantity)
			backingValid = backingValid && allocationHoldBacked(
				transactionFacts,
				reconciliationTransactionKey{
					LotID:           lot.ID,
					TransactionType: TransactionTypeRefundHold,
					BizType:         "refund",
					BizID:           allocation.WineTicketRefundID,
				},
				allocation.Quantity,
			)
		case RefundAllocationConsumed, RefundAllocationRestored:
		default:
			invalidStatus = allocation.Status
			invalidBizID = allocation.WineTicketRefundID
		}
	}
	projectionBounded := uint64(lot.AvailableQuantity)+
		heldQuantity+
		extractedQuantity <= uint64(lot.TotalQuantity)
	if backingValid && invalidStatus == "" && projectionBounded {
		return nil
	}
	return &reconciliationDiscrepancy{
		Rule:    reconciliationRuleAllocationView,
		Kind:    "allocation_projection",
		BizType: "wine_ticket_lot", BizID: lot.ID,
		BizNo: &lot.LotNo, IssuerMerchantID: &lot.IssuerMerchantID,
		Severity: "P1",
		Expected: map[string]any{
			"held_quantity_source":      "held_redemption_plus_held_gift_plus_held_refund",
			"extracted_quantity_source": "consumed_redemption_only",
			"restored_excluded":         true,
			"hold_transaction_backing":  true,
			"projection_within_total":   true,
		},
		Actual: map[string]any{
			"available_quantity":       lot.AvailableQuantity,
			"held_quantity":            heldQuantity,
			"extracted_quantity":       extractedQuantity,
			"total_quantity":           lot.TotalQuantity,
			"hold_transaction_backing": backingValid,
			"projection_within_total":  projectionBounded,
			"invalid_status":           invalidStatus,
			"invalid_business_id":      invalidBizID,
		},
	}
}

func allocationHoldBacked(
	facts map[reconciliationTransactionKey]reconciliationTransactionAggregate,
	key reconciliationTransactionKey,
	quantity uint,
) bool {
	fact := facts[key]
	return fact.Count == 1 && fact.Delta == -int64(quantity)
}

func (r *reconciliationRepository) listIntegrityLots(
	ctx context.Context,
	tx *gorm.DB,
	afterID uint64,
	upperID *uint64,
	limit int,
) ([]core.Lot, error) {
	var rows []core.Lot
	query := r.idWindow(
		tx.WithContext(ctx).Model(&core.Lot{}),
		"id",
		afterID,
		upperID,
	)
	err := query.Order("id").Limit(limit).Find(&rows).Error
	return rows, err
}

func (r *reconciliationRepository) listIntegrityLotTransactions(
	ctx context.Context,
	tx *gorm.DB,
	lotIDs []uint64,
) ([]core.Transaction, error) {
	var rows []core.Transaction
	err := tx.WithContext(ctx).
		Where("lot_id IN ?", lotIDs).
		Order("lot_id, created_at, id").
		Find(&rows).Error
	return rows, err
}

func (r *reconciliationRepository) loadIntegrityLotAllocations(
	ctx context.Context,
	tx *gorm.DB,
	lotIDs []uint64,
) (reconciliationLotAllocations, error) {
	var rows reconciliationLotAllocations
	if len(lotIDs) == 0 {
		return rows, nil
	}
	if err := tx.WithContext(ctx).
		Where("lot_id IN ?", lotIDs).
		Find(&rows.Redemptions).Error; err != nil {
		return reconciliationLotAllocations{}, fmt.Errorf(
			"load redemption allocations for reconciliation: %w",
			err,
		)
	}
	if err := tx.WithContext(ctx).
		Where("source_lot_id IN ?", lotIDs).
		Find(&rows.Gifts).Error; err != nil {
		return reconciliationLotAllocations{}, fmt.Errorf(
			"load gift allocations for reconciliation: %w",
			err,
		)
	}
	if err := tx.WithContext(ctx).
		Where("lot_id IN ?", lotIDs).
		Find(&rows.Refunds).Error; err != nil {
		return reconciliationLotAllocations{}, fmt.Errorf(
			"load refund allocations for reconciliation: %w",
			err,
		)
	}
	return rows, nil
}
