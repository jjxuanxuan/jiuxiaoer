package deliveryreturn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	mysqlinfra "jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/aftersale"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/deliveryincident"
	refundmodule "jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type closureRefundProvider struct{ state refundmodule.State }

func (p *closureRefundProvider) Code() string { return "wechat" }
func (p *closureRefundProvider) Refund(context.Context, refundmodule.Input) (refundmodule.State, error) {
	return p.state, nil
}
func (p *closureRefundProvider) QueryRefund(context.Context, string) (refundmodule.State, error) {
	return p.state, nil
}
func (p *closureRefundProvider) ParseRefundCallback(_ context.Context, request *http.Request) (refundmodule.CallbackEvent, error) {
	return refundmodule.CallbackEvent{EventID: request.Header.Get("X-Event-ID"), MchID: "local-mch", State: p.state}, nil
}

type closureFixture struct {
	orderID, paymentID, deliveryID, shopID, riderID, customerID, merchantID uint64
	merchantUserID, adminID, restockProductID, restockShopProductID         uint64
	paymentNo                                                               string
}

func TestDeliveryReturnRequestMySQLAcceptance(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run delivery return MySQL acceptance")
	}
	cfg := config.Load()
	db, err := mysqlinfra.Open(context.Background(), cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { _ = tx.Rollback().Error })

	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	ids := snowflake.New(995)
	deliveryID, orderID, shopID, riderID := ids.Next(), ids.Next(), ids.Next(), ids.Next()
	pickedUpAt := time.Now().UTC().Add(-10 * time.Minute)
	if err := tx.Exec(`INSERT INTO delivery_orders
		(id,order_id,shop_id,rider_id,status,assignment_version,picked_up_at,started_at)
		VALUES (?,?,?,?,?,?,?,?)`, deliveryID, orderID, shopID, riderID, "delivering", 3, pickedUpAt, pickedUpAt).Error; err != nil {
		t.Fatal(err)
	}
	cfg.DeliveryReturn.Enabled = true
	cfg.DeliveryReturn.RiderWriteEnabled = true
	cfg.DeliveryReturn.RiderAllowlist = []string{idString(riderID)}
	cfg.DeliveryReturn.RiderRatePer10Minutes = 100
	service := NewService(cfg, tx, redisClient, ids)
	claims := &auth.Claims{AccountType: "rider", RiderID: idString(riderID), Permissions: []string{"delivery_return:create", "delivery_return:view_own"}}
	request := CreateReq{ReasonCode: ReasonCustomerRefused, Note: "customer refused an intact order", ExpectedDeliveryVersion: 3}

	created, err := service.Create(context.Background(), claims, "POST", "/api/v1/delivery/orders/:id/returns", "delivery-return-create-001", idString(deliveryID), request)
	if err != nil || created.Status != StatusRequested || created.RefundStatus != "not_authorized" || created.InventoryStatus != "not_applicable" {
		t.Fatalf("create request failed: dto=%+v err=%v", created, err)
	}
	assertRequestFacts(t, tx, created, deliveryID)

	replayed, err := service.Create(context.Background(), claims, "POST", "/api/v1/delivery/orders/:id/returns", "delivery-return-create-001", idString(deliveryID), request)
	if err != nil || replayed.ID != created.ID || replayed.Deduplicated {
		t.Fatalf("same-key replay changed the response: dto=%+v err=%v", replayed, err)
	}
	deduplicated, err := service.Create(context.Background(), claims, "POST", "/api/v1/delivery/orders/:id/returns", "delivery-return-create-002", idString(deliveryID), request)
	if err != nil || deduplicated.ID != created.ID || !deduplicated.Deduplicated {
		t.Fatalf("business deduplication failed: dto=%+v err=%v", deduplicated, err)
	}
	changed := request
	changed.Note = "changed payload"
	_, err = service.Create(context.Background(), claims, "POST", "/api/v1/delivery/orders/:id/returns", "delivery-return-create-001", idString(deliveryID), changed)
	assertIntegrationProblemCode(t, err, "IDEMPOTENCY_KEY_REUSED")

	detail, err := service.RiderDetail(context.Background(), claims, created.ID)
	if err != nil || detail.ID != created.ID || len(detail.History) != 1 || detail.History[0].Action != "request" {
		t.Fatalf("rider detail failed: dto=%+v err=%v", detail, err)
	}
	other := &auth.Claims{AccountType: "rider", RiderID: idString(riderID + 1), Permissions: []string{"delivery_return:view_own"}}
	_, err = service.RiderDetail(context.Background(), other, created.ID)
	assertIntegrationProblemCode(t, err, "DELIVERY_RETURN_NOT_FOUND")

	active, err := service.HasActiveLocked(context.Background(), tx, deliveryID)
	if err != nil || !active {
		t.Fatalf("active completion guard failed: active=%v err=%v", active, err)
	}
}

