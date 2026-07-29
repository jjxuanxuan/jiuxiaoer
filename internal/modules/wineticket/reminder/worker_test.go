package reminder

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/notification"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/redemption"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type blockingReminderProvider struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func TestReminderLockOwnerTruncatesUTF8WithoutSplittingCodePoint(t *testing.T) {
	owner := reminderLockOwner(strings.Repeat("酒", 129))
	if !utf8.ValidString(owner) || utf8.RuneCountInString(owner) != 128 {
		t.Fatalf("invalid reminder lock owner truncation: %q", owner)
	}
}

func (p *blockingReminderProvider) Send(
	ctx context.Context,
	req notification.SendRequest,
) (notification.SendResult, error) {
	p.calls.Add(1)
	select {
	case p.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return notification.SendResult{}, ctx.Err()
	case <-p.release:
		return notification.SendResult{
			ProviderRequestID: req.ProviderRequestID,
			Status:            "succeeded",
		}, nil
	}
}

func (*blockingReminderProvider) Query(
	context.Context,
	string,
) (notification.SendResult, error) {
	panic("subscription reminder results must not be queried")
}

func TestReminderMaterializationUsesAbsoluteT7AndCatchupWindow(t *testing.T) {
	db := newReminderTestDB(t)
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, shanghaiLocation)
	worker := NewExpiryReminderWorker(
		db, snowflake.New(202), &reminderCountingProvider{}, "test", nil,
	).WithNow(func() time.Time { return now })
	lots := []core.Lot{
		reminderTestLot(1, 101, now.Add(168*time.Hour), now.Add(-time.Hour)),
		reminderTestLot(2, 102, now.Add(48*time.Hour), now),
		reminderTestLot(3, 103, now.Add(time.Hour), now.Add(-30*24*time.Hour)),
		reminderTestLot(4, 104, now, now.Add(-30*24*time.Hour)),
		reminderTestLot(5, 105, now.Add(169*time.Hour), now.Add(-time.Hour)),
	}
	if err := db.Create(&lots).Error; err != nil {
		t.Fatal(err)
	}
	created, err := worker.MaterializeRemindersOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if created != 6 {
		t.Fatalf("created reminder facts=%d, want 6", created)
	}
	var reminderCount, messageCount, outboxCount int64
	db.Model(&Reminder{}).Count(&reminderCount)
	db.Model(&notification.Message{}).Count(&messageCount)
	db.Table("outbox_events").Count(&outboxCount)
	if reminderCount != 6 || messageCount != 3 || outboxCount != 3 {
		t.Fatalf("facts reminders=%d messages=%d outbox=%d", reminderCount, messageCount, outboxCount)
	}
	var exact Reminder
	if err := db.Where("lot_id = ? AND channel = 'inbox'", 1).Take(&exact).Error; err != nil {
		t.Fatal(err)
	}
	if !exact.ScheduledAt.Equal(now) {
		t.Fatalf("exact T-7 scheduled_at=%s want %s", exact.ScheduledAt, now)
	}
	var short Reminder
	if err := db.Where("lot_id = ? AND channel = 'inbox'", 2).Take(&short).Error; err != nil {
		t.Fatal(err)
	}
	if !short.ScheduledAt.Equal(now) {
		t.Fatalf("short validity scheduled_at=%s want expiry_changed_at=%s", short.ScheduledAt, now)
	}
	var excluded int64
	db.Model(&Reminder{}).Where("lot_id IN ?", []uint64{4, 5}).Count(&excluded)
	if excluded != 0 {
		t.Fatalf("exact-expiry/outside-window lots produced %d reminder facts", excluded)
	}
}

