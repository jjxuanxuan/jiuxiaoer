package refund

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type testSettlementHandler struct {
	bizType string
	plan    []string

	mu           sync.Mutex
	invocations  int
	stateChanges int
}

func (h *testSettlementHandler) BusinessType() string { return h.bizType }

func (h *testSettlementHandler) LockAndApply(ctx context.Context, tx *gorm.DB, command RefundSettlementCommand) (RefundSettlementResult, error) {
	h.mu.Lock()
	h.invocations++
	h.mu.Unlock()

	var current Row
	for _, step := range h.plan {
		switch step {
		case "refund":
			if err := tx.WithContext(ctx).Where("id=?", command.Lookup.ID).Take(&current).Error; err != nil {
				return RefundSettlementResult{}, err
			}
		case "business":
			var business struct {
				ID     uint64
				Status string
			}
			_, bizID := RefundBusiness(command.Lookup)
			if err := tx.WithContext(ctx).Table("test_refund_businesses").Where("id=?", bizID).Take(&business).Error; err != nil {
				return RefundSettlementResult{}, err
			}
		default:
			return RefundSettlementResult{}, fmt.Errorf("unknown test lock step %q", step)
		}
	}
	if current.ID == 0 {
		if err := tx.WithContext(ctx).Where("id=?", command.Lookup.ID).Take(&current).Error; err != nil {
			return RefundSettlementResult{}, err
		}
	}
	if command.ClaimedVersion != nil && current.Version != *command.ClaimedVersion {
		return RefundSettlementResult{}, nil
	}
	if !SameRefundSettlementRoute(command.Lookup, current) {
		return RefundSettlementResult{
			Reject:            problem.Conflict("REFUND_SETTLEMENT_ROUTE_CHANGED", "refund settlement route changed"),
			CallbackErrorCode: "REFUND_SETTLEMENT_ROUTE_CHANGED",
		}, nil
	}

	incoming := strings.ToUpper(strings.TrimSpace(command.State.Status))
	switch {
	case current.Status == "succeeded" && incoming == "SUCCESS":
		return RefundSettlementResult{}, nil
	case current.Status == "failed" && incoming == "CLOSED":
		return RefundSettlementResult{}, nil
	case current.Status == "exception" && incoming == "ABNORMAL":
		return RefundSettlementResult{}, nil
	}

	status := ""
	switch incoming {
	case "SUCCESS":
		status = "succeeded"
	case "CLOSED":
		status = "failed"
	case "ABNORMAL":
		status = "exception"
	default:
		return RefundSettlementResult{
			Reject:            problem.InvalidArgument("REFUND_PROVIDER_STATUS_INVALID", "unsupported provider status"),
			CallbackErrorCode: "REFUND_PROVIDER_STATUS_INVALID",
		}, nil
	}
	if err := tx.WithContext(ctx).Model(&Row{}).Where("id=?", current.ID).
		Updates(map[string]any{"status": status, "version": gorm.Expr("version+1")}).Error; err != nil {
		return RefundSettlementResult{}, err
	}
	h.mu.Lock()
	h.stateChanges++
	h.mu.Unlock()
	return RefundSettlementResult{}, nil
}

func (h *testSettlementHandler) counts() (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.invocations, h.stateChanges
}

type testRefundCallbackProvider struct {
	event CallbackEvent
}

func (p *testRefundCallbackProvider) Code() string { return "wechat" }
func (p *testRefundCallbackProvider) Refund(context.Context, Input) (State, error) {
	return State{}, errors.New("not implemented")
}
func (p *testRefundCallbackProvider) QueryRefund(context.Context, string) (State, error) {
	return State{}, errors.New("not implemented")
}
func (p *testRefundCallbackProvider) ParseRefundCallback(context.Context, *http.Request) (CallbackEvent, error) {
	return p.event, nil
}

func TestRefundSettlementRegistryRejectsInvalidRegistration(t *testing.T) {
	service := &Service{settlements: newSettlementRegistry()}
	for _, handler := range []RefundSettlementHandler{
		nil,
		&testSettlementHandler{},
		&testSettlementHandler{bizType: RetailAfterSaleRefundBusiness},
	} {
		assertPanics(t, func() { service.WithRefundSettlementHandler(handler) })
	}

	handler := &testSettlementHandler{bizType: "wine_ticket_refund"}
	service.WithRefundSettlementHandler(handler)
	assertPanics(t, func() { service.WithRefundSettlementHandler(handler) })
}

