package deliveryreturn

import (
	"context"
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

	"jiuxiaoer-admin/backend-go/internal/config"
	mysqlinfra "jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/aftersale"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/deliveryincident"
	refundmodule "jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

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
	customerID, merchantID := ids.Next(), ids.Next()
	pickedUpAt := time.Now().UTC().Add(-10 * time.Minute)
	if err := tx.Exec(`INSERT INTO orders
		(id,order_no,order_type,settlement_mode,customer_id,merchant_id,shop_id,status,pay_status,delivery_status,goods_amount,payable_amount,paid_amount)
		VALUES (?,?,'retail','cash',?,?,?,'delivering','succeeded','delivering',1000,1000,1000)`,
		orderID, "ORDER-DR-REQUEST-"+idString(orderID), customerID, merchantID, shopID).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(`INSERT INTO delivery_orders
		(id,order_id,shop_id,rider_id,status,assignment_version,picked_up_at,started_at)
		VALUES (?,?,?,?,?,?,?,?)`, deliveryID, orderID, shopID, riderID, "delivering", 3, pickedUpAt, pickedUpAt).Error; err != nil {
		t.Fatal(err)
	}
	cfg.DeliveryReturn.Enabled = true
	cfg.DeliveryReturn.RiderWriteEnabled = true
	cfg.DeliveryReturn.RiderAllowlist = []string{idString(riderID)}
	cfg.DeliveryReturn.RiderRatePer10Minutes = 100
	service := NewService(cfg, tx, redisClient, ids).WithAfterSale(aftersale.NewService(cfg, tx, ids))
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
			mustClosureExec(t, tx, `INSERT INTO delivery_verifications
				(id,delivery_order_id,stage,mode_snapshot,code_hash,code_ciphertext,code_mask,policy_version,secret_key_version,status,max_attempts,expires_at,activated_at,version)
				VALUES (?,?, 'delivery','enforce',?,?,?,'cp1-v1','v1','active',5,?,?,1)`,
				ids.Next(), fx.deliveryID, fmt.Sprintf("%064d", 1), []byte("return-verification"), "****11", time.Now().UTC().Add(2*time.Hour), time.Now().UTC())
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
			var verification struct {
				Status     string
				ReasonCode *string    `gorm:"column:invalidation_reason_code"`
				InvalidAt  *time.Time `gorm:"column:invalidated_at"`
			}
			if err := tx.Table("delivery_verifications").Select("status,invalidation_reason_code,invalidated_at").Where("delivery_order_id=? AND stage='delivery'", fx.deliveryID).Scan(&verification).Error; err != nil || verification.Status != "invalidated" || verification.ReasonCode == nil || *verification.ReasonCode != "delivery_return_approved" || verification.InvalidAt == nil {
				t.Fatalf("return approval left delivery code usable: row=%+v err=%v", verification, err)
			}
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
		var replacement refundmodule.Row
		if err := tx.Where("replaces_refund_id=?", refundRow.ID).Take(&replacement).Error; err != nil {
			t.Fatalf("replacement refund: %v", err)
		}
		provider.state = refundmodule.State{
			ProviderRefundID: "wx-" + replacement.RefundNo, RefundNo: replacement.RefundNo, PaymentNo: fx.paymentNo,
			Status: "SUCCESS", Currency: replacement.Currency, Amount: replacement.Amount, TotalAmount: replacement.TotalAmount,
		}
		request, _ = http.NewRequest(http.MethodPost, "/api/v1/refunds/wechat/callback", nil)
		request.Header.Set("X-Event-ID", prefix+"succeeded")
		if err := refunds.ProcessCallback(context.Background(), "wechat", request, []byte(`{"status":"SUCCESS"}`)); err != nil {
			t.Fatal(err)
		}
		final, err := returns.AdminDetail(context.Background(), admin, created.ID)
		if err != nil || final.Status != StatusClosed || final.RefundStatus != "succeeded" {
			t.Fatalf("retry did not close return: dto=%+v err=%v", final, err)
		}
		var refundsCount, replacementCount, originalFailed, replacementSucceeded int64
		tx.Table("refunds").Where("after_sale_id=?", mustReturnID(t, approved.AfterSaleID)).Count(&refundsCount)
		tx.Table("refunds").Where("replaces_refund_id=?", refundRow.ID).Count(&replacementCount)
		tx.Table("refunds").Where("id=? AND status='failed'", refundRow.ID).Count(&originalFailed)
		tx.Table("refunds").Where("id=? AND status='succeeded'", replacement.ID).Count(&replacementSucceeded)
		if refundsCount != 2 || replacementCount != 1 || originalFailed != 1 || replacementSucceeded != 1 || stockAvailable(t, tx, fx.restockShopProductID) != before+1 {
			t.Fatalf("retry chain refunds=%d replacements=%d original_failed=%d replacement_succeeded=%d", refundsCount, replacementCount, originalFailed, replacementSucceeded)
		}
		assertClosureExactlyOnce(t, tx, created.ID, replacement.ID)
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
		if disputed.Status != StatusDisputed || disputed.AfterSaleID != "" ||
			disputed.SettlementStatus != "not_started" ||
			disputed.SettlementBizID != "" {
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
