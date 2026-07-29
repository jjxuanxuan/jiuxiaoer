package integrity

import (
	"context"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	purchasedomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/purchase"
	refunddomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/refund"
	renewaldomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/renewal"
)

type reconciliationCommonRefund struct {
	ID             uint64
	PaymentID      uint64
	RefundNo       string
	BizType        *string
	BizID          *uint64
	Status         string
	Currency       string
	Amount         int64
	TotalAmount    int64
	ProviderStatus *string
}

var reconciliationActiveRenewalStatuses = []string{
	RenewalStatusPendingPayment,
	RenewalStatusPaymentUnknown,
	RenewalStatusApplying,
	RenewalStatusCompensatingRefund,
	RenewalStatusRefundException,
}

type reconciliationRenewalBatchFacts struct {
	Lots          map[uint64]core.Lot
	RenewalsByLot map[uint64][]renewaldomain.Renewal
	Payments      map[uint64]reconciliationPayment
	Refunds       map[uint64]reconciliationCommonRefund
}

type reconciliationRefundBatchFacts struct {
	Purchases     map[uint64]purchasedomain.Purchase
	Payments      map[uint64]reconciliationPayment
	CommonRefunds map[uint64]reconciliationCommonRefund
	Allocations   map[uint64][]refunddomain.RefundAllocation
	OriginalLots  map[uint64][]core.Lot
	IssueFacts    map[uint64]purchaseIssueFacts
	ActiveCounts  map[uint64]int64
	RenewalsByLot map[uint64][]renewaldomain.Renewal
}

func (s *IntegrityService) scanRenewals(
	ctx context.Context,
	tx *gorm.DB,
	afterID uint64,
	upperID *uint64,
	limit int,
) (int, uint64, []reconciliationDiscrepancy, error) {
	rows, err := s.repo.listIntegrityRenewals(
		ctx,
		tx,
		afterID,
		upperID,
		limit,
	)
	if err != nil {
		return 0, afterID, nil, err
	}
	facts, err := s.repo.loadIntegrityRenewalBatch(ctx, tx, rows)
	if err != nil {
		return 0, afterID, nil, err
	}
	discrepancies := make([]reconciliationDiscrepancy, 0)
	for _, renewal := range rows {
		fact := checkRenewalFacts(renewal, facts)
		if fact != nil {
			discrepancies = append(discrepancies, *fact)
		}
	}
	if len(rows) == 0 {
		return 0, afterID, discrepancies, nil
	}
	return len(rows), rows[len(rows)-1].ID, discrepancies, nil
}

func (r *reconciliationRepository) listIntegrityRenewals(
	ctx context.Context,
	tx *gorm.DB,
	afterID uint64,
	upperID *uint64,
	limit int,
) ([]renewaldomain.Renewal, error) {
	var rows []renewaldomain.Renewal
	query := r.idWindow(
		tx.WithContext(ctx).Model(&renewaldomain.Renewal{}),
		"id",
		afterID,
		upperID,
	)
	err := query.Order("id").Limit(limit).Find(&rows).Error
	return rows, err
}

func (r *reconciliationRepository) loadIntegrityRenewalBatch(
	ctx context.Context,
	tx *gorm.DB,
	rows []renewaldomain.Renewal,
) (reconciliationRenewalBatchFacts, error) {
	facts := reconciliationRenewalBatchFacts{
		Lots:          make(map[uint64]core.Lot, len(rows)),
		RenewalsByLot: make(map[uint64][]renewaldomain.Renewal, len(rows)),
		Payments:      make(map[uint64]reconciliationPayment, len(rows)),
		Refunds:       make(map[uint64]reconciliationCommonRefund, len(rows)),
	}
	if len(rows) == 0 {
		return facts, nil
	}
	lotIDs := make([]uint64, 0, len(rows))
	paymentIDs := make([]uint64, 0, len(rows))
	refundIDs := make([]uint64, 0, len(rows))
	for _, row := range rows {
		lotIDs = append(lotIDs, row.LotID)
		if row.PaymentID != nil {
			paymentIDs = append(paymentIDs, *row.PaymentID)
		}
		if row.CompensatingRefundID != nil {
			refundIDs = append(refundIDs, *row.CompensatingRefundID)
		}
	}
	lotIDs = reconciliationUniqueIDs(lotIDs)
	var lots []core.Lot
	if err := tx.WithContext(ctx).Where("id IN ?", lotIDs).
		Find(&lots).Error; err != nil {
		return facts, err
	}
	for _, lot := range lots {
		facts.Lots[lot.ID] = lot
	}
	var renewals []renewaldomain.Renewal
	if err := tx.WithContext(ctx).Where("lot_id IN ?", lotIDs).
		Find(&renewals).Error; err != nil {
		return facts, err
	}
	for _, renewal := range renewals {
		facts.RenewalsByLot[renewal.LotID] = append(
			facts.RenewalsByLot[renewal.LotID],
			renewal,
		)
	}
	var err error
	facts.Payments, err = r.loadIntegrityPaymentsByID(
		ctx,
		tx,
		paymentIDs,
	)
	if err != nil {
		return facts, err
	}
	facts.Refunds, err = r.loadIntegrityCommonRefundsByID(
		ctx,
		tx,
		refundIDs,
	)
	return facts, err
}

