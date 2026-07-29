package redemption

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestRedemptionCreateFEFOZeroCashAndCancelRestoresExactlyOnce(t *testing.T) {
	fx := newRedemptionFixture(t)
	ctx := context.Background()
	slots, err := fx.service.DeliveryTimeSlots(ctx, fx.claims, RedemptionSlotQuery{
		ProductID: fx.productID, Quantity: 4, AddressID: fx.addressID,
		AddressVersion: 3, DateFrom: fx.slotStart, DateTo: fx.slotStart,
	})
	if err != nil {
		t.Fatalf("list slots: %v", err)
	}
	if len(slots) != 1 || slots[0].IssuerMerchantID != "1001" ||
		slots[0].AvailabilityStatus != "open" {
		t.Fatalf("unexpected slot projection: %+v", slots)
	}

	created, err := fx.service.Create(
		ctx, fx.claims, "POST", "/api/v1/wine-tickets/redemptions",
		"redemption-create-0001", fx.createRequest(4),
	)
	if err != nil {
		t.Fatalf("create redemption: %v", err)
	}
	if created.Status != RedemptionStatusScheduled || created.Quantity != 4 ||
		created.Product.ProductID != fmt.Sprint(fx.productID) || !created.CanCancel {
		t.Fatalf("unexpected created dto: %+v", created)
	}
	if stringsContains(created.AddressSummary, "13812345678") ||
		stringsContains(created.AddressSummary, "世纪大道") {
		t.Fatalf("address DTO leaked private snapshot fields: %q", created.AddressSummary)
	}
	if len(created.AllocationSummary) != 2 ||
		created.AllocationSummary[0].LotNo != "WTL_FEFO_EARLY" ||
		created.AllocationSummary[0].Quantity != 2 ||
		created.AllocationSummary[1].LotNo != "WTL_FEFO_LATER" ||
		created.AllocationSummary[1].Quantity != 2 {
		t.Fatalf("FEFO allocation mismatch: %+v", created.AllocationSummary)
	}

	replayed, err := fx.service.Create(
		ctx, fx.claims, "POST", "/api/v1/wine-tickets/redemptions",
		"redemption-create-0001", fx.createRequest(4),
	)
	if err != nil || replayed.RedemptionNo != created.RedemptionNo {
		t.Fatalf("idempotent create replay = %+v, %v", replayed, err)
	}
	assertRedemptionCreationFacts(t, fx, created)

	// 身份有效性变化后，资产退出操作仍必须可用。
	// 成年校验属于创建配送流程，不属于取消流程。
	revokedAt := fx.now
	if err := fx.db.Model(&redemptionTestRealname{}).
		Where("customer_id = ?", fx.customerID).
		Update("revoked_at", revokedAt).Error; err != nil {
		t.Fatal(err)
	}
	cancelled, err := fx.service.Cancel(
		ctx, fx.claims, "POST", "/api/v1/wine-tickets/redemptions/:redemption_no/cancel",
		"redemption-cancel-0001", created.RedemptionNo,
		RedemptionCancelRequest{ExpectedVersion: created.Version},
	)
	if err != nil {
		t.Fatalf("cancel redemption: %v", err)
	}
	if cancelled.Status != RedemptionStatusCancelled || cancelled.CanCancel ||
		cancelled.Version != created.Version+1 || cancelled.CancelResult == nil {
		t.Fatalf("unexpected cancel dto: %+v", cancelled)
	}
	assertRedemptionCancellationFacts(t, fx)

	replayedCancel, err := fx.service.Cancel(
		ctx, fx.claims, "POST", "/api/v1/wine-tickets/redemptions/:redemption_no/cancel",
		"redemption-cancel-0002", created.RedemptionNo,
		RedemptionCancelRequest{ExpectedVersion: created.Version},
	)
	if err != nil || replayedCancel.Status != RedemptionStatusCancelled {
		t.Fatalf("business-idempotent cancellation = %+v, %v", replayedCancel, err)
	}
	assertRedemptionCancellationFacts(t, fx)
}

