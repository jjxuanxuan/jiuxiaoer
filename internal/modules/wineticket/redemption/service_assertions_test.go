package redemption

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func assertRedemptionCreationFacts(t *testing.T, fx redemptionFixture, dto RedemptionDTO) {
	t.Helper()
	var redemption Redemption
	if err := fx.db.Where("redemption_no = ?", dto.RedemptionNo).Take(&redemption).Error; err != nil {
		t.Fatal(err)
	}
	var orderRow redemptionTestOrder
	if err := fx.db.First(&orderRow, "id = ?", redemption.OrderID).Error; err != nil {
		t.Fatal(err)
	}
	if orderRow.OrderType != redemptionOrderType ||
		orderRow.SettlementMode != redemptionSettlementMode ||
		orderRow.PayStatus != redemptionPayStatus || orderRow.Status != "paid" ||
		orderRow.GoodsAmount != 0 || orderRow.DeliveryFeeAmount != 0 ||
		orderRow.PayableAmount != 0 || orderRow.PaidAmount != 0 {
		t.Fatalf("zero-cash order mismatch: %+v", orderRow)
	}
	if !bytes.Equal(orderRow.AddressSnapshot, redemption.AddressSnapshot) ||
		!bytes.Equal(orderRow.DeliveryTimeSlotSnapshot, redemption.DeliveryTimeSlotSnapshot) {
		t.Fatal("order and redemption snapshots are not byte-identical")
	}
	var paymentCount int64
	if err := fx.db.Model(&redemptionTestPayment{}).Count(&paymentCount).Error; err != nil {
		t.Fatal(err)
	}
	if paymentCount != 0 {
		t.Fatalf("zero cash redemption created %d payments", paymentCount)
	}
	var delivery redemptionTestDelivery
	if err := fx.db.Where("order_id = ?", redemption.OrderID).Take(&delivery).Error; err != nil {
		t.Fatal(err)
	}
	if !delivery.ScheduledStartAt.Equal(fx.slotStart) ||
		!delivery.ScheduledEndAt.Equal(fx.slotEnd) ||
		!delivery.NotBeforeAt.Equal(fx.slotStart.Add(-redemptionDispatchLead)) ||
		!bytes.Equal(delivery.RecipientSnapshot, redemption.AddressSnapshot) {
		t.Fatalf("scheduled dispatch mismatch: %+v", delivery)
	}
	var slot DeliveryTimeSlot
	if err := fx.db.First(&slot, "id = ?", fx.slotID).Error; err != nil {
		t.Fatal(err)
	}
	if slot.ReservedOrders != 1 {
		t.Fatalf("reserved_orders = %d, want 1", slot.ReservedOrders)
	}
	var stock PhysicalStock
	if err := fx.db.First(&stock, "id = ?", fx.stockID).Error; err != nil {
		t.Fatal(err)
	}
	if stock.AvailableQty != 6 {
		t.Fatalf("stock available = %d, want 6", stock.AvailableQty)
	}
	var stockRecord redemptionTestStockRecord
	if err := fx.db.Where("source_id = ?", redemption.ID).Take(&stockRecord).Error; err != nil {
		t.Fatal(err)
	}
	if stockRecord.ChangeType != "wine_ticket_redeem" ||
		stockRecord.QuantityDelta != -4 || stockRecord.TotalQuantityDelta != -4 ||
		stockRecord.BeforeAvailableQty != 10 || stockRecord.AfterAvailableQty != 6 ||
		stockRecord.BeforeTotalQty != 10 || stockRecord.AfterTotalQty != 6 {
		t.Fatalf("stock dual delta mismatch: %+v", stockRecord)
	}
	var tooSoon, other core.Lot
	if err := fx.db.First(&tooSoon, "id = ?", fx.lotTooSoon).Error; err != nil {
		t.Fatal(err)
	}
	if err := fx.db.First(&other, "id = ?", fx.otherLotID).Error; err != nil {
		t.Fatal(err)
	}
	if tooSoon.AvailableQuantity != 5 || tooSoon.EverUsed ||
		other.AvailableQuantity != 10 || other.EverUsed {
		t.Fatalf("ineligible lots changed: tooSoon=%+v other=%+v", tooSoon, other)
	}
	var transactions []core.Transaction
	if err := fx.db.Where("biz_type = ? AND biz_id = ?", "wine_ticket_redemption", redemption.ID).
		Order("id ASC").Find(&transactions).Error; err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 2 {
		t.Fatalf("hold transaction count = %d, want 2", len(transactions))
	}
	var allocations []RedemptionAllocation
	if err := fx.db.Where("redemption_id = ?", redemption.ID).
		Find(&allocations).Error; err != nil {
		t.Fatal(err)
	}
	allocationByLotID := make(map[uint64]RedemptionAllocation, len(allocations))
	for _, allocation := range allocations {
		allocationByLotID[allocation.LotID] = allocation
	}
	var held uint
	for _, transaction := range transactions {
		if transaction.TransactionType != TransactionTypeRedemptionHold ||
			transaction.QuantityDelta >= 0 ||
			transaction.BizType != "wine_ticket_redemption" ||
			transaction.BizID != redemption.ID ||
			transaction.ActionKey != fmt.Sprintf(
				"redemption_hold:%d:%d",
				redemption.ID,
				transaction.LotID,
			) ||
			!transaction.CreatedAt.Equal(fx.now) {
			t.Fatalf("invalid hold transaction: %+v", transaction)
		}
		allocation, ok := allocationByLotID[transaction.LotID]
		if !ok {
			t.Fatalf("hold transaction has no allocation: %+v", transaction)
		}
		assertRedemptionTransactionMetadata(t, transaction, map[string]string{
			"redemption_no": redemption.RedemptionNo,
			"allocation_id": idString(allocation.ID),
		})
		held += uint(-transaction.QuantityDelta)
	}
	if held != dto.Quantity {
		t.Fatalf("held ledger quantity = %d, order quantity = %d", held, dto.Quantity)
	}
}

