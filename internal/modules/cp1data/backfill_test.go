package cp1data

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/modules/printjob"
)

func TestBackfillWriteGuard(t *testing.T) {
	base := BackfillOptions{
		Job: "print-tasks", BatchSize: 500, RowsPerSecond: 500,
		SampleLimit: 20, MaxRetries: 5,
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("dry-run should be valid: %v", err)
	}
	base.Execute = true
	base.CheckpointFile = filepath.Join(t.TempDir(), "checkpoint.json")
	if err := base.Validate(); err == nil {
		t.Fatal("write unexpectedly allowed without environment gate and confirmation")
	}
	base.AllowWrite = true
	base.Confirmation = WriteConfirmation
	if err := base.Validate(); err != nil {
		t.Fatalf("explicitly authorized write rejected: %v", err)
	}
	dry := base
	dry.Execute = false
	if dry.fingerprint() == base.fingerprint() {
		t.Fatal("dry-run checkpoint could be resumed as a write")
	}
}

func TestPrintTaskBackfillDryRunThenExecute(t *testing.T) {
	db := newBackfillDB(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	mustExec(t, db, `INSERT INTO shops(id,name,address,phone,deleted_at) VALUES (10,'门店','门店地址','02112345678',NULL)`)
	mustExec(t, db, `INSERT INTO orders(id,order_no,shop_id,goods_amount,discount_amount,delivery_fee_amount,payable_amount,paid_amount,address_snapshot,created_at,deleted_at)
		VALUES (20,'O20',10,2000,100,100,2000,2000,'{"contact_name":"张三","contact_phone":"13800138000","formatted_address":"上海市测试路1号"}',?,NULL)`, now)
	mustExec(t, db, `INSERT INTO order_items(id,order_id,shop_product_id,product_id,product_snapshot,quantity,sale_price_amount,total_amount,deleted_at)
		VALUES (30,20,40,50,'{"name":"下单商品","brand_name":"品牌","spec":"750ml"}',2,1000,2000,NULL)`)
	template := printjob.Template{ID: 9001, TemplateCode: "store_receipt", Version: "v1", PaperWidthMM: 58, PayloadSchemaVersion: "receipt.v1", TemplateBody: "receipt", Status: "published", CreatedBy: 1}
	if err := db.Create(&template).Error; err != nil {
		t.Fatal(err)
	}
	task := printjob.Task{ID: 100, TaskNo: "PT100", EventID: "event-100", OrderID: 20, ShopID: 10, EventType: "order_accepted", TemplateID: 77, TemplateVersion: "legacy", RenderPayload: []byte(`{"legacy":true}`), PayloadSchemaVersion: "legacy-v1", Provider: "fake", Status: "pending", CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}

	dryRunner, err := NewBackfiller(db, BackfillOptions{
		Job: "print-tasks", BatchSize: 500, RowsPerSecond: 10000, Range: IDRange{},
		FallbackTemplateID: 9001, SampleLimit: 20, MaxRetries: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	dryReport, err := dryRunner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !dryReport.DryRun || dryReport.Progress.Planned != 1 || dryReport.Progress.Updated != 0 {
		t.Fatalf("unexpected dry-run report: %+v", dryReport)
	}
	var unchanged printjob.Task
	if err := db.First(&unchanged, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if unchanged.PayloadSchemaVersion != "legacy-v1" {
		t.Fatalf("dry-run changed task: %+v", unchanged)
	}

	checkpoint := filepath.Join(t.TempDir(), "print-task.json")
	writeRunner, err := NewBackfiller(db, BackfillOptions{
		Job: "print-tasks", Execute: true, AllowWrite: true, Confirmation: WriteConfirmation,
		BatchSize: 500, RowsPerSecond: 10000, FallbackTemplateID: 9001,
		SampleLimit: 20, MaxRetries: 5, CheckpointFile: checkpoint,
	})
	if err != nil {
		t.Fatal(err)
	}
	writeReport, err := writeRunner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if writeReport.DryRun || writeReport.Progress.Updated != 1 || !writeReport.Completed {
		t.Fatalf("unexpected write report: %+v", writeReport)
	}
	var updated printjob.Task
	if err := db.First(&updated, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if updated.PayloadSchemaVersion != "receipt.v1" || updated.TemplateID != 9001 || updated.TemplateVersion != "v1" {
		t.Fatalf("task was not rebuilt: %+v", updated)
	}
	if _, err := LoadCheckpoint(checkpoint); err != nil {
		t.Fatalf("checkpoint is not readable: %v", err)
	}
	info, err := os.Stat(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("checkpoint permissions are not private: %o", info.Mode().Perm())
	}
}

func TestVerificationHistoryDoesNotDowngradeActiveCredential(t *testing.T) {
	db := newBackfillDB(t)
	cutover := time.Now().UTC()
	old := cutover.Add(-time.Hour)
	mustExec(t, db, `INSERT INTO delivery_verifications(id,delivery_order_id,stage,mode_snapshot,status,created_at) VALUES
		(101,201,'delivery','enforce','verified',?),
		(102,202,'pickup','enforce','active',?)`, old, old)
	mustExec(t, db, `INSERT INTO delivery_verification_attempts(id,verification_id,delivery_order_id,stage,mode_snapshot,created_at) VALUES
		(301,101,201,'delivery','enforce',?),
		(302,102,202,'pickup','enforce',?),
		(303,999,203,'delivery','enforce',?)`, old, old, old)
	checkpoint := filepath.Join(t.TempDir(), "verification.json")
	runner, err := NewBackfiller(db, BackfillOptions{
		Job: "verification-history", Execute: true, AllowWrite: true, Confirmation: WriteConfirmation,
		BatchSize: 500, RowsPerSecond: 10000, SampleLimit: 20, MaxRetries: 5,
		CheckpointFile: checkpoint, VerificationCutoverAt: &cutover, VerificationMappingReason: "pre-enforce history is unproven",
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Progress.Updated != 3 || len(report.Progress.Manual) != 2 {
		t.Fatalf("unexpected verification report: %+v", report.Progress)
	}
	if report.Progress.Manual[1].Code != "ORPHAN_HISTORICAL_VERIFICATION_ATTEMPT" || report.Progress.Manual[1].ObjectID != "303" {
		t.Fatalf("orphan attempt was not retained for manual repair: %+v", report.Progress.Manual)
	}
	type modeRow struct {
		ID   uint64
		Mode string
	}
	var verifications []modeRow
	if err := db.Table("delivery_verifications").Select("id,mode_snapshot AS mode").Order("id").Scan(&verifications).Error; err != nil {
		t.Fatal(err)
	}
	if verifications[0].Mode != "observe" || verifications[1].Mode != "enforce" {
		t.Fatalf("active credential was downgraded or terminal history not mapped: %+v", verifications)
	}
	var nonObserveAttempts int64
	if err := db.Table("delivery_verification_attempts").Where("mode_snapshot<>'observe'").Count(&nonObserveAttempts).Error; err != nil {
		t.Fatal(err)
	}
	if nonObserveAttempts != 1 {
		t.Fatalf("known attempts were not mapped or orphan was rewritten: %d", nonObserveAttempts)
	}
	audit := BuildVerificationAudit(report, cutover, "pre-enforce history is unproven")
	if len(audit.Manual) != 2 {
		t.Fatalf("manual repair queue was omitted from verification audit: %+v", audit)
	}
}

func TestPrintSettingBackfillMapsOrDisablesEveryInvalidSetting(t *testing.T) {
	db := newBackfillDB(t)
	template := printjob.Template{ID: 9001, TemplateCode: "store_receipt", Version: "v1", PaperWidthMM: 58, PayloadSchemaVersion: "receipt.v1", TemplateBody: "receipt", Status: "published", CreatedBy: 1}
	if err := db.Create(&template).Error; err != nil {
		t.Fatal(err)
	}
	settings := []printjob.Setting{
		{ID: 401, ShopID: 501, Provider: "fake", DeviceIDCiphertext: []byte("sealed"), DeviceIDMask: "a***b", DeviceStatus: "unknown", TemplateID: 77, Copies: 1, AutoPrintEvents: []byte(`["order_accepted"]`), Enabled: true, Version: 1, CreatedBy: 1, UpdatedBy: 1},
		{ID: 402, ShopID: 502, Provider: "fake", DeviceIDCiphertext: []byte("sealed"), DeviceIDMask: "c***d", DeviceStatus: "unknown", TemplateID: 78, Copies: 1, AutoPrintEvents: []byte(`["order_accepted"]`), Enabled: true, Version: 1, CreatedBy: 1, UpdatedBy: 1},
	}
	if err := db.Create(&settings).Error; err != nil {
		t.Fatal(err)
	}
	runner, err := NewBackfiller(db, BackfillOptions{
		Job: "print-settings", Execute: true, AllowWrite: true, Confirmation: WriteConfirmation,
		BatchSize: 500, RowsPerSecond: 10000, SampleLimit: 20, MaxRetries: 5,
		TemplateMap: map[uint64]uint64{78: 9001}, CheckpointFile: filepath.Join(t.TempDir(), "settings.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Progress.Updated != 2 || len(report.Progress.Manual) != 1 || report.Progress.Manual[0].ObjectID != "401" {
		t.Fatalf("unexpected setting report: %+v", report.Progress)
	}
	var rows []printjob.Setting
	if err := db.Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows[0].Enabled || rows[0].TemplateID != 77 || !rows[1].Enabled || rows[1].TemplateID != 9001 {
		t.Fatalf("settings were not safely mapped/disabled: %+v", rows)
	}
}

func newBackfillDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:cp1_backfill_%d?mode=memory&cache=shared", time.Now().UnixNano())
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
	statements := []string{
		`CREATE TABLE shops (id INTEGER PRIMARY KEY,name TEXT,address TEXT,phone TEXT,deleted_at DATETIME)`,
		`CREATE TABLE orders (id INTEGER PRIMARY KEY,order_no TEXT,shop_id INTEGER,goods_amount INTEGER,discount_amount INTEGER,delivery_fee_amount INTEGER,payable_amount INTEGER,paid_amount INTEGER,remark TEXT,address_snapshot JSON,paid_at DATETIME,created_at DATETIME,deleted_at DATETIME)`,
		`CREATE TABLE order_items (id INTEGER PRIMARY KEY,order_id INTEGER,shop_product_id INTEGER,product_id INTEGER,product_snapshot JSON,quantity INTEGER,sale_price_amount INTEGER,total_amount INTEGER,deleted_at DATETIME)`,
		`CREATE TABLE delivery_verifications (id INTEGER PRIMARY KEY,delivery_order_id INTEGER,stage TEXT,mode_snapshot TEXT,status TEXT,created_at DATETIME)`,
		`CREATE TABLE delivery_verification_attempts (id INTEGER PRIMARY KEY,verification_id INTEGER,delivery_order_id INTEGER,stage TEXT,mode_snapshot TEXT,created_at DATETIME)`,
	}
	for _, statement := range statements {
		mustExec(t, db, statement)
	}
	if err := db.AutoMigrate(&printjob.Setting{}, &printjob.Task{}, &printjob.Template{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE UNIQUE INDEX uk_test_print_event ON print_tasks(shop_id,order_id,event_type,template_version,reprint_seq)`).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func mustExec(t *testing.T, db *gorm.DB, query string, args ...any) {
	t.Helper()
	if err := db.Exec(query, args...).Error; err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
