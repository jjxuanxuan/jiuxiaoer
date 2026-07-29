package integrity

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/delivery"
	"jiuxiaoer-admin/backend-go/internal/modules/deliveryreturn"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	sharedrefund "jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	giftdomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/gift"
	purchasedomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/purchase"
	redemptiondomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/redemption"
	refunddomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/refund"
	reminderdomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/reminder"
	renewaldomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/renewal"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestIntegrityPhaseQueryCountDoesNotGrowWithBatchRows(t *testing.T) {
	db := newIntegrityTestDB(t)
	now := time.Date(2026, 7, 28, 2, 0, 0, 0, shanghaiLocation)
	purchaseBiz := PurchasePaymentBusiness
	providerSuccess := "SUCCESS"
	for index := uint64(1); index <= 10; index++ {
		missingPurchaseID := uint64(100_000 + index)
		fixtures := []any{
			&order.Payment{
				ID: 1_000 + index, PaymentNo: fmt.Sprintf("PAY-Q-%d", index),
				BizType: &purchaseBiz, BizID: &missingPurchaseID,
				Status: "succeeded", ProviderStatus: &providerSuccess,
				Amount: 100, Currency: "CNY",
			},
			&purchasedomain.Purchase{
				ID: 2_000 + index, PurchaseNo: fmt.Sprintf("WTP-Q-%d", index),
				Status: PurchaseStatusIssued, TotalBottleQuantity: 1,
			},
			&core.Lot{
				ID: 3_000 + index, LotNo: fmt.Sprintf("WTL-Q-%d", index),
				OwnerCustomerID: 1, PurchaseID: 2_000 + index,
				SourceType: LotSourcePurchase, IssuerMerchantID: 1,
				ProductID: 1, TotalQuantity: 1, AvailableQuantity: 1,
				OriginalExpiresAt: now.AddDate(0, 1, 0),
				ExpiresAt:         now.AddDate(0, 1, 0), Status: LotStatusActive,
			},
			&redemptiondomain.Redemption{
				ID: 4_000 + index, RedemptionNo: fmt.Sprintf("WTR-Q-%d", index),
				OrderID: 40_000 + index, ShopProductID: 50_000 + index,
				Quantity: 1, Status: RedemptionStatusScheduled,
			},
			&giftdomain.Gift{
				ID: 5_000 + index, GiftNo: fmt.Sprintf("WTG-Q-%d", index),
				Quantity: 1, Status: GiftStatusPending,
			},
			&renewaldomain.Renewal{
				ID: 6_000 + index, RenewalNo: fmt.Sprintf("WTRN-Q-%d", index),
				LotID: 60_000 + index, FeeAmount: 0,
				OldExpiresAt: now, NewExpiresAt: now.AddDate(0, 1, 0),
				Status: RenewalStatusCompleted,
			},
			&refunddomain.WineTicketRefund{
				ID:                 7_000 + index,
				WineTicketRefundNo: fmt.Sprintf("WTRF-Q-%d", index),
				PurchaseID:         70_000 + index, CurrentRefundID: 80_000 + index,
				RefundKind: RefundKindIssueCompensation,
				Status:     RefundStatusSucceeded,
			},
			&redemptiondomain.DeliveryTimeSlot{
				ID: 8_000 + index, CapacityOrders: 1, Status: "open",
			},
			&reminderdomain.Reminder{
				ID: 9_000 + index, LotID: 90_000 + index,
				ExpiresAt: now.AddDate(0, 1, 0), RemindDays: 7,
				Channel: "wechat_subscribe", Status: "pending",
			},
		}
		for _, fixture := range fixtures {
			if err := db.Create(fixture).Error; err != nil {
				t.Fatalf("create %T: %v", fixture, err)
			}
		}
	}

	counter := registerIntegrityQueryCounter(t, db)
	service := newTestIntegrityService(db, snowflake.New(424))
	type phaseScan func(int) error
	scans := map[string]phaseScan{
		"payments": func(limit int) error {
			_, _, _, err := service.scanPayments(
				context.Background(), db, 0, nil, limit,
			)
			return err
		},
		"purchases": func(limit int) error {
			_, _, _, err := service.scanPurchases(
				context.Background(), db, 0, nil, limit,
			)
			return err
		},
		"lots": func(limit int) error {
			_, _, _, err := service.scanLots(
				context.Background(), db, 0, nil, limit,
			)
			return err
		},
		"redemptions": func(limit int) error {
			_, _, _, err := service.scanRedemptions(
				context.Background(), db, 0, nil, limit,
			)
			return err
		},
		"gifts": func(limit int) error {
			_, _, _, err := service.scanGifts(
				context.Background(), db, 0, nil, limit,
			)
			return err
		},
		"renewals": func(limit int) error {
			_, _, _, err := service.scanRenewals(
				context.Background(), db, 0, nil, limit,
			)
			return err
		},
		"refunds": func(limit int) error {
			_, _, _, err := service.scanRefunds(
				context.Background(), db, 0, nil, limit,
			)
			return err
		},
		"slots": func(limit int) error {
			_, _, _, err := service.scanSlots(
				context.Background(), db, 0, nil, limit,
			)
			return err
		},
		"reminders": func(limit int) error {
			_, _, _, err := service.scanReminders(
				context.Background(), db, 0, nil, limit,
			)
			return err
		},
	}
	for name, scan := range scans {
		t.Run(name, func(t *testing.T) {
			counter.Store(0)
			if err := scan(1); err != nil {
				t.Fatal(err)
			}
			single := counter.Load()
			counter.Store(0)
			if err := scan(10); err != nil {
				t.Fatal(err)
			}
			batch := counter.Load()
			if single == 0 || batch > single {
				t.Fatalf(
					"query count grows with rows: one=%d ten=%d",
					single,
					batch,
				)
			}
		})
	}
}

