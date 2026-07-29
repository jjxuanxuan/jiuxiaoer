package integrity

import (
	"context"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/deliveryreturn"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	giftdomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/gift"
	redemptiondomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/redemption"
	renewaldomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/renewal"
)

type reconciliationOrder struct {
	ID                uint64
	OrderNo           string
	OrderType         string
	SettlementMode    string
	CustomerID        uint64
	MerchantID        uint64
	ShopID            uint64
	Status            string
	PayStatus         string
	DeliveryStatus    string
	GoodsAmount       int64
	DiscountAmount    int64
	DeliveryFeeAmount int64
	PayableAmount     int64
	PaidAmount        int64
}

type reconciliationDelivery struct {
	ID      uint64
	OrderID uint64
	ShopID  uint64
	Status  string
}

type reconciliationStockFacts struct {
	RecordCount    int64 `json:"record_count"`
	OutboundAmount int64 `json:"outbound_amount"`
	NetTotalDelta  int64 `json:"net_total_delta"`
}

type reconciliationStockIdentity struct {
	ID            uint64
	ShopProductID uint64
	ShopID        uint64
	ProductID     uint64
	AvailableQty  int
	ReservedQty   int
	LockedQty     int
}

type reconciliationOrderItemFacts struct {
	OrderID      uint64
	ItemCount    int64
	ItemQuantity int64
	ItemInvalid  int64
}

type reconciliationDeliveryFacts struct {
	Row   reconciliationDelivery
	Count int
}

type reconciliationRedemptionBatchFacts struct {
	Allocations    map[uint64][]redemptiondomain.RedemptionAllocation
	Orders         map[uint64]reconciliationOrder
	OrderItems     map[uint64]reconciliationOrderItemFacts
	Deliveries     map[uint64]reconciliationDeliveryFacts
	ReturnsByBiz   map[uint64][]deliveryreturn.Return
	ReturnsByOrder map[uint64][]deliveryreturn.Return
	Stocks         map[uint64]reconciliationStockIdentity
	StockRecords   map[uint64]reconciliationStockFacts
}

type reconciliationGiftBatchFacts struct {
	Allocations      map[uint64][]giftdomain.GiftAllocation
	TransactionFacts map[reconciliationTransactionKey]reconciliationTransactionAggregate
	Lots             map[uint64]core.Lot
	RenewalsByLot    map[uint64][]renewaldomain.Renewal
}

func (s *IntegrityService) scanRedemptions(
	ctx context.Context,
	tx *gorm.DB,
	afterID uint64,
	upperID *uint64,
	limit int,
) (int, uint64, []reconciliationDiscrepancy, error) {
	rows, err := s.repo.listIntegrityRedemptions(
		ctx,
		tx,
		afterID,
		upperID,
		limit,
	)
	if err != nil {
		return 0, afterID, nil, err
	}
	facts, err := s.repo.loadIntegrityRedemptionBatch(ctx, tx, rows)
	if err != nil {
		return 0, afterID, nil, err
	}
	discrepancies := make([]reconciliationDiscrepancy, 0)
	for _, redemption := range rows {
		ledgerFact, fulfillmentFact := checkRedemptionFacts(redemption, facts)
		if ledgerFact != nil {
			discrepancies = append(discrepancies, *ledgerFact)
		}
		if fulfillmentFact != nil {
			discrepancies = append(discrepancies, *fulfillmentFact)
		}
	}
	if len(rows) == 0 {
		return 0, afterID, discrepancies, nil
	}
	return len(rows), rows[len(rows)-1].ID, discrepancies, nil
}

