package deliveryincident

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	mysqlinfra "jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type incidentConcurrencyFixture struct {
	OrderID    uint64
	DeliveryID uint64
	ItemID     uint64
	ShopID     uint64
	RiderID    uint64
}

func TestDeliveryIncidentConcurrentCreate100(t *testing.T) {
	db, cfg, ids := openDeliveryIncidentConcurrencyDB(t)
	fixture := createIncidentConcurrencyFixture(t, db, ids, "accepted")
	defer cleanupIncidentConcurrencyFixture(t, db, fixture)
	cfg.DeliveryIncident.Enabled = true
	cfg.DeliveryIncident.AutoResolveEnabled = true
	cfg.DeliveryIncident.CreateRatePerHour = 1000
	cfg.DeliveryIncident.RiderAllowlist = []string{idString(fixture.RiderID)}
	cfg.DeliveryIncident.ShopAllowlist = []string{idString(fixture.ShopID)}
	service := NewService(cfg, db, ids, nil)
	claims := &auth.Claims{AccountType: "rider", RiderID: idString(fixture.RiderID), Permissions: []string{"delivery_incident:create"}}
	request := CreateReq{Type: TypeOutOfStock, Description: "one active incident under one hundred concurrent reports", Items: []ItemInput{{OrderItemID: fixture.ItemID, Quantity: 1}}}

	const workers = 100
	start := make(chan struct{})
	errorsByWorker := make(chan error, workers)
	durations := make(chan time.Duration, workers)
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			<-start
			began := time.Now()
			_, err := service.Create(context.Background(), claims, "POST", "/api/v1/delivery/orders/:id/incidents", fmt.Sprintf("concurrent-create-%d-%d", fixture.DeliveryID, worker), idString(fixture.DeliveryID), request)
			durations <- time.Since(began)
			errorsByWorker <- err
		}(index)
	}
	close(start)
	group.Wait()
	close(errorsByWorker)
	close(durations)

	successes, conflicts := 0, 0
	for err := range errorsByWorker {
		if err == nil {
			successes++
			continue
		}
		if problem.FromError(err).ErrorCode == "DELIVERY_INCIDENT_ACTIVE_EXISTS" {
			conflicts++
			continue
		}
		t.Fatalf("unexpected concurrent create error: %v", err)
	}
	if successes != 1 || conflicts != workers-1 {
		t.Fatalf("concurrent create result success=%d conflict=%d, want 1/%d", successes, conflicts, workers-1)
	}
	var active, total, histories, audits, events int64
	db.Model(&Incident{}).Where("delivery_order_id=? AND status IN ?", fixture.DeliveryID, activeStatuses).Count(&active)
	db.Model(&Incident{}).Where("delivery_order_id=?", fixture.DeliveryID).Count(&total)
	db.Model(&History{}).Where("incident_id IN (SELECT id FROM delivery_incidents WHERE delivery_order_id=?)", fixture.DeliveryID).Count(&histories)
	db.Table("audit_logs").Where("resource_type='delivery_incident' AND resource_id IN (SELECT id FROM delivery_incidents WHERE delivery_order_id=?)", fixture.DeliveryID).Count(&audits)
	db.Table("outbox_events").Where("aggregate_type='delivery_incident' AND aggregate_id IN (SELECT id FROM delivery_incidents WHERE delivery_order_id=?)", fixture.DeliveryID).Count(&events)
	if active != 1 || total != 1 || histories != 1 || audits != 1 || events != 1 {
		t.Fatalf("concurrent create invariant active=%d total=%d histories=%d audits=%d events=%d", active, total, histories, audits, events)
	}
	latencies := make([]time.Duration, 0, workers)
	for duration := range durations {
		latencies = append(latencies, duration)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	t.Logf("100 concurrent create race latency p95=%s p99=%s", percentileDuration(latencies, 95), percentileDuration(latencies, 99))
}