func TestReminderInFlightLeasePreventsSecondWorkerFromMutatingClaim(t *testing.T) {
	db := newReminderTestDB(t)
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, shanghaiLocation)
	provider := &blockingReminderProvider{
		entered: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	workerA := NewExpiryReminderWorker(
		db, snowflake.New(220), provider, "instance-a", nil,
	).WithNow(func() time.Time { return now })
	workerB := NewExpiryReminderWorker(
		db, snowflake.New(221), provider, "instance-b", nil,
	).WithNow(func() time.Time { return now })
	lot := reminderTestLot(81, 113, now.Add(24*time.Hour), now.Add(-24*time.Hour))
	consent := reminderTestConsent(82, 113, "template-v1", now.Add(-time.Hour))
	if err := db.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&consent).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := workerA.MaterializeRemindersOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	type dispatchResult struct {
		count int
		err   error
	}
	done := make(chan dispatchResult, 1)
	go func() {
		count, err := workerA.DispatchRemindersOnce(context.Background())
		done <- dispatchResult{count: count, err: err}
	}()
	select {
	case <-provider.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first worker did not enter provider send")
	}

	if count, err := workerB.DispatchRemindersOnce(context.Background()); err != nil || count != 0 {
		t.Fatalf("second worker dispatch count=%d err=%v", count, err)
	}
	var inFlight Reminder
	if err := db.Where("lot_id = ? AND channel = 'wechat_subscription'", lot.ID).
		Take(&inFlight).Error; err != nil {
		t.Fatal(err)
	}
	if inFlight.Status != "pending" || inFlight.Attempts != 1 ||
		inFlight.LockedBy == nil || *inFlight.LockedBy != "instance-a" {
		t.Fatalf("second worker mutated live claim: %+v", inFlight)
	}
	if err := db.First(&consent, consent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if consent.Status != "sending" {
		t.Fatalf("second worker mutated consent status=%s", consent.Status)
	}

	close(provider.release)
	result := <-done
	if result.err != nil || result.count != 1 {
		t.Fatalf("first worker dispatch count=%d err=%v", result.count, result.err)
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("provider calls=%d want=1", provider.calls.Load())
	}
	var completed Reminder
	if err := db.First(&completed, inFlight.ID).Error; err != nil {
		t.Fatal(err)
	}
	if completed.Status != "sent" || completed.LockedBy != nil || completed.LockedUntil != nil {
		t.Fatalf("completed reminder did not close lease: %+v", completed)
	}
	if err := db.First(&consent, consent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if consent.Status != "consumed" {
		t.Fatalf("completed consent status=%s", consent.Status)
	}
}

func TestReminderExpiredInFlightLeaseBecomesUnknownWithoutResend(t *testing.T) {
	db := newReminderTestDB(t)
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, shanghaiLocation)
	provider := &reminderCountingProvider{}
	worker := NewExpiryReminderWorker(
		db, snowflake.New(222), provider, "recovery-instance", nil,
	).WithNow(func() time.Time { return now })
	lot := reminderTestLot(91, 114, now.Add(24*time.Hour), now.Add(-24*time.Hour))
	consent := reminderTestConsent(92, 114, "template-v1", now.Add(-time.Hour))
	reminderID := uint64(93)
	lockOwner := "dead-instance"
	lockedUntil := now.Add(-time.Second)
	providerMessageID := fmt.Sprintf("wt-reminder-%d", reminderID)
	consent.Status = "sending"
	consent.ClaimedByReminderID = &reminderID
	consent.ClaimedAt = &now
	reminder := Reminder{
		ID: reminderID, LotID: lot.ID, OwnerCustomerID: lot.OwnerCustomerID,
		ExpiresAt: lot.ExpiresAt, RemindDays: expiryReminderDays,
		Channel: "wechat_subscription", Status: "pending", Attempts: 1,
		ProviderMessageID: &providerMessageID, LockedBy: &lockOwner,
		LockedUntil: &lockedUntil, ScheduledAt: now.Add(-time.Hour),
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}
	if err := db.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&consent).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&reminder).Error; err != nil {
		t.Fatal(err)
	}
	if count, err := worker.DispatchRemindersOnce(context.Background()); err != nil || count != 0 {
		t.Fatalf("stale claim dispatch count=%d err=%v", count, err)
	}
	if provider.CallCount() != 0 {
		t.Fatalf("expired in-flight result was resent: %d", provider.CallCount())
	}
	var recovered Reminder
	if err := db.First(&recovered, reminder.ID).Error; err != nil {
		t.Fatal(err)
	}
	if recovered.Status != "failed" || recovered.LockedBy != nil || recovered.LockedUntil != nil {
		t.Fatalf("stale reminder state=%+v", recovered)
	}
	if err := db.First(&consent, consent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if consent.Status != "unknown" {
		t.Fatalf("stale consent status=%s", consent.Status)
	}
}

