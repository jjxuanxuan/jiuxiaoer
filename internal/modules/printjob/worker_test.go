package printjob

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/securevalue"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestWorkerQueriesFirstWhenTaskMayAlreadyBeSubmitted(t *testing.T) {
	db, setting, task := newWorkerFixture(t)
	submittedAt := time.Now().Add(-time.Minute)
	providerRequestID := "vendor-existing-42"
	task.ProviderRequestID = &providerRequestID
	task.SubmittedAt = &submittedAt
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	provider := &workerRecordingProvider{queryResult: PrintResult{ProviderRequestID: providerRequestID, Status: "succeeded"}}
	worker := newTestWorker(db, provider)

	worker.execute(context.Background(), task, setting)

	if provider.submitCalls != 0 || provider.queryCalls != 1 || len(provider.queryIDs) != 1 || provider.queryIDs[0] != providerRequestID {
		t.Fatalf("worker did not query first: submit=%d query=%d ids=%v", provider.submitCalls, provider.queryCalls, provider.queryIDs)
	}
	assertWorkerTaskState(t, db, task.ID, "succeeded", providerRequestID, 1)
	var attempt Attempt
	if err := db.First(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	if attempt.Operation != "query" || attempt.Result != "succeeded" {
		t.Fatalf("unexpected reconciliation attempt: %+v", attempt)
	}
	assertWorkerAudit(t, db, task.ID, "print_task.worker_succeeded", "success")
}

func TestWorkerPersistsUnknownSubmissionBeforeQueryingReturnedProviderID(t *testing.T) {
	db, setting, task := newWorkerFixture(t)
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	provider := &workerRecordingProvider{
		submitResult: PrintResult{ProviderRequestID: "vendor-unknown-99", Status: "submitted"},
		submitErr:    &ProviderError{Code: "submit_timeout", Retryable: true, Unknown: true},
		queryResult:  PrintResult{ProviderRequestID: "vendor-unknown-99", Status: "succeeded"},
	}
	provider.beforeQuery = func(providerRequestID string) {
		if providerRequestID != "vendor-unknown-99" {
			t.Fatalf("queried wrong provider request ID: %q", providerRequestID)
		}
		var persisted Task
		if err := db.First(&persisted, task.ID).Error; err != nil {
			t.Fatal(err)
		}
		if persisted.Status != "querying" || persisted.ProviderRequestID == nil || *persisted.ProviderRequestID != "vendor-unknown-99" || persisted.SubmittedAt == nil {
			t.Fatalf("unknown submission was not persisted before Query: %+v", persisted)
		}
		var submitAttempts int64
		if err := db.Model(&Attempt{}).Where("print_task_id=? AND operation='submit' AND result='unknown'", task.ID).Count(&submitAttempts).Error; err != nil {
			t.Fatal(err)
		}
		if submitAttempts != 1 {
			t.Fatalf("unknown submit attempt was not persisted before Query: %d", submitAttempts)
		}
	}
	worker := newTestWorker(db, provider)

	worker.execute(context.Background(), task, setting)

	if provider.submitCalls != 1 || provider.queryCalls != 1 || provider.queryIDs[0] != "vendor-unknown-99" {
		t.Fatalf("unexpected unknown/query flow: submit=%d query=%d ids=%v", provider.submitCalls, provider.queryCalls, provider.queryIDs)
	}
	assertWorkerTaskState(t, db, task.ID, "succeeded", "vendor-unknown-99", 1)
	var attempts []Attempt
	if err := db.Order("operation").Find(&attempts).Error; err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 {
		t.Fatalf("expected submit and query attempts, got %+v", attempts)
	}
	assertWorkerAudit(t, db, task.ID, "print_task.worker_unknown", "failed")
	assertWorkerAudit(t, db, task.ID, "print_task.worker_succeeded", "success")
}