func checkRedemptionFacts(
	redemption redemptiondomain.Redemption,
	facts reconciliationRedemptionBatchFacts,
) (*reconciliationDiscrepancy, *reconciliationDiscrepancy) {
	allocations := facts.Allocations[redemption.ID]
	var allocationQuantity uint64
	allocationStatuses := make(map[string]int)
	for _, allocation := range allocations {
		allocationQuantity += uint64(allocation.Quantity)
		allocationStatuses[allocation.Status]++
	}

	orderRow, orderExists := facts.Orders[redemption.OrderID]
	itemFacts := facts.OrderItems[redemption.OrderID]
	itemQuantity := itemFacts.ItemQuantity
	itemCount := itemFacts.ItemCount
	itemInvalid := itemFacts.ItemInvalid
	deliveryFacts := facts.Deliveries[redemption.OrderID]
	deliveryRow := deliveryFacts.Row
	deliveryExists := deliveryFacts.Count > 0
	returnRows := reconciliationRedemptionReturns(
		facts,
		redemption.ID,
		redemption.OrderID,
	)
	returnEvidenceValid := redemptionReturnEvidenceValid(
		redemption,
		deliveryRow.ID,
		returnRows,
	)

	stockFacts := facts.StockRecords[redemption.ID]
	stock, stockExists := facts.Stocks[redemption.ShopProductID]

	expectedNetDelta := -int64(redemption.Quantity)
	if redemption.Status == RedemptionStatusCancelled {
		expectedNetDelta = 0
	}
	orderLedgerValid := orderExists &&
		orderRow.ID == redemption.OrderID &&
		orderRow.CustomerID == redemption.CustomerID &&
		orderRow.MerchantID == redemption.IssuerMerchantID &&
		orderRow.ShopID == redemption.ShopID &&
		orderRow.OrderType == redemptionOrderType &&
		orderRow.SettlementMode == redemptionSettlementMode &&
		orderRow.PayStatus == redemptionPayStatus &&
		orderRow.GoodsAmount == 0 &&
		orderRow.DiscountAmount == 0 &&
		orderRow.DeliveryFeeAmount == 0 &&
		orderRow.PayableAmount == 0 &&
		orderRow.PaidAmount == 0
	stockIdentityValid := stockExists &&
		stock.ShopProductID == redemption.ShopProductID &&
		stock.ShopID == redemption.ShopID &&
		stock.ProductID == redemption.ProductID &&
		stock.AvailableQty >= 0 &&
		stock.ReservedQty >= 0 &&
		stock.LockedQty >= 0
	ledgerValid := allocationQuantity == uint64(redemption.Quantity) &&
		len(allocations) > 0 &&
		itemCount == 1 &&
		itemQuantity == int64(redemption.Quantity) &&
		itemInvalid == 0 &&
		orderLedgerValid &&
		deliveryExists &&
		deliveryFacts.Count == 1 &&
		deliveryRow.OrderID == redemption.OrderID &&
		deliveryRow.ShopID == redemption.ShopID &&
		stockIdentityValid &&
		stockFacts.RecordCount > 0 &&
		stockFacts.OutboundAmount == int64(redemption.Quantity) &&
		stockFacts.NetTotalDelta == expectedNetDelta

	var ledgerFact *reconciliationDiscrepancy
	if !ledgerValid {
		ledgerFact = &reconciliationDiscrepancy{
			Rule:    reconciliationRuleRedemptionLedger,
			Kind:    "redemption_order_stock",
			BizType: "wine_ticket_redemption", BizID: redemption.ID,
			BizNo:            &redemption.RedemptionNo,
			IssuerMerchantID: &redemption.IssuerMerchantID,
			Severity:         "P1",
			Expected: map[string]any{
				"quantity":                  redemption.Quantity,
				"allocation_quantity":       redemption.Quantity,
				"order_item_quantity":       redemption.Quantity,
				"stock_outbound_amount":     redemption.Quantity,
				"stock_net_total_delta":     expectedNetDelta,
				"zero_cash_order":           true,
				"one_delivery_order":        true,
				"physical_stock_link_valid": true,
			},
			Actual: map[string]any{
				"allocation_quantity":       allocationQuantity,
				"allocation_count":          len(allocations),
				"order_exists":              orderExists,
				"order_item_count":          itemCount,
				"order_item_quantity":       itemQuantity,
				"order_item_invalid_count":  itemInvalid,
				"order_ledger_valid":        orderLedgerValid,
				"delivery_exists":           deliveryExists,
				"stock_record_facts":        stockFacts,
				"physical_stock_exists":     stockExists,
				"physical_stock_link_valid": stockIdentityValid,
			},
		}
	}

	fulfillmentValid := redemptionFulfillmentValid(
		redemption.Status,
		allocationStatuses,
		len(allocations),
		orderRow,
		orderExists,
		deliveryRow,
		deliveryExists,
		returnEvidenceValid,
	)
	var fulfillmentFact *reconciliationDiscrepancy
	if !fulfillmentValid {
		fulfillmentFact = &reconciliationDiscrepancy{
			Rule:    reconciliationRuleFulfillment,
			Kind:    "redemption_fulfillment_state",
			BizType: "wine_ticket_redemption", BizID: redemption.ID,
			BizNo:            &redemption.RedemptionNo,
			IssuerMerchantID: &redemption.IssuerMerchantID,
			Severity:         "P1",
			Expected: map[string]any{
				"redemption_status":              redemption.Status,
				"state_mapping_valid":            true,
				"delivered_allocations_consumed": true,
			},
			Actual: map[string]any{
				"redemption_status":     redemption.Status,
				"allocation_statuses":   allocationStatuses,
				"order_status":          orderRow.Status,
				"order_pay_status":      orderRow.PayStatus,
				"order_delivery_status": orderRow.DeliveryStatus,
				"delivery_status":       deliveryRow.Status,
				"order_exists":          orderExists,
				"delivery_exists":       deliveryExists,
				"return_count":          len(returnRows),
				"return_evidence_valid": returnEvidenceValid,
			},
		}
	}
	return ledgerFact, fulfillmentFact
}