func TestReminderBatchProgressAndNoConsentIsTerminal(t *testing.T) {
	db := newReminderTestDB(t)
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, shanghaiLocation)
	provider := &reminderCountingProvider{}
	worker := NewExpiryReminderWorker(db, snowflake.New(212), provider, "test", nil).
		WithNow(func() time.Time { return now }).WithBatchSize(1)
	lots := []core.Lot{
		reminderTestLot(6, 106, now.Add(time.Hour), now.Add(-30*24*time.Hour)),
		reminderTestLot(7, 107, now.Add(2*time.Hour), now.Add(-30*24*time.Hour)),
	}
	if err := db.Create(&lots).Error; err != nil {
		t.Fatal(err)
	}
	for range lots {
		if _, err := worker.MaterializeRemindersOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var inboxCount int64
	db.Model(&notification.Message{}).Count(&inboxCount)
	if inboxCount != 2 {
		t.Fatalf("existing first batch starved later lots; inbox count=%d", inboxCount)
	}
	for range lots {
		if _, err := worker.DispatchRemindersOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var skipped int64
	db.Model(&Reminder{}).
		Where("channel = 'wechat_subscription' AND status = 'skipped' AND last_error_code = 'CONSENT_NOT_AVAILABLE'").
		Count(&skipped)
	if skipped != 2 || provider.CallCount() != 0 {
		t.Fatalf("no-consent terminal facts=%d provider calls=%d", skipped, provider.CallCount())
	}
}

func TestWeChatReminderSwitchDoesNotConsumeConsentAndCatchesUpAfterEnable(t *testing.T) {
	db := newReminderTestDB(t)
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, shanghaiLocation)
	provider := &reminderCountingProvider{}
	worker := NewExpiryReminderWorker(db, snowflake.New(214), provider, "test", nil).
		WithNow(func() time.Time { return now }).
		WithWeChatEnabled(false)
	lot := reminderTestLot(8, 108, now.Add(24*time.Hour), now.Add(-24*time.Hour))
	consent := reminderTestConsent(9, 108, "template-v1", now.Add(-time.Hour))
	if err := db.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&consent).Error; err != nil {
		t.Fatal(err)
	}

	created, err := worker.MaterializeRemindersOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatalf("disabled WeChat channel created facts=%d want inbox only", created)
	}
	if sent, err := worker.DispatchRemindersOnce(context.Background()); err != nil || sent != 0 {
		t.Fatalf("disabled WeChat dispatch sent=%d err=%v", sent, err)
	}
	var wechatFacts int64
	if err := db.Model(&Reminder{}).
		Where("lot_id = ? AND channel = 'wechat_subscription'", lot.ID).
		Count(&wechatFacts).Error; err != nil {
		t.Fatal(err)
	}
	if wechatFacts != 0 || provider.CallCount() != 0 {
		t.Fatalf("disabled channel facts=%d provider calls=%d", wechatFacts, provider.CallCount())
	}
	if err := db.First(&consent, consent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if consent.Status != "available" || consent.ClaimedByReminderID != nil {
		t.Fatalf("disabled channel consumed consent: %+v", consent)
	}

	worker.WithWeChatEnabled(true)
	created, err = worker.MaterializeRemindersOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatalf("re-enabled channel catch-up facts=%d want=1", created)
	}
	if sent, err := worker.DispatchRemindersOnce(context.Background()); err != nil || sent != 1 {
		t.Fatalf("re-enabled channel sent=%d err=%v", sent, err)
	}
	if provider.CallCount() != 1 {
		t.Fatalf("re-enabled provider calls=%d want=1", provider.CallCount())
	}
}