func TestDeliveryReturnFullClosureMySQLAcceptance(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run delivery return closure acceptance")
	}
	cfg := config.Load()
	db, err := mysqlinfra.Open(context.Background(), cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { _ = tx.Rollback().Error })

	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	ids := snowflake.New(989)
	cfg.AfterSale.Enabled = true
	cfg.AfterSale.RefundExecutionEnabled = true
	cfg.DeliveryReturn.Enabled = true
	cfg.DeliveryReturn.RiderWriteEnabled = true
	cfg.DeliveryReturn.ApprovalEnabled = true
	cfg.DeliveryReturn.ReceiptEnabled = true
	cfg.DeliveryReturn.SystemAfterSaleEnabled = true
	cfg.DeliveryReturn.SLAWorkerEnabled = true
	cfg.DeliveryReturn.RiderRatePer10Minutes = 100
	cfg.DeliveryReturn.ReceiptReminderAfter = 2 * time.Hour
	cfg.DeliveryReturn.ReceiptDeadlineAfter = 24 * time.Hour
	cfg.DeliveryReturn.SLAWorkerBatchSize = 100
	cfg.WeChat.PayMockEnabled = true

	for _, flow := range []struct {
		name        string
		refundFirst bool
	}{{"ACC-DR-013-refund-before-receipt", true}, {"ACC-DR-014-receipt-before-refund", false}} {
		t.Run(flow.name, func(t *testing.T) {
			fx := insertClosureFixture(t, tx, ids)
			flowCfg := cfg
			flowCfg.DeliveryReturn.RiderAllowlist = []string{idString(fx.riderID)}
			flowCfg.DeliveryReturn.ShopAllowlist = []string{idString(fx.shopID)}
			afterSales := aftersale.NewService(flowCfg, tx, ids)
			returns := NewService(flowCfg, tx, redisClient, ids).WithAfterSale(afterSales)
			rider, admin, store := closureClaims(fx)
			keyPrefix := flow.name + "-"

			created, err := returns.Create(context.Background(), rider, "POST", "/api/v1/delivery/orders/:id/returns", keyPrefix+"create", idString(fx.deliveryID), CreateReq{
				ReasonCode: ReasonCustomerRefused, Note: "customer refused the completed pickup", ExpectedDeliveryVersion: 3,
			})
			if err != nil || created.Status != StatusRequested || created.AfterSaleID != "" {
				t.Fatalf("ACC-DR-001 create: dto=%+v err=%v", created, err)
			}
			approved, err := returns.Approve(context.Background(), admin, "POST", "/api/v1/admin/delivery-returns/:id/approve", keyPrefix+"approve", created.ID,
				ApproveReq{ExpectedVersion: 1, DecisionNote: "operations verified failed delivery"})
			if err != nil || approved.Status != StatusReturning || approved.AfterSaleID == "" || approved.RefundStatus != "processing" || len(approved.Items) != 2 {
				t.Fatalf("ACC-DR-004 approve: dto=%+v err=%v", approved, err)
			}
			assertAtomicApproval(t, tx, created.ID, fx, approved)
			customerView, err := afterSales.DetailCustomer(context.Background(), &auth.Claims{AccountType: "customer", CustomerID: idString(fx.customerID)}, approved.AfterSaleID)
			if err != nil || customerView.SourceType != "delivery_return" || customerView.SourceID != created.ID || customerView.InitiatorType != "system" {
				t.Fatalf("ACC-DR-012 customer after-sale: dto=%+v err=%v", customerView, err)
			}

			beforeStock := stockAvailable(t, tx, fx.restockShopProductID)
			arrived, err := returns.Arrive(context.Background(), rider, "POST", "/api/v1/delivery/returns/:id/arrive", keyPrefix+"arrive", created.ID, ArriveReq{ExpectedVersion: 2})
			if err != nil || arrived.Status != StatusArrived || arrived.HandoffCode == "" {
				t.Fatalf("ACC-DR-007 arrive: dto=%+v err=%v", arrived, err)
			}
			var storedHash string
			if err := tx.Table("delivery_returns").Select("handoff_code_hash").Where("id=?", mustReturnID(t, created.ID)).Scan(&storedHash).Error; err != nil || storedHash == "" || storedHash == arrived.HandoffCode {
				t.Fatalf("handoff plaintext leak/hash missing: hash=%q code=%q err=%v", storedHash, arrived.HandoffCode, err)
			}
			if got := stockAvailable(t, tx, fx.restockShopProductID); got != beforeStock {
				t.Fatalf("arrive changed inventory: before=%d after=%d", beforeStock, got)
			}
			replayedArrival, err := returns.Arrive(context.Background(), rider, "POST", "/api/v1/delivery/returns/:id/arrive", keyPrefix+"arrive", created.ID, ArriveReq{ExpectedVersion: 2})
			if err != nil || replayedArrival.HandoffCode != arrived.HandoffCode {
				t.Fatalf("arrive idempotency lost one-time code: dto=%+v err=%v", replayedArrival, err)
			}

			refundRow := closureRefund(t, tx, approved.AfterSaleID)
			provider := &closureRefundProvider{state: refundmodule.State{
				ProviderRefundID: "wx-" + refundRow.RefundNo, RefundNo: refundRow.RefundNo, PaymentNo: fx.paymentNo,
				Status: "SUCCESS", Currency: refundRow.Currency, Amount: refundRow.Amount, TotalAmount: refundRow.TotalAmount,
			}}
			refunds := refundmodule.NewService(flowCfg, tx, ids, provider).WithDeliveryReturnClosure(returns)
			callback := func() {
				t.Helper()
				request, _ := http.NewRequest(http.MethodPost, "/api/v1/refunds/wechat/callback", nil)
				request.Header.Set("X-Event-ID", keyPrefix+"refund-success")
				if err := refunds.ProcessCallback(context.Background(), "wechat", request, []byte(`{"status":"SUCCESS"}`)); err != nil {
					t.Fatalf("refund callback: %v", err)
				}
				for index := 0; index < 9; index++ {
					request, _ = http.NewRequest(http.MethodPost, "/api/v1/refunds/wechat/callback", nil)
					request.Header.Set("X-Event-ID", keyPrefix+"refund-success")
					if err := refunds.ProcessCallback(context.Background(), "wechat", request, []byte(`{"status":"SUCCESS"}`)); err != nil {
						t.Fatalf("refund callback replay %d: %v", index, err)
					}
				}
			}
			receiveReq := closureReceiveRequest(arrived, fx)
			var firstReceipt DTO
			if flow.refundFirst {
				callback()
				middle, detailErr := returns.AdminDetail(context.Background(), admin, created.ID)
				if detailErr != nil || middle.Status != StatusArrived || middle.RefundStatus != "succeeded" {
					t.Fatalf("refund-first intermediate: dto=%+v err=%v", middle, detailErr)
				}
				firstReceipt, err = returns.Receive(context.Background(), store, "POST", "/api/v1/store/delivery-returns/:id/receive", keyPrefix+"receive", created.ID, receiveReq)
				if err != nil || firstReceipt.Status != StatusClosed {
					t.Fatalf("refund-first receive: dto=%+v err=%v", firstReceipt, err)
				}
			} else {
				firstReceipt, err = returns.Receive(context.Background(), store, "POST", "/api/v1/store/delivery-returns/:id/receive", keyPrefix+"receive", created.ID, receiveReq)
				if err != nil || firstReceipt.Status != StatusReceived || firstReceipt.RefundStatus != "processing" {
					t.Fatalf("receipt-first intermediate: dto=%+v err=%v", firstReceipt, err)
				}
				callback()
			}

			replayedReceipt, err := returns.Receive(context.Background(), store, "POST", "/api/v1/store/delivery-returns/:id/receive", keyPrefix+"receive", created.ID, receiveReq)
			if err != nil || replayedReceipt.Status != firstReceipt.Status {
				t.Fatalf("ACC-DR-011 same-key receipt replay: dto=%+v err=%v", replayedReceipt, err)
			}
			if _, err := returns.Receive(context.Background(), store, "POST", "/api/v1/store/delivery-returns/:id/receive", keyPrefix+"receive-again", created.ID, receiveReq); err == nil {
				t.Fatal("ACC-DR-011 different-key duplicate receipt was accepted")
			}
			final, err := returns.AdminDetail(context.Background(), admin, created.ID)
			if err != nil || final.Status != StatusClosed || final.RefundStatus != "succeeded" || final.LogisticsStatus != StatusReceived {
				t.Fatalf("final closure: dto=%+v err=%v", final, err)
			}
			if got := stockAvailable(t, tx, fx.restockShopProductID); got != beforeStock+1 {
				t.Fatalf("ACC-DR-008/011 inventory mismatch: before=%d after=%d", beforeStock, got)
			}
			assertClosureExactlyOnce(t, tx, created.ID, refundRow.ID)
		})
	}

	t.Run("ACC-DR-016-receipt-then-refund-failure-retry-closes", func(t *testing.T) {
		fx := insertClosureFixture(t, tx, ids)
		flowCfg := cfg
		flowCfg.DeliveryReturn.RiderAllowlist = []string{idString(fx.riderID)}
		flowCfg.DeliveryReturn.ShopAllowlist = []string{idString(fx.shopID)}
		returns := NewService(flowCfg, tx, redisClient, ids).WithAfterSale(aftersale.NewService(flowCfg, tx, ids))
		rider, admin, store := closureClaims(fx)
		prefix := "retry-" + idString(fx.orderID) + "-"
		created, err := returns.Create(context.Background(), rider, "POST", "/api/v1/delivery/orders/:id/returns", prefix+"create", idString(fx.deliveryID),
			CreateReq{ReasonCode: ReasonCustomerRefused, Note: "refund retry fixture", ExpectedDeliveryVersion: 3})
		if err != nil {
			t.Fatal(err)
		}
		approved, err := returns.Approve(context.Background(), admin, "POST", "/api/v1/admin/delivery-returns/:id/approve", prefix+"approve", created.ID,
			ApproveReq{ExpectedVersion: 1, DecisionNote: "approve retry fixture"})
		if err != nil {
			t.Fatal(err)
		}
		arrived, err := returns.Arrive(context.Background(), rider, "POST", "/api/v1/delivery/returns/:id/arrive", prefix+"arrive", created.ID, ArriveReq{ExpectedVersion: 2})
		if err != nil {
			t.Fatal(err)
		}
		before := stockAvailable(t, tx, fx.restockShopProductID)
		received, err := returns.Receive(context.Background(), store, "POST", "/api/v1/store/delivery-returns/:id/receive", prefix+"receive", created.ID, closureReceiveRequest(arrived, fx))
		if err != nil || received.Status != StatusReceived {
			t.Fatalf("receipt before failed refund: dto=%+v err=%v", received, err)
		}
		refundRow := closureRefund(t, tx, approved.AfterSaleID)
		provider := &closureRefundProvider{state: refundmodule.State{
			ProviderRefundID: "wx-" + refundRow.RefundNo, RefundNo: refundRow.RefundNo, PaymentNo: fx.paymentNo,
			Status: "CLOSED", Currency: refundRow.Currency, Amount: refundRow.Amount, TotalAmount: refundRow.TotalAmount,
		}}
		refunds := refundmodule.NewService(flowCfg, tx, ids, provider).WithDeliveryReturnClosure(returns)
		request, _ := http.NewRequest(http.MethodPost, "/api/v1/refunds/wechat/callback", nil)
		request.Header.Set("X-Event-ID", prefix+"failed")
		if err := refunds.ProcessCallback(context.Background(), "wechat", request, []byte(`{"status":"CLOSED"}`)); err != nil {
			t.Fatal(err)
		}
		middle, err := returns.AdminDetail(context.Background(), admin, created.ID)
		if err != nil || middle.Status != StatusException || middle.RefundStatus != "failed" || stockAvailable(t, tx, fx.restockShopProductID) != before+1 {
			t.Fatalf("refund failure must preserve receipt: dto=%+v err=%v", middle, err)
		}
		refundAdmin := &auth.Claims{AccountType: "admin", AdminUserID: idString(fx.adminID), Permissions: []string{"refund:retry"}}
		if err := refunds.Retry(context.Background(), refundAdmin, "POST", "/api/v1/admin/refunds/:id/retry", prefix+"manual-retry", refundRow.RefundNo); err != nil {
			t.Fatalf("retry failed refund: %v", err)
		}
		provider.state.Status = "SUCCESS"
		request, _ = http.NewRequest(http.MethodPost, "/api/v1/refunds/wechat/callback", nil)
		request.Header.Set("X-Event-ID", prefix+"succeeded")
		if err := refunds.ProcessCallback(context.Background(), "wechat", request, []byte(`{"status":"SUCCESS"}`)); err != nil {
			t.Fatal(err)
		}
		final, err := returns.AdminDetail(context.Background(), admin, created.ID)
		if err != nil || final.Status != StatusClosed || final.RefundStatus != "succeeded" {
			t.Fatalf("retry did not close return: dto=%+v err=%v", final, err)
		}
		var refundsCount, receipts, stockRows, closeEvents int64
		tx.Table("refunds").Where("after_sale_id=?", mustReturnID(t, approved.AfterSaleID)).Count(&refundsCount)
		tx.Table("return_receipts").Where("after_sale_id=?", mustReturnID(t, approved.AfterSaleID)).Count(&receipts)
		tx.Table("stock_records").Where("source_type='delivery_return' AND source_id=?", mustReturnID(t, created.ID)).Count(&stockRows)
		tx.Table("outbox_events").Where("aggregate_type='delivery_return' AND aggregate_id=? AND event_type='delivery.return_closed'", mustReturnID(t, created.ID)).Count(&closeEvents)
		if refundsCount != 1 || receipts != 1 || stockRows != 1 || closeEvents != 1 || stockAvailable(t, tx, fx.restockShopProductID) != before+1 {
			t.Fatalf("retry exactly-once refunds=%d receipts=%d stock=%d close_events=%d", refundsCount, receipts, stockRows, closeEvents)
		}
	})
}