func (r *reconciliationRepository) listIntegrityRedemptions(
	ctx context.Context,
	tx *gorm.DB,
	afterID uint64,
	upperID *uint64,
	limit int,
) ([]redemptiondomain.Redemption, error) {
	var rows []redemptiondomain.Redemption
	query := r.idWindow(
		tx.WithContext(ctx).Model(&redemptiondomain.Redemption{}),
		"id",
		afterID,
		upperID,
	)
	err := query.Order("id").Limit(limit).Find(&rows).Error
	return rows, err
}

func (r *reconciliationRepository) loadIntegrityRedemptionBatch(
	ctx context.Context,
	tx *gorm.DB,
	rows []redemptiondomain.Redemption,
) (reconciliationRedemptionBatchFacts, error) {
	facts := reconciliationRedemptionBatchFacts{
		Allocations:    make(map[uint64][]redemptiondomain.RedemptionAllocation, len(rows)),
		Orders:         make(map[uint64]reconciliationOrder, len(rows)),
		OrderItems:     make(map[uint64]reconciliationOrderItemFacts, len(rows)),
		Deliveries:     make(map[uint64]reconciliationDeliveryFacts, len(rows)),
		ReturnsByBiz:   make(map[uint64][]deliveryreturn.Return, len(rows)),
		ReturnsByOrder: make(map[uint64][]deliveryreturn.Return, len(rows)),
		Stocks:         make(map[uint64]reconciliationStockIdentity, len(rows)),
		StockRecords:   make(map[uint64]reconciliationStockFacts, len(rows)),
	}
	if len(rows) == 0 {
		return facts, nil
	}
	redemptionIDs := make([]uint64, 0, len(rows))
	orderIDs := make([]uint64, 0, len(rows))
	shopProductIDs := make([]uint64, 0, len(rows))
	redemptionsByOrder := make(map[uint64]redemptiondomain.Redemption, len(rows))
	for _, row := range rows {
		redemptionIDs = append(redemptionIDs, row.ID)
		orderIDs = append(orderIDs, row.OrderID)
		shopProductIDs = append(shopProductIDs, row.ShopProductID)
		redemptionsByOrder[row.OrderID] = row
	}
	redemptionIDs = reconciliationUniqueIDs(redemptionIDs)
	orderIDs = reconciliationUniqueIDs(orderIDs)
	shopProductIDs = reconciliationUniqueIDs(shopProductIDs)

	var allocations []redemptiondomain.RedemptionAllocation
	if err := tx.WithContext(ctx).
		Where("redemption_id IN ?", redemptionIDs).
		Order("redemption_id, id").
		Find(&allocations).Error; err != nil {
		return facts, err
	}
	for _, allocation := range allocations {
		facts.Allocations[allocation.RedemptionID] = append(
			facts.Allocations[allocation.RedemptionID],
			allocation,
		)
	}

	var orders []reconciliationOrder
	if err := tx.WithContext(ctx).Table("orders").
		Where("id IN ? AND deleted_at IS NULL", orderIDs).
		Scan(&orders).Error; err != nil {
		return facts, err
	}
	for _, row := range orders {
		facts.Orders[row.ID] = row
	}

	var items []struct {
		OrderID         uint64
		ShopProductID   uint64
		ProductID       uint64
		Quantity        int64
		SalePriceAmount int64
		TotalAmount     int64
	}
	if err := tx.WithContext(ctx).Table("order_items").
		Select(`
			order_id, shop_product_id, product_id, quantity,
			sale_price_amount, total_amount
		`).
		Where("order_id IN ? AND deleted_at IS NULL", orderIDs).
		Scan(&items).Error; err != nil {
		return facts, err
	}
	for _, item := range items {
		fact := facts.OrderItems[item.OrderID]
		fact.OrderID = item.OrderID
		fact.ItemCount++
		fact.ItemQuantity += item.Quantity
		expected := redemptionsByOrder[item.OrderID]
		if item.ShopProductID != expected.ShopProductID ||
			item.ProductID != expected.ProductID ||
			item.SalePriceAmount != 0 ||
			item.TotalAmount != 0 {
			fact.ItemInvalid++
		}
		facts.OrderItems[item.OrderID] = fact
	}

	var deliveries []reconciliationDelivery
	if err := tx.WithContext(ctx).Table("delivery_orders").
		Where("order_id IN ? AND deleted_at IS NULL", orderIDs).
		Order("order_id, id").
		Scan(&deliveries).Error; err != nil {
		return facts, err
	}
	for _, row := range deliveries {
		fact := facts.Deliveries[row.OrderID]
		if fact.Count == 0 {
			fact.Row = row
		}
		fact.Count++
		facts.Deliveries[row.OrderID] = fact
	}

	var returns []deliveryreturn.Return
	if err := tx.WithContext(ctx).
		Where("settlement_type = ?", deliveryreturn.SettlementWineTicketRestore).
		Where(
			"settlement_biz_id IN ? OR order_id IN ?",
			redemptionIDs,
			orderIDs,
		).
		Order("id").
		Find(&returns).Error; err != nil {
		return facts, err
	}
	for _, row := range returns {
		if row.SettlementBizID != nil {
			facts.ReturnsByBiz[*row.SettlementBizID] = append(
				facts.ReturnsByBiz[*row.SettlementBizID],
				row,
			)
		}
		facts.ReturnsByOrder[row.OrderID] = append(
			facts.ReturnsByOrder[row.OrderID],
			row,
		)
	}

	var stocks []reconciliationStockIdentity
	if err := tx.WithContext(ctx).Table("product_stocks").
		Where(
			"shop_product_id IN ? AND deleted_at IS NULL",
			shopProductIDs,
		).
		Scan(&stocks).Error; err != nil {
		return facts, err
	}
	for _, row := range stocks {
		facts.Stocks[row.ShopProductID] = row
	}

	var stockRecords []struct {
		SourceID       uint64
		RecordCount    int64
		OutboundAmount int64
		NetTotalDelta  int64
	}
	if err := tx.WithContext(ctx).Table("stock_records").
		Select(`
			source_id,
			COUNT(*) AS record_count,
			COALESCE(SUM(CASE
				WHEN total_quantity_delta < 0 THEN -total_quantity_delta
				ELSE 0
			END), 0) AS outbound_amount,
			COALESCE(SUM(total_quantity_delta), 0) AS net_total_delta
		`).
		Where(
			"source_type = ? AND source_id IN ? AND deleted_at IS NULL",
			"wine_ticket_redemption",
			redemptionIDs,
		).
		Group("source_id").
		Scan(&stockRecords).Error; err != nil {
		return facts, err
	}
	for _, row := range stockRecords {
		facts.StockRecords[row.SourceID] = reconciliationStockFacts{
			RecordCount:    row.RecordCount,
			OutboundAmount: row.OutboundAmount,
			NetTotalDelta:  row.NetTotalDelta,
		}
	}
	return facts, nil
}