func renewalCompletionAfter(candidate renewaldomain.Renewal, current renewaldomain.Renewal) bool {
	if candidate.CompletedAt == nil {
		return false
	}
	if current.CompletedAt == nil {
		return true
	}
	if candidate.CompletedAt.Equal(*current.CompletedAt) {
		return candidate.ID > current.ID
	}
	return candidate.CompletedAt.After(*current.CompletedAt)
}

func checkRenewalFacts(
	renewal renewaldomain.Renewal,
	facts reconciliationRenewalBatchFacts,
) *reconciliationDiscrepancy {
	lot, lotExists := facts.Lots[renewal.LotID]
	var activeCount int64
	var completedCount int64
	var latestCompleted renewaldomain.Renewal
	latestCompletedExists := false
	for _, row := range facts.RenewalsByLot[renewal.LotID] {
		if reconciliationContainsString(
			reconciliationActiveRenewalStatuses,
			row.Status,
		) {
			activeCount++
		}
		if row.Status == RenewalStatusCompleted {
			completedCount++
			if !latestCompletedExists ||
				renewalCompletionAfter(row, latestCompleted) {
				latestCompleted = row
				latestCompletedExists = true
			}
		}
	}

	paymentExists := false
	var payment reconciliationPayment
	if renewal.PaymentID != nil {
		payment, paymentExists = facts.Payments[*renewal.PaymentID]
	}
	var common reconciliationCommonRefund
	refundExists := false
	if renewal.CompensatingRefundID != nil {
		common, refundExists = facts.Refunds[*renewal.CompensatingRefundID]
	}

	baseValid := lotExists &&
		lot.OwnerCustomerID == renewal.CustomerID &&
		renewal.OldExpiresAt.Before(renewal.NewExpiresAt) &&
		activeCount <= 1 &&
		(completedCount == 0 ||
			(latestCompletedExists &&
				lot.ExpiresAt.Equal(latestCompleted.NewExpiresAt)))
	paymentValid := false
	if renewal.FeeAmount == 0 {
		paymentValid = renewal.PaymentID == nil && !paymentExists
	} else {
		paymentValid = renewal.PaymentID != nil &&
			paymentExists &&
			pointerString(payment.BizType) == RenewalPaymentBusiness &&
			pointerUint64(payment.BizID) == renewal.ID &&
			payment.CustomerID == renewal.CustomerID &&
			payment.Amount == renewal.FeeAmount &&
			payment.Currency == renewal.Currency
	}

	stateValid := false
	switch renewal.Status {
	case RenewalStatusPendingPayment, RenewalStatusPaymentUnknown:
		stateValid = renewal.FeeAmount > 0 &&
			paymentExists &&
			payment.Status != "succeeded" &&
			renewal.CompensatingRefundID == nil
	case RenewalStatusApplying:
		stateValid = renewal.FeeAmount > 0 &&
			paymentExists &&
			payment.Status == "succeeded" &&
			renewal.CompensatingRefundID == nil
	case RenewalStatusCompleted:
		stateValid = renewal.CompletedAt != nil &&
			renewal.CompensatingRefundID == nil &&
			(renewal.FeeAmount == 0 || payment.Status == "succeeded") &&
			lotExists &&
			!lot.ExpiresAt.Before(renewal.NewExpiresAt) &&
			int64(lot.RenewalCount) >= completedCount
	case RenewalStatusClosed:
		stateValid = renewal.ClosedAt != nil &&
			renewal.CompensatingRefundID == nil &&
			renewal.FeeAmount > 0 &&
			paymentExists &&
			(payment.Status == "closed" || payment.Status == "failed")
	case RenewalStatusCompensatingRefund, RenewalStatusRefundException:
		stateValid = renewal.FeeAmount > 0 &&
			paymentExists &&
			payment.Status == "succeeded" &&
			refundExists &&
			common.PaymentID == payment.ID &&
			pointerString(common.BizType) == RenewalCompensationRefundBusiness &&
			pointerUint64(common.BizID) == renewal.ID &&
			common.Amount == renewal.FeeAmount &&
			common.TotalAmount == payment.Amount &&
			common.Currency == renewal.Currency &&
			common.Status != "succeeded"
	case RenewalStatusRefunded:
		stateValid = renewal.RefundedAt != nil &&
			renewal.FeeAmount > 0 &&
			paymentExists &&
			payment.Status == "succeeded" &&
			refundExists &&
			common.PaymentID == payment.ID &&
			pointerString(common.BizType) == RenewalCompensationRefundBusiness &&
			pointerUint64(common.BizID) == renewal.ID &&
			common.Amount == renewal.FeeAmount &&
			common.TotalAmount == payment.Amount &&
			common.Currency == renewal.Currency &&
			common.Status == "succeeded"
	}
	if baseValid && paymentValid && stateValid {
		return nil
	}
	return &reconciliationDiscrepancy{
		Rule:    reconciliationRuleRenewal,
		Kind:    "renewal_payment_application",
		BizType: "wine_ticket_renewal", BizID: renewal.ID,
		BizNo: &renewal.RenewalNo, Severity: "P1",
		Expected: map[string]any{
			"one_active_renewal_per_lot":             true,
			"payment_link_valid":                     true,
			"application_or_compensation_consistent": true,
		},
		Actual: map[string]any{
			"status":                      renewal.Status,
			"lot_exists":                  lotExists,
			"active_renewal_count":        activeCount,
			"completed_renewal_count":     completedCount,
			"latest_completed_renewal_id": latestCompleted.ID,
			"latest_completed_expires_at": latestCompleted.NewExpiresAt,
			"lot_renewal_count":           lot.RenewalCount,
			"lot_expires_at":              lot.ExpiresAt,
			"renewal_new_expires_at":      renewal.NewExpiresAt,
			"payment_exists":              paymentExists,
			"payment_status":              payment.Status,
			"compensating_refund_exists":  refundExists,
			"compensating_refund_status":  common.Status,
			"base_valid":                  baseValid,
			"payment_link_valid":          paymentValid,
			"state_valid":                 stateValid,
		},
	}
}

