package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/mq"
	"jiuxiaoer-admin/backend-go/internal/modules/notification"
)

func TestDeliveryIncidentOutboxRabbitMQNotificationEndToEnd(t *testing.T) {
	if os.Getenv("JXE_RUN_MQ_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_MQ_INTEGRATION=1 and use an isolated RabbitMQ vhost")
	}
	fixture := newMQDomainFixture(t)
	fixture.purge(t, acceptanceNotificationQueue)
	defer fixture.purge(t, acceptanceNotificationQueue)

	merchantID, shopID, adminID, cleanupRecipients := createIncidentNotificationRecipients(t, fixture)
	defer cleanupRecipients()
	eventTypes := []string{
		"delivery.incident.reported",
		"delivery.incident.evidence_added",
		"delivery.incident.acknowledged",
		"delivery.incident.resolved",
		"delivery.incident.rejected",
	}
	eventIDs := make([]string, 0, len(eventTypes))
	templateIDs := make([]uint64, 0, len(eventTypes))
	for _, eventType := range eventTypes {
		templateID := fixture.ids.Next()
		templateIDs = append(templateIDs, templateID)
		if err := fixture.db.Create(&notification.Template{
			ID: templateID, TemplateCode: fmt.Sprintf("incident_e2e_%d", templateID), EventType: eventType,
			Channel: "wechat", Version: "e2e-v1", TitleTemplate: "incident", BodyTemplate: "incident",
			AllowedFields: datatypes.JSON(`[]`), Status: "published", CreatedBy: adminID,
		}).Error; err != nil {
			t.Fatalf("create %s template: %v", eventType, err)
		}
	}

	outboxIDs := priorityIncidentOutboxIDs(t, fixture.db, len(eventTypes), fixture.ids)
	rows := make([]mq.OutboxEvent, 0, len(eventTypes))
	for index, eventType := range eventTypes {
		eventID := uuid.NewString()
		eventIDs = append(eventIDs, eventID)
		incidentID, deliveryID, orderID := fixture.ids.Next(), fixture.ids.Next(), fixture.ids.Next()
		payload := map[string]any{
			"incident_id": fmt.Sprint(incidentID), "incident_no": fmt.Sprintf("DI%d", incidentID),
			"delivery_order_id": fmt.Sprint(deliveryID), "order_id": fmt.Sprint(orderID), "shop_id": fmt.Sprint(shopID),
			"type": "alcohol_damaged", "stage": "delivery", "to_status": incidentEventTargetStatus(eventType), "actor_type": incidentEventActorType(eventType),
		}
		if eventType != "delivery.incident.reported" {
			payload["from_status"] = "open"
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, mq.OutboxEvent{
			ID: outboxIDs[index], EventID: eventID, EventType: eventType, AggregateType: "delivery_incident", AggregateID: incidentID,
			Payload: datatypes.JSON(raw), Status: "pending", CreatedAt: time.Now().UTC().Add(time.Duration(index) * time.Millisecond),
		})
	}
	if err := fixture.db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	defer func() {
		fixture.db.Where("event_id IN ?", eventIDs).Delete(&notification.Delivery{})
		fixture.db.Where("source_event_id IN ?", eventIDs).Delete(&notification.Message{})
		fixture.db.Where("consumer_name='notification' AND event_id IN ?", eventIDs).Delete(&mq.ConsumerReceipt{})
		fixture.db.Where("event_id IN ?", eventIDs).Delete(&mq.OutboxEvent{})
		fixture.db.Where("id IN ?", templateIDs).Delete(&notification.Template{})
	}()

	cfg := config.Load()
	cfg.CP1.NotificationEnabled = true
	cfg.CP1.WorkerBatchSize = 100
	worker := notification.NewWorker(cfg.CP1, fixture.db, fixture.ids, &notification.FakeProvider{}, "delivery-incident-e2e", fixture.log).
		WithDeliveryIncidentNotifications(true)
	consumerCtx, cancelConsumer := context.WithCancel(fixture.ctx)
	consumerDone := fixture.startConsumer(consumerCtx, "notification", notification.NewMQHandler(worker))
	fixture.waitConsumers(t, acceptanceNotificationQueue, 1)

	publisherCtx, cancelPublisher := context.WithCancel(fixture.ctx)
	publisher := mq.NewPublisher(fixture.db, fixture.rabbit, nil, "delivery-incident-e2e", fixture.log,
		mq.WithPublisherRegistry(fixture.registry), mq.WithPublisherEnvironment("integration"), mq.WithPublisherBatchSize(100))
	publisherDone := make(chan struct{})
	go func() {
		defer close(publisherDone)
		publisher.Run(publisherCtx)
	}()
	waitDomain(t, 10*time.Second, func() bool {
		var receipts, published int64
		fixture.db.Model(&mq.ConsumerReceipt{}).Where("consumer_name='notification' AND event_id IN ? AND status='succeeded'", eventIDs).Count(&receipts)
		fixture.db.Model(&mq.OutboxEvent{}).Where("event_id IN ? AND status='published'", eventIDs).Count(&published)
		return receipts == int64(len(eventIDs)) && published == int64(len(eventIDs))
	})
	cancelPublisher()
	select {
	case <-publisherDone:
	case <-time.After(5 * time.Second):
		t.Fatal("incident outbox publisher did not stop")
	}

	var published, customerInbox, customerDeliveries, unexpectedRecipients int64
	fixture.db.Model(&mq.OutboxEvent{}).Where("event_id IN ? AND status='published'", eventIDs).Count(&published)
	fixture.db.Model(&notification.Message{}).Where("source_event_id IN ?", eventIDs).Count(&customerInbox)
	fixture.db.Model(&notification.Delivery{}).Where("event_id IN ? AND recipient_type='customer'", eventIDs).Count(&customerDeliveries)
	fixture.db.Model(&notification.Delivery{}).Where("event_id IN ? AND recipient_type NOT IN ('merchant','admin')", eventIDs).Count(&unexpectedRecipients)
	if published != int64(len(eventIDs)) || customerInbox != 0 || customerDeliveries != 0 || unexpectedRecipients != 0 {
		t.Fatalf("incident MQ isolation published=%d inbox=%d customer_delivery=%d unexpected_recipient=%d", published, customerInbox, customerDeliveries, unexpectedRecipients)
	}
	for _, eventID := range eventIDs {
		var merchantDeliveries, adminDeliveries int64
		fixture.db.Model(&notification.Delivery{}).Where("event_id=? AND recipient_type='merchant' AND recipient_id=?", eventID, merchantID).Count(&merchantDeliveries)
		fixture.db.Model(&notification.Delivery{}).Where("event_id=? AND recipient_type='admin' AND recipient_id=?", eventID, adminID).Count(&adminDeliveries)
		if merchantDeliveries != 1 || adminDeliveries != 1 {
			t.Fatalf("event %s recipient fanout merchant=%d admin=%d", eventID, merchantDeliveries, adminDeliveries)
		}
	}
	cancelConsumer()
	fixture.awaitStop(t, consumerDone)
}