func TestDeliveryReturnIncidentAndSLAMySQLAcceptance(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run delivery return incident/SLA acceptance")
	}
	cfg := config.Load()
	db, err := mysqlinfra.Open(context.Background(), cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { _ = tx.Rollback().Error })
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	ids := snowflake.New(988)
	cfg.AfterSale.Enabled, cfg.AfterSale.RefundExecutionEnabled = true, true
	cfg.DeliveryIncident.Enabled = true
	cfg.DeliveryReturn.Enabled, cfg.DeliveryReturn.RiderWriteEnabled = true, true
	cfg.DeliveryReturn.ApprovalEnabled, cfg.DeliveryReturn.ReceiptEnabled, cfg.DeliveryReturn.SystemAfterSaleEnabled = true, true, true
	cfg.DeliveryReturn.SLAWorkerEnabled, cfg.DeliveryReturn.SLAWorkerBatchSize = true, 100
	cfg.DeliveryReturn.ReceiptReminderAfter, cfg.DeliveryReturn.ReceiptDeadlineAfter = 2*time.Hour, 24*time.Hour

	t.Run("ACC-DR-005-incident-return-required-is-atomic", func(t *testing.T) {
		fx := insertClosureFixture(t, tx, ids)
		caseCfg := cfg
		caseCfg.DeliveryReturn.RiderAllowlist = []string{idString(fx.riderID)}
		caseCfg.DeliveryReturn.ShopAllowlist = []string{idString(fx.shopID)}
		returns := NewService(caseCfg, tx, redisClient, ids).WithAfterSale(aftersale.NewService(caseCfg, tx, ids))
		incidentID := ids.Next()
		now := time.Now().UTC()
		if err := tx.Exec(`INSERT INTO delivery_incidents
			(id,incident_no,delivery_order_id,order_id,shop_id,rider_id,type,stage,status,priority,description,delivery_status_snapshot,assignment_version_snapshot,reported_at,version,created_at,updated_at)
			VALUES (?,?,?,?,?,?,'customer_refused','delivery','open','high',?,'delivering',3,?,1,?,?)`,
			incidentID, "DI"+idString(incidentID), fx.deliveryID, fx.orderID, fx.shopID, fx.riderID, "customer refused after pickup", now, now, now).Error; err != nil {
			t.Fatal(err)
		}
		incidents := deliveryincident.NewService(caseCfg, tx, ids, nil).WithReturnOrchestrator(returns)
		admin := &auth.Claims{AccountType: "admin", AdminUserID: idString(fx.adminID), Permissions: []string{"delivery_incident:resolve", "delivery_return:approve"}}
		resolved, err := incidents.Resolve(context.Background(), admin, "POST", "/api/v1/admin/delivery-incidents/:id/resolve", "incident-return-required", idString(incidentID),
			deliveryincident.ResolveReq{ExpectedVersion: 1, ResolutionCode: "return_required", ResolutionNote: "return required after operations review"})
		if err != nil || resolved.Status != deliveryincident.StatusResolved || resolved.DeliveryReturnID == "" {
			t.Fatalf("incident orchestration: dto=%+v err=%v", resolved, err)
		}
		var row Return
		if err := tx.Where("incident_id=?", incidentID).Take(&row).Error; err != nil || row.Status != StatusReturning || row.AfterSaleID == nil {
			t.Fatalf("incident-linked return: row=%+v err=%v", row, err)
		}
		var receipts int64
		tx.Table("return_receipts").Where("after_sale_id=?", *row.AfterSaleID).Count(&receipts)
		if receipts != 0 {
			t.Fatalf("return_required fabricated receipt: %d", receipts)
		}
		assertIntegrationProblemCode(t, returns.ValidateIncidentResolutionWithTx(context.Background(), tx, incidentID, "returned_to_store"), "INVALID_RETURN_STATE")
	})

	t.Run("ACC-DR-015-SLA-never-fabricates-receipt-or-stock", func(t *testing.T) {
		fx := insertClosureFixture(t, tx, ids)
		caseCfg := cfg
		caseCfg.DeliveryReturn.RiderAllowlist = []string{idString(fx.riderID)}
		caseCfg.DeliveryReturn.ShopAllowlist = []string{idString(fx.shopID)}
		afterSales := aftersale.NewService(caseCfg, tx, ids)
		returns := NewService(caseCfg, tx, redisClient, ids).WithAfterSale(afterSales)
		base := time.Now().UTC().Add(-48 * time.Hour)
		returns.now = func() time.Time { return base }
		rider, admin, _ := closureClaims(fx)
		created, err := returns.Create(context.Background(), rider, "POST", "/api/v1/delivery/orders/:id/returns", "sla-create-"+idString(fx.orderID), idString(fx.deliveryID),
			CreateReq{ReasonCode: ReasonCustomerUnreachable, Note: "customer unreachable", ExpectedDeliveryVersion: 3})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := returns.Approve(context.Background(), admin, "POST", "/api/v1/admin/delivery-returns/:id/approve", "sla-approve-"+idString(fx.orderID), created.ID,
			ApproveReq{ExpectedVersion: 1, DecisionNote: "approve SLA fixture"}); err != nil {
			t.Fatal(err)
		}
		before := stockAvailable(t, tx, fx.restockShopProductID)
		worker := NewSLAWorker(returns, slog.New(slog.NewTextHandler(io.Discard, nil)))
		returns.now = func() time.Time { return base.Add(3 * time.Hour) }
		worker.RunOnce(context.Background())
		returns.now = func() time.Time { return base.Add(25 * time.Hour) }
		worker.RunOnce(context.Background())
		worker.RunOnce(context.Background())
		var row Return
		if err := tx.First(&row, mustReturnID(t, created.ID)).Error; err != nil || row.Status != StatusException {
			t.Fatalf("SLA status: row=%+v err=%v", row, err)
		}
		var reminders, breaches, receipts, stockRows int64
		tx.Model(&History{}).Where("delivery_return_id=? AND action='sla_reminder'", row.ID).Count(&reminders)
		tx.Model(&History{}).Where("delivery_return_id=? AND action='sla_breach'", row.ID).Count(&breaches)
		tx.Table("return_receipts").Where("after_sale_id=?", *row.AfterSaleID).Count(&receipts)
		tx.Table("stock_records").Where("source_type='delivery_return' AND source_id=?", row.ID).Count(&stockRows)
		if reminders != 1 || breaches != 1 || receipts != 0 || stockRows != 0 || stockAvailable(t, tx, fx.restockShopProductID) != before {
			t.Fatalf("SLA invariant reminders=%d breaches=%d receipts=%d stock_rows=%d", reminders, breaches, receipts, stockRows)
		}
		metricSamples := collectMetrics(tx, returns.now().UTC())
		if sampleValue(metricSamples, "jxe_delivery_return_sla_breached") < 1 || sampleValue(metricSamples, "jxe_delivery_return_requested_customer_notifications") != 0 {
			t.Fatalf("SLA/notification metrics are inconsistent: %+v", metricSamples)
		}
	})
}