func (s *IntegrityService) scanRefunds(
	ctx context.Context,
	tx *gorm.DB,
	afterID uint64,
	upperID *uint64,
	limit int,
) (int, uint64, []reconciliationDiscrepancy, error) {
	rows, err := s.repo.listIntegrityRefunds(
		ctx,
		tx,
		afterID,
		upperID,
		limit,
	)
	if err != nil {
		return 0, afterID, nil, err
	}
	facts, err := s.repo.loadIntegrityRefundBatch(ctx, tx, rows)
	if err != nil {
		return 0, afterID, nil, err
	}
	discrepancies := make([]reconciliationDiscrepancy, 0)
	for _, refund := range rows {
		fact := checkRefundFacts(refund, facts)
		if fact != nil {
			discrepancies = append(discrepancies, *fact)
		}
	}
	if len(rows) == 0 {
		return 0, afterID, discrepancies, nil
	}
	return len(rows), rows[len(rows)-1].ID, discrepancies, nil
}

func (r *reconciliationRepository) listIntegrityRefunds(
	ctx context.Context,
	tx *gorm.DB,
	afterID uint64,
	upperID *uint64,
	limit int,
) ([]refunddomain.WineTicketRefund, error) {
	var rows []refunddomain.WineTicketRefund
	query := r.idWindow(
		tx.WithContext(ctx).Model(&refunddomain.WineTicketRefund{}),
		"id",
		afterID,
		upperID,
	)
	err := query.Order("id").Limit(limit).Find(&rows).Error
	return rows, err
}

