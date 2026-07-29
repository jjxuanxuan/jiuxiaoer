package refund

import (
	"context"
	"errors"
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/config"
	sharedrefund "jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	purchasedomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/purchase"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestWorkerProviderErrorsRouteThroughPurchaseFailureHandlerAndKeepHold(t *testing.T) {
	service, db, now, purchase, lot := newRefundTestFixture(t)
	business, common := createRefundForSettlement(t, service, db, purchase)
	settlement := NewWineTicketRefundSettlement(db, snowflake.New(910)).
		WithNow(func() time.Time { return now.Add(time.Minute) })
	commonService := sharedrefund.NewService(
		config.Config{}, db, snowflake.New(911), nil,
	).WithRefundSettlementHandler(settlement)

	// 不调用支付机构后台任务，直接模拟 Claim 的版本防线。
	if err := db.Model(&commonRefundRow{}).Where("id = ?", common.ID).
		Updates(map[string]any{
			"version": 2, "locked_by": "worker-1",
			"locked_until": now.Add(time.Minute),
		}).Error; err != nil {
		t.Fatal(err)
	}
	if err := commonService.MarkAttemptError(
		context.Background(), common.ID, 2, errors.New("transport timeout"),
	); err != nil {
		t.Fatal(err)
	}
	var retryCommon commonRefundRow
	if err := db.First(&retryCommon, common.ID).Error; err != nil {
		t.Fatal(err)
	}
	var retryBusiness WineTicketRefund
	if err := db.First(&retryBusiness, business.ID).Error; err != nil {
		t.Fatal(err)
	}
	var retryLot core.Lot
	if err := db.First(&retryLot, lot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if retryCommon.Status != "submission_unknown" ||
		retryBusiness.Status != RefundStatusSubmissionUnknown ||
		retryLot.AvailableQuantity != 0 {
		t.Fatalf("retry common=%+v business=%+v lot=%+v", retryCommon, retryBusiness, retryLot)
	}

	// 后续明确的支付机构或预检拒绝会进入异常状态，但绝不会恢复权益。
	if err := db.Model(&commonRefundRow{}).Where("id = ?", common.ID).
		Updates(map[string]any{
			"version": retryCommon.Version + 1, "locked_by": "worker-2",
			"locked_until": now.Add(2 * time.Minute),
		}).Error; err != nil {
		t.Fatal(err)
	}
	claimedVersion := retryCommon.Version + 1
	if err := commonService.MarkPermanentError(
		context.Background(), common.ID, claimedVersion,
		"ORIGINAL_PAYMENT_AMOUNT_MISMATCH", errors.New("payment preflight mismatch"),
	); err != nil {
		t.Fatal(err)
	}
	var failedCommon commonRefundRow
	if err := db.First(&failedCommon, common.ID).Error; err != nil {
		t.Fatal(err)
	}
	var failedBusiness WineTicketRefund
	if err := db.First(&failedBusiness, business.ID).Error; err != nil {
		t.Fatal(err)
	}
	var failedPurchase purchasedomain.Purchase
	if err := db.First(&failedPurchase, purchase.ID).Error; err != nil {
		t.Fatal(err)
	}
	var allocation RefundAllocation
	if err := db.Where("wine_ticket_refund_id = ?", business.ID).Take(&allocation).Error; err != nil {
		t.Fatal(err)
	}
	if failedCommon.Status != "exception" ||
		failedBusiness.Status != RefundStatusException ||
		failedPurchase.Status != PurchaseStatusRefundException ||
		allocation.Status != RefundAllocationHeld {
		t.Fatalf("common=%+v business=%+v purchase=%+v allocation=%+v",
			failedCommon, failedBusiness, failedPurchase, allocation)
	}
}
