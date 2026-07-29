package refund

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	sharedrefund "jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/gift"
	purchasedomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/purchase"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestUnusedPurchaseRefundHoldsEveryOriginalLotAndIsIdempotent(t *testing.T) {
	service, db, _, purchase, lot := newRefundTestFixture(t)
	claims := refundCustomerClaims(purchase.CustomerID)

	quote, err := service.Quote(context.Background(), claims, purchase.PurchaseNo)
	if err != nil {
		t.Fatal(err)
	}
	if !quote.Eligible || quote.RefundableAmount != purchase.PaidAmount ||
		quote.ExpectedPurchaseVersion != purchase.Version || quote.QuoteToken == "" {
		t.Fatalf("unexpected refund quote: %+v", quote)
	}
	request := RefundCreateRequest{
		ReasonCode: "changed_mind", ExpectedPurchaseVersion: purchase.Version,
		QuoteToken: quote.QuoteToken,
	}
	create := func() (RefundDTO, error) {
		return service.Create(
			context.Background(), claims, "POST",
			"/api/v1/wine-tickets/purchases/:purchase_no/refunds",
			"refund-idempotency-key-001", purchase.PurchaseNo, request,
		)
	}
	beforeCreate, err := service.repo.transactionNow(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	first, err := create()
	if err != nil {
		t.Fatal(err)
	}
	afterCreate, err := service.repo.transactionNow(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	second, err := create()
	if err != nil {
		t.Fatalf("idempotent replay failed: %v", err)
	}
	if first.RefundNo == "" || second.RefundNo != first.RefundNo ||
		first.Status != RefundStatusHolding || first.EntitlementStatus != "held" {
		t.Fatalf("refund responses first=%+v second=%+v", first, second)
	}
	detail, err := service.Detail(context.Background(), claims, first.RefundNo)
	if err != nil {
		t.Fatal(err)
	}
	items, next, err := service.List(
		context.Background(), claims, pagination.Query{PageSize: 20}, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if detail.RefundNo != first.RefundNo || len(items) != 1 ||
		items[0].RefundNo != first.RefundNo || next != "" {
		t.Fatalf("detail=%+v list=%+v next=%q", detail, items, next)
	}

	var heldLot core.Lot
	if err := db.First(&heldLot, lot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if heldLot.AvailableQuantity != 0 || heldLot.Status != LotStatusDepleted ||
		heldLot.EverUsed {
		t.Fatalf("held lot=%+v", heldLot)
	}
	var business WineTicketRefund
	if err := db.Where("wine_ticket_refund_no = ?", first.RefundNo).Take(&business).Error; err != nil {
		t.Fatal(err)
	}
	var allocation RefundAllocation
	if err := db.Where("wine_ticket_refund_id = ?", business.ID).Take(&allocation).Error; err != nil {
		t.Fatal(err)
	}
	if allocation.LotID != lot.ID || allocation.Quantity != lot.TotalQuantity ||
		allocation.Status != RefundAllocationHeld {
		t.Fatalf("refund allocation=%+v", allocation)
	}
	var common commonRefundRow
	if err := db.First(&common, business.CurrentRefundID).Error; err != nil {
		t.Fatal(err)
	}
	if common.BizType == nil || *common.BizType != WineTicketPurchaseRefundBusiness ||
		common.BizID == nil || *common.BizID != business.ID ||
		common.OrderID != nil || common.AfterSaleID != nil ||
		common.PaymentID != purchase.PaymentID || common.Amount != purchase.PaidAmount ||
		common.Status != "creating" {
		t.Fatalf("common refund=%+v", common)
	}
	var hold core.Transaction
	if err := db.Where("action_key = ?", "refund_hold:"+idString(business.ID)+":"+idString(lot.ID)).
		Take(&hold).Error; err != nil {
		t.Fatal(err)
	}
	if hold.TransactionType != TransactionTypeRefundHold ||
		hold.QuantityDelta != -int(lot.TotalQuantity) ||
		hold.BeforeAvailableQuantity != lot.TotalQuantity ||
		hold.AfterAvailableQuantity != 0 ||
		hold.OwnerCustomerID != purchase.CustomerID ||
		hold.BizType != "refund" ||
		hold.BizID != business.ID ||
		string(hold.MetadataJSON) !=
			`{"refund_no":"`+business.WineTicketRefundNo+
				`","purchase_no":"`+purchase.PurchaseNo+
				`","source":"user_unused","rule_version":1}` ||
		!hold.CreatedAt.Equal(business.RequestedAt) {
		t.Fatalf("refund hold transaction=%+v", hold)
	}
	var holdCount int64
	if err := db.Model(&core.Transaction{}).
		Where("action_key = ?", hold.ActionKey).
		Count(&holdCount).Error; err != nil {
		t.Fatal(err)
	}
	if holdCount != 1 {
		t.Fatalf("refund hold transaction count=%d want=1", holdCount)
	}
	var gotPurchase purchasedomain.Purchase
	if err := db.First(&gotPurchase, purchase.ID).Error; err != nil {
		t.Fatal(err)
	}
	if gotPurchase.Status != PurchaseStatusRefundHolding {
		t.Fatalf("purchase status=%s", gotPurchase.Status)
	}
	if business.RequestedAt.Before(beforeCreate) || business.RequestedAt.After(afterCreate) {
		t.Fatalf(
			"requested_at=%s must be bounded by database time [%s,%s]",
			business.RequestedAt, beforeCreate, afterCreate,
		)
	}
}

func TestRefundQuoteAndCreateAllowExpiredRealnameForAssetExit(t *testing.T) {
	service, db, now, purchase, _ := newRefundTestFixture(t)
	expiredAt := now.Add(-time.Hour)
	revokedAt := now.Add(-time.Minute)
	if err := db.Model(&refundTestRealname{}).
		Where("customer_id = ?", purchase.CustomerID).
		Updates(map[string]any{
			"status":       "expired",
			"adult_result": "minor",
			"expires_at":   expiredAt,
			"revoked_at":   revokedAt,
		}).Error; err != nil {
		t.Fatal(err)
	}
	claims := refundCustomerClaims(purchase.CustomerID)
	quote, err := service.Quote(context.Background(), claims, purchase.PurchaseNo)
	if err != nil {
		t.Fatalf("asset-exit quote must not require current adult verification: %v", err)
	}
	if !quote.Eligible {
		t.Fatalf("expired realname unexpectedly changed refund eligibility: %+v", quote)
	}
	created, err := service.Create(
		context.Background(), claims, "POST",
		"/api/v1/wine-tickets/purchases/:purchase_no/refunds",
		"refund-expired-realname-001", purchase.PurchaseNo,
		RefundCreateRequest{
			ReasonCode:              "changed_mind",
			ExpectedPurchaseVersion: quote.ExpectedPurchaseVersion,
			QuoteToken:              quote.QuoteToken,
		},
	)
	if err != nil {
		t.Fatalf("asset-exit create must not require current adult verification: %v", err)
	}
	if created.Status != RefundStatusHolding {
		t.Fatalf("created refund=%+v", created)
	}
}

func TestRefundQuoteAndCreateStillRequireActiveCustomer(t *testing.T) {
	service, db, _, purchase, _ := newRefundTestFixture(t)
	claims := refundCustomerClaims(purchase.CustomerID)
	quote, err := service.Quote(context.Background(), claims, purchase.PurchaseNo)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&refundTestCustomer{}).
		Where("id = ?", purchase.CustomerID).
		Update("status", "disabled").Error; err != nil {
		t.Fatal(err)
	}
	_, err = service.Quote(context.Background(), claims, purchase.PurchaseNo)
	requireProblemCode(t, err, "PERM_FORBIDDEN")
	_, err = service.Create(
		context.Background(), claims, "POST",
		"/api/v1/wine-tickets/purchases/:purchase_no/refunds",
		"refund-inactive-customer-001", purchase.PurchaseNo,
		RefundCreateRequest{
			ReasonCode:              "changed_mind",
			ExpectedPurchaseVersion: quote.ExpectedPurchaseVersion,
			QuoteToken:              quote.QuoteToken,
		},
	)
	requireProblemCode(t, err, "PERM_FORBIDDEN")
}

func TestRefundCreateUsesDatabaseClockForQuoteExpiry(t *testing.T) {
	service, db, _, purchase, _ := newRefundTestFixture(t)
	claims := refundCustomerClaims(purchase.CustomerID)
	quote, err := service.Quote(context.Background(), claims, purchase.PurchaseNo)
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.verifyQuote(quote.QuoteToken)
	if err != nil {
		t.Fatal(err)
	}
	databaseNow, err := service.repo.transactionNow(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	token.QuoteExpiresAtMS = databaseNow.Add(-time.Millisecond).UnixMilli()
	expiredToken, err := service.signQuote(token)
	if err != nil {
		t.Fatal(err)
	}
	// 有意把应用时钟调整到令牌时间之前。
	// Create 仍必须使用事务数据库时钟拒绝请求。
	service.WithNow(func() time.Time { return databaseNow.Add(-24 * time.Hour) })
	_, err = service.Create(
		context.Background(), claims, "POST",
		"/api/v1/wine-tickets/purchases/:purchase_no/refunds",
		"refund-db-clock-expiry-001", purchase.PurchaseNo,
		RefundCreateRequest{
			ReasonCode:              "changed_mind",
			ExpectedPurchaseVersion: quote.ExpectedPurchaseVersion,
			QuoteToken:              expiredToken,
		},
	)
	requireProblemCode(t, err, "WT_REFUND_QUOTE_EXPIRED")
}

func TestDuplicateRefundCreateReentersCanonicalExistingLockPlan(t *testing.T) {
	service, _, _, purchase, _ := newRefundTestFixture(t)
	claims := refundCustomerClaims(purchase.CustomerID)
	quote, err := service.Quote(context.Background(), claims, purchase.PurchaseNo)
	if err != nil {
		t.Fatal(err)
	}
	request := RefundCreateRequest{
		ReasonCode:              "changed_mind",
		ExpectedPurchaseVersion: quote.ExpectedPurchaseVersion,
		QuoteToken:              quote.QuoteToken,
	}
	if _, err := service.Create(
		context.Background(), claims, "POST",
		"/api/v1/wine-tickets/purchases/:purchase_no/refunds",
		"refund-duplicate-first-001", purchase.PurchaseNo, request,
	); err != nil {
		t.Fatal(err)
	}
	_, err = service.Create(
		context.Background(), claims, "POST",
		"/api/v1/wine-tickets/purchases/:purchase_no/refunds",
		"refund-duplicate-second-001", purchase.PurchaseNo, request,
	)
	requireProblemCode(t, err, "WT_REFUND_IN_PROGRESS")
}

func TestConcurrentRefundCreateProducesOneActiveRefund(t *testing.T) {
	service, db, _, purchase, _ := newRefundTestFixture(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	claims := refundCustomerClaims(purchase.CustomerID)
	quote, err := service.Quote(context.Background(), claims, purchase.PurchaseNo)
	if err != nil {
		t.Fatal(err)
	}
	request := RefundCreateRequest{
		ReasonCode:              "changed_mind",
		ExpectedPurchaseVersion: quote.ExpectedPurchaseVersion,
		QuoteToken:              quote.QuoteToken,
	}
	type result struct {
		dto RefundDTO
		err error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, key := range []string{"refund-concurrent-a-001", "refund-concurrent-b-001"} {
		key := key
		go func() {
			<-start
			dto, createErr := service.Create(
				context.Background(), claims, "POST",
				"/api/v1/wine-tickets/purchases/:purchase_no/refunds",
				key, purchase.PurchaseNo, request,
			)
			results <- result{dto: dto, err: createErr}
		}()
	}
	close(start)
	successes := 0
	conflicts := 0
	for range 2 {
		got := <-results
		if got.err == nil {
			successes++
			if got.dto.Status != RefundStatusHolding {
				t.Fatalf("successful refund=%+v", got.dto)
			}
			continue
		}
		if problem.FromError(got.err).ErrorCode == "WT_REFUND_IN_PROGRESS" {
			conflicts++
			continue
		}
		t.Fatalf("unexpected concurrent create error: %v", got.err)
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	var active int64
	if err := db.Model(&WineTicketRefund{}).
		Where("purchase_id = ? AND status IN ?", purchase.ID, wineTicketRefundActiveStatuses).
		Count(&active).Error; err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active refunds=%d want=1", active)
	}
}

func TestRefundQuoteRejectsAnyHistoricalGiftEvenAfterRestore(t *testing.T) {
	service, db, _, purchase, lot := newRefundTestFixture(t)
	if err := db.Create(&gift.Gift{
		ID: 7801, GiftNo: "WTG7801", GiverCustomerID: purchase.CustomerID,
		IssuerMerchantID: purchase.IssuerMerchantID, ProductID: purchase.ProductID,
		RedeemCityCode: purchase.RedeemCityCode, Quantity: lot.TotalQuantity,
		Status: GiftStatusCancelled, ClaimDeadline: lot.ExpiresAt,
		EarliestExpiresAt: lot.ExpiresAt, Version: 2,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&gift.GiftAllocation{
		ID: 7802, GiftID: 7801, SourceLotID: lot.ID,
		Quantity: lot.TotalQuantity, SourceExpiresAt: lot.ExpiresAt,
		Status: GiftAllocationStatusRestored,
	}).Error; err != nil {
		t.Fatal(err)
	}
	quote, err := service.Quote(
		context.Background(), refundCustomerClaims(purchase.CustomerID), purchase.PurchaseNo,
	)
	if err != nil {
		t.Fatal(err)
	}
	if quote.Eligible || !hasRefundReason(quote.IneligibleReasons, "entitlement_used") {
		t.Fatalf("historical gift must permanently block refund: %+v", quote)
	}
}

func TestRefundQuoteRequiresTheOriginalPaymentToBeFullyUnrefunded(t *testing.T) {
	service, db, _, purchase, _ := newRefundTestFixture(t)
	if err := db.Model(&refundPayment{}).
		Where("id = ?", purchase.PaymentID).
		Update("refunded_amount", 1).Error; err != nil {
		t.Fatal(err)
	}
	quote, err := service.Quote(
		context.Background(),
		refundCustomerClaims(purchase.CustomerID),
		purchase.PurchaseNo,
	)
	if err != nil {
		t.Fatal(err)
	}
	if quote.Eligible ||
		quote.RefundableAmount != purchase.PaidAmount-1 ||
		!hasRefundReason(quote.IneligibleReasons, "payment_not_succeeded") {
		t.Fatalf("partial prior refund must block full purchase refund: %+v", quote)
	}
}

func TestRefundSuccessConsumesHoldWithoutSecondQuantityDeduction(t *testing.T) {
	service, db, now, purchase, lot := newRefundTestFixture(t)
	business, common := createRefundForSettlement(t, service, db, purchase)
	settlement := NewWineTicketRefundSettlement(db, snowflake.New(904)).
		WithNow(func() time.Time { return now.Add(time.Minute) })
	commonService := sharedrefund.NewService(
		config.Config{}, db, snowflake.New(905), nil,
	).WithRefundSettlementHandler(settlement)
	state := sharedrefund.State{
		ProviderRefundID: "wx-refund-success-1",
		RefundNo:         common.RefundNo, PaymentNo: "PAY-" + idString(purchase.ID),
		Status: "SUCCESS", Currency: "CNY", CurrencyRequired: true,
		Amount: common.Amount, TotalAmount: common.TotalAmount,
		SucceededAt: timePtr(now.Add(30 * time.Second)),
	}
	if err := commonService.ApplyProviderState(context.Background(), common.ID, state); err != nil {
		t.Fatal(err)
	}
	if err := commonService.ApplyProviderState(context.Background(), common.ID, state); err != nil {
		t.Fatalf("success replay failed: %v", err)
	}

	var gotBusiness WineTicketRefund
	if err := db.First(&gotBusiness, business.ID).Error; err != nil {
		t.Fatal(err)
	}
	if gotBusiness.Status != RefundStatusSucceeded || gotBusiness.SucceededAt == nil {
		t.Fatalf("business refund=%+v", gotBusiness)
	}
	var gotPurchase purchasedomain.Purchase
	if err := db.First(&gotPurchase, purchase.ID).Error; err != nil {
		t.Fatal(err)
	}
	if gotPurchase.Status != PurchaseStatusRefunded || gotPurchase.RefundedAt == nil {
		t.Fatalf("purchase=%+v", gotPurchase)
	}
	var gotLot core.Lot
	if err := db.First(&gotLot, lot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if gotLot.Status != LotStatusRefunded || gotLot.AvailableQuantity != 0 {
		t.Fatalf("lot=%+v", gotLot)
	}
	var allocation RefundAllocation
	if err := db.Where("wine_ticket_refund_id = ?", business.ID).Take(&allocation).Error; err != nil {
		t.Fatal(err)
	}
	if allocation.Status != RefundAllocationConsumed {
		t.Fatalf("allocation=%+v", allocation)
	}
	var payment refundPayment
	if err := db.First(&payment, purchase.PaymentID).Error; err != nil {
		t.Fatal(err)
	}
	if payment.RefundedAmount != purchase.PaidAmount {
		t.Fatalf("payment refunded_amount=%d", payment.RefundedAmount)
	}
	var transactionCount int64
	if err := db.Model(&core.Transaction{}).Where("lot_id = ?", lot.ID).Count(&transactionCount).Error; err != nil {
		t.Fatal(err)
	}
	if transactionCount != 2 {
		t.Fatalf("transactions=%d want issue+hold only", transactionCount)
	}
}

func TestProviderMismatchKeepsEntitlementHeldAndPersistsException(t *testing.T) {
	service, db, now, purchase, lot := newRefundTestFixture(t)
	business, common := createRefundForSettlement(t, service, db, purchase)
	settlement := NewWineTicketRefundSettlement(db, snowflake.New(906)).
		WithNow(func() time.Time { return now.Add(time.Minute) })
	commonService := sharedrefund.NewService(
		config.Config{}, db, snowflake.New(907), nil,
	).WithRefundSettlementHandler(settlement)
	state := sharedrefund.State{
		RefundNo: common.RefundNo, PaymentNo: "PAY-" + idString(purchase.ID),
		Status: "SUCCESS", Currency: "CNY", CurrencyRequired: true,
		Amount: common.Amount - 1, TotalAmount: common.TotalAmount,
	}
	err := commonService.ApplyProviderState(context.Background(), common.ID, state)
	if err == nil || problem.FromError(err).ErrorCode != "REFUND_AMOUNT_MISMATCH" {
		t.Fatalf("mismatch error=%v", err)
	}
	var gotBusiness WineTicketRefund
	if err := db.First(&gotBusiness, business.ID).Error; err != nil {
		t.Fatal(err)
	}
	var gotCommon commonRefundRow
	if err := db.First(&gotCommon, common.ID).Error; err != nil {
		t.Fatal(err)
	}
	var gotPurchase purchasedomain.Purchase
	if err := db.First(&gotPurchase, purchase.ID).Error; err != nil {
		t.Fatal(err)
	}
	var gotLot core.Lot
	if err := db.First(&gotLot, lot.ID).Error; err != nil {
		t.Fatal(err)
	}
	var allocation RefundAllocation
	if err := db.Where("wine_ticket_refund_id = ?", business.ID).Take(&allocation).Error; err != nil {
		t.Fatal(err)
	}
	if gotBusiness.Status != RefundStatusException ||
		gotCommon.Status != "exception" || gotPurchase.Status != PurchaseStatusRefundException ||
		gotLot.AvailableQuantity != 0 || allocation.Status != RefundAllocationHeld {
		t.Fatalf("business=%+v common=%+v purchase=%+v lot=%+v allocation=%+v",
			gotBusiness, gotCommon, gotPurchase, gotLot, allocation)
	}
}

func TestClosedReplacementKeepsHoldUntilReplacementSuccess(t *testing.T) {
	service, db, now, purchase, lot := newRefundTestFixture(t)
	business, common := createRefundForSettlement(t, service, db, purchase)
	settlement := NewWineTicketRefundSettlement(db, snowflake.New(908)).
		WithNow(func() time.Time { return now.Add(time.Minute) })
	commonService := sharedrefund.NewService(
		config.Config{}, db, snowflake.New(909), nil,
	).WithRefundSettlementHandler(settlement)

	closed := sharedrefund.State{
		ProviderRefundID: "wx-closed-1", RefundNo: common.RefundNo,
		PaymentNo: "PAY-" + idString(purchase.ID), Status: "CLOSED",
		Currency: "CNY", CurrencyRequired: true,
		Amount: common.Amount, TotalAmount: common.TotalAmount,
	}
	if err := commonService.ApplyProviderState(context.Background(), common.ID, closed); err != nil {
		t.Fatal(err)
	}
	var afterClosed WineTicketRefund
	if err := db.First(&afterClosed, business.ID).Error; err != nil {
		t.Fatal(err)
	}
	if afterClosed.Status != RefundStatusSubmitting ||
		afterClosed.CurrentRefundID == common.ID {
		t.Fatalf("closed business=%+v", afterClosed)
	}

	var replacement commonRefundRow
	if err := db.First(&replacement, afterClosed.CurrentRefundID).Error; err != nil {
		t.Fatal(err)
	}
	if replacement.ReplacesRefundID == nil ||
		*replacement.ReplacesRefundID != common.ID ||
		replacement.Status != "creating" ||
		replacement.NextRetryAt == nil {
		t.Fatalf("replacement=%+v", replacement)
	}

	// 重复的 CLOSED 观测不得让替代链产生分叉。
	if err := commonService.ApplyProviderState(context.Background(), common.ID, closed); err != nil {
		t.Fatal(err)
	}
	var replacementCount int64
	if err := db.Model(&commonRefundRow{}).
		Where("replaces_refund_id = ?", common.ID).
		Count(&replacementCount).Error; err != nil {
		t.Fatal(err)
	}
	if replacementCount != 1 {
		t.Fatalf("replacement count=%d", replacementCount)
	}

	replacementID := replacement.ID
	processing := sharedrefund.State{
		ProviderRefundID: "wx-replacement-1", RefundNo: replacement.RefundNo,
		PaymentNo: "PAY-" + idString(purchase.ID), Status: "PROCESSING",
		Currency: "CNY", CurrencyRequired: true,
		Amount: replacement.Amount, TotalAmount: replacement.TotalAmount,
	}
	if err := commonService.ApplyProviderState(context.Background(), replacementID, processing); err != nil {
		t.Fatal(err)
	}
	var processingBusiness WineTicketRefund
	if err := db.First(&processingBusiness, business.ID).Error; err != nil {
		t.Fatal(err)
	}
	var processingLot core.Lot
	if err := db.First(&processingLot, lot.ID).Error; err != nil {
		t.Fatal(err)
	}
	if processingBusiness.CurrentRefundID != replacementID ||
		processingBusiness.Status != RefundStatusProcessing ||
		processingLot.AvailableQuantity != 0 {
		t.Fatalf("replacement processing business=%+v lot=%+v", processingBusiness, processingLot)
	}
	processing.Status = "SUCCESS"
	processing.SucceededAt = timePtr(now.Add(3 * time.Minute))
	if err := commonService.ApplyProviderState(context.Background(), replacementID, processing); err != nil {
		t.Fatal(err)
	}
	var succeeded WineTicketRefund
	if err := db.First(&succeeded, business.ID).Error; err != nil {
		t.Fatal(err)
	}
	if succeeded.Status != RefundStatusSucceeded ||
		succeeded.CurrentRefundID != replacementID {
		t.Fatalf("replacement success=%+v", succeeded)
	}
}

func newRefundTestFixture(
	t *testing.T,
) (*RefundService, *gorm.DB, time.Time, purchasedomain.Purchase, core.Lot) {
	t.Helper()
	db := newRefundTestDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sqlDB.Close()
	})
	if err := db.AutoMigrate(&sharedrefund.Row{}, &idempotency.Record{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE refunds ADD COLUMN deleted_at datetime").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`
		CREATE UNIQUE INDEX uk_refund_test_active_purchase
		ON wine_ticket_refunds(purchase_id)
		WHERE status IN (
			'holding','submitting','processing','submission_unknown',
			'retry_pending','exception'
		)
	`).Error; err != nil {
		t.Fatal(err)
	}
	now, _ := refundTestTransactionNow(context.Background(), db)
	customerID := uint64(6101)
	expiresAt := now.AddDate(1, 0, 0)
	for _, row := range []any{
		&refundTestCustomer{
			ID: customerID, Phone: "13800006101", Status: "active",
		},
		&refundTestRealname{
			CustomerID: customerID, Status: "verified",
			AdultResult: "adult", ExpiresAt: &expiresAt,
		},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	purchase := seedRefundPurchase(t, db, now, 6201, customerID, 1, 6)
	paidAt := now.Add(-2 * time.Hour)
	issuedAt := paidAt.Add(time.Second)
	if err := db.Model(&purchasedomain.Purchase{}).Where("id = ?", purchase.ID).Updates(map[string]any{
		"status": PurchaseStatusIssued, "paid_amount": purchase.PayableAmount,
		"paid_at": paidAt, "issued_at": issuedAt, "version": 3,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("payments").Where("id = ?", purchase.PaymentID).Updates(map[string]any{
		"status": "succeeded", "provider_status": "SUCCESS",
		"provider_trade_no": "WX-" + idString(purchase.ID),
		"paid_at":           paidAt, "version": 3,
	}).Error; err != nil {
		t.Fatal(err)
	}
	lot := core.Lot{
		ID: 6301, LotNo: "WTL6301", OwnerCustomerID: customerID,
		PurchaseID: purchase.ID, SourceType: LotSourcePurchase,
		IssuerMerchantID: purchase.IssuerMerchantID, ProductID: purchase.ProductID,
		RedeemCityCode:    purchase.RedeemCityCode,
		TotalQuantity:     purchase.TotalBottleQuantity,
		AvailableQuantity: purchase.TotalBottleQuantity,
		OriginalExpiresAt: now.AddDate(0, 0, 365),
		ExpiresAt:         now.AddDate(0, 0, 365), ExpiryChangedAt: issuedAt,
		Status: LotStatusActive, Version: 1, CreatedAt: issuedAt, UpdatedAt: issuedAt,
	}
	if err := db.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}
	issueID := uint64(6401)
	if err := db.Create(&core.Transaction{
		ID: issueID, TransactionNo: "WTT6401", LotID: lot.ID,
		OwnerCustomerID: customerID, TransactionType: TransactionTypePurchaseIssue,
		QuantityDelta: int(lot.TotalQuantity), BeforeAvailableQuantity: 0,
		AfterAvailableQuantity: lot.TotalQuantity, BizType: "purchase",
		BizID: purchase.ID, ActionKey: "purchase_issue:6201:6301",
		MetadataJSON: datatypes.JSON(`{"source":"test"}`), CreatedAt: issuedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&purchase, purchase.ID).Error; err != nil {
		t.Fatal(err)
	}
	service := NewRefundService(
		db, snowflake.New(903), "refund-test-quote-secret-at-least-32-bytes",
	).WithNow(func() time.Time { return now })
	service.repo.clock = refundTestTransactionNow
	return service, db, now, purchase, lot
}

func refundTestTransactionNow(context.Context, *gorm.DB) (time.Time, error) {
	return time.Now().In(shanghaiLocation).Truncate(time.Millisecond), nil
}

func createRefundForSettlement(
	t *testing.T,
	service *RefundService,
	db *gorm.DB,
	purchase purchasedomain.Purchase,
) (WineTicketRefund, commonRefundRow) {
	t.Helper()
	claims := refundCustomerClaims(purchase.CustomerID)
	quote, err := service.Quote(context.Background(), claims, purchase.PurchaseNo)
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(
		context.Background(), claims, "POST",
		"/api/v1/wine-tickets/purchases/:purchase_no/refunds",
		"refund-settlement-idem-"+idString(purchase.ID),
		purchase.PurchaseNo,
		RefundCreateRequest{
			ReasonCode:              "duplicate_purchase",
			ExpectedPurchaseVersion: quote.ExpectedPurchaseVersion,
			QuoteToken:              quote.QuoteToken,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var business WineTicketRefund
	if err := db.Where("wine_ticket_refund_no = ?", created.RefundNo).Take(&business).Error; err != nil {
		t.Fatal(err)
	}
	var common commonRefundRow
	if err := db.First(&common, business.CurrentRefundID).Error; err != nil {
		t.Fatal(err)
	}
	return business, common
}

func refundCustomerClaims(customerID uint64) *auth.Claims {
	return customerClaimsFor(
		customerID,
		"wine_ticket_refund:quote",
		"wine_ticket_refund:create",
		"wine_ticket_refund:view",
	)
}

func hasRefundReason(rows []RefundIneligibleReasonDTO, code string) bool {
	for _, row := range rows {
		if row.Code == code {
			return true
		}
	}
	return false
}

func requireProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s", code)
	}
	var details *problem.Details
	if !errors.As(err, &details) || details.ErrorCode != code {
		t.Fatalf("error=%v want=%s", err, code)
	}
}
