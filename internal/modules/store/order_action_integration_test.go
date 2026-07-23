package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type storeOrderActionFixture struct {
	orderID, deliveryID, shopID, merchantID, merchantUserID uint64
}

func TestStoreOrderActionsMySQL(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run merchant order action acceptance")
	}
	db := openStoreOrderActionDB(t)
	ids := snowflake.New(965)
	ctx := context.Background()

	t.Run("state-version-idempotency-and-safe-latest-facts", func(t *testing.T) {
		fixture := insertStoreOrderActionFixture(t, db, ids, "paid", 1)
		defer cleanupStoreOrderActionFixture(t, db, fixture)
		service := NewService(db, nil, ids).WithCP1(config.CP1Config{
			PickupVerificationMode: "enforce", DataEncryptionKey: "store-action-integration-key", VerificationTTL: time.Hour,
		})
		claims := storeOrderActionClaims(fixture)
		acceptPath := "/api/v1/store/orders/:id/accept"
		acceptReq := storeOrderActionReq(1)

		accepted, err := service.AcceptOrder(ctx, claims, http.MethodPost, acceptPath, fmt.Sprintf("accept-%d", fixture.orderID), fmt.Sprint(fixture.orderID), acceptReq)
		if err != nil || accepted.Status != "accepted" || accepted.Version != 2 || accepted.DeliverySummary == nil {
			t.Fatalf("accept result=%+v err=%v", accepted, err)
		}
		replayed, err := service.AcceptOrder(ctx, claims, http.MethodPost, acceptPath, fmt.Sprintf("accept-%d", fixture.orderID), fmt.Sprint(fixture.orderID), acceptReq)
		if err != nil || !reflect.DeepEqual(replayed, accepted) {
			t.Fatalf("same key/same request must replay first result: first=%+v replay=%+v err=%v", accepted, replayed, err)
		}
		if _, err := service.AcceptOrder(ctx, claims, http.MethodPost, acceptPath, fmt.Sprintf("accept-%d", fixture.orderID), fmt.Sprint(fixture.orderID), storeOrderActionReq(2)); problem.FromError(err).ErrorCode != "IDEMPOTENCY_KEY_REUSED" {
			t.Fatalf("same key/different version must conflict: %v", err)
		}
		if _, err := service.AcceptOrder(ctx, claims, http.MethodPost, acceptPath, fmt.Sprintf("accept-stale-%d", fixture.orderID), fmt.Sprint(fixture.orderID), acceptReq); problem.FromError(err).ErrorCode != "VERSION_CONFLICT" {
			t.Fatalf("stale version must conflict: %v", err)
		}

		preparing, err := service.StartPreparingOrder(ctx, claims, http.MethodPost, "/api/v1/store/orders/:id/start-preparing", fmt.Sprintf("preparing-%d", fixture.orderID), fmt.Sprint(fixture.orderID), storeOrderActionReq(2))
		if err != nil || preparing.Status != "preparing" || preparing.Version != 3 {
			t.Fatalf("start preparing result=%+v err=%v", preparing, err)
		}
		ready, err := service.PrepareOrder(ctx, claims, http.MethodPost, "/api/v1/store/orders/:id/prepare", fmt.Sprintf("prepare-%d", fixture.orderID), fmt.Sprint(fixture.orderID), storeOrderActionReq(3))
		if err != nil || ready.Status != "ready_for_pickup" || ready.Version != 4 || ready.DeliverySummary == nil || ready.DeliverySummary.PickupReadyStatus != "ready" {
			t.Fatalf("prepare result=%+v err=%v", ready, err)
		}
		if _, err := service.AcceptOrder(ctx, claims, http.MethodPost, acceptPath, fmt.Sprintf("accept-status-%d", fixture.orderID), fmt.Sprint(fixture.orderID), storeOrderActionReq(4)); problem.FromError(err).ErrorCode != "ORDER_INVALID_STATUS" {
			t.Fatalf("current version in an illegal state must return a stable status conflict: %v", err)
		}
		missingOrderID := ids.Next()
		defer db.Where("resource_type = 'order' AND resource_id = ?", missingOrderID).Delete(&AuditLog{})
		if _, err := service.AcceptOrder(ctx, claims, http.MethodPost, acceptPath, fmt.Sprintf("accept-missing-%d", missingOrderID), fmt.Sprint(missingOrderID), storeOrderActionReq(0)); problem.FromError(err).ErrorCode != "ORDER_NOT_FOUND" {
			t.Fatalf("out-of-scope or missing order must return the closed object error: %v", err)
		}
		encoded, _ := json.Marshal(ready)
		if containsAny(string(encoded), `"customer_id"`, `"merchant_id"`) {
			t.Fatalf("action response leaked internal subject IDs: %s", encoded)
		}

		assertStoreOrderActionCount(t, db, "order_logs", "order_id = ?", fixture.orderID, 3)
		assertStoreOrderActionCount(t, db, "outbox_events", "aggregate_type = 'order' AND aggregate_id = ? AND event_type IN ?", []any{fixture.orderID, []string{"store.order.accepted", "store.order.preparing", "store.order.prepared"}}, 3)
		assertStoreOrderActionCount(t, db, "audit_logs", "resource_type = 'order' AND resource_id = ? AND result = 'success'", fixture.orderID, 3)
		assertStoreOrderActionCount(t, db, "audit_logs", "resource_type = 'order' AND resource_id = ? AND action = 'store.order.accept' AND result = 'failed'", fixture.orderID, 3)
		assertStoreOrderActionCount(t, db, "audit_logs", "resource_type = 'order' AND resource_id = ? AND action = 'store.order.accept' AND result = 'failed'", missingOrderID, 1)
		assertStoreOrderActionCount(t, db, "delivery_verifications", "delivery_order_id = ? AND stage = 'pickup' AND status = 'active'", fixture.deliveryID, 1)
	})

	t.Run("concurrent-accept-has-one-winner", func(t *testing.T) {
		fixture := insertStoreOrderActionFixture(t, db, ids, "paid", 8)
		defer cleanupStoreOrderActionFixture(t, db, fixture)
		service := NewService(db, nil, ids)
		claims := storeOrderActionClaims(fixture)
		request := storeOrderActionReq(8)
		errorsCh := make(chan error, 2)
		var wait sync.WaitGroup
		for index := 0; index < 2; index++ {
			wait.Add(1)
			go func(index int) {
				defer wait.Done()
				_, err := service.AcceptOrder(ctx, claims, http.MethodPost, "/api/v1/store/orders/:id/accept", fmt.Sprintf("accept-race-%d-%d", fixture.orderID, index), fmt.Sprint(fixture.orderID), request)
				errorsCh <- err
			}(index)
		}
		wait.Wait()
		close(errorsCh)
		successes, conflicts := 0, 0
		for err := range errorsCh {
			if err == nil {
				successes++
			} else if problem.FromError(err).Status == http.StatusConflict {
				conflicts++
			} else {
				t.Fatalf("unexpected concurrent accept error: %v", err)
			}
		}
		if successes != 1 || conflicts != 1 {
			t.Fatalf("concurrent accept successes=%d conflicts=%d", successes, conflicts)
		}
		assertStoreOrderActionCount(t, db, "order_logs", "order_id = ? AND action = 'store_accept'", fixture.orderID, 1)
		assertStoreOrderActionCount(t, db, "outbox_events", "aggregate_id = ? AND event_type = 'store.order.accepted'", fixture.orderID, 1)
	})

	t.Run("pickup-verification-failure-rolls-back-ready-transition", func(t *testing.T) {
		fixture := insertStoreOrderActionFixture(t, db, ids, "preparing", 5)
		defer cleanupStoreOrderActionFixture(t, db, fixture)
		service := NewService(db, nil, ids).WithCP1(config.CP1Config{PickupVerificationMode: "enforce"})
		service.generatePickupVerifier = func(context.Context, *gorm.DB, config.CP1Config, *snowflake.Generator, uint64) error {
			return fmt.Errorf("injected pickup verification failure")
		}
		_, err := service.PrepareOrder(ctx, storeOrderActionClaims(fixture), http.MethodPost, "/api/v1/store/orders/:id/prepare", fmt.Sprintf("prepare-fail-%d", fixture.orderID), fmt.Sprint(fixture.orderID), storeOrderActionReq(5))
		if err == nil {
			t.Fatal("injected pickup verification failure must reject prepare")
		}
		var order Order
		if err := db.First(&order, fixture.orderID).Error; err != nil {
			t.Fatal(err)
		}
		var delivery DeliveryOrder
		if err := db.First(&delivery, fixture.deliveryID).Error; err != nil {
			t.Fatal(err)
		}
		if order.Status != "preparing" || order.Version != 5 || delivery.PickupReadyStatus != "waiting_store" || delivery.PickupReadyAt != nil {
			t.Fatalf("failed verification left partial fulfilment facts: order=%+v delivery=%+v", order, delivery)
		}
		assertStoreOrderActionCount(t, db, "order_logs", "order_id = ?", fixture.orderID, 0)
		assertStoreOrderActionCount(t, db, "outbox_events", "aggregate_id = ? AND event_type = 'store.order.prepared'", fixture.orderID, 0)
		assertStoreOrderActionCount(t, db, "audit_logs", "resource_id = ? AND action = 'store.order.prepare' AND result = 'failed'", fixture.orderID, 1)
	})
}

