package printjob

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/securevalue"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

const printTestEncryptionKey = "print-tests-012345678901234567890123456789"

func TestCreateSettingsIsScopedEncryptedAndIdempotent(t *testing.T) {
	service, db, _ := newPrintServiceFixture(t)
	claims := printMerchantClaims("77", []string{"100"}, "print_setting:update_shop")
	req := SettingCreateReq{
		ShopID: "100", Provider: "fake", DeviceID: "DEVICE-SECRET-001", TemplateID: "9001",
		Copies: 2, AutoPrintEvents: []string{"order_accepted"}, Enabled: false,
	}

	created, err := service.CreateSettings(context.Background(), claims, "POST", "/store/print-settings", "create-setting-0001", req)
	if err != nil {
		t.Fatalf("CreateSettings() error = %v", err)
	}
	replayed, err := service.CreateSettings(context.Background(), claims, "POST", "/store/print-settings", "create-setting-0001", req)
	if err != nil {
		t.Fatalf("idempotent CreateSettings() error = %v", err)
	}
	if !reflect.DeepEqual(created, replayed) {
		t.Fatalf("idempotent response changed: first=%+v replay=%+v", created, replayed)
	}
	if created.ShopID != "100" || created.DeviceIDMask != "DE***01" || created.Provider != "fake" {
		t.Fatalf("unexpected setting projection: %+v", created)
	}

	var rows []Setting
	if err := db.Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("idempotent create persisted %d settings", len(rows))
	}
	if strings.Contains(string(rows[0].DeviceIDCiphertext), req.DeviceID) {
		t.Fatalf("device identifier was stored in plaintext: %q", rows[0].DeviceIDCiphertext)
	}
	opened, err := securevalue.Open(printTestEncryptionKey, rows[0].DeviceIDCiphertext)
	if err != nil || opened != req.DeviceID {
		t.Fatalf("stored device identifier cannot be decrypted: value=%q err=%v", opened, err)
	}
	var auditCount int64
	if err := db.Table("audit_logs").Where("action='print_setting.create'").Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("expected one create audit, got %d", auditCount)
	}

	otherShop := printMerchantClaims("77", []string{"200"}, "print_setting:update_shop")
	_, err = service.CreateSettings(context.Background(), otherShop, "POST", "/store/print-settings", "create-setting-0002", req)
	detail := problem.FromError(err)
	if detail == nil || detail.Status != 403 || detail.ErrorCode != "SHOP_SCOPE_FORBIDDEN" {
		t.Fatalf("unauthorized shop create returned %#v", detail)
	}
}