func TestWeChatReminderResolvesOpenIDWithoutPersistingItInReminder(t *testing.T) {
	db := newReminderTestDB(t)
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, shanghaiLocation)
	provider := &reminderCountingProvider{}
	worker := NewExpiryReminderWorker(db, snowflake.New(215), provider, "test", nil).
		WithNow(func() time.Time { return now }).
		WithWeChatAppID("wx-reminder-app")
	lot := reminderTestLot(51, 109, now.Add(24*time.Hour), now.Add(-24*time.Hour))
	consent := reminderTestConsent(52, 109, "template-v1", now.Add(-time.Hour))
	identity := auth.CustomerIdentity{
		ID: 53, CustomerID: 109, Provider: "wechat_miniapp",
		AppID: "wx-reminder-app", ProviderSubject: "openid-reminder-109",
		Status: "active",
	}
	if err := db.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&consent).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&identity).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := worker.MaterializeRemindersOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.DispatchRemindersOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.LastRecipient() != "openid-reminder-109" {
		t.Fatalf("provider recipient=%q", provider.LastRecipient())
	}
	var payload map[string]any
	if err := json.Unmarshal(provider.LastPayload(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) != 3 ||
		payload["product_short_name"] != "典藏干红葡萄酒" ||
		payload["remaining_bottles"] != float64(6) ||
		payload["expiry_date"] != "2026-07-28" {
		t.Fatalf("unexpected controlled reminder payload: %#v", payload)
	}
	var reminder Reminder
	if err := db.Where("lot_id = ? AND channel = 'wechat_subscription'", lot.ID).
		Take(&reminder).Error; err != nil {
		t.Fatal(err)
	}
	providerMessageID := ""
	if reminder.ProviderMessageID != nil {
		providerMessageID = *reminder.ProviderMessageID
	}
	if strings.Contains(providerMessageID, "openid") {
		t.Fatalf("reminder persisted OpenID: %+v", reminder)
	}
}

func TestWeChatReminderMissingOpenIDSkipsWithoutConsumingConsent(t *testing.T) {
	db := newReminderTestDB(t)
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, shanghaiLocation)
	provider := &reminderCountingProvider{}
	worker := NewExpiryReminderWorker(db, snowflake.New(216), provider, "test", nil).
		WithNow(func() time.Time { return now }).
		WithWeChatAppID("wx-reminder-app")
	lot := reminderTestLot(54, 110, now.Add(24*time.Hour), now.Add(-24*time.Hour))
	consent := reminderTestConsent(55, 110, "template-v1", now.Add(-time.Hour))
	if err := db.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&consent).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := worker.MaterializeRemindersOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.DispatchRemindersOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.CallCount() != 0 {
		t.Fatalf("provider called without OpenID: %d", provider.CallCount())
	}
	if err := db.First(&consent, consent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if consent.Status != "available" || consent.ClaimedByReminderID != nil {
		t.Fatalf("missing OpenID consumed consent: %+v", consent)
	}
	var reminder Reminder
	if err := db.Where("lot_id = ? AND channel = 'wechat_subscription'", lot.ID).
		Take(&reminder).Error; err != nil {
		t.Fatal(err)
	}
	lastErrorCode := ""
	if reminder.LastErrorCode != nil {
		lastErrorCode = *reminder.LastErrorCode
	}
	if reminder.Status != "skipped" || lastErrorCode != "OPENID_NOT_AVAILABLE" {
		t.Fatalf("missing OpenID reminder=%+v", reminder)
	}
}