func TestDeliveryReturnConflictAndRejectionMySQLAcceptance(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run delivery return conflict acceptance")
	}
	cfg := config.Load()
	db, err := mysqlinfra.Open(context.Background(), cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { _ = tx.Rollback().Error })
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	ids := snowflake.New(987)
	cfg.AfterSale.Enabled, cfg.AfterSale.RefundExecutionEnabled = true, true
	cfg.DeliveryReturn.Enabled, cfg.DeliveryReturn.RiderWriteEnabled = true, true
	cfg.DeliveryReturn.ApprovalEnabled, cfg.DeliveryReturn.ReceiptEnabled, cfg.DeliveryReturn.SystemAfterSaleEnabled = true, true, true
	cfg.DeliveryReturn.RiderRatePer10Minutes = 100

	t.Run("ACC-DR-006-existing-customer-claim-goes-to-disputed", func(t *testing.T) {
		fx := insertClosureFixture(t, tx, ids)
		caseCfg := cfg
		caseCfg.DeliveryReturn.RiderAllowlist = []string{idString(fx.riderID)}
		caseCfg.DeliveryReturn.ShopAllowlist = []string{idString(fx.shopID)}
		returns := NewService(caseCfg, tx, redisClient, ids).WithAfterSale(aftersale.NewService(caseCfg, tx, ids))
		rider, admin, _ := closureClaims(fx)
		created, err := returns.Create(context.Background(), rider, "POST", "/api/v1/delivery/orders/:id/returns", "conflict-create-"+idString(fx.orderID), idString(fx.deliveryID),
			CreateReq{ReasonCode: ReasonCustomerRefused, Note: "conflict fixture", ExpectedDeliveryVersion: 3})
		if err != nil {
			t.Fatal(err)
		}
		customerAfterSaleID := ids.Next()
		mustClosureExec(t, tx, `INSERT INTO after_sales
			(id,after_sale_no,order_id,customer_id,merchant_id,shop_id,type,requested_resolution,status,requested_amount,description,submitted_at)
			VALUES (?,?,?,?,?,?,'other','refund_only','submitted',100,?,?)`, customerAfterSaleID, "AS-CUSTOMER-"+idString(customerAfterSaleID),
			fx.orderID, fx.customerID, fx.merchantID, fx.shopID, "customer claim already in progress", time.Now().UTC())
		disputed, err := returns.Approve(context.Background(), admin, "POST", "/api/v1/admin/delivery-returns/:id/approve", "conflict-approve-"+idString(fx.orderID), created.ID,
			ApproveReq{ExpectedVersion: 1, DecisionNote: "operations review with conflict"})
		assertIntegrationProblemCode(t, err, "MANUAL_REVIEW_REQUIRED")
		if disputed.Status != StatusDisputed || disputed.AfterSaleID != "" {
			t.Fatalf("manual review projection: %+v", disputed)
		}
		var systemAfterSales, refunds, deniedAudits int64
		tx.Table("after_sales").Where("source_type='delivery_return' AND source_id=?", mustReturnID(t, created.ID)).Count(&systemAfterSales)
		tx.Table("refunds").Where("order_id=?", fx.orderID).Count(&refunds)
		tx.Table("audit_logs").Where("resource_type='delivery_return' AND resource_id=? AND actor_type='admin' AND actor_id=? AND action='delivery_return.approve_denied'", mustReturnID(t, created.ID), fx.adminID).Count(&deniedAudits)
		if systemAfterSales != 0 || refunds != 0 || deniedAudits != 1 {
			t.Fatalf("manual review invariant system_after_sales=%d refunds=%d denied_audits=%d", systemAfterSales, refunds, deniedAudits)
		}
	})

	t.Run("ACC-DR-009-010-invalid-handoff-and-quantity-never-restock", func(t *testing.T) {
		fx := insertClosureFixture(t, tx, ids)
		caseCfg := cfg
		caseCfg.DeliveryReturn.RiderAllowlist = []string{idString(fx.riderID)}
		caseCfg.DeliveryReturn.ShopAllowlist = []string{idString(fx.shopID)}
		returns := NewService(caseCfg, tx, redisClient, ids).WithAfterSale(aftersale.NewService(caseCfg, tx, ids))
		rider, admin, store := closureClaims(fx)
		prefix := "reject-" + idString(fx.orderID) + "-"
		created, err := returns.Create(context.Background(), rider, "POST", "/api/v1/delivery/orders/:id/returns", prefix+"create", idString(fx.deliveryID),
			CreateReq{ReasonCode: ReasonCustomerRefused, Note: "rejection fixture", ExpectedDeliveryVersion: 3})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := returns.Approve(context.Background(), admin, "POST", "/api/v1/admin/delivery-returns/:id/approve", prefix+"approve", created.ID,
			ApproveReq{ExpectedVersion: 1, DecisionNote: "approve rejection fixture"}); err != nil {
			t.Fatal(err)
		}
		arrived, err := returns.Arrive(context.Background(), rider, "POST", "/api/v1/delivery/returns/:id/arrive", prefix+"arrive", created.ID, ArriveReq{ExpectedVersion: 2})
		if err != nil {
			t.Fatal(err)
		}
		before := stockAvailable(t, tx, fx.restockShopProductID)
		wrong := closureReceiveRequest(arrived, fx)
		wrong.HandoffCode = "WRONG999"
		_, err = returns.Receive(context.Background(), store, "POST", "/api/v1/store/delivery-returns/:id/receive", prefix+"wrong-code", created.ID, wrong)
		assertIntegrationProblemCode(t, err, "HANDOFF_CODE_INVALID")
		_, err = returns.Receive(context.Background(), store, "POST", "/api/v1/store/delivery-returns/:id/receive", prefix+"wrong-code", created.ID, wrong)
		assertIntegrationProblemCode(t, err, "HANDOFF_CODE_INVALID")
		var row Return
		if err := tx.First(&row, mustReturnID(t, created.ID)).Error; err != nil || row.HandoffFailedAttempts != 1 || row.Version != 4 {
			t.Fatalf("handoff rejection persistence: row=%+v err=%v", row, err)
		}
		quantityMismatch := closureReceiveRequest(arrived, fx)
		quantityMismatch.ExpectedVersion = 4
		quantityMismatch.Items[0].ReceivedQuantity = 0
		disputed, err := returns.Receive(context.Background(), store, "POST", "/api/v1/store/delivery-returns/:id/receive", prefix+"quantity", created.ID, quantityMismatch)
		assertIntegrationProblemCode(t, err, "MANUAL_REVIEW_REQUIRED")
		if disputed.Status != StatusDisputed {
			t.Fatalf("quantity mismatch did not dispute return: %+v", disputed)
		}
		var receipts, stockRows, handoffAudits, receiveAudits int64
		tx.Table("return_receipts").Where("after_sale_id=?", mustReturnID(t, disputed.AfterSaleID)).Count(&receipts)
		tx.Table("stock_records").Where("source_type='delivery_return' AND source_id=?", mustReturnID(t, created.ID)).Count(&stockRows)
		tx.Table("audit_logs").Where("resource_type='delivery_return' AND resource_id=? AND action='delivery_return.handoff_rejected'", mustReturnID(t, created.ID)).Count(&handoffAudits)
		tx.Table("audit_logs").Where("resource_type='delivery_return' AND resource_id=? AND action='delivery_return.receive_denied' AND actor_type='merchant' AND actor_id=?", mustReturnID(t, created.ID), fx.merchantUserID).Count(&receiveAudits)
		if receipts != 0 || stockRows != 0 || stockAvailable(t, tx, fx.restockShopProductID) != before || handoffAudits != 1 || receiveAudits < 2 {
			t.Fatalf("rejection invariant receipts=%d stock=%d handoff_audits=%d receive_audits=%d", receipts, stockRows, handoffAudits, receiveAudits)
		}
	})
}