func reconciliationRedemptionReturns(
	facts reconciliationRedemptionBatchFacts,
	redemptionID uint64,
	orderID uint64,
) []deliveryreturn.Return {
	byBiz := facts.ReturnsByBiz[redemptionID]
	byOrder := facts.ReturnsByOrder[orderID]
	if len(byBiz) == 0 {
		return byOrder
	}
	result := make([]deliveryreturn.Return, 0, len(byBiz)+len(byOrder))
	seen := make(map[uint64]struct{}, len(byBiz)+len(byOrder))
	for _, rows := range [][]deliveryreturn.Return{byBiz, byOrder} {
		for _, row := range rows {
			if _, exists := seen[row.ID]; exists {
				continue
			}
			seen[row.ID] = struct{}{}
			result = append(result, row)
		}
	}
	return result
}

func redemptionFulfillmentValid(
	status string,
	allocationStatuses map[string]int,
	allocationCount int,
	orderRow reconciliationOrder,
	orderExists bool,
	deliveryRow reconciliationDelivery,
	deliveryExists bool,
	returnEvidenceValid bool,
) bool {
	if !orderExists || !deliveryExists || allocationCount == 0 {
		return false
	}
	allAre := func(allocationStatus string) bool {
		return allocationStatuses[allocationStatus] == allocationCount
	}
	switch status {
	case RedemptionStatusScheduled:
		return allAre(RedemptionAllocationStatusHeld) &&
			orderRow.Status == "paid" &&
			orderRow.DeliveryStatus == "pending_assign" &&
			deliveryRow.Status == "pending_assign"
	case RedemptionStatusAssigned:
		return allAre(RedemptionAllocationStatusHeld) &&
			orderRow.Status == "paid" &&
			orderRow.DeliveryStatus == "accepted" &&
			deliveryRow.Status == "accepted"
	case RedemptionStatusPickedUp:
		return allAre(RedemptionAllocationStatusHeld) &&
			orderRow.Status == "delivering" &&
			orderRow.DeliveryStatus == "delivering" &&
			deliveryRow.Status == "delivering"
	case RedemptionStatusDelivered:
		return allAre(RedemptionAllocationStatusConsumed) &&
			orderRow.Status == "completed" &&
			orderRow.DeliveryStatus == "completed" &&
			deliveryRow.Status == "completed"
	case RedemptionStatusCancelled:
		return allAre(RedemptionAllocationStatusRestored) &&
			orderRow.Status == "cancelled" &&
			orderRow.DeliveryStatus == "cancelled" &&
			deliveryRow.Status == "cancelled"
	case RedemptionStatusReturnInProgress:
		heldOrConsumed := allocationStatuses[RedemptionAllocationStatusHeld]+
			allocationStatuses[RedemptionAllocationStatusConsumed] == allocationCount
		return returnEvidenceValid &&
			heldOrConsumed &&
			orderRow.Status == "returning" &&
			(orderRow.DeliveryStatus == "returning" ||
				orderRow.DeliveryStatus == "returned") &&
			(deliveryRow.Status == "returning" ||
				deliveryRow.Status == "returned")
	case RedemptionStatusRestored:
		return returnEvidenceValid &&
			allAre(RedemptionAllocationStatusRestored) &&
			orderRow.Status == "cancelled" &&
			orderRow.DeliveryStatus == "returned" &&
			deliveryRow.Status == "returned"
	case RedemptionStatusException:
		// 异常状态是一种失败关闭冻结。
		// 具体修复事实归有效异常流程负责，但关联关系和数量仍由 REC-WT-004 校验。
		return returnEvidenceValid
	default:
		return false
	}
}