func TestRedemptionCancellationCrossExpiryRestoresThenExpires(t *testing.T) {
	fx := newRedemptionFixture(t)
	ctx := context.Background()
	created, err := fx.service.Create(
		ctx, fx.claims, "POST", "/api/v1/wine-tickets/redemptions",
		"redemption-create-expiry", fx.createRequest(2),
	)
	if err != nil {
		t.Fatalf("create redemption: %v", err)
	}
	cancelNow := fx.lotExpiry.Add(time.Hour)
	fx.service.WithNow(func() time.Time { return cancelNow })
	cancelled, err := fx.service.Cancel(
		ctx, fx.claims, "POST", "/api/v1/wine-tickets/redemptions/:redemption_no/cancel",
		"redemption-cancel-expiry", created.RedemptionNo,
		RedemptionCancelRequest{ExpectedVersion: created.Version},
	)
	if err != nil {
		t.Fatalf("cancel after source expiry: %v", err)
	}
	if cancelled.CancelResult == nil ||
		!stringsContains(*cancelled.CancelResult, "已到期部分") {
		t.Fatalf("cross-expiry result missing: %+v", cancelled)
	}
	var lot core.Lot
	if err := fx.db.First(&lot, "id = ?", fx.lotEarlyID).Error; err != nil {
		t.Fatal(err)
	}
	if lot.AvailableQuantity != 0 || lot.Status != LotStatusExpired || !lot.EverUsed {
		t.Fatalf("expired restored lot = %+v", lot)
	}
	var transactions []core.Transaction
	if err := fx.db.Where("lot_id = ?", fx.lotEarlyID).Order("created_at ASC, id ASC").
		Find(&transactions).Error; err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 3 ||
		transactions[0].TransactionType != TransactionTypeRedemptionHold ||
		transactions[0].QuantityDelta != -2 ||
		transactions[1].TransactionType != TransactionTypeRedemptionRestore ||
		transactions[1].QuantityDelta != 2 ||
		transactions[2].TransactionType != TransactionTypeExpiry ||
		transactions[2].QuantityDelta != -2 {
		t.Fatalf("cross-expiry ledger mismatch: %+v", transactions)
	}
	for _, transaction := range transactions {
		if transaction.QuantityDelta == 0 {
			t.Fatalf("zero transaction persisted: %+v", transaction)
		}
	}
	redemptionID := transactions[0].BizID
	var allocation RedemptionAllocation
	if err := fx.db.Where(
		"redemption_id = ? AND lot_id = ?",
		redemptionID,
		fx.lotEarlyID,
	).Take(&allocation).Error; err != nil {
		t.Fatal(err)
	}
	if transactions[1].ActionKey != fmt.Sprintf(
		"redemption_restore:%d:%d",
		redemptionID,
		fx.lotEarlyID,
	) || transactions[1].BizType != "wine_ticket_redemption" ||
		transactions[1].BizID != redemptionID ||
		!transactions[1].CreatedAt.Equal(cancelNow) {
		t.Fatalf("redemption restore identity changed: %+v", transactions[1])
	}
	assertRedemptionTransactionMetadata(t, transactions[1], map[string]string{
		"redemption_no": created.RedemptionNo,
		"allocation_id": idString(allocation.ID),
	})
	if transactions[2].ActionKey != fmt.Sprintf(
		"expiry:%d:%d:after:redemption_restore:%d",
		fx.lotEarlyID,
		fx.lotExpiry.UnixMilli(),
		redemptionID,
	) || transactions[2].BizType != "wine_ticket_lot" ||
		transactions[2].BizID != fx.lotEarlyID ||
		!transactions[2].CreatedAt.Equal(cancelNow) {
		t.Fatalf("redemption expiry identity changed: %+v", transactions[2])
	}
	assertRedemptionTransactionMetadata(t, transactions[2], map[string]string{
		"trigger":       "redemption_cancel",
		"redemption_no": created.RedemptionNo,
	})
}