func TestDeliveryReturnCallbackReceiptConcurrencyMySQLAcceptance(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run callback/receipt concurrency acceptance")
	}
	cfg := config.Load()
	db, err := mysqlinfra.Open(context.Background(), cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	ids := snowflake.New(986)
	cfg.AfterSale.Enabled, cfg.AfterSale.RefundExecutionEnabled = true, true
	cfg.DeliveryReturn.Enabled, cfg.DeliveryReturn.RiderWriteEnabled = true, true
	cfg.DeliveryReturn.ApprovalEnabled, cfg.DeliveryReturn.ReceiptEnabled, cfg.DeliveryReturn.SystemAfterSaleEnabled = true, true, true
	cfg.DeliveryReturn.RiderRatePer10Minutes = 100
	cfg.WeChat.PayMockEnabled = true

	for iteration := 0; iteration < 10; iteration++ {
		func() {
			fx := insertClosureFixture(t, db, ids)
			defer cleanupClosureFixture(t, db, fx)
			caseCfg := cfg
			caseCfg.DeliveryReturn.RiderAllowlist = []string{idString(fx.riderID)}
			caseCfg.DeliveryReturn.ShopAllowlist = []string{idString(fx.shopID)}
			returns := NewService(caseCfg, db, redisClient, ids).WithAfterSale(aftersale.NewService(caseCfg, db, ids))
			rider, admin, store := closureClaims(fx)
			prefix := fmt.Sprintf("concurrent-%d-%d-", iteration, fx.orderID)
			created, err := returns.Create(context.Background(), rider, "POST", "/api/v1/delivery/orders/:id/returns", prefix+"create", idString(fx.deliveryID),
				CreateReq{ReasonCode: ReasonCustomerRefused, Note: "callback receipt concurrency", ExpectedDeliveryVersion: 3})
			if err != nil {
				t.Fatal(err)
			}
			approved, err := returns.Approve(context.Background(), admin, "POST", "/api/v1/admin/delivery-returns/:id/approve", prefix+"approve", created.ID,
				ApproveReq{ExpectedVersion: 1, DecisionNote: "approve concurrency fixture"})
			if err != nil {
				t.Fatal(err)
			}
			arrived, err := returns.Arrive(context.Background(), rider, "POST", "/api/v1/delivery/returns/:id/arrive", prefix+"arrive", created.ID, ArriveReq{ExpectedVersion: 2})
			if err != nil {
				t.Fatal(err)
			}
			refundRow := closureRefund(t, db, approved.AfterSaleID)
			provider := &closureRefundProvider{state: refundmodule.State{
				ProviderRefundID: "wx-" + refundRow.RefundNo, RefundNo: refundRow.RefundNo, PaymentNo: fx.paymentNo,
				Status: "SUCCESS", Currency: refundRow.Currency, Amount: refundRow.Amount, TotalAmount: refundRow.TotalAmount,
			}}
			refunds := refundmodule.NewService(caseCfg, db, ids, provider).WithDeliveryReturnClosure(returns)
			start := make(chan struct{})
			errorsCh := make(chan error, 2)
			var wait sync.WaitGroup
			wait.Add(2)
			go func() {
				defer wait.Done()
				<-start
				_, receiveErr := returns.Receive(context.Background(), store, "POST", "/api/v1/store/delivery-returns/:id/receive", prefix+"receive", created.ID, closureReceiveRequest(arrived, fx))
				errorsCh <- receiveErr
			}()
			go func() {
				defer wait.Done()
				<-start
				request, _ := http.NewRequest(http.MethodPost, "/api/v1/refunds/wechat/callback", nil)
				request.Header.Set("X-Event-ID", prefix+"refund-success")
				errorsCh <- refunds.ProcessCallback(context.Background(), "wechat", request, []byte(`{"status":"SUCCESS"}`))
			}()
			close(start)
			wait.Wait()
			close(errorsCh)
			for concurrentErr := range errorsCh {
				if concurrentErr != nil {
					t.Fatalf("iteration %d callback/receipt concurrency: %v", iteration, concurrentErr)
				}
			}
			final, err := returns.AdminDetail(context.Background(), admin, created.ID)
			if err != nil || final.Status != StatusClosed || final.RefundStatus != "succeeded" {
				t.Fatalf("iteration %d final=%+v err=%v", iteration, final, err)
			}
			var receipts, stockRows, closeEvents int64
			db.Table("return_receipts").Where("after_sale_id=?", mustReturnID(t, approved.AfterSaleID)).Count(&receipts)
			db.Table("stock_records").Where("source_type='delivery_return' AND source_id=?", mustReturnID(t, created.ID)).Count(&stockRows)
			db.Table("outbox_events").Where("aggregate_type='delivery_return' AND aggregate_id=? AND event_type='delivery.return_closed'", mustReturnID(t, created.ID)).Count(&closeEvents)
			if receipts != 1 || stockRows != 1 || closeEvents != 1 {
				t.Fatalf("iteration %d receipts=%d stock=%d close_events=%d", iteration, receipts, stockRows, closeEvents)
			}
		}()
	}
}

