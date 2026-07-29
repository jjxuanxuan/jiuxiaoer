package reminder

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/notification"
	"jiuxiaoer-admin/backend-go/internal/modules/product"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type mysqlBlockingReminderProvider struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (p *mysqlBlockingReminderProvider) Send(
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

func (*mysqlBlockingReminderProvider) Query(
	context.Context,
	string,
) (notification.SendResult, error) {
	panic("subscription reminder results must not be queried")
}

// TestMySQLReminderLeaseAllowsExactlyOneConcurrentSender 验证生产环境的
// InnoDB 认领契约。SQLite 测试有意只使用一个连接，因此无法证明：
// 当第一个任务阻塞于外部服务商调用时，另一个任务能看见已提交的租约。
func TestMySQLReminderLeaseAllowsExactlyOneConcurrentSender(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run wine-ticket MySQL lease acceptance")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cfg := config.Load()
	cfg.MySQL.Required = true
	cfg.MySQL.RequiredTimeZone = "+08:00"
	cfg.MySQL.RequireWineTicketSchema = true
	cfg.MySQL.RequireWineTicketMoneyContract = false
	if cfg.MySQL.MaxOpenConns < 4 {
		cfg.MySQL.MaxOpenConns = 4
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := mysql.Open(ctx, cfg.MySQL, log)
	if err != nil || db == nil {
		t.Fatalf("open schema- and timezone-verified mysql: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get mysql connection pool: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	ids := snowflake.New(987)
	productID := ids.Next()
	customerID := ids.Next()
	lotID := ids.Next()
	consentID := ids.Next()
	reminderID := ids.Next()
	now := time.Now().In(shanghaiLocation).Truncate(time.Millisecond)
	expiresAt := now.Add(24 * time.Hour)
	scheduledAt := now.Add(-time.Hour)

	productRow := product.Product{
		ID: productID, CategoryID: ids.Next(), Name: "MySQL 租约验收酒",
		Status: "on_sale", AgeRestricted: true,
	}
	lot := reminderTestLot(lotID, customerID, expiresAt, now.Add(-24*time.Hour))
	lot.LotNo = "MYSQL-WTL-" + strconv.FormatUint(lotID, 10)
	lot.PurchaseID = ids.Next()
	lot.ProductID = productID
	consent := reminderTestConsent(consentID, customerID, "mysql-template-v1", now.Add(-time.Hour))
	reminder := Reminder{
		ID: reminderID, LotID: lotID, OwnerCustomerID: customerID,
		ExpiresAt: expiresAt, RemindDays: expiryReminderDays,
		Channel: "wechat_subscription", Status: "pending",
		ScheduledAt: scheduledAt, CreatedAt: scheduledAt, UpdatedAt: scheduledAt,
	}

	if err := db.Create(&productRow).Error; err != nil {
		t.Fatalf("insert product fixture: %v", err)
	}
	if err := db.Create(&lot).Error; err != nil {
		t.Fatalf("insert lot fixture: %v", err)
	}
	if err := db.Create(&consent).Error; err != nil {
		t.Fatalf("insert consent fixture: %v", err)
	}
	if err := db.Create(&reminder).Error; err != nil {
		t.Fatalf("insert reminder fixture: %v", err)
	}
	t.Cleanup(func() {
		fixtures := []struct {
			name string
			err  error
		}{
			{
				name: "reminder",
				err:  db.Where("id = ?", reminderID).Delete(&Reminder{}).Error,
			},
			{
				name: "consent",
				err: db.Where("id = ?", consentID).
					Delete(&NotificationSubscriptionConsent{}).Error,
			},
			{
				name: "lot",
				err:  db.Where("id = ?", lotID).Delete(&core.Lot{}).Error,
			},
			{
				name: "product",
				err:  db.Where("id = ?", productID).Delete(&product.Product{}).Error,
			},
		}
		for _, fixture := range fixtures {
			if fixture.err != nil {
				t.Errorf("delete %s fixture: %v", fixture.name, fixture.err)
			}
		}
	})

	provider := &mysqlBlockingReminderProvider{
		entered: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	workerA := NewExpiryReminderWorker(
		db, snowflake.New(986), provider, "mysql-worker-a", log,
	).WithNow(func() time.Time { return now }).WithSendLease(time.Minute)
	workerB := NewExpiryReminderWorker(
		db, snowflake.New(985), provider, "mysql-worker-b", log,
	).WithNow(func() time.Time { return now }).WithSendLease(time.Minute)

	type dispatchOutcome struct {
		count int
		err   error
	}
	firstResult := make(chan dispatchOutcome, 1)
	firstFinished := make(chan struct{})
	var releaseOnce sync.Once
	releaseProvider := func() {
		releaseOnce.Do(func() { close(provider.release) })
	}
	go func() {
		defer close(firstFinished)
		count, dispatchErr := workerA.DispatchRemindersOnce(ctx)
		firstResult <- dispatchOutcome{count: count, err: dispatchErr}
	}()
	t.Cleanup(func() {
		releaseProvider()
		select {
		case <-firstFinished:
		case <-time.After(3 * time.Second):
		}
	})

	select {
	case <-provider.entered:
	case <-ctx.Done():
		t.Fatalf("first worker did not enter provider send: %v", ctx.Err())
	}

	secondCtx, secondCancel := context.WithTimeout(ctx, 2*time.Second)
	secondCount, secondErr := workerB.DispatchRemindersOnce(secondCtx)
	secondCancel()
	if secondErr != nil || secondCount != 0 {
		t.Fatalf("second worker crossed the active lease: count=%d err=%v", secondCount, secondErr)
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("provider calls while first lease is active=%d, want 1", provider.calls.Load())
	}

	var inFlight Reminder
	if err := db.First(&inFlight, reminderID).Error; err != nil {
		t.Fatalf("load in-flight reminder: %v", err)
	}
	if inFlight.Status != "pending" || inFlight.Attempts != 1 ||
		inFlight.LockedBy == nil || *inFlight.LockedBy != "mysql-worker-a" ||
		inFlight.LockedUntil == nil || !inFlight.LockedUntil.After(now) {
		t.Fatalf("unexpected committed in-flight lease: %+v", inFlight)
	}
	var inFlightConsent NotificationSubscriptionConsent
	if err := db.First(&inFlightConsent, consentID).Error; err != nil {
		t.Fatalf("load claimed consent: %v", err)
	}
	if inFlightConsent.Status != "sending" ||
		inFlightConsent.ClaimedByReminderID == nil ||
		*inFlightConsent.ClaimedByReminderID != reminderID {
		t.Fatalf("unexpected committed consent claim: %+v", inFlightConsent)
	}

	releaseProvider()
	select {
	case result := <-firstResult:
		if result.err != nil || result.count != 1 {
			t.Fatalf("first worker dispatch: count=%d err=%v", result.count, result.err)
		}
	case <-ctx.Done():
		t.Fatalf("first worker did not complete: %v", ctx.Err())
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("provider calls after both workers=%d, want exactly 1", provider.calls.Load())
	}

	var completed Reminder
	if err := db.First(&completed, reminderID).Error; err != nil {
		t.Fatalf("load completed reminder: %v", err)
	}
	if completed.Status != "sent" || completed.Attempts != 1 ||
		completed.LockedBy != nil || completed.LockedUntil != nil {
		t.Fatalf("lease was not closed after send: %+v", completed)
	}
	var consumed NotificationSubscriptionConsent
	if err := db.First(&consumed, consentID).Error; err != nil {
		t.Fatalf("load consumed consent: %v", err)
	}
	if consumed.Status != "consumed" {
		t.Fatalf("consent status=%s, want consumed", consumed.Status)
	}
}