func TestReminderConsentConsumedOnceAndUnknownNeverResent(t *testing.T) {
	t.Run("success consumes earliest consent once", func(t *testing.T) {
		db := newReminderTestDB(t)
		now := time.Date(2026, 7, 27, 10, 0, 0, 0, shanghaiLocation)
		provider := &reminderCountingProvider{}
		worker := NewExpiryReminderWorker(db, snowflake.New(203), provider, "test", nil).
			WithNow(func() time.Time { return now })
		lot := reminderTestLot(11, 501, now.Add(24*time.Hour), now.Add(-24*time.Hour))
		if err := db.Create(&lot).Error; err != nil {
			t.Fatal(err)
		}
		consents := []NotificationSubscriptionConsent{
			reminderTestConsent(21, 501, "template-old", now.Add(-2*time.Hour)),
			reminderTestConsent(22, 501, "template-new", now.Add(-time.Hour)),
		}
		if err := db.Create(&consents).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := worker.MaterializeRemindersOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := worker.DispatchRemindersOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := worker.DispatchRemindersOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if provider.CallCount() != 1 {
			t.Fatalf("provider calls=%d, want 1", provider.CallCount())
		}
		var first, second NotificationSubscriptionConsent
		db.First(&first, 21)
		db.First(&second, 22)
		if first.Status != "consumed" || first.ClaimedByReminderID == nil || second.Status != "available" {
			t.Fatalf("consent states first=%+v second=%+v", first, second)
		}
	})

	t.Run("unknown result is terminal and not queried or resent", func(t *testing.T) {
		db := newReminderTestDB(t)
		now := time.Date(2026, 7, 27, 10, 0, 0, 0, shanghaiLocation)
		provider := &reminderCountingProvider{failure: &notification.ProviderError{
			Code: "network_result_unknown", Retryable: true, Unknown: true,
		}}
		worker := NewExpiryReminderWorker(db, snowflake.New(204), provider, "test", nil).
			WithNow(func() time.Time { return now })
		lot := reminderTestLot(31, 601, now.Add(24*time.Hour), now.Add(-24*time.Hour))
		if err := db.Create(&lot).Error; err != nil {
			t.Fatal(err)
		}
		consent := reminderTestConsent(32, 601, "template-v1", now.Add(-time.Hour))
		if err := db.Create(&consent).Error; err != nil {
			t.Fatal(err)
		}
		_, _ = worker.MaterializeRemindersOnce(context.Background())
		if _, err := worker.DispatchRemindersOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := worker.DispatchRemindersOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if provider.CallCount() != 1 {
			t.Fatalf("unknown provider calls=%d, want 1", provider.CallCount())
		}
		db.First(&consent, consent.ID)
		if consent.Status != "unknown" {
			t.Fatalf("unknown send consent status=%s", consent.Status)
		}
		var reminder Reminder
		if err := db.Where("lot_id = ? AND channel = 'wechat_subscription'", lot.ID).
			Take(&reminder).Error; err != nil {
			t.Fatal(err)
		}
		if reminder.Status != "failed" || reminder.Attempts != 1 {
			t.Fatalf("unknown send reminder=%+v", reminder)
		}
	})

	t.Run("access token failure releases unspent consent", func(t *testing.T) {
		db := newReminderTestDB(t)
		now := time.Date(2026, 7, 27, 10, 0, 0, 0, shanghaiLocation)
		provider := &reminderCountingProvider{failure: &notification.ProviderError{
			Code: "wechat_access_token_unavailable", Retryable: true,
		}}
		worker := NewExpiryReminderWorker(db, snowflake.New(220), provider, "test", nil).
			WithNow(func() time.Time { return now })
		lot := reminderTestLot(33, 603, now.Add(24*time.Hour), now.Add(-24*time.Hour))
		consent := reminderTestConsent(34, 603, "template-v1", now.Add(-time.Hour))
		if err := db.Create(&lot).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&consent).Error; err != nil {
			t.Fatal(err)
		}
		_, _ = worker.MaterializeRemindersOnce(context.Background())
		if _, err := worker.DispatchRemindersOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := db.First(&consent, consent.ID).Error; err != nil {
			t.Fatal(err)
		}
		if consent.Status != "available" || consent.ClaimedByReminderID != nil {
			t.Fatalf("unsent provider attempt consumed consent: %+v", consent)
		}
		var reminder Reminder
		if err := db.Where("lot_id = ? AND channel = 'wechat_subscription'", lot.ID).
			Take(&reminder).Error; err != nil {
			t.Fatal(err)
		}
		if reminder.Status != "pending" || reminder.Attempts != 0 ||
			reminder.ProviderMessageID != nil {
			t.Fatalf("unsent provider attempt was not released: %+v", reminder)
		}
	})

	t.Run("known quota exhaustion consumes the one-shot allowance", func(t *testing.T) {
		db := newReminderTestDB(t)
		now := time.Date(2026, 7, 27, 10, 0, 0, 0, shanghaiLocation)
		provider := &reminderCountingProvider{failure: &notification.ProviderError{
			Code: "provider_quota_exhausted", Retryable: false,
		}}
		worker := NewExpiryReminderWorker(db, snowflake.New(213), provider, "test", nil).
			WithNow(func() time.Time { return now })
		lot := reminderTestLot(35, 605, now.Add(24*time.Hour), now.Add(-24*time.Hour))
		consent := reminderTestConsent(36, 605, "template-v1", now.Add(-time.Hour))
		if err := db.Create(&lot).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&consent).Error; err != nil {
			t.Fatal(err)
		}
		_, _ = worker.MaterializeRemindersOnce(context.Background())
		if _, err := worker.DispatchRemindersOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		db.First(&consent, consent.ID)
		if consent.Status != "exhausted" || provider.CallCount() != 1 {
			t.Fatalf("quota outcome consent=%+v calls=%d", consent, provider.CallCount())
		}
	})
}