func assertRedemptionCancellationFacts(t *testing.T, fx redemptionFixture) {
	t.Helper()
	var early, later core.Lot
	if err := fx.db.First(&early, "id = ?", fx.lotEarlyID).Error; err != nil {
		t.Fatal(err)
	}
	if err := fx.db.First(&later, "id = ?", fx.lotLaterID).Error; err != nil {
		t.Fatal(err)
	}
	if early.AvailableQuantity != 2 || later.AvailableQuantity != 3 ||
		!early.EverUsed || !later.EverUsed {
		t.Fatalf("lots were not restored exactly: early=%+v later=%+v", early, later)
	}
	var allocations []RedemptionAllocation
	if err := fx.db.Find(&allocations).Error; err != nil {
		t.Fatal(err)
	}
	if len(allocations) != 2 {
		t.Fatalf("allocation count = %d", len(allocations))
	}
	for _, allocation := range allocations {
		if allocation.Status != RedemptionAllocationStatusRestored {
			t.Fatalf("allocation not restored: %+v", allocation)
		}
	}
	var slot DeliveryTimeSlot
	if err := fx.db.First(&slot, "id = ?", fx.slotID).Error; err != nil {
		t.Fatal(err)
	}
	var stock PhysicalStock
	if err := fx.db.First(&stock, "id = ?", fx.stockID).Error; err != nil {
		t.Fatal(err)
	}
	if slot.ReservedOrders != 0 || stock.AvailableQty != 10 {
		t.Fatalf("slot/stock not restored: slot=%+v stock=%+v", slot, stock)
	}
	var transactions []core.Transaction
	if err := fx.db.Order("id ASC").Find(&transactions).Error; err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 4 {
		t.Fatalf("transaction count after repeated cancel = %d, want 4", len(transactions))
	}
	for _, transaction := range transactions {
		if transaction.QuantityDelta == 0 {
			t.Fatalf("zero transaction persisted: %+v", transaction)
		}
	}
	var stockRecords []redemptionTestStockRecord
	if err := fx.db.Order("id ASC").Find(&stockRecords).Error; err != nil {
		t.Fatal(err)
	}
	if len(stockRecords) != 2 ||
		stockRecords[1].QuantityDelta != 4 ||
		stockRecords[1].TotalQuantityDelta != 4 ||
		stockRecords[1].AfterAvailableQty != 10 ||
		stockRecords[1].AfterTotalQty != 10 {
		t.Fatalf("stock restore dual delta mismatch: %+v", stockRecords)
	}
}

func assertNoRedemptionWrites(t *testing.T, fx redemptionFixture, expectedLaterValues ...uint) {
	t.Helper()
	for name, model := range map[string]any{
		"redemptions": &Redemption{}, "allocations": &RedemptionAllocation{},
		"transactions": &core.Transaction{}, "orders": &redemptionTestOrder{},
		"order_items": &redemptionTestOrderItem{}, "delivery_orders": &redemptionTestDelivery{},
		"stock_records": &redemptionTestStockRecord{}, "idempotency": &idempotency.Record{},
		"audit": &redemptionTestAudit{}, "outbox": &redemptionTestOutbox{},
	} {
		var count int64
		if err := fx.db.Model(model).Count(&count).Error; err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		if count != 0 {
			t.Fatalf("%s write count = %d, want 0", name, count)
		}
	}
	var slot DeliveryTimeSlot
	if err := fx.db.First(&slot, "id = ?", fx.slotID).Error; err != nil {
		t.Fatal(err)
	}
	var stock PhysicalStock
	if err := fx.db.First(&stock, "id = ?", fx.stockID).Error; err != nil {
		t.Fatal(err)
	}
	if slot.ReservedOrders != 0 || stock.AvailableQty != 10 {
		t.Fatalf("rollback missed slot/stock: slot=%+v stock=%+v", slot, stock)
	}
	var early, later core.Lot
	if err := fx.db.First(&early, "id = ?", fx.lotEarlyID).Error; err != nil {
		t.Fatal(err)
	}
	if err := fx.db.First(&later, "id = ?", fx.lotLaterID).Error; err != nil {
		t.Fatal(err)
	}
	expectedLater := uint(3)
	if len(expectedLaterValues) > 0 {
		expectedLater = expectedLaterValues[0]
	}
	if early.AvailableQuantity != 2 || later.AvailableQuantity != expectedLater ||
		early.EverUsed || later.EverUsed {
		t.Fatalf("rollback missed wine ticket lots: early=%+v later=%+v", early, later)
	}
}

func redemptionAssertProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var details *problem.Details
	if !errors.As(err, &details) {
		t.Fatalf("error %v is not a problem response", err)
	}
	if details.ErrorCode != code {
		t.Fatalf("error code = %s, want %s (%v)", details.ErrorCode, code, err)
	}
}

func stringsContains(value string, fragment string) bool {
	return len(fragment) > 0 && bytes.Contains([]byte(value), []byte(fragment))
}