func redemptionReturnEvidenceValid(
	redemption redemptiondomain.Redemption,
	deliveryID uint64,
	rows []deliveryreturn.Return,
) bool {
	validLineage := func(row deliveryreturn.Return) bool {
		return row.SettlementType != nil &&
			*row.SettlementType == deliveryreturn.SettlementWineTicketRestore &&
			row.SettlementBizID != nil &&
			*row.SettlementBizID == redemption.ID &&
			row.OrderID == redemption.OrderID &&
			row.DeliveryOrderID == deliveryID
	}
	switch redemption.Status {
	case RedemptionStatusReturnInProgress:
		if len(rows) != 1 || !validLineage(rows[0]) {
			return false
		}
		row := rows[0]
		return row.SettlementStatus != nil &&
			(*row.SettlementStatus == "processing" ||
				*row.SettlementStatus == "exception") &&
			(row.Status == deliveryreturn.StatusReturning ||
				row.Status == deliveryreturn.StatusArrived ||
				row.Status == deliveryreturn.StatusReceived ||
				row.Status == deliveryreturn.StatusDisputed ||
				row.Status == deliveryreturn.StatusException)
	case RedemptionStatusRestored:
		if len(rows) != 1 || !validLineage(rows[0]) {
			return false
		}
		row := rows[0]
		return row.Status == deliveryreturn.StatusClosed &&
			row.SettlementStatus != nil &&
			*row.SettlementStatus == "succeeded" &&
			row.SettledAt != nil
	case RedemptionStatusException:
		// 异常可能发生在逆向物流绑定之前或之后。
		if len(rows) == 0 {
			return true
		}
		return len(rows) == 1 && validLineage(rows[0])
	default:
		return len(rows) == 0
	}
}