func TestExternalRefundHandlersOwnTheirLockPlanAndIdempotence(t *testing.T) {
	db := newSettlementTestDB(t)
	insertSettlementRow(t, db, 101, 201, "wine_ticket_purchase_refund", 301, "pending", 7)
	insertSettlementRow(t, db, 102, 202, "wine_ticket_renewal_compensation", 302, "pending", 11)
	for _, id := range []uint64{301, 302} {
		if err := db.Exec("INSERT INTO test_refund_businesses (id,status) VALUES (?,?)", id, "pending").Error; err != nil {
			t.Fatal(err)
		}
	}

	purchase := &testSettlementHandler{bizType: "wine_ticket_purchase_refund", plan: []string{"refund", "business"}}
	renewal := &testSettlementHandler{bizType: "wine_ticket_renewal_compensation", plan: []string{"business", "refund"}}
	service := NewService(config.Config{}, db, snowflake.New(801), nil).
		WithRefundSettlementHandler(purchase).
		WithRefundSettlementHandler(renewal)

	success := State{RefundNo: "RF101", PaymentNo: "P201", Status: "SUCCESS", Amount: 100, TotalAmount: 1000, Currency: "CNY"}
	if err := service.ApplyProviderState(context.Background(), 101, success); err != nil {
		t.Fatalf("purchase settlement: %v", err)
	}
	if err := service.ApplyProviderState(context.Background(), 101, success); err != nil {
		t.Fatalf("idempotent purchase settlement: %v", err)
	}
	if invocations, changes := purchase.counts(); invocations != 2 || changes != 1 {
		t.Fatalf("purchase invocations=%d state_changes=%d", invocations, changes)
	}

	abnormal := State{RefundNo: "RF102", PaymentNo: "P202", Status: "ABNORMAL", Amount: 100, TotalAmount: 1000, Currency: "CNY"}
	if err := service.ApplyProviderState(context.Background(), 102, abnormal); err != nil {
		t.Fatalf("renewal exception: %v", err)
	}
	if err := service.ApplyProviderState(context.Background(), 102, abnormal); err != nil {
		t.Fatalf("idempotent renewal exception: %v", err)
	}
	if invocations, changes := renewal.counts(); invocations != 2 || changes != 1 {
		t.Fatalf("renewal invocations=%d state_changes=%d", invocations, changes)
	}
}

func TestClaimedExternalSettlementRequiresExactVersion(t *testing.T) {
	db := newSettlementTestDB(t)
	insertSettlementRow(t, db, 111, 211, "wine_ticket_refund", 311, "pending", 9)
	if err := db.Exec("INSERT INTO test_refund_businesses (id,status) VALUES (?,?)", 311, "pending").Error; err != nil {
		t.Fatal(err)
	}
	handler := &testSettlementHandler{bizType: "wine_ticket_refund", plan: []string{"refund", "business"}}
	service := NewService(config.Config{}, db, snowflake.New(802), nil).WithRefundSettlementHandler(handler)

	state := State{RefundNo: "RF111", PaymentNo: "P211", Status: "SUCCESS", Amount: 100, TotalAmount: 1000, Currency: "CNY"}
	if err := service.ApplyClaimedProviderState(context.Background(), 111, 8, state); err != nil {
		t.Fatal(err)
	}
	if invocations, changes := handler.counts(); invocations != 0 || changes != 0 {
		t.Fatalf("stale claim invoked handler: invocations=%d changes=%d", invocations, changes)
	}
	if err := service.ApplyClaimedProviderState(context.Background(), 111, 9, state); err != nil {
		t.Fatal(err)
	}
	if invocations, changes := handler.counts(); invocations != 1 || changes != 1 {
		t.Fatalf("current claim invocations=%d changes=%d", invocations, changes)
	}
}

