package deliveryincident

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	mysqlinfra "jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestDeliveryIncidentMySQLInvariants(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run delivery incident MySQL acceptance")
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

	ids := snowflake.New(996)
	now := time.Now().UTC()
	testDeliveryIncidentLifecycle(t, tx, cfg, ids, now)

	deliveryID, orderID, shopID, riderID := ids.Next(), ids.Next(), ids.Next(), ids.Next()
	incident := func(kind, stage, status string) Incident {
		id := ids.Next()
		return Incident{ID: id, IncidentNo: "DI" + idString(id), DeliveryOrderID: deliveryID, OrderID: orderID, ShopID: shopID, RiderID: riderID,
			Type: kind, Stage: stage, Status: status, Priority: priorityFor(kind), Description: "integration fact",
			DeliveryStatusSnapshot: "accepted", AssignmentVersionSnapshot: 1, ReportedAt: now, Version: 1, CreatedAt: now, UpdatedAt: now}
	}
	first := incident(TypeOutOfStock, StagePickup, StatusOpen)
	if err := tx.Create(&first).Error; err != nil {
		t.Fatal(err)
	}
	duplicate := incident(TypeOutOfStock, StagePickup, StatusAcknowledged)
	if err := tx.Create(&duplicate).Error; !isDuplicate(err) {
		t.Fatalf("active generated-column uniqueness was not enforced: %v", err)
	}
	if err := tx.Model(&Incident{}).Where("id=?", first.ID).Update("status", StatusResolved).Error; err != nil {
		t.Fatal(err)
	}
	second := incident(TypeOutOfStock, StagePickup, StatusOpen)
	third := incident(TypeCustomerRefused, StageDelivery, StatusAcknowledged)
	if err := tx.Create(&second).Error; err != nil {
		t.Fatalf("terminal incident did not release the active key: %v", err)
	}
	if err := tx.Create(&third).Error; err != nil {
		t.Fatal(err)
	}

	cfg.DeliveryIncident.Enabled = true
	cfg.DeliveryIncident.AutoResolveEnabled = true
	service := NewService(cfg, tx, ids, nil)
	if err := service.ResolveActiveLocked(context.Background(), tx, deliveryID, "", "order_cancelled"); err != nil {
		t.Fatal(err)
	}
	var active int64
	if err := tx.Model(&Incident{}).Where("delivery_order_id=? AND status IN ?", deliveryID, activeStatuses).Count(&active).Error; err != nil || active != 0 {
		t.Fatalf("natural closure residual=%d err=%v", active, err)
	}
	var histories, audits, events int64
	idsToCheck := []uint64{second.ID, third.ID}
	if err := tx.Model(&History{}).Where("incident_id IN ? AND action='resolved'", idsToCheck).Count(&histories).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Table("audit_logs").Where("resource_type='delivery_incident' AND resource_id IN ? AND action='incident.auto_resolve'", idsToCheck).Count(&audits).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Table("outbox_events").Where("aggregate_type='delivery_incident' AND aggregate_id IN ? AND event_type='delivery.incident.resolved'", idsToCheck).Count(&events).Error; err != nil {
		t.Fatal(err)
	}
	if histories != 2 || audits != 2 || events != 2 {
		t.Fatalf("natural closure durability mismatch: histories=%d audits=%d events=%d", histories, audits, events)
	}
	assertNoSecondNaturalClose(t, service, tx, deliveryID, histories)
}