func (s *IntegrityService) scanGifts(
	ctx context.Context,
	tx *gorm.DB,
	afterID uint64,
	upperID *uint64,
	limit int,
) (int, uint64, []reconciliationDiscrepancy, error) {
	gifts, err := s.repo.listIntegrityGifts(
		ctx,
		tx,
		afterID,
		upperID,
		limit,
	)
	if err != nil {
		return 0, afterID, nil, err
	}
	facts, err := s.repo.loadIntegrityGiftBatch(ctx, tx, gifts)
	if err != nil {
		return 0, afterID, nil, err
	}
	discrepancies := make([]reconciliationDiscrepancy, 0)
	for _, gift := range gifts {
		fact := checkGiftFacts(gift, facts)
		if fact != nil {
			discrepancies = append(discrepancies, *fact)
		}
	}
	if len(gifts) == 0 {
		return 0, afterID, discrepancies, nil
	}
	return len(gifts), gifts[len(gifts)-1].ID, discrepancies, nil
}

func (r *reconciliationRepository) listIntegrityGifts(
	ctx context.Context,
	tx *gorm.DB,
	afterID uint64,
	upperID *uint64,
	limit int,
) ([]giftdomain.Gift, error) {
	var rows []giftdomain.Gift
	query := r.idWindow(
		tx.WithContext(ctx).Model(&giftdomain.Gift{}),
		"id",
		afterID,
		upperID,
	)
	err := query.Order("id").Limit(limit).Find(&rows).Error
	return rows, err
}