func TestWeChatReminderUsesUnconsumedQuantityAndDoesNotSpendConsentAtZero(t *testing.T) {
	t.Run("depleted lot with active hold remains part of customer balance", func(t *testing.T) {
		db := newReminderTestDB(t)
		now := time.Date(2026, 7, 27, 10, 0, 0, 0, shanghaiLocation)
		provider := &reminderCountingProvider{}
		worker := NewExpiryReminderWorker(
			db, snowflake.New(218), provider, "test", nil,
		).WithNow(func() time.Time { return now })
		lot := reminderTestLot(61, 111, now.Add(24*time.Hour), now.Add(-24*time.Hour))
		lot.AvailableQuantity = 0
		lot.Status = LotStatusDepleted
		consent := reminderTestConsent(62, 111, "template-v1", now.Add(-time.Hour))
		allocation := redemption.RedemptionAllocation{
			ID: 63, RedemptionID: 64, LotID: lot.ID, Quantity: 2,
			SourceExpiresAt: lot.ExpiresAt, Status: RedemptionAllocationStatusHeld,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := db.Create(&lot).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&consent).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&allocation).Error; err != nil {
			t.Fatal(err)
		}
		created, err := worker.MaterializeRemindersOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := worker.DispatchRemindersOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if created != 2 || provider.CallCount() != 1 {
			t.Fatalf("depleted held lot created=%d provider_calls=%d", created, provider.CallCount())
		}
		var messageCount int64
		if err := db.Model(&notification.Message{}).Where("customer_id = ?", lot.OwnerCustomerID).
			Count(&messageCount).Error; err != nil {
			t.Fatal(err)
		}
		if messageCount != 1 {
			t.Fatalf("depleted held lot inbox messages=%d, want 1", messageCount)
		}
		var payload map[string]any
		if err := json.Unmarshal(provider.LastPayload(), &payload); err != nil {
			t.Fatal(err)
		}
		if payload["remaining_bottles"] != float64(2) {
			t.Fatalf("held bottles omitted from reminder balance: %#v", payload)
		}
	})

	t.Run("zero balance never consumes one-shot consent", func(t *testing.T) {
		db := newReminderTestDB(t)
		now := time.Date(2026, 7, 27, 10, 0, 0, 0, shanghaiLocation)
		provider := &reminderCountingProvider{}
		worker := NewExpiryReminderWorker(
			db, snowflake.New(219), provider, "test", nil,
		).WithNow(func() time.Time { return now })
		lot := reminderTestLot(71, 112, now.Add(24*time.Hour), now.Add(-24*time.Hour))
		lot.AvailableQuantity = 0
		lot.Status = LotStatusDepleted
		consent := reminderTestConsent(72, 112, "template-v1", now.Add(-time.Hour))
		if err := db.Create(&lot).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&consent).Error; err != nil {
			t.Fatal(err)
		}
		created, err := worker.MaterializeRemindersOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := worker.DispatchRemindersOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if created != 0 {
			t.Fatalf("depleted zero-balance lot created %d sendable reminders", created)
		}
		if provider.CallCount() != 0 {
			t.Fatalf("provider called for zero balance: %d", provider.CallCount())
		}
		var messageCount int64
		if err := db.Model(&notification.Message{}).Where("customer_id = ?", lot.OwnerCustomerID).
			Count(&messageCount).Error; err != nil {
			t.Fatal(err)
		}
		if messageCount != 0 {
			t.Fatalf("depleted zero-balance lot created %d inbox messages", messageCount)
		}
		var reminderCount int64
		if err := db.Model(&Reminder{}).Where("lot_id = ?", lot.ID).
			Count(&reminderCount).Error; err != nil {
			t.Fatal(err)
		}
		if reminderCount != 0 {
			t.Fatalf("depleted zero-balance lot created %d reminder facts", reminderCount)
		}
		if err := db.First(&consent, consent.ID).Error; err != nil {
			t.Fatal(err)
		}
		if consent.Status != "available" || consent.ClaimedByReminderID != nil {
			t.Fatalf("zero balance consumed consent: %+v", consent)
		}
	})
}