func registerIntegrityQueryCounter(
	t *testing.T,
	db *gorm.DB,
) *atomic.Int64 {
	t.Helper()
	var counter atomic.Int64
	count := func(*gorm.DB) { counter.Add(1) }
	for name, register := range map[string]func(func(*gorm.DB)) error{
		"query": func(callback func(*gorm.DB)) error {
			return db.Callback().Query().
				Before("gorm:query").
				Register("wine_ticket_reconciliation_query_count", callback)
		},
		"raw": func(callback func(*gorm.DB)) error {
			return db.Callback().Raw().
				Before("gorm:raw").
				Register("wine_ticket_reconciliation_raw_count", callback)
		},
		"row": func(callback func(*gorm.DB)) error {
			return db.Callback().Row().
				Before("gorm:row").
				Register("wine_ticket_reconciliation_row_count", callback)
		},
	} {
		if err := register(count); err != nil {
			t.Fatalf("register %s query counter: %v", name, err)
		}
	}
	return &counter
}

func TestIntegrityReturnEvidenceFollowsFulfillmentState(t *testing.T) {
	settlementType := deliveryreturn.SettlementWineTicketRestore
	settlementBizID := uint64(71)
	processing := "processing"
	succeeded := "succeeded"
	now := time.Now()
	base := deliveryreturn.Return{
		ID: 75, DeliveryOrderID: 73, OrderID: 72,
		SettlementType: &settlementType, SettlementBizID: &settlementBizID,
	}
	inProgress := base
	inProgress.Status = deliveryreturn.StatusReturning
	inProgress.SettlementStatus = &processing
	closed := base
	closed.Status = deliveryreturn.StatusClosed
	closed.SettlementStatus = &succeeded
	closed.SettledAt = &now

	if !redemptionReturnEvidenceValid(
		redemptiondomain.Redemption{
			ID: settlementBizID, OrderID: base.OrderID,
			Status: RedemptionStatusReturnInProgress,
		},
		base.DeliveryOrderID,
		[]deliveryreturn.Return{inProgress},
	) {
		t.Fatal("valid in-progress wine-ticket return was rejected")
	}
	if !redemptionReturnEvidenceValid(
		redemptiondomain.Redemption{
			ID: settlementBizID, OrderID: base.OrderID,
			Status: RedemptionStatusRestored,
		},
		base.DeliveryOrderID,
		[]deliveryreturn.Return{closed},
	) {
		t.Fatal("valid closed wine-ticket return was rejected")
	}
	if redemptionReturnEvidenceValid(
		redemptiondomain.Redemption{
			ID: settlementBizID, OrderID: base.OrderID,
			Status: RedemptionStatusRestored,
		},
		base.DeliveryOrderID,
		[]deliveryreturn.Return{inProgress},
	) {
		t.Fatal("restored redemption accepted an unfinished return")
	}
	if redemptionReturnEvidenceValid(
		redemptiondomain.Redemption{
			ID: settlementBizID, OrderID: base.OrderID,
			Status: RedemptionStatusDelivered,
		},
		base.DeliveryOrderID,
		[]deliveryreturn.Return{closed},
	) {
		t.Fatal("non-return redemption accepted a bound return")
	}
}