func TestWorkerPreservesProviderRequestIDWhileQueryIsStillPending(t *testing.T) {
	db, setting, task := newWorkerFixture(t)
	submittedAt := time.Now().Add(-time.Minute)
	providerRequestID := "vendor-pending-7"
	task.ProviderRequestID = &providerRequestID
	task.SubmittedAt = &submittedAt
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	provider := &workerRecordingProvider{queryResult: PrintResult{Status: "processing"}}
	worker := newTestWorker(db, provider)

	worker.execute(context.Background(), task, setting)

	var persisted Task
	if err := db.First(&persisted, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if provider.submitCalls != 0 || provider.queryCalls != 1 {
		t.Fatalf("unexpected provider calls: submit=%d query=%d", provider.submitCalls, provider.queryCalls)
	}
	if persisted.Status != "querying" || persisted.ProviderRequestID == nil || *persisted.ProviderRequestID != providerRequestID {
		t.Fatalf("pending query lost provider identity: %+v", persisted)
	}
	if persisted.NextRetryAt == nil || persisted.LockedBy != nil || persisted.LockedUntil != nil {
		t.Fatalf("pending query was not safely released for retry: %+v", persisted)
	}
	assertWorkerAudit(t, db, task.ID, "print_task.worker_unknown", "failed")
}

func newWorkerFixture(t *testing.T) (*gorm.DB, Setting, Task) {
	t.Helper()
	dsn := fmt.Sprintf("file:print_worker_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&Setting{}, &Task{}, &Attempt{}, &printAuditFixture{}); err != nil {
		t.Fatal(err)
	}
	sealed, err := securevalue.Seal(printTestEncryptionKey, "worker-device-1")
	if err != nil {
		t.Fatal(err)
	}
	setting := Setting{
		ID: 8001, ShopID: 100, Provider: "fake", DeviceIDCiphertext: sealed, DeviceIDMask: "wo***-1",
		DeviceStatus: "online", TemplateID: 9001, Copies: 1,
		AutoPrintEvents: datatypes.JSON(`["order_accepted"]`), Enabled: true, Version: 1, CreatedBy: 77, UpdatedBy: 77,
	}
	if err := db.Create(&setting).Error; err != nil {
		t.Fatal(err)
	}
	owner := "worker-test:lease"
	lockedUntil := time.Now().Add(time.Minute)
	task := Task{
		ID: 7001, TaskNo: "PT7001", EventID: "event-7001", OrderID: 1000, ShopID: 100,
		EventType: "order_accepted", TemplateID: 9001, TemplateVersion: "v1",
		RenderPayload: datatypes.JSON(`{"schema_version":"receipt.v1"}`), PayloadSchemaVersion: "receipt.v1",
		Provider: "fake", Status: "processing", LockedBy: &owner, LockedUntil: &lockedUntil,
	}
	return db, setting, task
}

func newTestWorker(db *gorm.DB, provider Provider) *Worker {
	return NewWorker(
		config.CP1Config{DataEncryptionKey: printTestEncryptionKey}, db, snowflake.New(74), provider,
		"worker-test", slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

func assertWorkerTaskState(t *testing.T, db *gorm.DB, id uint64, status, providerRequestID string, attempts uint) {
	t.Helper()
	var persisted Task
	if err := db.First(&persisted, id).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.Status != status || persisted.ProviderRequestID == nil || *persisted.ProviderRequestID != providerRequestID || persisted.Attempts != attempts {
		t.Fatalf("unexpected task state: %+v", persisted)
	}
	if persisted.LockedBy != nil || persisted.LockedUntil != nil {
		t.Fatalf("completed task retained lease: %+v", persisted)
	}
	if status == "succeeded" && (persisted.SucceededAt == nil || persisted.ConfirmedAt == nil) {
		t.Fatalf("successful task missing terminal timestamps: %+v", persisted)
	}
}

func assertWorkerAudit(t *testing.T, db *gorm.DB, taskID uint64, action, result string) {
	t.Helper()
	var count int64
	if err := db.Table("audit_logs").Where("resource_id=? AND action=? AND result=?", taskID, action, result).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("audit %s/%s count=%d, want 1", action, result, count)
	}
}

type workerRecordingProvider struct {
	submitCalls  int
	queryCalls   int
	queryIDs     []string
	submitResult PrintResult
	submitErr    error
	queryResult  PrintResult
	queryErr     error
	beforeQuery  func(string)
}

func (p *workerRecordingProvider) Submit(_ context.Context, _ PrintRequest) (PrintResult, error) {
	p.submitCalls++
	return p.submitResult, p.submitErr
}

func (p *workerRecordingProvider) Query(_ context.Context, providerRequestID string) (PrintResult, error) {
	p.queryCalls++
	p.queryIDs = append(p.queryIDs, providerRequestID)
	if p.beforeQuery != nil {
		p.beforeQuery(providerRequestID)
	}
	return p.queryResult, p.queryErr
}