func (r *reconciliationRepository) loadIntegrityGiftBatch(
	ctx context.Context,
	tx *gorm.DB,
	gifts []giftdomain.Gift,
) (reconciliationGiftBatchFacts, error) {
	facts := reconciliationGiftBatchFacts{
		Allocations: make(map[uint64][]giftdomain.GiftAllocation, len(gifts)),
		TransactionFacts: make(
			map[reconciliationTransactionKey]reconciliationTransactionAggregate,
		),
		Lots:          make(map[uint64]core.Lot),
		RenewalsByLot: make(map[uint64][]renewaldomain.Renewal),
	}
	if len(gifts) == 0 {
		return facts, nil
	}
	giftIDs := make([]uint64, 0, len(gifts))
	for _, gift := range gifts {
		giftIDs = append(giftIDs, gift.ID)
	}
	giftIDs = reconciliationUniqueIDs(giftIDs)

	var allocations []giftdomain.GiftAllocation
	if err := tx.WithContext(ctx).
		Where("gift_id IN ?", giftIDs).
		Order("gift_id, id").
		Find(&allocations).Error; err != nil {
		return facts, err
	}
	lotIDs := make([]uint64, 0, 2*len(allocations))
	for _, allocation := range allocations {
		facts.Allocations[allocation.GiftID] = append(
			facts.Allocations[allocation.GiftID],
			allocation,
		)
		lotIDs = append(lotIDs, allocation.SourceLotID)
		if allocation.ReceiverLotID != nil {
			lotIDs = append(lotIDs, *allocation.ReceiverLotID)
		}
	}

	var transactions []core.Transaction
	if err := tx.WithContext(ctx).
		Where("biz_type = ? AND biz_id IN ?", "gift", giftIDs).
		Find(&transactions).Error; err != nil {
		return facts, err
	}
	for _, entry := range transactions {
		key := reconciliationTransactionKey{
			LotID: entry.LotID, TransactionType: entry.TransactionType,
			BizType: entry.BizType, BizID: entry.BizID,
		}
		aggregate := facts.TransactionFacts[key]
		aggregate.Count++
		aggregate.Delta += int64(entry.QuantityDelta)
		facts.TransactionFacts[key] = aggregate
	}

	lotIDs = reconciliationUniqueIDs(lotIDs)
	if len(lotIDs) == 0 {
		return facts, nil
	}
	var lots []core.Lot
	if err := tx.WithContext(ctx).Where("id IN ?", lotIDs).
		Find(&lots).Error; err != nil {
		return facts, err
	}
	for _, lot := range lots {
		facts.Lots[lot.ID] = lot
	}
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
	return facts, nil
}