func TestCreateSettingsRejectsPublishedIncompatibleTemplate(t *testing.T) {
	service, db, _ := newPrintServiceFixture(t)
	if err := db.Create(&Template{
		ID: 9002, TemplateCode: "legacy_receipt", Version: "v1", PaperWidthMM: 58,
		PayloadSchemaVersion: "legacy-v1", TemplateBody: "legacy", Status: "published", CreatedBy: 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	claims := printMerchantClaims("77", []string{"101"}, "print_setting:update_shop")
	req := SettingCreateReq{
		ShopID: "101", Provider: "fake", DeviceID: "DEVICE-SECRET-002", TemplateID: "9002",
		Copies: 1, AutoPrintEvents: []string{"order_accepted"}, Enabled: false,
	}
	_, err := service.CreateSettings(context.Background(), claims, "POST", "/store/print-settings", "create-setting-legacy", req)
	detail := problem.FromError(err)
	if detail == nil || detail.Status != 400 || detail.ErrorCode != "PRINT_TEMPLATE_INVALID" {
		t.Fatalf("incompatible published template returned %#v", detail)
	}
	var count int64
	if err := db.Model(&Setting{}).Where("shop_id=101").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("incompatible template created %d settings", count)
	}
}

func TestPatchSettingsAppliesOnlyPresentFieldsAndReplays(t *testing.T) {
	service, db, _ := newPrintServiceFixture(t)
	claims := printMerchantClaims("77", []string{"100"}, "print_setting:update_shop")
	created, err := service.CreateSettings(context.Background(), claims, "POST", "/store/print-settings", "create-setting-patch", SettingCreateReq{
		ShopID: "100", Provider: "fake", DeviceID: "DEVICE-SECRET-001", TemplateID: "9001",
		Copies: 2, AutoPrintEvents: []string{"order_accepted"}, Enabled: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	var before Setting
	if err := db.First(&before, created.ID).Error; err != nil {
		t.Fatal(err)
	}

	enabled := true
	req := SettingPatchReq{Enabled: &enabled, Version: created.Version}
	patched, err := service.PatchSettings(context.Background(), claims, "PATCH", "/store/print-settings/:id", "patch-setting-0001", created.ID, req)
	if err != nil {
		t.Fatalf("partial patch failed: %v", err)
	}
	replayed, err := service.PatchSettings(context.Background(), claims, "PATCH", "/store/print-settings/:id", "patch-setting-0001", created.ID, req)
	if err != nil || !reflect.DeepEqual(patched, replayed) {
		t.Fatalf("partial patch replay changed: first=%+v replay=%+v err=%v", patched, replayed, err)
	}
	var after Setting
	if err := db.First(&after, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !after.Enabled || after.Version != 2 {
		t.Fatalf("partial patch did not update enabled/version: %+v", after)
	}
	if after.Provider != before.Provider || after.TemplateID != before.TemplateID || after.Copies != before.Copies || after.DeviceIDMask != before.DeviceIDMask || !reflect.DeepEqual(after.DeviceIDCiphertext, before.DeviceIDCiphertext) || string(after.AutoPrintEvents) != string(before.AutoPrintEvents) {
		t.Fatalf("partial patch overwrote omitted fields: before=%+v after=%+v", before, after)
	}

	_, err = service.PatchSettings(context.Background(), claims, "PATCH", "/store/print-settings/:id", "patch-setting-empty", created.ID, SettingPatchReq{Version: 2})
	if got := problem.FromError(err).ErrorCode; got != "PRINT_SETTING_NO_CHANGES" {
		t.Fatalf("empty patch error = %q", got)
	}
}

func TestListStoreTasksWithNoAuthorizedShopsReturnsEmpty(t *testing.T) {
	service, db, _ := newPrintServiceFixture(t)
	if err := db.Create(&Task{
		ID: 7001, TaskNo: "PT7001", EventID: "event-7001", OrderID: 3001,
		ShopID: 100, EventType: "order_accepted", TemplateID: 9001,
		TemplateVersion: "v1", PayloadSchemaVersion: "receipt.v1",
		Provider: "fake", Status: "pending",
	}).Error; err != nil {
		t.Fatal(err)
	}

	claims := printMerchantClaims("77", []string{}, "print_task:list_shop")
	items, next, err := service.ListStoreTasks(context.Background(), claims, pagination.Query{PageSize: 20}, "")
	if err != nil {
		t.Fatalf("ListStoreTasks() error = %v", err)
	}
	if len(items) != 0 || next != "" {
		t.Fatalf("empty shop scope leaked print tasks: items=%+v next=%q", items, next)
	}
}

func TestListStoreTasksUsesStableCreatedAtIDKeyset(t *testing.T) {
	service, db, _ := newPrintServiceFixture(t)
	base := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	rows := make([]Task, 0, 4)
	for index, id := range []uint64{7001, 7002, 7003, 7004} {
		createdAt := base.Add(time.Duration(index+1) * time.Minute)
		rows = append(rows, Task{
			ID: id, TaskNo: fmt.Sprintf("PT%d", id), EventID: fmt.Sprintf("event-%d", id),
			OrderID: id + 1000, ShopID: 100, EventType: "order_accepted",
			TemplateID: 9001, TemplateVersion: "v1", PayloadSchemaVersion: "receipt.v1",
			Provider: "fake", Status: "pending", CreatedAt: createdAt, UpdatedAt: createdAt,
		})
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	shopIDs := []uint64{100}
	first, next, err := service.listTasks(context.Background(), pagination.Query{PageSize: 2}, "", shopIDs)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].ID != "7004" || first[1].ID != "7003" || next == "" {
		t.Fatalf("unexpected first print-task page: items=%+v next=%q", first, next)
	}

	// Inserting a newer task and deleting an already consumed task must not
	// shift the continuation window after the last item on page one.
	if err := db.Delete(&Task{}, 7004).Error; err != nil {
		t.Fatal(err)
	}
	newer := base.Add(5 * time.Minute)
	if err := db.Create(&Task{
		ID: 7005, TaskNo: "PT7005", EventID: "event-7005", OrderID: 8005,
		ShopID: 100, EventType: "order_accepted", TemplateID: 9001,
		TemplateVersion: "v1", PayloadSchemaVersion: "receipt.v1", Provider: "fake",
		Status: "pending", CreatedAt: newer, UpdatedAt: newer,
	}).Error; err != nil {
		t.Fatal(err)
	}
	second, _, err := service.listTasks(context.Background(), pagination.Query{
		PageSize: 2,
		Cursor:   []string{base.Add(3 * time.Minute).Format(time.RFC3339Nano), "7003"},
	}, "", shopIDs)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 || second[0].ID != "7002" || second[1].ID != "7001" {
		t.Fatalf("print-task keyset shifted after insert/delete: %+v", second)
	}
}

func TestRetryRejectsSameKeyBodyAcrossDifferentPrintTasks(t *testing.T) {
	service, db, _ := newPrintServiceFixture(t)
	if err := db.Exec(`CREATE TABLE outbox_events (
		id INTEGER PRIMARY KEY, event_id TEXT, event_type TEXT, aggregate_type TEXT,
		aggregate_id INTEGER, payload JSON, status TEXT, request_id TEXT
	)`).Error; err != nil {
		t.Fatal(err)
	}
	rows := []Task{
		{
			ID: 7101, TaskNo: "PT7101", EventID: "event-7101", OrderID: 8101,
			ShopID: 100, EventType: "order_accepted", TemplateID: 9001,
			TemplateVersion: "v1", PayloadSchemaVersion: "receipt.v1", Provider: "fake", Status: "dead",
		},
		{
			ID: 7102, TaskNo: "PT7102", EventID: "event-7102", OrderID: 8102,
			ShopID: 100, EventType: "order_accepted", TemplateID: 9001,
			TemplateVersion: "v1", PayloadSchemaVersion: "receipt.v1", Provider: "fake", Status: "dead",
		},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	claims := &auth.Claims{AccountType: "admin", AdminUserID: "88", Permissions: []string{"print_task:retry_all"}}
	path, key := "/admin/print-tasks/:id/retry", "retry-cross-resource"
	request := RetryReq{Reason: "operator retry"}

	first, err := service.Retry(context.Background(), claims, "POST", path, key, "7101", request)
	if err != nil || first.ID != "7101" {
		t.Fatalf("first retry result=%+v err=%v", first, err)
	}
	second, err := service.Retry(context.Background(), claims, "POST", path, key, "7102", request)
	if detail := problem.FromError(err); detail == nil || detail.ErrorCode != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("cross-resource key reuse returned result=%+v problem=%+v", second, detail)
	}
	if second.ID != "" {
		t.Fatalf("retry of task B replayed task A response: %+v", second)
	}
	var taskB Task
	if err := db.First(&taskB, 7102).Error; err != nil {
		t.Fatal(err)
	}
	if taskB.Status != "dead" {
		t.Fatalf("rejected cross-resource retry mutated task B: status=%q", taskB.Status)
	}
}

func TestTestSettingsIsIdempotentScopedAndContainsNoPII(t *testing.T) {
	service, db, provider := newPrintServiceFixture(t)
	deviceID := "DEVICE-SECRET-001"
	sealed, err := securevalue.Seal(printTestEncryptionKey, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Setting{
		ID: 8001, ShopID: 100, Provider: "fake", DeviceIDCiphertext: sealed, DeviceIDMask: "DE***01",
		DeviceStatus: "unknown", TemplateID: 9001, Copies: 2,
		AutoPrintEvents: datatypes.JSON(`["order_accepted"]`), Enabled: false, Version: 1, CreatedBy: 77, UpdatedBy: 77,
	}).Error; err != nil {
		t.Fatal(err)
	}
	claims := printMerchantClaims("77", []string{"100"}, "print_setting:test_shop")
	path := "/store/print-settings/8001/test"

	first, err := service.TestSettings(context.Background(), claims, "POST", path, "test-setting-0001", "8001")
	if err != nil {
		t.Fatalf("TestSettings() error = %v", err)
	}
	second, err := service.TestSettings(context.Background(), claims, "POST", path, "test-setting-0001", "8001")
	if err != nil {
		t.Fatalf("idempotent TestSettings() error = %v", err)
	}
	if first != second {
		t.Fatalf("idempotent test response changed: first=%+v replay=%+v", first, second)
	}
	if provider.submitCalls != 1 {
		t.Fatalf("idempotent test submitted %d physical prints", provider.submitCalls)
	}
	if len(provider.submitted) != 1 || provider.submitted[0].ProviderRequestID != testProviderRequestID(77, 8001, "test-setting-0001") {
		t.Fatalf("provider request ID is not stable/scoped: %+v", provider.submitted)
	}
	payload := string(provider.submitted[0].Payload)
	for _, forbidden := range []string{deviceID, "13800138000", "张三", "pickup_code", "delivery_code", "latitude", "longitude"} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("test page leaked %q: %s", forbidden, payload)
		}
	}
	var page map[string]any
	if err := json.Unmarshal(provider.submitted[0].Payload, &page); err != nil {
		t.Fatalf("decode test page: %v", err)
	}
	if page["schema_version"] != "print.test.v1" || page["shop_id"] != "100" {
		t.Fatalf("unexpected test page: %#v", page)
	}

	var taskCount, attemptCount int64
	if err := db.Model(&Task{}).Where("event_type='test_print'").Count(&taskCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&Attempt{}).Count(&attemptCount).Error; err != nil {
		t.Fatal(err)
	}
	if taskCount != 1 || attemptCount != 1 {
		t.Fatalf("unexpected persisted test-print records: tasks=%d attempts=%d", taskCount, attemptCount)
	}
	var setting Setting
	if err := db.First(&setting, 8001).Error; err != nil {
		t.Fatal(err)
	}
	if setting.DeviceStatus != "online" || setting.LastHealthAt == nil || setting.LastHealthErrorCode != nil {
		t.Fatalf("device health was not updated: %+v", setting)
	}

	unauthorized := printMerchantClaims("77", []string{"200"}, "print_setting:test_shop")
	_, err = service.TestSettings(context.Background(), unauthorized, "POST", path, "test-setting-0002", "8001")
	detail := problem.FromError(err)
	if detail == nil || detail.Status != 404 || detail.ErrorCode != "PRINT_SETTING_NOT_FOUND" {
		t.Fatalf("BOLA request returned %#v", detail)
	}
	if provider.submitCalls != 1 {
		t.Fatalf("BOLA request reached provider, submit calls=%d", provider.submitCalls)
	}
}

func TestTestProviderRequestIDDoesNotCollideAcrossSettings(t *testing.T) {
	first := testProviderRequestID(77, 8001, "client-reused-key")
	second := testProviderRequestID(77, 8002, "client-reused-key")
	third := testProviderRequestID(78, 8001, "client-reused-key")
	if first == second || first == third || second == third {
		t.Fatalf("scoped provider request IDs collided: %q %q %q", first, second, third)
	}
	if first != testProviderRequestID(77, 8001, "client-reused-key") {
		t.Fatal("provider request ID is not deterministic")
	}
}

type printAuditFixture struct {
	ID           uint64
	ActorType    string
	ActorID      uint64
	Action       string
	ResourceType string
	ResourceID   *uint64
	BeforeData   datatypes.JSON
	AfterData    datatypes.JSON
	Result       string
	RequestID    *string
	CreatedAt    time.Time
}

func (printAuditFixture) TableName() string { return "audit_logs" }

func newPrintServiceFixture(t *testing.T) (*Service, *gorm.DB, *recordingPrintProvider) {
	t.Helper()
	dsn := fmt.Sprintf("file:print_service_%d?mode=memory&cache=shared", time.Now().UnixNano())
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
	if err := db.AutoMigrate(&Setting{}, &Task{}, &Attempt{}, &Template{}, &printAuditFixture{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE UNIQUE INDEX uk_test_print_setting_shop ON print_settings(shop_id)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Template{
		ID: 9001, TemplateCode: "store_receipt", Version: "v1", PaperWidthMM: 58,
		PayloadSchemaVersion: "receipt.v1", TemplateBody: "test", Status: "published", CreatedBy: 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	provider := &recordingPrintProvider{}
	service := NewService(config.CP1Config{PrintProvider: "fake", DataEncryptionKey: printTestEncryptionKey}, db, snowflake.New(72)).WithProvider(provider)
	service.idem = newMemoryIdempotency()
	return service, db, provider
}

func printMerchantClaims(userID string, shops []string, permissions ...string) *auth.Claims {
	return &auth.Claims{AccountType: "merchant", MerchantUserID: userID, AuthorizedShopIDs: shops, Permissions: permissions}
}

type recordingPrintProvider struct {
	submitCalls  int
	queryCalls   int
	submitted    []PrintRequest
	submitResult PrintResult
	submitErr    error
	queryResult  PrintResult
	queryErr     error
}

func (p *recordingPrintProvider) Submit(_ context.Context, req PrintRequest) (PrintResult, error) {
	p.submitCalls++
	p.submitted = append(p.submitted, req)
	if p.submitResult.Status == "" && p.submitErr == nil {
		return PrintResult{ProviderRequestID: req.ProviderRequestID, Status: "succeeded"}, nil
	}
	return p.submitResult, p.submitErr
}

func (p *recordingPrintProvider) Query(_ context.Context, providerRequestID string) (PrintResult, error) {
	p.queryCalls++
	if p.queryResult.Status == "" && p.queryErr == nil {
		return PrintResult{ProviderRequestID: providerRequestID, Status: "succeeded"}, nil
	}
	return p.queryResult, p.queryErr
}

type memoryIdempotency struct {
	mu      sync.Mutex
	records map[string]*memoryIdempotencyRecord
}

type memoryIdempotencyRecord struct {
	requestHash string
	status      string
	response    []byte
}

func newMemoryIdempotency() *memoryIdempotency {
	return &memoryIdempotency{records: make(map[string]*memoryIdempotencyRecord)}
}

func (m *memoryIdempotency) Start(_ context.Context, _ *gorm.DB, _ uint64, actorType string, actorID uint64, _ string, path, key, requestHash string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(key) < 8 || len(key) > 128 {
		return false, problem.InvalidArgument("IDEMPOTENCY_KEY_INVALID", "invalid key")
	}
	scope := fmt.Sprintf("%s:%d:%s:%s", actorType, actorID, path, key)
	if existing, ok := m.records[scope]; ok {
		if existing.requestHash != requestHash {
			return false, problem.Conflict("IDEMPOTENCY_KEY_REUSED", "request changed")
		}
		if existing.status == "succeeded" {
			return false, nil
		}
		return false, problem.Conflict("IDEMPOTENCY_IN_PROGRESS", "request is processing")
	}
	m.records[scope] = &memoryIdempotencyRecord{requestHash: requestHash, status: "processing"}
	return true, nil
}

func (m *memoryIdempotency) Succeed(_ context.Context, _ *gorm.DB, actorType string, actorID uint64, path, key string, response any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	scope := fmt.Sprintf("%s:%d:%s:%s", actorType, actorID, path, key)
	record := m.records[scope]
	if record == nil {
		return fmt.Errorf("idempotency record not found")
	}
	record.response, _ = json.Marshal(response)
	record.status = "succeeded"
	return nil
}

func (m *memoryIdempotency) CachedResponse(_ context.Context, _ *gorm.DB, actorType string, actorID uint64, path, key string, target any) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	scope := fmt.Sprintf("%s:%d:%s:%s", actorType, actorID, path, key)
	record := m.records[scope]
	if record == nil || record.status != "succeeded" {
		return false, nil
	}
	return true, json.Unmarshal(record.response, target)
}