func (r *reconciliationRepository) loadIntegrityRefundBatch(
	ctx context.Context,
	tx *gorm.DB,
	rows []refunddomain.WineTicketRefund,
) (reconciliationRefundBatchFacts, error) {
	facts := reconciliationRefundBatchFacts{
		Purchases:     make(map[uint64]purchasedomain.Purchase, len(rows)),
		Payments:      make(map[uint64]reconciliationPayment, len(rows)),
		CommonRefunds: make(map[uint64]reconciliationCommonRefund, len(rows)),
		Allocations:   make(map[uint64][]refunddomain.RefundAllocation, len(rows)),
		OriginalLots:  make(map[uint64][]core.Lot, len(rows)),
		IssueFacts:    make(map[uint64]purchaseIssueFacts, len(rows)),
		ActiveCounts:  make(map[uint64]int64, len(rows)),
		RenewalsByLot: make(map[uint64][]renewaldomain.Renewal),
	}
	if len(rows) == 0 {
		return facts, nil
	}
	refundIDs := make([]uint64, 0, len(rows))
	purchaseIDs := make([]uint64, 0, len(rows))
	commonRefundIDs := make([]uint64, 0, len(rows))
	for _, row := range rows {
		refundIDs = append(refundIDs, row.ID)
		purchaseIDs = append(purchaseIDs, row.PurchaseID)
		commonRefundIDs = append(commonRefundIDs, row.CurrentRefundID)
	}
	refundIDs = reconciliationUniqueIDs(refundIDs)
	purchaseIDs = reconciliationUniqueIDs(purchaseIDs)

	var err error
	facts.Purchases, err = r.loadIntegrityPurchases(
		ctx,
		tx,
		purchaseIDs,
	)
	if err != nil {
		return facts, err
	}
	paymentIDs := make([]uint64, 0, len(facts.Purchases))
	for _, purchase := range facts.Purchases {
		paymentIDs = append(paymentIDs, purchase.PaymentID)
	}
	facts.Payments, err = r.loadIntegrityPaymentsByID(
		ctx,
		tx,
		paymentIDs,
	)
	if err != nil {
		return facts, err
	}
	facts.CommonRefunds, err = r.loadIntegrityCommonRefundsByID(
		ctx,
		tx,
		commonRefundIDs,
	)
	if err != nil {
		return facts, err
	}

	var allocations []refunddomain.RefundAllocation
	if err := tx.WithContext(ctx).
		Where("wine_ticket_refund_id IN ?", refundIDs).
		Order("wine_ticket_refund_id, id").
		Find(&allocations).Error; err != nil {
		return facts, err
	}
	for _, allocation := range allocations {
		facts.Allocations[allocation.WineTicketRefundID] = append(
			facts.Allocations[allocation.WineTicketRefundID],
			allocation,
		)
	}

	var lots []core.Lot
	if err := tx.WithContext(ctx).
		Where(
			"purchase_id IN ? AND source_type = ?",
			purchaseIDs,
			LotSourcePurchase,
		).
		Order("purchase_id, id").
		Find(&lots).Error; err != nil {
		return facts, err
	}
	lotIDs := make([]uint64, 0, len(lots))
	for _, lot := range lots {
		facts.OriginalLots[lot.PurchaseID] = append(
			facts.OriginalLots[lot.PurchaseID],
			lot,
		)
		lotIDs = append(lotIDs, lot.ID)
	}
	facts.IssueFacts, err = r.loadPurchaseIssueFactsBatch(
		ctx,
		tx,
		purchaseIDs,
	)
	if err != nil {
		return facts, err
	}
	var counts []struct {
		PurchaseID uint64
		Count      int64
	}
	if err := tx.WithContext(ctx).Model(&refunddomain.WineTicketRefund{}).
		Select("purchase_id, COUNT(*) AS count").
		Where(
			"purchase_id IN ? AND status IN ?",
			purchaseIDs,
			wineTicketRefundActiveStatuses,
		).
		Group("purchase_id").
		Scan(&counts).Error; err != nil {
		return facts, err
	}
	for _, count := range counts {
		facts.ActiveCounts[count.PurchaseID] = count.Count
	}

	lotIDs = reconciliationUniqueIDs(lotIDs)
	if len(lotIDs) > 0 {
		var renewals []renewaldomain.Renewal
		if err := tx.WithContext(ctx).
			Where("lot_id IN ? AND status = ?", lotIDs, RenewalStatusCompleted).
			Order("lot_id, created_at, id").
			Find(&renewals).Error; err != nil {
			return facts, err
		}
		for _, renewal := range renewals {
			facts.RenewalsByLot[renewal.LotID] = append(
				facts.RenewalsByLot[renewal.LotID],
				renewal,
			)
		}
	}
	return facts, nil
}