func checkGiftFacts(
	gift giftdomain.Gift,
	facts reconciliationGiftBatchFacts,
) *reconciliationDiscrepancy {
	allocations := facts.Allocations[gift.ID]
	var (
		allocationQuantity uint64
		statuses           = make(map[string]int)
		lineageValid       = true
	)
	for _, allocation := range allocations {
		allocationQuantity += uint64(allocation.Quantity)
		statuses[allocation.Status]++
		source, sourceExists := facts.Lots[allocation.SourceLotID]
		lineageValid = lineageValid &&
			sourceExists &&
			source.OwnerCustomerID == gift.GiverCustomerID &&
			source.IssuerMerchantID == gift.IssuerMerchantID &&
			source.ProductID == gift.ProductID &&
			source.RedeemCityCode == gift.RedeemCityCode &&
			allocation.Quantity <= source.TotalQuantity
		hold := facts.TransactionFacts[reconciliationTransactionKey{
			LotID:           allocation.SourceLotID,
			TransactionType: TransactionTypeGiftHold,
			BizType:         "gift", BizID: gift.ID,
		}]
		if hold.Count != 1 || hold.Delta != -int64(allocation.Quantity) {
			lineageValid = false
		}
		switch allocation.Status {
		case GiftAllocationStatusClaimed:
			if allocation.ReceiverLotID == nil {
				lineageValid = false
				continue
			}
			receiver, receiverExists := facts.Lots[*allocation.ReceiverLotID]
			if !receiverExists {
				lineageValid = false
				continue
			}
			claim := facts.TransactionFacts[reconciliationTransactionKey{
				LotID:           receiver.ID,
				TransactionType: TransactionTypeGiftClaim,
				BizType:         "gift", BizID: gift.ID,
			}]
			expiryValid := reconciliationLotExpiryDescendsFromRows(
				facts.RenewalsByLot[receiver.ID],
				allocation.SourceExpiresAt,
				receiver.ExpiresAt,
				receiver.CreatedAt,
			)
			lineageValid = lineageValid &&
				gift.ReceiverCustomerID != nil &&
				receiver.OwnerCustomerID == *gift.ReceiverCustomerID &&
				receiver.PurchaseID == source.PurchaseID &&
				receiver.SourceType == LotSourceGift &&
				receiver.SourceLotID != nil &&
				*receiver.SourceLotID == allocation.SourceLotID &&
				receiver.SourceGiftID != nil &&
				*receiver.SourceGiftID == gift.ID &&
				receiver.IssuerMerchantID == gift.IssuerMerchantID &&
				receiver.ProductID == gift.ProductID &&
				receiver.RedeemCityCode == gift.RedeemCityCode &&
				receiver.TotalQuantity == allocation.Quantity &&
				expiryValid &&
				claim.Count == 1 &&
				claim.Delta == int64(allocation.Quantity)
		case GiftAllocationStatusRestored:
			restore := facts.TransactionFacts[reconciliationTransactionKey{
				LotID:           allocation.SourceLotID,
				TransactionType: TransactionTypeGiftRestore,
				BizType:         "gift", BizID: gift.ID,
			}]
			lineageValid = lineageValid &&
				allocation.ReceiverLotID == nil &&
				restore.Count == 1 &&
				restore.Delta == int64(allocation.Quantity)
		case GiftAllocationStatusHeld:
			lineageValid = lineageValid &&
				allocation.ReceiverLotID == nil &&
				source.ExpiresAt.Equal(allocation.SourceExpiresAt)
		default:
			lineageValid = false
		}
	}

	stateValid := false
	switch gift.Status {
	case GiftStatusPending:
		stateValid = gift.ReceiverCustomerID == nil &&
			statuses[GiftAllocationStatusHeld] == len(allocations)
	case GiftStatusClaimed:
		stateValid = gift.ReceiverCustomerID != nil &&
			*gift.ReceiverCustomerID != gift.GiverCustomerID &&
			statuses[GiftAllocationStatusClaimed] == len(allocations)
	case GiftStatusCancelled, GiftStatusExpiredReturned:
		stateValid = gift.ReceiverCustomerID == nil &&
			statuses[GiftAllocationStatusRestored] == len(allocations)
	case GiftStatusException:
		// 数量和不可变流水血缘规则仍然适用，
		// 但混合状态本身会有意保留，供人工复核。
		stateValid = true
	}
	valid := len(allocations) > 0 &&
		allocationQuantity == uint64(gift.Quantity) &&
		lineageValid &&
		stateValid
	if valid {
		return nil
	}
	return &reconciliationDiscrepancy{
		Rule:    reconciliationRuleGift,
		Kind:    "gift_conservation",
		BizType: "wine_ticket_gift", BizID: gift.ID,
		BizNo: &gift.GiftNo, IssuerMerchantID: &gift.IssuerMerchantID,
		Severity: "P1",
		Expected: map[string]any{
			"quantity":           gift.Quantity,
			"single_receiver":    true,
			"allocation_lineage": true,
			"state_conservation": true,
		},
		Actual: map[string]any{
			"status":               gift.Status,
			"allocation_count":     len(allocations),
			"allocation_quantity":  allocationQuantity,
			"allocation_statuses":  statuses,
			"receiver_customer_id": gift.ReceiverCustomerID,
			"allocation_lineage":   lineageValid,
			"state_conservation":   stateValid,
		},
	}
}