func TestUnknownExternalRefundCallbackPersistsFailureAndRejectsReplays(t *testing.T) {
	db := newSettlementTestDB(t)
	insertSettlementRow(t, db, 121, 221, "wine_ticket_refund", 321, "pending", 3)
	eventID := "unknown-business-event"
	state := State{RefundNo: "RF121", PaymentNo: "P221", Status: "SUCCESS", Amount: 100, TotalAmount: 1000, Currency: "CNY"}
	provider := &testRefundCallbackProvider{event: CallbackEvent{EventID: eventID, State: state}}
	cfg := config.Config{}
	cfg.WeChat.PayMockEnabled = true
	service := NewService(cfg, db, snowflake.New(803), provider)

	for attempt := 0; attempt < 2; attempt++ {
		request, err := http.NewRequest(http.MethodPost, "/refunds/wechat/callbacks", strings.NewReader(`{"event":"unknown"}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := service.ProcessCallback(context.Background(), "wechat", request, []byte(`{"event":"unknown"}`)); err == nil {
			t.Fatalf("attempt %d unexpectedly succeeded", attempt+1)
		}
	}

	var callback Callback
	if err := db.Where("provider=? AND provider_event_id=?", "wechat", eventID).Take(&callback).Error; err != nil {
		t.Fatal(err)
	}
	if callback.ProcessStatus != "failed" || callback.ErrorCode == nil || *callback.ErrorCode != "REFUND_SETTLEMENT_HANDLER_NOT_FOUND" {
		t.Fatalf("callback=%+v", callback)
	}
	var count int64
	if err := db.Model(&Callback{}).Where("provider=? AND provider_event_id=?", "wechat", eventID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("callback rows=%d", count)
	}
}

func TestUnknownExternalRefundPollingFailsClosed(t *testing.T) {
	db := newSettlementTestDB(t)
	insertSettlementRow(t, db, 131, 231, "wine_ticket_refund", 331, "pending", 1)
	service := NewService(config.Config{}, db, snowflake.New(804), nil)
	err := service.ApplyProviderState(context.Background(), 131, State{RefundNo: "RF131", Status: "SUCCESS"})
	if err == nil || problem.FromError(err).Status != 500 || problem.FromError(err).ErrorCode != "REFUND_SETTLEMENT_HANDLER_NOT_FOUND" {
		t.Fatalf("error=%v", err)
	}
	var status string
	if err := db.Table("refunds").Select("status").Where("id=?", 131).Scan(&status).Error; err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("status=%q", status)
	}
}

func TestLegacyRefundBusinessDefaultsToRetail(t *testing.T) {
	afterSaleID := uint64(77)
	bizType, bizID := RefundBusiness(Row{AfterSaleID: &afterSaleID})
	if bizType != RetailAfterSaleRefundBusiness || bizID != 77 {
		t.Fatalf("biz_type=%q biz_id=%d", bizType, bizID)
	}
}

func TestLegacyAdminRefundSurfaceExcludesTypedBusinessRefunds(t *testing.T) {
	db := newSettlementTestDB(t)
	now := time.Now()
	if err := db.Exec(
		`INSERT INTO refunds
		 (id,after_sale_id,order_id,payment_id,refund_no,provider,status,currency,
		  biz_type,biz_id,amount,total_amount,requested_at,version,created_at,updated_at)
		 VALUES
		 (201,301,401,501,'RF201','wechat','pending','CNY',
		  'retail_after_sale',301,100,1000,?,1,?,?),
		 (202,NULL,NULL,502,'RF202','wechat','pending','CNY',
		  'wine_ticket_refund',602,100,1000,?,1,?,?)`,
		now, now, now, now, now, now,
	).Error; err != nil {
		t.Fatal(err)
	}

	rows, err := NewRepository(db).List(context.Background(), "", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != 201 {
		t.Fatalf("legacy admin list leaked typed refunds: %+v", rows)
	}
	candidates, err := NewRepository(db).RepairCandidates(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != 201 {
		t.Fatalf("legacy repair list leaked typed refunds: %+v", candidates)
	}

	wineType := "wine_ticket_refund"
	wineBizID := uint64(602)
	gateErr := requireRetailAdminRefund(Row{
		ID: 202, BizType: &wineType, BizID: &wineBizID,
	})
	if problem.FromError(gateErr).ErrorCode != "REFUND_TYPED_ADMIN_ACTION_REQUIRED" {
		t.Fatalf("typed admin action gate err=%v", gateErr)
	}
}

func newSettlementTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:refund-settlement-%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	schema := []string{
		`CREATE TABLE refunds (
			id INTEGER PRIMARY KEY,
			after_sale_id INTEGER,
			order_id INTEGER,
			payment_id INTEGER NOT NULL DEFAULT 0,
			refund_no TEXT NOT NULL UNIQUE,
			provider TEXT NOT NULL,
			status TEXT NOT NULL,
			currency TEXT NOT NULL,
			biz_type TEXT,
			biz_id INTEGER,
			amount INTEGER NOT NULL DEFAULT 0,
			total_amount INTEGER NOT NULL DEFAULT 0,
			attempts INTEGER NOT NULL DEFAULT 0,
			requested_at DATETIME NOT NULL,
			version INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME,
			updated_at DATETIME,
			deleted_at DATETIME
		)`,
		`CREATE TABLE test_refund_businesses (
			id INTEGER PRIMARY KEY,
			status TEXT NOT NULL
		)`,
	}
	for _, statement := range schema {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AutoMigrate(&Callback{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE UNIQUE INDEX uk_test_refund_callbacks_event ON refund_callbacks(provider,provider_event_id)").Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func insertSettlementRow(t *testing.T, db *gorm.DB, refundID, paymentID uint64, bizType string, bizID uint64, status string, version uint32) {
	t.Helper()
	if err := db.Exec(
		"INSERT INTO refunds (id,after_sale_id,order_id,payment_id,refund_no,provider,status,currency,biz_type,biz_id,amount,total_amount,requested_at,version) VALUES (?,NULL,NULL,?,?,'wechat',?,'CNY',?,?,100,1000,?,?)",
		refundID, paymentID, fmt.Sprintf("RF%d", refundID), status, bizType, bizID, time.Now(), version,
	).Error; err != nil {
		t.Fatal(err)
	}
}

func assertPanics(t *testing.T, action func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	action()
}