func checkRefundFacts(
	business refunddomain.WineTicketRefund,
	facts reconciliationRefundBatchFacts,
) *reconciliationDiscrepancy {
	purchase, purchaseExists := facts.Purchases[business.PurchaseID]
	common, commonExists := facts.CommonRefunds[business.CurrentRefundID]
	payment, paymentExists := facts.Payments[purchase.PaymentID]
	if !purchaseExists {
		paymentExists = false
	}
	allocations := facts.Allocations[business.ID]
	originalLots := facts.OriginalLots[business.PurchaseID]
	issueFacts := facts.IssueFacts[business.PurchaseID]
	activeCount := facts.ActiveCounts[business.PurchaseID]

	var (
		allocationQuantity uint64
		allocationStatuses = make(map[string]int)
		allocationLotsSeen = make(map[uint64]struct{}, len(allocations))
		allocationCoverage = len(allocations) == len(originalLots)
		lotQuantity        uint64
		allLotsZero        = true
		allLotsRefunded    = len(originalLots) > 0
	)
	originalLotsByID := make(map[uint64]core.Lot, len(originalLots))
	for _, lot := range originalLots {
		originalLotsByID[lot.ID] = lot
	}
	for _, allocation := range allocations {
		allocationQuantity += uint64(allocation.Quantity)
		allocationStatuses[allocation.Status]++
		lot, belongsToPurchase := originalLotsByID[allocation.LotID]
		_, duplicate := allocationLotsSeen[allocation.LotID]
		allocationLotsSeen[allocation.LotID] = struct{}{}
		expiryValid := false
		if belongsToPurchase {
			expiryValid = reconciliationLotExpiryDescendsFromRows(
				facts.RenewalsByLot[lot.ID],
				allocation.SourceExpiresAt,
				lot.ExpiresAt,
				allocation.CreatedAt,
			)
		}
		allocationCoverage = allocationCoverage &&
			belongsToPurchase &&
			!duplicate &&
			allocation.Quantity == lot.TotalQuantity &&
			expiryValid
	}
	for _, lot := range originalLots {
		lotQuantity += uint64(lot.TotalQuantity)
		allLotsZero = allLotsZero && lot.AvailableQuantity == 0
		allLotsRefunded = allLotsRefunded &&
			lot.Status == LotStatusRefunded &&
			lot.AvailableQuantity == 0
	}

	linkValid := purchaseExists &&
		paymentExists &&
		commonExists &&
		business.CustomerID == purchase.CustomerID &&
		business.Amount == common.Amount &&
		business.Currency == common.Currency &&
		common.PaymentID == purchase.PaymentID &&
		pointerString(common.BizType) == WineTicketPurchaseRefundBusiness &&
		pointerUint64(common.BizID) == business.ID &&
		common.TotalAmount == payment.Amount &&
		payment.Status == "succeeded" &&
		pointerString(payment.BizType) == PurchasePaymentBusiness &&
		pointerUint64(payment.BizID) == purchase.ID
	stateValid := false
	switch business.RefundKind {
	case RefundKindUserUnused:
		coverageValid := len(originalLots) > 0 &&
			allocationCoverage &&
			allocationQuantity == uint64(purchase.TotalBottleQuantity) &&
			lotQuantity == uint64(purchase.TotalBottleQuantity)
		switch business.Status {
		case RefundStatusSucceeded:
			stateValid = coverageValid &&
				allocationStatuses[RefundAllocationConsumed] == len(allocations) &&
				allLotsRefunded &&
				purchase.Status == PurchaseStatusRefunded &&
				common.Status == "succeeded" &&
				payment.RefundedAmount >= business.Amount
		case RefundStatusCancelled:
			stateValid = coverageValid &&
				allocationStatuses[RefundAllocationRestored] == len(allocations) &&
				purchase.Status == PurchaseStatusIssued
		default:
			if reconciliationContainsString(wineTicketRefundActiveStatuses, business.Status) {
				stateValid = coverageValid &&
					allocationStatuses[RefundAllocationHeld] == len(allocations) &&
					allLotsZero &&
					common.Status != "succeeded" &&
					(purchase.Status == PurchaseStatusRefundHolding ||
						purchase.Status == PurchaseStatusRefundException)
			}
		}
	case RefundKindIssueCompensation:
		zeroEntitlement := len(allocations) == 0 &&
			len(originalLots) == 0 &&
			issueFacts.OriginalLotQuantity == 0 &&
			issueFacts.IssueEntryCount == 0 &&
			issueFacts.IssueQuantity == 0
		switch business.Status {
		case RefundStatusSucceeded:
			stateValid = zeroEntitlement &&
				purchase.Status == PurchaseStatusRefunded &&
				common.Status == "succeeded" &&
				payment.RefundedAmount >= business.Amount
		case RefundStatusCancelled:
			stateValid = zeroEntitlement
		default:
			if reconciliationContainsString(wineTicketRefundActiveStatuses, business.Status) {
				stateValid = zeroEntitlement &&
					common.Status != "succeeded" &&
					(purchase.Status == PurchaseStatusRefundHolding ||
						purchase.Status == PurchaseStatusRefundException)
			}
		}
	}
	if linkValid && activeCount <= 1 && stateValid {
		return nil
	}
	severity := "P1"
	if business.RefundKind == RefundKindIssueCompensation &&
		(len(allocations) > 0 ||
			len(originalLots) > 0 ||
			issueFacts.IssueEntryCount > 0) {
		severity = "P0"
	}
	return &reconciliationDiscrepancy{
		Rule:    reconciliationRuleRefund,
		Kind:    "refund_entitlement_closure",
		BizType: "wine_ticket_refund", BizID: business.ID,
		BizNo: &business.WineTicketRefundNo, Severity: severity,
		Expected: map[string]any{
			"business_common_payment_link":   true,
			"one_active_refund_per_purchase": true,
			"user_unused_success":            "allocations_consumed_and_purchase_refunded",
			"issuance_compensation":          "zero_lot_zero_allocation_and_purchase_refunded_on_success",
		},
		Actual: map[string]any{
			"refund_kind":           business.RefundKind,
			"refund_status":         business.Status,
			"purchase_exists":       purchaseExists,
			"purchase_status":       purchase.Status,
			"payment_exists":        paymentExists,
			"payment_status":        payment.Status,
			"common_refund_exists":  commonExists,
			"common_refund_status":  common.Status,
			"active_refund_count":   activeCount,
			"allocation_count":      len(allocations),
			"allocation_quantity":   allocationQuantity,
			"allocation_statuses":   allocationStatuses,
			"original_lot_count":    len(originalLots),
			"original_lot_quantity": lotQuantity,
			"all_lots_zero":         allLotsZero,
			"all_lots_refunded":     allLotsRefunded,
			"issue_facts":           issueFacts,
			"link_valid":            linkValid,
			"state_valid":           stateValid,
		},
	}
}

