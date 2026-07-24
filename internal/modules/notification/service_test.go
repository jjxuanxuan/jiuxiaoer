package notification

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestValidateTemplateFieldAllowlist 验证模板字段白名单。
func TestValidateTemplateFieldAllowlist(t *testing.T) {
	valid := TemplateReq{AllowedFields: []string{"order_no", "amount"}, TitleTemplate: "订单 {{ order_no }}", BodyTemplate: "金额 {{amount}}"}
	if err := validateTemplate(valid); err != nil {
		t.Fatalf("valid template rejected: %v", err)
	}
	invalid := valid
	invalid.BodyTemplate = "手机号 {{customer_phone}}"
	if err := validateTemplate(invalid); err == nil {
		t.Fatal("field outside allowlist was accepted")
	}
	unclosed := valid
	unclosed.TitleTemplate = "订单 {{order_no"
	if err := validateTemplate(unclosed); err == nil {
		t.Fatal("unclosed template variable was accepted")
	}
}

func TestDeliveryIncidentNotificationNeverTargetsCustomer(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Template{}, &Delivery{}, &Message{}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"CREATE TABLE shops (id INTEGER PRIMARY KEY, merchant_id INTEGER, deleted_at DATETIME)",
		"CREATE TABLE admin_users (id INTEGER PRIMARY KEY, role_id INTEGER, status TEXT, deleted_at DATETIME)",
		"CREATE TABLE permissions (id INTEGER PRIMARY KEY, code TEXT, status TEXT, deleted_at DATETIME)",
		"CREATE TABLE role_permissions (role_id INTEGER, permission_id INTEGER, deleted_at DATETIME)",
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Table("shops").Create(map[string]any{"id": 10, "merchant_id": 20}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("admin_users").Create(map[string]any{"id": 30, "role_id": 40, "status": "active"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("permissions").Create(map[string]any{"id": 50, "code": "delivery_incident:list_all", "status": "active"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("role_permissions").Create(map[string]any{"role_id": 40, "permission_id": 50}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Template{ID: 60, TemplateCode: "incident-reported", EventType: "delivery.incident.reported", Channel: "wechat", Version: "v1", Status: "published"}).Error; err != nil {
		t.Fatal(err)
	}

	worker := NewWorker(config.CP1Config{}, db, snowflake.New(999), &FakeProvider{}, "incident-notification-test", slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithDeliveryIncidentNotifications(true)
	payload := datatypes.JSON(`{"incident_id":"1","shop_id":"10","order_id":"99"}`)
	if err := worker.MaterializeEvent(context.Background(), db, "event-1", "delivery.incident.reported", "delivery_incident", 1, payload, time.Now()); err != nil {
		t.Fatal(err)
	}
	var customerMessages int64
	if err := db.Model(&Message{}).Count(&customerMessages).Error; err != nil || customerMessages != 0 {
		t.Fatalf("customer inbox rows = %d, err=%v", customerMessages, err)
	}
	var deliveries []Delivery
	if err := db.Order("recipient_type").Find(&deliveries).Error; err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 2 || deliveries[0].RecipientType != "admin" || deliveries[1].RecipientType != "merchant" {
		t.Fatalf("unexpected incident recipients: %+v", deliveries)
	}
	for _, delivery := range deliveries {
		if delivery.RecipientType == "customer" {
			t.Fatalf("incident delivery targeted a customer: %+v", delivery)
		}
	}

	disabled := NewWorker(config.CP1Config{}, db, snowflake.New(998), &FakeProvider{}, "incident-notification-disabled", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := disabled.MaterializeEvent(context.Background(), db, "event-2", "delivery.incident.reported", "delivery_incident", 1, payload, time.Now()); err != nil {
		t.Fatal(err)
	}
	var afterDisabled int64
	if err := db.Model(&Delivery{}).Count(&afterDisabled).Error; err != nil || afterDisabled != 2 {
		t.Fatalf("disabled incident notification created rows: count=%d err=%v", afterDisabled, err)
	}
}

func TestDeliveryReturnNotificationUsesExplicitRecipients(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Template{}, &Delivery{}, &Message{}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"CREATE TABLE orders (id INTEGER PRIMARY KEY, customer_id INTEGER, merchant_id INTEGER)",
		"CREATE TABLE shops (id INTEGER PRIMARY KEY, merchant_id INTEGER, deleted_at DATETIME)",
		"CREATE TABLE admin_users (id INTEGER PRIMARY KEY, role_id INTEGER, status TEXT, deleted_at DATETIME)",
		"CREATE TABLE permissions (id INTEGER PRIMARY KEY, code TEXT, status TEXT, deleted_at DATETIME)",
		"CREATE TABLE role_permissions (role_id INTEGER, permission_id INTEGER, deleted_at DATETIME)",
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, insert := range []struct {
		table string
		data  map[string]any
	}{
		{"orders", map[string]any{"id": 10, "customer_id": 20, "merchant_id": 30}},
		{"shops", map[string]any{"id": 40, "merchant_id": 30}},
		{"admin_users", map[string]any{"id": 50, "role_id": 60, "status": "active"}},
		{"permissions", map[string]any{"id": 70, "code": "delivery_return:list_all", "status": "active"}},
		{"role_permissions", map[string]any{"role_id": 60, "permission_id": 70}},
	} {
		if err := db.Table(insert.table).Create(insert.data).Error; err != nil {
			t.Fatal(err)
		}
	}
	returnEventTypes := []string{
		"delivery.return_requested",
		"delivery.return_approved",
		"delivery.return_arrived",
		"delivery.return_received",
		"delivery.return_closed",
		"delivery.return_disputed",
		"delivery.return_exception",
		"delivery.return_sla_reminder",
		"delivery.return_sla_breached",
	}
	for index, eventType := range returnEventTypes {
		if err := db.Create(&Template{ID: uint64(80 + index), TemplateCode: "delivery-return-" + eventType, EventType: eventType, Channel: "wechat", Version: "v1", Status: "published"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	worker := NewWorker(config.CP1Config{NotificationEnabled: true}, db, snowflake.New(997), &FakeProvider{}, "return-notification-test", slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithDeliveryReturnNotifications(true)
	payload := datatypes.JSON(`{"delivery_return_id":"90","order_id":"10","shop_id":"40","rider_id":"100"}`)
	if err := worker.MaterializeEvent(context.Background(), db, "return-requested", "delivery.return_requested", "delivery_return", 90, payload, time.Now()); err != nil {
		t.Fatal(err)
	}
	var requested []Delivery
	if err := db.Where("event_id=?", "return-requested").Order("recipient_type").Find(&requested).Error; err != nil {
		t.Fatal(err)
	}
	if len(requested) != 2 || requested[0].RecipientType != "admin" || requested[1].RecipientType != "merchant" {
		t.Fatalf("requested recipients must be admin+merchant: %+v", requested)
	}
	var requestedCustomerMessages int64
	if err := db.Model(&Message{}).Where("source_event_id=?", "return-requested").Count(&requestedCustomerMessages).Error; err != nil || requestedCustomerMessages != 0 {
		t.Fatalf("requested incorrectly notified customer: count=%d err=%v", requestedCustomerMessages, err)
	}

	if err := worker.MaterializeEvent(context.Background(), db, "return-approved", "delivery.return_approved", "delivery_return", 90, payload, time.Now()); err != nil {
		t.Fatal(err)
	}
	var approved []Delivery
	if err := db.Where("event_id=?", "return-approved").Order("recipient_type").Find(&approved).Error; err != nil {
		t.Fatal(err)
	}
	if len(approved) != 3 || approved[0].RecipientType != "customer" || approved[0].RecipientID != 20 || approved[1].RecipientType != "merchant" || approved[1].RecipientID != 30 || approved[2].RecipientType != "rider" || approved[2].RecipientID != 100 {
		t.Fatalf("approved recipients must be exact customer+merchant+rider: %+v", approved)
	}
	var approvedMessages []Message
	if err := db.Where("source_event_id=?", "return-approved").Find(&approvedMessages).Error; err != nil || len(approvedMessages) != 1 || approvedMessages[0].CustomerID != 20 {
		t.Fatalf("approved customer inbox mismatch: messages=%+v err=%v", approvedMessages, err)
	}

	for _, test := range []struct {
		eventType string
		want      []recipient
	}{
		{eventType: "delivery.return_arrived", want: []recipient{{kind: "merchant", id: 30}}},
		{eventType: "delivery.return_received", want: []recipient{{kind: "admin", id: 50}, {kind: "customer", id: 20}}},
		{eventType: "delivery.return_closed", want: []recipient{{kind: "admin", id: 50}, {kind: "customer", id: 20}}},
		{eventType: "delivery.return_disputed", want: []recipient{{kind: "admin", id: 50}, {kind: "merchant", id: 30}}},
		{eventType: "delivery.return_exception", want: []recipient{{kind: "admin", id: 50}, {kind: "merchant", id: 30}}},
		{eventType: "delivery.return_sla_reminder", want: []recipient{{kind: "admin", id: 50}, {kind: "merchant", id: 30}}},
		{eventType: "delivery.return_sla_breached", want: []recipient{{kind: "admin", id: 50}, {kind: "merchant", id: 30}}},
	} {
		t.Run(test.eventType, func(t *testing.T) {
			eventID := "matrix-" + test.eventType
			if err := worker.MaterializeEvent(context.Background(), db, eventID, test.eventType, "delivery_return", 90, payload, time.Now()); err != nil {
				t.Fatal(err)
			}
			var deliveries []Delivery
			if err := db.Where("event_id=?", eventID).Order("recipient_type").Find(&deliveries).Error; err != nil {
				t.Fatal(err)
			}
			if len(deliveries) != len(test.want) {
				t.Fatalf("recipient count mismatch: got=%+v want=%+v", deliveries, test.want)
			}
			for index, want := range test.want {
				if deliveries[index].RecipientType != want.kind || deliveries[index].RecipientID != want.id {
					t.Fatalf("recipient mismatch at %d: got=%+v want=%+v", index, deliveries[index], want)
				}
			}
		})
	}
}

// TestNotificationProviders 验证通知 Providers的预期行为。
func TestNotificationProviders(t *testing.T) {
	result, err := (&FakeProvider{}).Send(context.Background(), SendRequest{ProviderRequestID: "notify-1"})
	if err != nil || result.Status != "succeeded" {
		t.Fatalf("unexpected fake result: %#v, %v", result, err)
	}
	queried, err := (&FakeProvider{}).Query(context.Background(), "notify-1")
	if err != nil || queried.Status != "succeeded" {
		t.Fatalf("unexpected reconciliation result: %#v, %v", queried, err)
	}
	if _, err := (&UnavailableProvider{}).Send(context.Background(), SendRequest{}); err == nil {
		t.Fatal("unconfigured provider must fail closed")
	}
}