func insertClosureFixture(t *testing.T, tx *gorm.DB, ids *snowflake.Generator) closureFixture {
	t.Helper()
	fx := closureFixture{
		orderID: ids.Next(), paymentID: ids.Next(), deliveryID: ids.Next(), shopID: ids.Next(), riderID: ids.Next(),
		customerID: ids.Next(), merchantID: ids.Next(), merchantUserID: ids.Next(), adminID: ids.Next(),
		restockProductID: ids.Next(), restockShopProductID: ids.Next(),
	}
	fx.paymentNo = "PAY-DR-" + idString(fx.paymentID)
	now := time.Now().UTC()
	mustClosureExec(t, tx, `INSERT INTO orders
		(id,order_no,customer_id,merchant_id,shop_id,status,pay_status,delivery_status,goods_amount,delivery_fee_amount,payable_amount,paid_amount,refunded_amount,after_sale_status)
		VALUES (?,?,?,?,?,'delivering','succeeded','delivering',1800,200,2000,2000,0,'none')`,
		fx.orderID, "ORDER-DR-"+idString(fx.orderID), fx.customerID, fx.merchantID, fx.shopID)
	mustClosureExec(t, tx, `INSERT INTO payments
		(id,payment_no,order_id,customer_id,channel,provider,status,amount,refunded_amount,currency,paid_at)
		VALUES (?,?,?,?,'miniapp','wechat','succeeded',2000,0,'CNY',?)`, fx.paymentID, fx.paymentNo, fx.orderID, fx.customerID, now)
	discardItemID, discardShopProductID, discardProductID := ids.Next(), ids.Next(), ids.Next()
	restockItemID := ids.Next()
	mustClosureExec(t, tx, `INSERT INTO order_items
		(id,order_id,shop_product_id,product_id,product_snapshot,quantity,sale_price_amount,total_amount)
		VALUES (?,?,?,?,?,2,600,1200)`, discardItemID, fx.orderID, discardShopProductID, discardProductID,
		`{"name":"prepared meal","return_policy":{"eligible":false,"policy_code":"food-discard","policy_version":"1"}}`)
	mustClosureExec(t, tx, `INSERT INTO order_items
		(id,order_id,shop_product_id,product_id,product_snapshot,quantity,sale_price_amount,total_amount)
		VALUES (?,?,?,?,?,1,600,600)`, restockItemID, fx.orderID, fx.restockShopProductID, fx.restockProductID,
		`{"name":"sealed bottle","return_policy":{"eligible":true,"policy_code":"sealed-goods","policy_version":"2"}}`)
	mustClosureExec(t, tx, `INSERT INTO product_stocks (id,shop_product_id,shop_id,product_id,available_qty,reserved_qty,version) VALUES (?,?,?,?,3,0,1)`,
		ids.Next(), discardShopProductID, fx.shopID, discardProductID)
	mustClosureExec(t, tx, `INSERT INTO product_stocks (id,shop_product_id,shop_id,product_id,available_qty,reserved_qty,version) VALUES (?,?,?,?,7,0,1)`,
		ids.Next(), fx.restockShopProductID, fx.shopID, fx.restockProductID)
	pickedUp := now.Add(-10 * time.Minute)
	mustClosureExec(t, tx, `INSERT INTO delivery_orders
		(id,order_id,shop_id,rider_id,status,assignment_version,picked_up_at,started_at)
		VALUES (?,?,?,?, 'delivering',3,?,?)`, fx.deliveryID, fx.orderID, fx.shopID, fx.riderID, pickedUp, pickedUp)
	return fx
}