func TestDeliveryIncidentConcurrentManualAndNaturalClose100(t *testing.T) {
	db, cfg, ids := openDeliveryIncidentConcurrencyDB(t)
	fixture := createIncidentConcurrencyFixture(t, db, ids, "delivering")
	defer cleanupIncidentConcurrencyFixture(t, db, fixture)
	cfg.DeliveryIncident.Enabled = true
	cfg.DeliveryIncident.AutoResolveEnabled = true
	cfg.DeliveryIncident.CreateRatePerHour = 1000
	cfg.DeliveryIncident.RiderAllowlist = []string{idString(fixture.RiderID)}
	cfg.DeliveryIncident.ShopAllowlist = []string{idString(fixture.ShopID)}
	service := NewService(cfg, db, ids, nil)
	rider := &auth.Claims{AccountType: "rider", RiderID: idString(fixture.RiderID), Permissions: []string{"delivery_incident:create"}}
	created, err := service.Create(context.Background(), rider, "POST", "/api/v1/delivery/orders/:id/incidents", fmt.Sprintf("close-race-create-%d", fixture.DeliveryID), idString(fixture.DeliveryID), CreateReq{Type: TypeCustomerRefused, ReasonCode: "CUSTOMER_CHANGED_MIND", Description: "manual and natural close race"})
	if err != nil {
		t.Fatal(err)
	}
	admin := &auth.Claims{AccountType: "admin", AdminUserID: idString(fixture.RiderID), Permissions: []string{"delivery_incident:resolve"}}

	const workers = 100
	start := make(chan struct{})
	errorsByWorker := make(chan error, workers)
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			<-start
			if worker%2 == 0 {
				_, closeErr := service.Resolve(context.Background(), admin, "POST", "/api/v1/admin/delivery-incidents/:id/resolve", fmt.Sprintf("manual-close-%s-%d", created.ID, worker), created.ID, ResolveReq{ExpectedVersion: 1, ResolutionCode: "other", ResolutionNote: "concurrent manual resolution"})
				errorsByWorker <- closeErr
				return
			}
			closeErr := db.Transaction(func(tx *gorm.DB) error {
				var delivery DeliveryOrder
				if lockErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&delivery, fixture.DeliveryID).Error; lockErr != nil {
					return lockErr
				}
				return service.ResolveActiveLocked(context.Background(), tx, fixture.DeliveryID, StageDelivery, "delivery_completed")
			})
			errorsByWorker <- closeErr
		}(index)
	}
	close(start)
	group.Wait()
	close(errorsByWorker)

	for err := range errorsByWorker {
		if err == nil {
			continue
		}
		code := problem.FromError(err).ErrorCode
		if code == "DELIVERY_INCIDENT_VERSION_CONFLICT" || code == "DELIVERY_INCIDENT_STATUS_CONFLICT" {
			continue
		}
		t.Fatalf("unexpected close race error: %v", err)
	}
	var row Incident
	if err := db.First(&row, "id=?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != StatusResolved || row.Version != 2 {
		t.Fatalf("close race final incident=%+v", row)
	}
	var resolvedHistories, histories, events int64
	db.Model(&History{}).Where("incident_id=? AND action='resolved'", row.ID).Count(&resolvedHistories)
	db.Model(&History{}).Where("incident_id=?", row.ID).Count(&histories)
	db.Table("outbox_events").Where("aggregate_type='delivery_incident' AND aggregate_id=?", row.ID).Count(&events)
	if resolvedHistories != 1 || histories != 2 || events != 2 {
		t.Fatalf("close race duplicated durable facts: resolved_history=%d histories=%d events=%d", resolvedHistories, histories, events)
	}
}