func TestReminderAtExactExpiryIsNotSent(t *testing.T) {
	db := newReminderTestDB(t)
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, shanghaiLocation)
	provider := &reminderCountingProvider{}
	worker := NewExpiryReminderWorker(db, snowflake.New(205), provider, "test", nil).
		WithNow(func() time.Time { return now })
	lot := reminderTestLot(41, 701, now, now.Add(-24*time.Hour))
	if err := db.Create(&lot).Error; err != nil {
		t.Fatal(err)
	}
	consent := reminderTestConsent(42, 701, "template-v1", now.Add(-time.Hour))
	if err := db.Create(&consent).Error; err != nil {
		t.Fatal(err)
	}
	scheduled := now.Add(-168 * time.Hour)
	reminder := Reminder{
		ID: 43, LotID: lot.ID, OwnerCustomerID: lot.OwnerCustomerID,
		ExpiresAt: now, RemindDays: 7, Channel: "wechat_subscription",
		Status: "pending", ScheduledAt: scheduled, CreatedAt: scheduled, UpdatedAt: scheduled,
	}
	if err := db.Create(&reminder).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := worker.DispatchRemindersOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.CallCount() != 0 {
		t.Fatalf("provider called at exact expiry: %d", provider.CallCount())
	}
	if err := db.First(&reminder, reminder.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reminder.Status != "skipped" {
		t.Fatalf("exact-expiry reminder must be terminal, got %+v", reminder)
	}
}

func reminderTestLot(id, customerID uint64, expiresAt, expiryChangedAt time.Time) core.Lot {
	return core.Lot{
		ID: id, LotNo: "WTL" + idString(id), OwnerCustomerID: customerID,
		PurchaseID: id + 10_000, SourceType: LotSourcePurchase,
		IssuerMerchantID: 1, ProductID: 2, RedeemCityCode: "310100",
		TotalQuantity: 6, AvailableQuantity: 6,
		OriginalExpiresAt: expiresAt, ExpiresAt: expiresAt,
		ExpiryChangedAt: expiryChangedAt, Status: LotStatusActive, Version: 1,
		CreatedAt: expiryChangedAt, UpdatedAt: expiryChangedAt,
	}
}

func reminderTestConsent(
	id, customerID uint64, template string, consentedAt time.Time,
) NotificationSubscriptionConsent {
	return NotificationSubscriptionConsent{
		ID: id, CustomerID: customerID, Scene: expiryReminderScene,
		TemplateCode: template, ConsentResult: "accepted", Status: "available",
		ConsentedAt: consentedAt, CreatedAt: consentedAt, UpdatedAt: consentedAt,
	}
}
