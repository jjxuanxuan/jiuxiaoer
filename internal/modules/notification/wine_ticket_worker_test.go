package notification

import (
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

type wineTicketNotificationRenewal struct {
	ID         uint64
	RenewalNo  string
	CustomerID uint64
	Status     string
}

func (wineTicketNotificationRenewal) TableName() string {
	return "wine_ticket_renewals"
}

type wineTicketNotificationPurchase struct {
	ID         uint64
	PurchaseNo string
}

func (wineTicketNotificationPurchase) TableName() string {
	return "wine_ticket_purchases"
}

type wineTicketNotificationRefund struct {
	ID                 uint64
	WineTicketRefundNo string
	PurchaseID         uint64
	CustomerID         uint64
	Status             string
}

func (wineTicketNotificationRefund) TableName() string {
	return "wine_ticket_refunds"
}

func TestWineTicketSettlementInboxUsesAuthoritativeOwnerAndSourceEventDedupe(t *testing.T) {
	t.Parallel()
	db, err := gorm.Open(
		sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&Message{},
		&wineTicketNotificationRenewal{},
		&wineTicketNotificationPurchase{},
		&wineTicketNotificationRefund{},
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&wineTicketNotificationRenewal{
		ID: 11, RenewalNo: "WTRN-SENSITIVE-11", CustomerID: 101, Status: "completed",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&wineTicketNotificationPurchase{
		ID: 22, PurchaseNo: "WTPU-SENSITIVE-22",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&wineTicketNotificationRefund{
		ID: 33, WineTicketRefundNo: "WTRF-SENSITIVE-33",
		PurchaseID: 22, CustomerID: 202, Status: "succeeded",
	}).Error; err != nil {
		t.Fatal(err)
	}

	worker := NewWorker(
		config.CP1Config{},
		db,
		snowflake.New(994),
		&FakeProvider{},
		"wine-ticket-settlement-notification-test",
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	occurredAt := time.Date(2026, 7, 27, 10, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	events := []struct {
		eventID      string
		eventType    string
		aggregate    string
		aggregateID  uint64
		payload      datatypes.JSON
		wantCustomer uint64
		wantTitle    string
	}{
		{
			eventID: "wine-renewed-event-11", eventType: "wine_ticket.renewed",
			aggregate: "wine_ticket_renewal", aggregateID: 11,
			payload:      datatypes.JSON(`{"renewal_no":"WTRN-SENSITIVE-11","lot_no":"WTL11","customer_id":"101","old_expires_at":"2026-07-27T00:00:00+08:00","new_expires_at":"2026-08-26T00:00:00+08:00"}`),
			wantCustomer: 101, wantTitle: "酒票续期成功",
		},
		{
			eventID: "wine-refunded-event-33", eventType: "wine_ticket.refund_succeeded",
			aggregate: "wine_ticket_refund", aggregateID: 33,
			payload:      datatypes.JSON(`{"refund_no":"WTRF-SENSITIVE-33","purchase_no":"WTPU-SENSITIVE-22","customer_id":"202","amount":12800}`),
			wantCustomer: 202, wantTitle: "酒票退款成功",
		},
	}
	for _, event := range events {
		for attempt := 0; attempt < 2; attempt++ {
			if err := worker.MaterializeEvent(
				t.Context(),
				db,
				event.eventID,
				event.eventType,
				event.aggregate,
				event.aggregateID,
				event.payload,
				occurredAt,
			); err != nil {
				t.Fatalf("%s attempt %d: %v", event.eventType, attempt+1, err)
			}
		}
		var messages []Message
		if err := db.Where(
			"customer_id=? AND source_event_id=?",
			event.wantCustomer,
			event.eventID,
		).Find(&messages).Error; err != nil {
			t.Fatal(err)
		}
		if len(messages) != 1 {
			t.Fatalf("%s duplicate inbox rows: %+v", event.eventType, messages)
		}
		message := messages[0]
		if message.Title != event.wantTitle ||
			message.TargetType == nil ||
			*message.TargetType != event.aggregate ||
			message.TargetID == nil ||
			*message.TargetID != event.aggregateID {
			t.Fatalf("%s inbox target mismatch: %+v", event.eventType, message)
		}
		for _, sensitive := range []string{
			"WTRN-SENSITIVE-11",
			"WTRF-SENSITIVE-33",
			"WTPU-SENSITIVE-22",
			"12800",
		} {
			if strings.Contains(message.Title, sensitive) ||
				strings.Contains(message.Summary, sensitive) {
				t.Fatalf("%s leaked business data in inbox text: %+v", event.eventType, message)
			}
		}
	}

	untrusted := []struct {
		eventID     string
		eventType   string
		aggregate   string
		aggregateID uint64
		payload     datatypes.JSON
	}{
		{
			eventID: "spoofed-renewal-owner", eventType: "wine_ticket.renewed",
			aggregate: "wine_ticket_renewal", aggregateID: 11,
			payload: datatypes.JSON(`{"renewal_no":"WTRN-SENSITIVE-11","customer_id":"999"}`),
		},
		{
			eventID: "spoofed-refund-source", eventType: "wine_ticket.refund_succeeded",
			aggregate: "wine_ticket_refund", aggregateID: 33,
			payload: datatypes.JSON(`{"refund_no":"WTRF-SENSITIVE-33","purchase_no":"ANOTHER-PURCHASE","customer_id":"202","amount":12800}`),
		},
	}
	for _, event := range untrusted {
		if err := worker.MaterializeEvent(
			t.Context(),
			db,
			event.eventID,
			event.eventType,
			event.aggregate,
			event.aggregateID,
			event.payload,
			occurredAt,
		); err != nil {
			t.Fatalf("%s: %v", event.eventID, err)
		}
	}
	var spoofed int64
	if err := db.Model(&Message{}).
		Where("source_event_id IN ?", []string{"spoofed-renewal-owner", "spoofed-refund-source"}).
		Count(&spoofed).Error; err != nil || spoofed != 0 {
		t.Fatalf("untrusted event created inbox rows: count=%d err=%v", spoofed, err)
	}
}