func TestDeliveryIncidentCreateWritePointFailuresRollback(t *testing.T) {
	db, cfg, ids := openDeliveryIncidentConcurrencyDB(t)
	for _, table := range []string{"delivery_incident_history", "audit_logs", "outbox_events"} {
		t.Run(table, func(t *testing.T) {
			tx := db.Begin()
			if tx.Error != nil {
				t.Fatal(tx.Error)
			}
			defer tx.Rollback()
			fixture := createIncidentConcurrencyFixture(t, tx, ids, "accepted")
			cfg.DeliveryIncident.Enabled = true
			cfg.DeliveryIncident.CreateRatePerHour = 1000
			cfg.DeliveryIncident.RiderAllowlist = []string{idString(fixture.RiderID)}
			service := NewService(cfg, tx, ids, nil)
			callbackName := "delivery_incident_fail_create_" + table
			if err := tx.Callback().Create().Before("gorm:create").Register(callbackName, func(callbackTx *gorm.DB) {
				if callbackTx.Statement != nil && callbackTx.Statement.Table == table {
					callbackTx.AddError(errors.New("injected " + table + " write failure"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			claims := &auth.Claims{AccountType: "rider", RiderID: idString(fixture.RiderID), Permissions: []string{"delivery_incident:create"}}
			_, createErr := service.Create(context.Background(), claims, "POST", "/api/v1/delivery/orders/:id/incidents", fmt.Sprintf("write-failure-%s-%d", table, fixture.DeliveryID), idString(fixture.DeliveryID), CreateReq{Type: TypeOutOfStock, Description: "write point failure rollback", Items: []ItemInput{{OrderItemID: fixture.ItemID, Quantity: 1}}})
			if err := tx.Callback().Create().Remove(callbackName); err != nil {
				t.Fatal(err)
			}
			if createErr == nil {
				t.Fatalf("injected %s failure was ignored", table)
			}
			var incidents, items, histories, audits, events int64
			tx.Model(&Incident{}).Where("delivery_order_id=?", fixture.DeliveryID).Count(&incidents)
			tx.Model(&Item{}).Where("incident_id IN (SELECT id FROM delivery_incidents WHERE delivery_order_id=?)", fixture.DeliveryID).Count(&items)
			tx.Model(&History{}).Where("incident_id IN (SELECT id FROM delivery_incidents WHERE delivery_order_id=?)", fixture.DeliveryID).Count(&histories)
			tx.Table("audit_logs").Where("resource_type='delivery_incident' AND resource_id IN (SELECT id FROM delivery_incidents WHERE delivery_order_id=?)", fixture.DeliveryID).Count(&audits)
			tx.Table("outbox_events").Where("aggregate_type='delivery_incident' AND aggregate_id IN (SELECT id FROM delivery_incidents WHERE delivery_order_id=?)", fixture.DeliveryID).Count(&events)
			if incidents != 0 || items != 0 || histories != 0 || audits != 0 || events != 0 {
				t.Fatalf("%s failure partially committed incident=%d items=%d history=%d audit=%d outbox=%d", table, incidents, items, histories, audits, events)
			}
		})
	}
}

func openDeliveryIncidentConcurrencyDB(t *testing.T) (*gorm.DB, config.Config, *snowflake.Generator) {
	t.Helper()
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run delivery incident concurrency acceptance")
	}
	cfg := config.Load()
	db, err := mysqlinfra.Open(context.Background(), cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	db = db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db, cfg, snowflake.New(983)
}

func createIncidentConcurrencyFixture(t *testing.T, db *gorm.DB, ids *snowflake.Generator, status string) incidentConcurrencyFixture {
	t.Helper()
	fixture := incidentConcurrencyFixture{OrderID: ids.Next(), DeliveryID: ids.Next(), ItemID: ids.Next(), ShopID: ids.Next(), RiderID: ids.Next()}
	orderStatus := "paid"
	if status == "delivering" {
		orderStatus = "delivering"
	}
	if err := db.Table("orders").Create(map[string]any{
		"id": fixture.OrderID, "order_no": fmt.Sprintf("DICON%d", fixture.OrderID), "customer_id": ids.Next(), "merchant_id": ids.Next(),
		"shop_id": fixture.ShopID, "status": orderStatus, "pay_status": "succeeded", "delivery_status": status, "address_snapshot": datatypes.JSON(`{}`), "version": 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	delivery := map[string]any{
		"id": fixture.DeliveryID, "order_id": fixture.OrderID, "shop_id": fixture.ShopID, "rider_id": fixture.RiderID, "status": status,
		"assignment_version": 1, "accepted_at": time.Now().UTC().Add(-time.Hour), "recipient_snapshot": datatypes.JSON(`{"district":"fixture"}`),
	}
	if status == "delivering" {
		delivery["picked_up_at"], delivery["started_at"] = time.Now().Add(-10*time.Minute), time.Now().Add(-10*time.Minute)
	}
	if err := db.Table("delivery_orders").Create(delivery).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("order_items").Create(map[string]any{
		"id": fixture.ItemID, "order_id": fixture.OrderID, "shop_product_id": ids.Next(), "product_id": ids.Next(),
		"product_snapshot": datatypes.JSON(`{"name":"concurrency bottle","spec":"500ml"}`), "quantity": 2, "sale_price_amount": 1000, "total_amount": 2000,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return fixture
}

func cleanupIncidentConcurrencyFixture(t *testing.T, db *gorm.DB, fixture incidentConcurrencyFixture) {
	t.Helper()
	var incidentIDs []uint64
	db.Model(&Incident{}).Select("id").Where("delivery_order_id=?", fixture.DeliveryID).Scan(&incidentIDs)
	if len(incidentIDs) > 0 {
		db.Where("aggregate_type='delivery_incident' AND aggregate_id IN ?", incidentIDs).Delete(&OutboxEvent{})
		db.Where("resource_type='delivery_incident' AND resource_id IN ?", incidentIDs).Delete(&AuditLog{})
		db.Where("incident_id IN ?", incidentIDs).Delete(&History{})
		db.Where("incident_id IN ?", incidentIDs).Delete(&Evidence{})
		db.Where("incident_id IN ?", incidentIDs).Delete(&Item{})
		db.Where("id IN ?", incidentIDs).Delete(&Incident{})
	}
	db.Exec("DELETE FROM idempotency_keys WHERE actor_type IN ('rider','admin') AND actor_id=?", fixture.RiderID)
	db.Exec("DELETE FROM order_items WHERE order_id=?", fixture.OrderID)
	db.Exec("DELETE FROM delivery_orders WHERE id=?", fixture.DeliveryID)
	db.Exec("DELETE FROM orders WHERE id=?", fixture.OrderID)
}

func percentileDuration(values []time.Duration, percentile int) time.Duration {
	if len(values) == 0 {
		return 0
	}
	index := (len(values)*percentile + 99) / 100
	if index < 1 {
		index = 1
	}
	if index > len(values) {
		index = len(values)
	}
	return values[index-1]
}