func testDeliveryIncidentLifecycle(t *testing.T, tx *gorm.DB, cfg config.Config, ids *snowflake.Generator, now time.Time) {
	t.Helper()
	deliveryID, orderID, shopID, riderID, itemID := ids.Next(), ids.Next(), ids.Next(), ids.Next(), ids.Next()
	acceptedAt := now.Add(-time.Hour)
	if err := tx.Exec("INSERT INTO delivery_orders (id,order_id,shop_id,rider_id,status,recipient_snapshot,accepted_at,assignment_version) VALUES (?,?,?,?,?,?,?,?)",
		deliveryID, orderID, shopID, riderID, "accepted", `{"district":"fixture"}`, acceptedAt, 2).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec("INSERT INTO order_items (id,order_id,shop_product_id,product_id,product_snapshot,quantity,sale_price_amount,total_amount) VALUES (?,?,?,?,?,?,?,?)",
		itemID, orderID, ids.Next(), ids.Next(), `{"name":"fixture bottle","spec":"500ml","cost":"private"}`, 2, 1000, 2000).Error; err != nil {
		t.Fatal(err)
	}
	cfg.DeliveryIncident.Enabled = true
	cfg.DeliveryIncident.AutoResolveEnabled = true
	cfg.DeliveryIncident.RiderAllowlist = []string{idString(riderID)}
	cfg.DeliveryIncident.ShopAllowlist = []string{idString(shopID)}
	service := NewService(cfg, tx, ids, nil)
	service.now = func() time.Time { return now }
	riderClaims := &auth.Claims{AccountType: "rider", RiderID: idString(riderID), Permissions: []string{"delivery_incident:create", "delivery_incident:view_own", "delivery_incident:evidence_add"}}
	request := CreateReq{Type: TypeOutOfStock, Description: "fixture is out of stock", Items: []ItemInput{{OrderItemID: itemID, Quantity: 1}}}
	created, err := service.Create(context.Background(), riderClaims, "POST", "/api/v1/delivery/orders/:id/incidents", "incident-create-001", idString(deliveryID), request)
	if err != nil || created.Status != StatusOpen || created.Stage != StagePickup || len(created.Items) != 1 {
		t.Fatalf("create lifecycle failed: dto=%+v err=%v", created, err)
	}
	replayed, err := service.Create(context.Background(), riderClaims, "POST", "/api/v1/delivery/orders/:id/incidents", "incident-create-001", idString(deliveryID), request)
	if err != nil || replayed.ID != created.ID {
		t.Fatalf("idempotent replay failed: dto=%+v err=%v", replayed, err)
	}
	otherRider := &auth.Claims{AccountType: "rider", RiderID: idString(riderID + 1), Permissions: []string{"delivery_incident:view_own"}}
	cfgOther := cfg
	cfgOther.DeliveryIncident.RiderAllowlist = append(cfgOther.DeliveryIncident.RiderAllowlist, idString(riderID+1))
	if _, err := NewService(cfgOther, tx, ids, nil).RiderDetail(context.Background(), otherRider, created.ID); problemCode(err) != "DELIVERY_INCIDENT_NOT_FOUND" {
		t.Fatalf("rider BOLA must be hidden as 404: %v", err)
	}
	merchant := &auth.Claims{AccountType: "merchant", MerchantUserID: idString(ids.Next()), AuthorizedShopIDs: []string{idString(shopID)}, Permissions: []string{"delivery_incident:view_shop"}}
	if detail, err := service.StoreDetail(context.Background(), merchant, created.ID); err != nil || detail.ID != created.ID {
		t.Fatalf("authorized shop could not read incident: dto=%+v err=%v", detail, err)
	}
	merchant.AuthorizedShopIDs = []string{idString(shopID + 1)}
	if _, err := service.StoreDetail(context.Background(), merchant, created.ID); problemCode(err) != "DELIVERY_INCIDENT_NOT_FOUND" {
		t.Fatalf("shop BOLA must be hidden as 404: %v", err)
	}
	adminID := ids.Next()
	admin := &auth.Claims{AccountType: "admin", AdminUserID: idString(adminID), Permissions: []string{"delivery_incident:acknowledge", "delivery_incident:resolve"}}
	acknowledged, err := service.Acknowledge(context.Background(), admin, "POST", "/api/v1/admin/delivery-incidents/:id/acknowledge", "incident-ack-001", created.ID, AcknowledgeReq{ExpectedVersion: 1, Note: "checking"})
	if err != nil || acknowledged.Status != StatusAcknowledged || acknowledged.Version != 2 {
		t.Fatalf("acknowledge failed: dto=%+v err=%v", acknowledged, err)
	}
	resolved, err := service.Resolve(context.Background(), admin, "POST", "/api/v1/admin/delivery-incidents/:id/resolve", "incident-resolve-001", created.ID,
		ResolveReq{ExpectedVersion: 2, ResolutionCode: "returned_to_store", ResolutionNote: "store confirmed"})
	if err != nil || resolved.Status != StatusResolved || resolved.Version != 3 {
		t.Fatalf("resolve failed: dto=%+v err=%v", resolved, err)
	}
	var deliveryStatus string
	if err := tx.Table("delivery_orders").Select("status").Where("id=?", deliveryID).Scan(&deliveryStatus).Error; err != nil || deliveryStatus != "accepted" {
		t.Fatalf("incident actions changed the delivery state: %q err=%v", deliveryStatus, err)
	}
	incidentID, _ := parseID(created.ID)
	var histories, events int64
	tx.Model(&History{}).Where("incident_id=?", incidentID).Count(&histories)
	tx.Table("outbox_events").Where("aggregate_type='delivery_incident' AND aggregate_id=?", incidentID).Count(&events)
	if histories != 3 || events != 3 {
		t.Fatalf("lifecycle durability mismatch: histories=%d events=%d", histories, events)
	}
}

func problemCode(err error) string {
	if err == nil {
		return ""
	}
	return problem.FromError(err).ErrorCode
}

func assertNoSecondNaturalClose(t *testing.T, service *Service, tx *gorm.DB, deliveryID uint64, wantHistories int64) {
	t.Helper()
	if err := service.ResolveActiveLocked(context.Background(), tx, deliveryID, "", "order_cancelled"); err != nil {
		t.Fatal(err)
	}
	var histories int64
	if err := tx.Model(&History{}).Where("incident_id IN (SELECT id FROM delivery_incidents WHERE delivery_order_id=?) AND action='resolved'", deliveryID).Count(&histories).Error; err != nil {
		t.Fatal(err)
	}
	if histories != wantHistories {
		t.Fatalf("repeated natural closure duplicated history: got=%d want=%d", histories, wantHistories)
	}
}