func closureClaims(fx closureFixture) (*auth.Claims, *auth.Claims, *auth.Claims) {
	rider := &auth.Claims{AccountType: "rider", RiderID: idString(fx.riderID), Permissions: []string{"delivery_return:create", "delivery_return:view_own", "delivery_return:arrive"}}
	admin := &auth.Claims{AccountType: "admin", AdminUserID: idString(fx.adminID), Permissions: []string{"delivery_return:approve", "delivery_return:view_all", "delivery_return:list_all"}}
	store := &auth.Claims{AccountType: "merchant", MerchantUserID: idString(fx.merchantUserID), MerchantID: idString(fx.merchantID), AuthorizedShopIDs: []string{idString(fx.shopID)}, Permissions: []string{"delivery_return:receive_shop", "delivery_return:view_shop", "delivery_return:list_shop"}}
	return rider, admin, store
}

func closureReceiveRequest(arrived DTO, fx closureFixture) ReceiveReq {
	items := make([]ReceiveItemReq, 0, len(arrived.Items))
	for _, item := range arrived.Items {
		disposition := "discard"
		if item.ProductID == idString(fx.restockProductID) {
			disposition = "restock"
		}
		items = append(items, ReceiveItemReq{AfterSaleItemID: item.AfterSaleItemID, ReceivedQuantity: item.ExpectedQuantity, Disposition: disposition})
	}
	return ReceiveReq{ExpectedVersion: 3, HandoffCode: arrived.HandoffCode, Items: items}
}