func TestIntegrityValidFactsStayClean(t *testing.T) {
	db := newIntegrityTestDB(t)
	now := time.Date(2026, 7, 27, 8, 0, 0, 0, shanghaiLocation)
	purchaseBiz := PurchasePaymentBusiness
	purchaseID := uint64(101)
	providerSuccess := "SUCCESS"
	payment := order.Payment{
		ID: 100, PaymentNo: "PAY100", BizType: &purchaseBiz, BizID: &purchaseID,
		CustomerID: 10, Provider: "wechat", ProviderStatus: &providerSuccess,
		Status: "succeeded", Amount: 1000, Currency: "CNY",
		RefundedAmount: 1000, UpdatedAt: now,
	}
	purchase := purchasedomain.Purchase{
		ID: purchaseID, PurchaseNo: "WTP101", CustomerID: 10,
		PaymentID: payment.ID, IssuerMerchantID: 20,
		ProductID: 30, TotalBottleQuantity: 2,
		PayableAmount: 1000, PaidAmount: 1000, Currency: "CNY",
		Status: PurchaseStatusRefunded, RefundedAt: &now,
		CreatedAt: now, UpdatedAt: now,
	}
	lot := core.Lot{
		ID: 102, LotNo: "WTL102", OwnerCustomerID: 10,
		PurchaseID: purchase.ID, SourceType: LotSourcePurchase,
		IssuerMerchantID: 20, ProductID: 30,
		TotalQuantity: 2, AvailableQuantity: 0,
		OriginalExpiresAt: now.AddDate(0, 1, 0),
		ExpiresAt:         now.AddDate(0, 1, 0),
		Status:            LotStatusRefunded,
	}
	issue := core.Transaction{
		ID: 103, TransactionNo: "WTT103", LotID: lot.ID,
		OwnerCustomerID: 10, TransactionType: TransactionTypePurchaseIssue,
		QuantityDelta: 2, BeforeAvailableQuantity: 0,
		AfterAvailableQuantity: 2, BizType: "purchase",
		BizID: purchase.ID, ActionKey: "purchase_issue:101:102",
		CreatedAt: now,
	}
	refundHold := core.Transaction{
		ID: 104, TransactionNo: "WTT104", LotID: lot.ID,
		OwnerCustomerID: 10, TransactionType: TransactionTypeRefundHold,
		QuantityDelta: -2, BeforeAvailableQuantity: 2,
		AfterAvailableQuantity: 0, BizType: "refund",
		BizID: 105, ActionKey: "refund_hold:105:102",
		CreatedAt: now.Add(time.Second),
	}
	refundBizType := WineTicketPurchaseRefundBusiness
	refundBizID := uint64(105)
	common := sharedrefund.Row{
		ID: 106, PaymentID: payment.ID, RefundNo: "RF106",
		BizType: &refundBizType, BizID: &refundBizID,
		Provider: "wechat", Status: "succeeded", Currency: "CNY",
		Amount: 1000, TotalAmount: 1000, RequestedAt: now,
	}
	business := refunddomain.WineTicketRefund{
		ID: refundBizID, WineTicketRefundNo: "WTRF105",
		PurchaseID: purchase.ID, CustomerID: 10,
		CurrentRefundID: common.ID, RefundKind: RefundKindUserUnused,
		Amount: 1000, Currency: "CNY", Status: RefundStatusSucceeded,
		RequestedAt: now, SucceededAt: &now,
	}
	allocation := refunddomain.RefundAllocation{
		ID: 107, WineTicketRefundID: business.ID, LotID: lot.ID,
		Quantity: 2, SourceExpiresAt: lot.ExpiresAt,
		Status: RefundAllocationConsumed,
	}
	for _, row := range []any{
		&payment, &purchase, &lot, &issue, &refundHold,
		&common, &business, &allocation,
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatalf("create %T: %v", row, err)
		}
	}

	// 取货前取消的核销单必须恢复权益和实物库存，并释放配送时段。
	slot := redemptiondomain.DeliveryTimeSlot{
		ID: 200, ShopID: 40, CapacityOrders: 5, ReservedOrders: 0,
		Status: "open",
	}
	redemption := redemptiondomain.Redemption{
		ID: 201, RedemptionNo: "WTR201", CustomerID: 10,
		IssuerMerchantID: 20, ProductID: 30, ShopID: 40,
		ShopProductID: 50, DeliveryTimeSlotID: slot.ID,
		OrderID: 202, Quantity: 1, Status: RedemptionStatusCancelled,
	}
	redemptionAllocation := redemptiondomain.RedemptionAllocation{
		ID: 203, RedemptionID: redemption.ID, LotID: lot.ID,
		Quantity: 1, Status: RedemptionAllocationStatusRestored,
	}
	orderRow := order.Order{
		ID: redemption.OrderID, OrderNo: "O202",
		OrderType: redemptionOrderType, SettlementMode: redemptionSettlementMode,
		CustomerID: 10, MerchantID: 20, ShopID: 40,
		Status: "cancelled", PayStatus: redemptionPayStatus,
		DeliveryStatus: "cancelled",
	}
	orderItem := order.OrderItem{
		ID: 204, OrderID: orderRow.ID, ShopProductID: 50,
		ProductID: 30, Quantity: 1,
	}
	deliveryRow := delivery.DeliveryOrder{
		ID: 205, OrderID: orderRow.ID, ShopID: 40, Status: "cancelled",
	}
	stock := order.ProductStock{
		ID: 206, ShopProductID: 50, ShopID: 40,
		ProductID: 30, AvailableQty: 10,
	}
	outbound := order.StockRecord{
		ID: 207, ShopProductID: 50, ShopID: 40, ProductID: 30,
		QuantityDelta: -1, TotalQuantityDelta: -1,
		SourceType: "wine_ticket_redemption", SourceID: redemption.ID,
	}
	restored := order.StockRecord{
		ID: 208, ShopProductID: 50, ShopID: 40, ProductID: 30,
		QuantityDelta: 1, TotalQuantityDelta: 1,
		SourceType: "wine_ticket_redemption", SourceID: redemption.ID,
	}
	for _, row := range []any{
		&slot, &redemption, &redemptionAllocation, &orderRow,
		&orderItem, &deliveryRow, &stock, &outbound, &restored,
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatalf("create %T: %v", row, err)
		}
	}

	// 已领取礼赠的守恒关系基于不可变血缘，
	// 不依赖接收方批次后续的可用数量。
	receiverID := uint64(12)
	gift := giftdomain.Gift{
		ID: 301, GiftNo: "WTG301", GiverCustomerID: 11,
		ReceiverCustomerID: &receiverID, IssuerMerchantID: 20,
		ProductID: 30, Quantity: 1, Status: GiftStatusClaimed,
	}
	sourceLotID := uint64(302)
	receiverLotID := uint64(303)
	sourceLot := core.Lot{
		ID: sourceLotID, LotNo: "WTL302", OwnerCustomerID: 11,
		PurchaseID: 309, SourceType: LotSourcePurchase,
		IssuerMerchantID: 20, ProductID: 30,
		TotalQuantity: 2, AvailableQuantity: 1,
		ExpiresAt: now.AddDate(0, 2, 0),
	}
	receiverLot := core.Lot{
		ID: receiverLotID, LotNo: "WTL303", OwnerCustomerID: receiverID,
		PurchaseID: sourceLot.PurchaseID, SourceType: LotSourceGift,
		SourceLotID: &sourceLotID, SourceGiftID: &gift.ID,
		IssuerMerchantID: 20, ProductID: 30,
		TotalQuantity: 1, AvailableQuantity: 0,
		ExpiresAt: now.AddDate(0, 1, 0),
	}
	giftAllocation := giftdomain.GiftAllocation{
		ID: 304, GiftID: gift.ID, SourceLotID: sourceLot.ID,
		ReceiverLotID: &receiverLotID, Quantity: 1,
		SourceExpiresAt: receiverLot.ExpiresAt,
		Status:          GiftAllocationStatusClaimed,
	}
	giftHold := core.Transaction{
		ID: 305, TransactionNo: "WTT305", LotID: sourceLot.ID,
		OwnerCustomerID: 11, TransactionType: TransactionTypeGiftHold,
		QuantityDelta: -1, BeforeAvailableQuantity: 2,
		AfterAvailableQuantity: 1, BizType: "gift", BizID: gift.ID,
		ActionKey: "gift_hold:301:302",
	}
	giftClaim := core.Transaction{
		ID: 306, TransactionNo: "WTT306", LotID: receiverLot.ID,
		OwnerCustomerID: receiverID, TransactionType: TransactionTypeGiftClaim,
		QuantityDelta: 1, BeforeAvailableQuantity: 0,
		AfterAvailableQuantity: 1, BizType: "gift", BizID: gift.ID,
		ActionKey: "gift_claim:301:303",
	}
	for _, row := range []any{
		&gift, &sourceLot, &receiverLot, &giftAllocation, &giftHold, &giftClaim,
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatalf("create %T: %v", row, err)
		}
	}

	renewalLot := core.Lot{
		ID: 401, LotNo: "WTL401", OwnerCustomerID: 13,
		PurchaseID: 409, SourceType: LotSourcePurchase,
		IssuerMerchantID: 20, ProductID: 30,
		TotalQuantity: 1, AvailableQuantity: 1,
		ExpiresAt: now.AddDate(0, 2, 0), RenewalCount: 1,
	}
	renewal := renewaldomain.Renewal{
		ID: 402, RenewalNo: "WTRN402", LotID: renewalLot.ID,
		CustomerID: 13, OldExpiresAt: now.AddDate(0, 1, 0),
		NewExpiresAt: renewalLot.ExpiresAt, ExtensionDays: 30,
		FeeAmount: 0, Currency: "CNY", Status: RenewalStatusCompleted,
		CompletedAt: &now,
	}
	reminder := reminderdomain.Reminder{
		ID: 501, LotID: renewalLot.ID, OwnerCustomerID: renewalLot.OwnerCustomerID,
		ExpiresAt: renewalLot.ExpiresAt, RemindDays: 7,
		Channel: "wechat_subscribe", Status: "pending",
	}
	for _, row := range []any{&renewalLot, &renewal, &reminder} {
		if err := db.Create(row).Error; err != nil {
			t.Fatalf("create %T: %v", row, err)
		}
	}

	service := newTestIntegrityService(db, snowflake.New(413))
	for _, phase := range []IntegrityPhase{
		IntegrityPhasePayments,
		IntegrityPhasePurchases,
		IntegrityPhaseRedemptions,
		IntegrityPhaseGifts,
		IntegrityPhaseRenewals,
		IntegrityPhaseRefunds,
		IntegrityPhaseSlots,
		IntegrityPhaseReminders,
	} {
		result, err := service.ScanBatch(
			context.Background(),
			IntegrityCursor{Phase: phase},
			100,
		)
		if err != nil {
			t.Fatalf("%s scan failed: %v", phase, err)
		}
		if result.Detected != 0 {
			var exceptions []Exception
			_ = db.Where("correlation_id = ?", ruleForPhase(phase)).
				Find(&exceptions).Error
			t.Fatalf("%s produced false positives: %+v / %+v", phase, result, exceptions)
		}
	}

	// 仅分配记录数量和权益数量相等还不够：
	// 每条退款分配记录都必须指向该购买记录自身的原始批次。
	// 已成功但结构异常的普通退款仍为 P1；
	// P0 只用于已证实存在双重获益的发放补偿事实。
	if err := db.Model(&refunddomain.RefundAllocation{}).
		Where("id = ?", allocation.ID).
		Update("lot_id", receiverLot.ID).Error; err != nil {
		t.Fatal(err)
	}
	result, err := service.ScanBatch(
		context.Background(),
		IntegrityCursor{Phase: IntegrityPhaseRefunds},
		100,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Detected != 1 {
		t.Fatalf("wrong-purchase refund allocation was not detected: %+v", result)
	}
	var refundException Exception
	if err := db.Where(
		"correlation_id = ? AND biz_type = ? AND biz_id = ?",
		reconciliationRuleRefund,
		"wine_ticket_refund",
		business.ID,
	).Take(&refundException).Error; err != nil {
		t.Fatal(err)
	}
	if refundException.Severity != "P1" {
		t.Fatalf("ordinary successful refund mismatch must be P1: %+v", refundException)
	}
}