func TestRedemptionFailClosedAndDispatchFailureRollbackEveryWrite(t *testing.T) {
	t.Run("missing coordinator", func(t *testing.T) {
		fx := newRedemptionFixture(t)
		service := NewRedemptionService(fx.db, snowflake.New(38)).
			WithNow(func() time.Time { return fx.now })
		_, err := service.Create(
			context.Background(), fx.claims, "POST", "/api/v1/wine-tickets/redemptions",
			"redemption-no-dispatch", fx.createRequest(4),
		)
		redemptionAssertProblemCode(t, err, "WT_DISPATCH_UNAVAILABLE")
		assertNoRedemptionWrites(t, fx)
	})
	t.Run("coordinator error rolls back", func(t *testing.T) {
		fx := newRedemptionFixture(t)
		fx.dispatch.failEnsure = true
		_, err := fx.service.Create(
			context.Background(), fx.claims, "POST", "/api/v1/wine-tickets/redemptions",
			"redemption-dispatch-fail", fx.createRequest(4),
		)
		if err == nil {
			t.Fatal("expected dispatch failure")
		}
		assertNoRedemptionWrites(t, fx)
	})
	t.Run("address version conflict rolls back idempotency", func(t *testing.T) {
		fx := newRedemptionFixture(t)
		req := fx.createRequest(4)
		req.AddressVersion = 2
		_, err := fx.service.Create(
			context.Background(), fx.claims, "POST", "/api/v1/wine-tickets/redemptions",
			"redemption-bad-address", req,
		)
		redemptionAssertProblemCode(t, err, "ADDRESS_VERSION_CONFLICT")
		assertNoRedemptionWrites(t, fx)
	})
}

func TestRedemptionNeverCombinesIssuers(t *testing.T) {
	fx := newRedemptionFixture(t)
	if err := fx.db.Model(&core.Lot{}).Where("id = ?", fx.lotLaterID).
		Update("available_quantity", 1).Error; err != nil {
		t.Fatal(err)
	}
	_, err := fx.service.Create(
		context.Background(), fx.claims, "POST", "/api/v1/wine-tickets/redemptions",
		"redemption-cross-issuer", fx.createRequest(4),
	)
	redemptionAssertProblemCode(t, err, "WT_INSUFFICIENT_QUANTITY")
	assertNoRedemptionWrites(t, fx, 1)
	var other core.Lot
	if err := fx.db.First(&other, "id = ?", fx.otherLotID).Error; err != nil {
		t.Fatal(err)
	}
	if other.AvailableQuantity != 10 || other.EverUsed {
		t.Fatalf("other issuer lot was touched: %+v", other)
	}
}

func TestRedemptionRoutesStrictJSONQueryAndDecimalIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := newRedemptionFixture(t)
	engine := gin.New()
	api := engine.Group("/api/v1", func(c *gin.Context) {
		c.Set("auth_claims", fx.claims)
		c.Next()
	})
	RegisterRedemptionCustomerRoutes(api, NewRedemptionHandler(fx.service))
	routes := make(map[string]struct{})
	for _, route := range engine.Routes() {
		routes[route.Method+" "+route.Path] = struct{}{}
	}
	for _, expected := range []string{
		"GET /api/v1/wine-tickets/delivery-time-slots",
		"GET /api/v1/wine-tickets/redemptions",
		"POST /api/v1/wine-tickets/redemptions",
		"GET /api/v1/wine-tickets/redemptions/:redemption_no",
		"POST /api/v1/wine-tickets/redemptions/:redemption_no/cancel",
	} {
		if _, ok := routes[expected]; !ok {
			t.Fatalf("route %s was not registered; got %v", expected, routes)
		}
	}

	payload := fmt.Sprintf(
		`{"product_id":"%d","quantity":1,"address_id":"%d","address_version":3,"delivery_time_slot_id":"%d","unexpected":true}`,
		fx.productID, fx.addressID, fx.slotID,
	)
	request := httptest.NewRequest(
		http.MethodPost, "/api/v1/wine-tickets/redemptions", bytes.NewBufferString(payload),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "redemption-strict-json")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest ||
		!stringsContains(recorder.Body.String(), "unknown field") {
		t.Fatalf("unknown JSON field status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf(
			"/api/v1/wine-tickets/delivery-time-slots?product_id=0%d&quantity=1&address_id=%d&address_version=3",
			fx.productID, fx.addressID,
		),
		nil,
	)
	recorder = httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest ||
		!stringsContains(recorder.Body.String(), "VALIDATION_FAILED") {
		t.Fatalf("non-canonical decimal ID status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(
		http.MethodGet, "/api/v1/wine-tickets/redemptions?customer_id=7002", nil,
	)
	recorder = httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest ||
		!stringsContains(recorder.Body.String(), "VALIDATION_INVALID_QUERY") {
		t.Fatalf("unknown self-scope query status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
