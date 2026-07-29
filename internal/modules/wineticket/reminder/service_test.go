package reminder

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/notification"
	"jiuxiaoer-admin/backend-go/internal/modules/product"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/gift"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/redemption"
	refunddomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/renewal"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
	"jiuxiaoer-admin/backend-go/internal/testutil"
)

type reminderTestOutbox struct {
	ID            uint64 `gorm:"primaryKey"`
	EventID       string
	EventType     string
	EventVersion  uint
	SpecVersion   string
	AggregateType string
	AggregateID   uint64
	Producer      string
	Payload       []byte
	Status        string
	RetryCount    uint
	CreatedAt     time.Time
}

func (reminderTestOutbox) TableName() string { return "outbox_events" }

func newReminderTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := testutil.UniqueSQLiteMemoryDSN(t, "_busy_timeout=5000")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(
		&core.Lot{}, &core.Transaction{}, &Reminder{}, &NotificationSubscriptionConsent{},
		&renewal.Renewal{}, &refunddomain.RefundAllocation{},
		&redemption.RedemptionAllocation{}, &gift.GiftAllocation{},
		&auth.CustomerIdentity{},
		&product.Product{},
		&notification.Message{}, &idempotency.Record{}, &reminderTestOutbox{},
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&product.Product{
		ID: 2, CategoryID: 1, Name: "典藏干红葡萄酒",
		Status: "on_sale", AgeRestricted: true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	statements := []string{
		"ALTER TABLE customer_identities ADD COLUMN deleted_at DATETIME",
		"CREATE UNIQUE INDEX uk_reminder_test_dedupe ON wine_ticket_reminders(lot_id, expires_at, remind_days, channel)",
		"CREATE UNIQUE INDEX uk_reminder_test_consent_claim ON notification_subscription_consents(claimed_by_reminder_id)",
		"CREATE UNIQUE INDEX uk_reminder_test_transaction_action ON wine_ticket_transactions(action_key)",
		"CREATE UNIQUE INDEX uk_reminder_test_transaction_no ON wine_ticket_transactions(transaction_no)",
		"CREATE UNIQUE INDEX uk_reminder_test_message_source ON message_inboxes(customer_id, source_event_id, type)",
		"CREATE UNIQUE INDEX uk_reminder_test_outbox_event ON outbox_events(event_id)",
		"CREATE UNIQUE INDEX uk_reminder_test_idempotency ON idempotency_keys(actor_type, actor_id, path, key_hash)",
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func reminderCustomer(customerID string, permissions ...string) *auth.Claims {
	return &auth.Claims{
		AccountType: "customer", CustomerID: customerID, Permissions: permissions,
	}
}

func TestReminderServiceConsentMappingOwnershipAndIdempotency(t *testing.T) {
	db := newReminderTestDB(t)
	now := time.Date(2026, 7, 27, 12, 34, 56, 123000000, shanghaiLocation)
	service := NewReminderService(db, snowflake.New(201)).WithNow(func() time.Time { return now })
	claims := reminderCustomer(
		"9001",
		"wine_ticket_notification_consent:create",
		"wine_ticket_notification_consent:view",
	)
	ctx := context.Background()
	path := "/api/v1/wine-tickets/notification-subscriptions"
	req := NotificationConsentCreateRequest{
		Scene: expiryReminderScene, TemplateCode: "expiry-template-v1",
		ConsentResult: "accepted", ProviderReceipt: "receipt-1",
	}
	first, err := service.RecordConsent(ctx, claims, "POST", path, "consent-key-0001", req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "available" || first.ConsentResult != "accepted" ||
		!strings.HasSuffix(first.ConsentedAt, "+08:00") {
		t.Fatalf("unexpected accepted consent: %+v", first)
	}
	replayed, err := service.RecordConsent(ctx, claims, "POST", path, "consent-key-0001", req)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ConsentID != first.ConsentID {
		t.Fatalf("idempotent replay changed consent: first=%+v replay=%+v", first, replayed)
	}
	var count int64
	if err := db.Model(&NotificationSubscriptionConsent{}).Where("customer_id = ?", 9001).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("consent facts=%d, want 1", count)
	}

	rejected, err := service.RecordConsent(ctx, claims, "POST", path, "consent-key-0002", NotificationConsentCreateRequest{
		Scene: expiryReminderScene, TemplateCode: "expiry-template-v1", ConsentResult: "rejected",
	})
	if err != nil || rejected.Status != "rejected" {
		t.Fatalf("rejected mapping: item=%+v err=%v", rejected, err)
	}
	unknown, err := service.RecordConsent(ctx, claims, "POST", path, "consent-key-0003", NotificationConsentCreateRequest{
		Scene: expiryReminderScene, TemplateCode: "expiry-template-v1", ConsentResult: "unknown",
	})
	if err != nil || unknown.Status != "unknown" {
		t.Fatalf("unknown mapping: item=%+v err=%v", unknown, err)
	}
	latest, err := service.LatestConsent(ctx, claims, expiryReminderScene)
	if err != nil || latest.ConsentID != unknown.ConsentID {
		t.Fatalf("latest consent: item=%+v err=%v", latest, err)
	}

	_, err = service.LatestConsent(
		ctx,
		reminderCustomer("9002", "wine_ticket_notification_consent:view"),
		expiryReminderScene,
	)
	var details *problem.Details
	if !errors.As(err, &details) || details.ErrorCode != "WT_NOTIFICATION_CONSENT_NOT_FOUND" {
		t.Fatalf("other customer must not see facts: %v", err)
	}
}

type reminderCountingProvider struct {
	mu         sync.Mutex
	calls      int
	recipients []string
	payloads   [][]byte
	failure    error
}

func (p *reminderCountingProvider) Send(
	_ context.Context, req notification.SendRequest,
) (notification.SendResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.recipients = append(p.recipients, req.Recipient)
	p.payloads = append(p.payloads, append([]byte(nil), req.Payload...))
	if p.failure != nil {
		return notification.SendResult{}, p.failure
	}
	return notification.SendResult{
		ProviderRequestID: req.ProviderRequestID, Status: "succeeded",
	}, nil
}

func (p *reminderCountingProvider) LastPayload() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.payloads) == 0 {
		return nil
	}
	return append([]byte(nil), p.payloads[len(p.payloads)-1]...)
}

func (p *reminderCountingProvider) Query(
	context.Context, string,
) (notification.SendResult, error) {
	panic("expiry reminder worker must never query/retry an unknown send automatically")
}

func (p *reminderCountingProvider) CallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *reminderCountingProvider) LastRecipient() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.recipients) == 0 {
		return ""
	}
	return p.recipients[len(p.recipients)-1]
}