func openStoreOrderActionDB(t *testing.T) *gorm.DB {
	t.Helper()
	cfg := config.Load()
	db, err := mysql.Open(context.Background(), cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func insertStoreOrderActionFixture(t *testing.T, db *gorm.DB, ids *snowflake.Generator, status string, version int) storeOrderActionFixture {
	t.Helper()
	fixture := storeOrderActionFixture{
		orderID: ids.Next(), deliveryID: ids.Next(), shopID: ids.Next(), merchantID: ids.Next(), merchantUserID: ids.Next(),
	}
	shop := Shop{ID: fixture.shopID, MerchantID: fixture.merchantID, Name: "动作契约测试店", City: "上海市", District: "浦东新区", Address: "测试路1号", CoordinateSystem: "gcj02", Status: "active", BusinessStatus: "open"}
	if err := db.Create(&shop).Error; err != nil {
		t.Fatal(err)
	}
	order := Order{
		ID: fixture.orderID, OrderNo: fmt.Sprintf("STORE-ACTION-%d", fixture.orderID), CustomerID: ids.Next(), MerchantID: fixture.merchantID, ShopID: fixture.shopID,
		Status: status, PayStatus: "succeeded", DeliveryStatus: "pending_assign", GoodsAmount: 100, PayableAmount: 100, PaidAmount: 100, Version: version,
	}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	delivery := DeliveryOrder{ID: fixture.deliveryID, OrderID: fixture.orderID, ShopID: fixture.shopID, Status: "pending_assign", AssignmentVersion: 1, DispatchStatus: "pending", PickupReadyStatus: "waiting_store"}
	if err := db.Create(&delivery).Error; err != nil {
		t.Fatal(err)
	}
	return fixture
}

func cleanupStoreOrderActionFixture(t *testing.T, db *gorm.DB, fixture storeOrderActionFixture) {
	t.Helper()
	statements := []struct {
		query string
		args  []any
	}{
		{"DELETE FROM delivery_verification_attempts WHERE delivery_order_id = ?", []any{fixture.deliveryID}},
		{"DELETE FROM delivery_verifications WHERE delivery_order_id = ?", []any{fixture.deliveryID}},
		{"DELETE FROM idempotency_keys WHERE actor_type = 'merchant' AND actor_id = ?", []any{fixture.merchantUserID}},
		{"DELETE FROM print_tasks WHERE order_id = ?", []any{fixture.orderID}},
		{"DELETE FROM outbox_events WHERE aggregate_id = ?", []any{fixture.orderID}},
		{"DELETE FROM audit_logs WHERE resource_type = 'order' AND resource_id = ?", []any{fixture.orderID}},
		{"DELETE FROM order_logs WHERE order_id = ?", []any{fixture.orderID}},
		{"DELETE FROM order_items WHERE order_id = ?", []any{fixture.orderID}},
		{"DELETE FROM payments WHERE order_id = ?", []any{fixture.orderID}},
		{"DELETE FROM delivery_orders WHERE id = ?", []any{fixture.deliveryID}},
		{"DELETE FROM orders WHERE id = ?", []any{fixture.orderID}},
		{"DELETE FROM shops WHERE id = ?", []any{fixture.shopID}},
	}
	for _, statement := range statements {
		if err := db.Exec(statement.query, statement.args...).Error; err != nil {
			t.Errorf("cleanup merchant action fixture: %v", err)
		}
	}
}

func storeOrderActionClaims(fixture storeOrderActionFixture) *auth.Claims {
	return &auth.Claims{
		AccountType: "merchant", MerchantUserID: fmt.Sprint(fixture.merchantUserID), MerchantID: fmt.Sprint(fixture.merchantID),
		AuthorizedShopIDs: []string{fmt.Sprint(fixture.shopID)}, Permissions: []string{"store_order:accept", "store_order:prepare", "store_order:view"},
	}
}

func storeOrderActionReq(version uint) StoreOrderActionReq {
	return StoreOrderActionReq{ExpectedVersion: &version}
}

func assertStoreOrderActionCount(t *testing.T, db *gorm.DB, table, where string, args any, want int64) {
	t.Helper()
	query := db.Table(table).Where(where)
	switch values := args.(type) {
	case []any:
		query = db.Table(table).Where(where, values...)
	default:
		query = db.Table(table).Where(where, values)
	}
	var got int64
	if err := query.Count(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s count=%d want=%d", table, got, want)
	}
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