func createIncidentNotificationRecipients(t *testing.T, fixture *mqDomainFixture) (uint64, uint64, uint64, func()) {
	t.Helper()
	merchantID, shopID, roleID, adminID := fixture.ids.Next(), fixture.ids.Next(), fixture.ids.Next(), fixture.ids.Next()
	if err := fixture.db.Table("merchants").Create(map[string]any{
		"id": merchantID, "code": fmt.Sprintf("incident-mq-%d", merchantID), "name": "Incident MQ merchant", "status": "active", "review_status": "approved",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Table("shops").Create(map[string]any{
		"id": shopID, "merchant_id": merchantID, "name": "Incident MQ shop", "city": "深圳市", "district": "南山区", "address": "MQ acceptance", "status": "active", "business_status": "open",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Table("roles").Create(map[string]any{
		"id": roleID, "code": fmt.Sprintf("incident_mq_%d", roleID), "name": "Incident MQ role", "scope": "all", "status": "active",
	}).Error; err != nil {
		t.Fatal(err)
	}
	permissionID := uint64(0)
	if err := fixture.db.Table("permissions").Select("id").Where("code='delivery_incident:list_all'").Scan(&permissionID).Error; err != nil {
		t.Fatal(err)
	}
	createdPermission := false
	if permissionID == 0 {
		permissionID = fixture.ids.Next()
		createdPermission = true
		if err := fixture.db.Table("permissions").Create(map[string]any{
			"id": permissionID, "code": "delivery_incident:list_all", "resource": "delivery_incident", "action": "list_all", "description": "Incident MQ acceptance", "status": "active",
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	rolePermissionID := fixture.ids.Next()
	if err := fixture.db.Table("role_permissions").Create(map[string]any{"id": rolePermissionID, "role_id": roleID, "permission_id": permissionID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Table("admin_users").Create(map[string]any{
		"id": adminID, "account_id": fixture.ids.Next(), "role_id": roleID, "admin_sub_role": "operations", "name": "Incident MQ admin", "status": "active",
	}).Error; err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		fixture.db.Exec("DELETE FROM admin_users WHERE id=?", adminID)
		fixture.db.Exec("DELETE FROM role_permissions WHERE id=?", rolePermissionID)
		fixture.db.Exec("DELETE FROM roles WHERE id=?", roleID)
		fixture.db.Exec("DELETE FROM shops WHERE id=?", shopID)
		fixture.db.Exec("DELETE FROM merchants WHERE id=?", merchantID)
		if createdPermission {
			fixture.db.Exec("DELETE FROM permissions WHERE id=?", permissionID)
		}
	}
	return merchantID, shopID, adminID, cleanup
}

func priorityIncidentOutboxIDs(t *testing.T, db *gorm.DB, count int, ids interface{ Next() uint64 }) []uint64 {
	t.Helper()
	var minimum uint64
	if err := db.Raw("SELECT COALESCE(MIN(id),0) FROM outbox_events").Scan(&minimum).Error; err != nil {
		t.Fatal(err)
	}
	result := make([]uint64, count)
	if minimum > uint64(count) {
		for index := range result {
			result[index] = minimum - uint64(count-index)
		}
		return result
	}
	for index := range result {
		result[index] = ids.Next()
	}
	return result
}

func incidentEventTargetStatus(eventType string) string {
	switch eventType {
	case "delivery.incident.acknowledged":
		return "acknowledged"
	case "delivery.incident.resolved":
		return "resolved"
	case "delivery.incident.rejected":
		return "rejected"
	default:
		return "open"
	}
}

func incidentEventActorType(eventType string) string {
	if eventType == "delivery.incident.reported" || eventType == "delivery.incident.evidence_added" {
		return "rider"
	}
	return "admin"
}