func closureRefund(t *testing.T, tx *gorm.DB, afterSaleIDRaw string) refundmodule.Row {
	t.Helper()
	afterSaleID := mustReturnID(t, afterSaleIDRaw)
	var row refundmodule.Row
	if err := tx.Where("after_sale_id=?", afterSaleID).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func assertAtomicApproval(t *testing.T, tx *gorm.DB, returnIDRaw string, fx closureFixture, approved DTO) {
	t.Helper()
	returnID, afterSaleID := mustReturnID(t, returnIDRaw), mustReturnID(t, approved.AfterSaleID)
	var afterSales, refunds, refundItems int64
	tx.Table("after_sales").Where("id=? AND source_type='delivery_return' AND source_id=? AND initiator_type='system'", afterSaleID, returnID).Count(&afterSales)
	tx.Table("refunds").Where("after_sale_id=? AND amount=2000", afterSaleID).Count(&refunds)
	tx.Table("refund_items").Where("refund_id IN (SELECT id FROM refunds WHERE after_sale_id=?)", afterSaleID).Count(&refundItems)
	var deliveryStatus, orderStatus string
	tx.Table("delivery_orders").Select("status").Where("id=?", fx.deliveryID).Scan(&deliveryStatus)
	tx.Table("orders").Select("status").Where("id=?", fx.orderID).Scan(&orderStatus)
	if afterSales != 1 || refunds != 1 || refundItems != 2 || deliveryStatus != "returning" || orderStatus != "refunding" {
		t.Fatalf("approval atomicity after_sales=%d refunds=%d refund_items=%d delivery=%s order=%s", afterSales, refunds, refundItems, deliveryStatus, orderStatus)
	}
}

func assertClosureExactlyOnce(t *testing.T, tx *gorm.DB, returnIDRaw string, refundID uint64) {
	t.Helper()
	returnID := mustReturnID(t, returnIDRaw)
	var closedHistory, closedEvents, receipts, stockRows, callbackRows, refundEvents int64
	tx.Model(&History{}).Where("delivery_return_id=? AND action='close'", returnID).Count(&closedHistory)
	tx.Table("outbox_events").Where("aggregate_type='delivery_return' AND aggregate_id=? AND event_type='delivery.return_closed'", returnID).Count(&closedEvents)
	tx.Table("return_receipts").Where("after_sale_id IN (SELECT after_sale_id FROM refunds WHERE id=?)", refundID).Count(&receipts)
	tx.Table("stock_records").Where("source_type='delivery_return' AND source_id=?", returnID).Count(&stockRows)
	tx.Table("refund_callbacks").Where("refund_id=?", refundID).Count(&callbackRows)
	tx.Table("outbox_events").Where("aggregate_type='refund' AND aggregate_id=? AND event_type='refund.succeeded'", refundID).Count(&refundEvents)
	if closedHistory != 1 || closedEvents != 1 || receipts != 1 || stockRows != 1 || callbackRows != 1 || refundEvents != 1 {
		t.Fatalf("exactly-once mismatch close_history=%d close_events=%d receipts=%d stock=%d callbacks=%d refund_events=%d", closedHistory, closedEvents, receipts, stockRows, callbackRows, refundEvents)
	}
}

func stockAvailable(t *testing.T, tx *gorm.DB, shopProductID uint64) int {
	t.Helper()
	var quantity int
	if err := tx.Table("product_stocks").Select("available_qty").Where("shop_product_id=?", shopProductID).Scan(&quantity).Error; err != nil {
		t.Fatal(err)
	}
	return quantity
}

func mustClosureExec(t *testing.T, tx *gorm.DB, query string, args ...any) {
	t.Helper()
	if err := tx.Exec(query, args...).Error; err != nil {
		t.Fatalf("fixture query failed: %v", err)
	}
}

func mustReturnID(t *testing.T, raw string) uint64 {
	t.Helper()
	id, err := parseID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func cleanupClosureFixture(t *testing.T, db *gorm.DB, fx closureFixture) {
	t.Helper()
	var returnIDs, afterSaleIDs, refundIDs, receiptIDs []uint64
	_ = db.Table("delivery_returns").Where("order_id=?", fx.orderID).Pluck("id", &returnIDs).Error
	_ = db.Table("after_sales").Where("order_id=?", fx.orderID).Pluck("id", &afterSaleIDs).Error
	_ = db.Table("refunds").Where("order_id=?", fx.orderID).Pluck("id", &refundIDs).Error
	if len(afterSaleIDs) > 0 {
		_ = db.Table("return_receipts").Where("after_sale_id IN ?", afterSaleIDs).Pluck("id", &receiptIDs).Error
	}
	statements := []struct {
		query string
		args  []any
	}{
		{"DELETE FROM return_receipt_items WHERE return_receipt_id IN ?", []any{receiptIDs}},
		{"DELETE FROM return_receipts WHERE after_sale_id IN ?", []any{afterSaleIDs}},
		{"DELETE FROM refund_callbacks WHERE refund_id IN ?", []any{refundIDs}},
		{"DELETE FROM refund_items WHERE refund_id IN ?", []any{refundIDs}},
		{"DELETE FROM refunds WHERE order_id=?", []any{fx.orderID}},
		{"DELETE FROM after_sale_history WHERE after_sale_id IN ?", []any{afterSaleIDs}},
		{"DELETE FROM after_sale_evidence WHERE after_sale_id IN ?", []any{afterSaleIDs}},
		{"DELETE FROM after_sale_items WHERE after_sale_id IN ?", []any{afterSaleIDs}},
		{"DELETE FROM after_sales WHERE order_id=?", []any{fx.orderID}},
		{"DELETE FROM stock_records WHERE source_type='delivery_return' AND source_id IN ?", []any{returnIDs}},
		{"DELETE FROM delivery_return_history WHERE delivery_return_id IN ?", []any{returnIDs}},
		{"DELETE FROM delivery_returns WHERE order_id=?", []any{fx.orderID}},
		{"DELETE FROM outbox_events WHERE (aggregate_type='delivery_return' AND aggregate_id IN ?) OR (aggregate_type='after_sale' AND aggregate_id IN ?) OR (aggregate_type='refund' AND aggregate_id IN ?)", []any{returnIDs, afterSaleIDs, refundIDs}},
		{"DELETE FROM audit_logs WHERE (resource_type='delivery_return' AND resource_id IN ?) OR (resource_type='after_sale' AND resource_id IN ?) OR (resource_type='refund' AND resource_id IN ?)", []any{returnIDs, afterSaleIDs, refundIDs}},
		{"DELETE FROM idempotency_keys WHERE actor_id IN ?", []any{[]uint64{fx.riderID, fx.adminID, fx.merchantUserID}}},
		{"DELETE FROM product_stocks WHERE shop_id=?", []any{fx.shopID}},
		{"DELETE FROM order_items WHERE order_id=?", []any{fx.orderID}},
		{"DELETE FROM delivery_orders WHERE id=?", []any{fx.deliveryID}},
		{"DELETE FROM payments WHERE id=?", []any{fx.paymentID}},
		{"DELETE FROM orders WHERE id=?", []any{fx.orderID}},
	}
	for _, statement := range statements {
		skip := false
		for _, arg := range statement.args {
			switch values := arg.(type) {
			case []uint64:
				if len(values) == 0 {
					skip = true
				}
			}
		}
		if skip {
			continue
		}
		if err := db.Exec(statement.query, statement.args...).Error; err != nil {
			t.Errorf("cleanup query failed: %v", err)
		}
	}
}

func sampleValue(samples []metrics.Sample, name string) float64 {
	for _, sample := range samples {
		if sample.Name == name {
			return sample.Value
		}
	}
	return 0
}

func assertRequestFacts(t *testing.T, tx *gorm.DB, created DTO, deliveryID uint64) {
	t.Helper()
	returnID, err := parseID(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	var histories, audits, events, afterSales, refunds int64
	tx.Model(&History{}).Where("delivery_return_id=?", returnID).Count(&histories)
	tx.Table("audit_logs").Where("resource_type='delivery_return' AND resource_id=?", returnID).Count(&audits)
	tx.Table("outbox_events").Where("aggregate_type='delivery_return' AND aggregate_id=? AND event_type='delivery.return_requested'", returnID).Count(&events)
	tx.Table("after_sales").Where("source_type='delivery_return' AND source_id=?", returnID).Count(&afterSales)
	tx.Table("refunds").Where("after_sale_id IN (SELECT id FROM after_sales WHERE source_type='delivery_return' AND source_id=?)", returnID).Count(&refunds)
	if histories != 1 || audits != 1 || events != 1 || afterSales != 0 || refunds != 0 {
		t.Fatalf("request fact invariant histories=%d audits=%d events=%d after_sales=%d refunds=%d", histories, audits, events, afterSales, refunds)
	}
	var status string
	if err := tx.Table("delivery_orders").Select("status").Where("id=?", deliveryID).Scan(&status).Error; err != nil || status != "delivering" {
		t.Fatalf("request changed delivery status: status=%q err=%v", status, err)
	}
}

func assertIntegrationProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var details *problem.Details
	if !errors.As(err, &details) || details.ErrorCode != code {
		t.Fatalf("expected problem %s, got %v", code, err)
	}
}