func reconciliationLotExpiryDescendsFromRows(
	renewals []renewaldomain.Renewal,
	initial time.Time,
	current time.Time,
	since time.Time,
) bool {
	expected := initial
	for _, renewal := range renewals {
		if !since.IsZero() && renewal.CreatedAt.Before(since) {
			continue
		}
		if !renewal.OldExpiresAt.Equal(expected) ||
			!renewal.NewExpiresAt.After(renewal.OldExpiresAt) {
			return false
		}
		expected = renewal.NewExpiresAt
	}
	return current.Equal(expected)
}

func reconciliationContainsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (r *reconciliationRepository) loadIntegrityPaymentsByID(
	ctx context.Context,
	tx *gorm.DB,
	ids []uint64,
) (map[uint64]reconciliationPayment, error) {
	ids = reconciliationUniqueIDs(ids)
	result := make(map[uint64]reconciliationPayment, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	var rows []reconciliationPayment
	if err := tx.WithContext(ctx).Table("payments").
		Where("id IN ?", ids).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.ID] = row
	}
	return result, nil
}

func (r *reconciliationRepository) loadIntegrityCommonRefundsByID(
	ctx context.Context,
	tx *gorm.DB,
	ids []uint64,
) (map[uint64]reconciliationCommonRefund, error) {
	ids = reconciliationUniqueIDs(ids)
	result := make(map[uint64]reconciliationCommonRefund, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	var rows []reconciliationCommonRefund
	if err := tx.WithContext(ctx).Table("refunds").
		Where("id IN ?", ids).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.ID] = row
	}
	return result, nil
}
