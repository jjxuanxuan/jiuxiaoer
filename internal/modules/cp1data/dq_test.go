package cp1data

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestDefaultCheckIDsCoverPRD(t *testing.T) {
	want := []string{"DQ-001", "DQ-002", "DQ-003", "DQ-004", "DQ-005", "DQ-006", "DQ-007", "DQ-008", "DQ-009", "DQ-010"}
	if got := DefaultCheckIDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("default checks = %v, want %v", got, want)
	}
}

func TestAmountChecksReportExactViolations(t *testing.T) {
	db := newDQDB(t,
		`CREATE TABLE orders (id INTEGER PRIMARY KEY,goods_amount INTEGER,discount_amount INTEGER,delivery_fee_amount INTEGER,payable_amount INTEGER,deleted_at DATETIME)`,
		`CREATE TABLE order_items (id INTEGER PRIMARY KEY,order_id INTEGER,total_amount INTEGER,deleted_at DATETIME)`,
	)
	mustDQExec(t, db, `INSERT INTO orders(id,goods_amount,discount_amount,delivery_fee_amount,payable_amount) VALUES (1,2000,100,100,1900),(2,1000,0,0,1000)`)
	mustDQExec(t, db, `INSERT INTO order_items(id,order_id,total_amount) VALUES (11,1,1500),(12,2,1000)`)
	checker, err := NewChecker(db, DQOptions{CheckIDs: []string{"DQ-003", "DQ-004"}, SampleLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	report, err := checker.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || len(report.Results) != 2 || report.Results[0].Violations != 1 || report.Results[1].Violations != 1 {
		t.Fatalf("unexpected amount report: %+v", report)
	}
	if report.Results[0].Samples[0].ObjectID != "1" || report.Results[1].Samples[0].ObjectID != "1" {
		t.Fatalf("wrong samples: %+v", report.Results)
	}
}

func TestStockCheckValidatesAvailableAndTotalLedgers(t *testing.T) {
	db := newDQDB(t,
		`CREATE TABLE product_stocks (id INTEGER PRIMARY KEY,shop_product_id INTEGER,available_qty INTEGER,reserved_qty INTEGER,locked_qty INTEGER,deleted_at DATETIME)`,
		`CREATE TABLE stock_records (
			id INTEGER PRIMARY KEY,shop_product_id INTEGER,quantity_delta INTEGER,
			before_available_qty INTEGER,after_available_qty INTEGER,total_quantity_delta INTEGER,
			before_total_qty INTEGER,after_total_qty INTEGER,created_at DATETIME,deleted_at DATETIME
		)`,
	)
	mustDQExec(t, db, `INSERT INTO product_stocks VALUES (1,10,8,0,0,NULL)`)
	// 较新的账本记录刻意使用更小的 ID。DQ-002 必须按 (created_at,id)
	// 选择最新事实，而不能使用 MAX(id)。
	mustDQExec(t, db, `INSERT INTO stock_records VALUES
		(200,10,-2,10,8,0,10,10,'2026-07-22 10:00:00',NULL),
		(100,10,0,8,8,-2,10,8,'2026-07-22 11:00:00',NULL)`)
	checker, err := NewChecker(db, DQOptions{CheckIDs: []string{"DQ-002"}, SampleLimit: 20})
	if err != nil {
		t.Fatal(err)
	}
	report, err := checker.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.Results[0].Violations != 0 {
		t.Fatalf("valid available/total ledgers did not pass: %+v", report)
	}
}

func TestStockCheckReportsEveryLedgerInvariant(t *testing.T) {
	db := newDQDB(t,
		`CREATE TABLE product_stocks (id INTEGER PRIMARY KEY,shop_product_id INTEGER,available_qty INTEGER,reserved_qty INTEGER,locked_qty INTEGER,deleted_at DATETIME)`,
		`CREATE TABLE stock_records (
			id INTEGER PRIMARY KEY,shop_product_id INTEGER,quantity_delta INTEGER,
			before_available_qty INTEGER,after_available_qty INTEGER,total_quantity_delta INTEGER,
			before_total_qty INTEGER,after_total_qty INTEGER,created_at DATETIME,deleted_at DATETIME
		)`,
	)
	mustDQExec(t, db, `INSERT INTO product_stocks VALUES
		(1,1,-1,0,0,NULL),
		(2,2,8,2,0,NULL),
		(3,3,9,1,0,NULL),
		(4,4,8,1,0,NULL),
		(5,5,7,3,0,NULL),
		(6,6,5,2,0,NULL),
		(7,7,7,3,0,NULL),
		(8,8,7,2,0,NULL)`)
	mustDQExec(t, db, `INSERT INTO stock_records VALUES
		(20,2,-2,10,8,0,NULL,NULL,'2026-07-22 10:00:00',NULL),
		(30,3,-2,10,9,0,10,10,'2026-07-22 10:00:00',NULL),
		(40,4,-2,10,8,-2,10,9,'2026-07-22 10:00:00',NULL),
		(50,5,-2,10,8,0,10,10,'2026-07-22 10:00:00',NULL),
		(51,5,-2,9,7,0,10,10,'2026-07-22 11:00:00',NULL),
		(60,6,0,5,5,-2,10,8,'2026-07-22 10:00:00',NULL),
		(61,6,0,5,5,-2,9,7,'2026-07-22 11:00:00',NULL),
		(70,7,-2,10,8,0,10,10,'2026-07-22 10:00:00',NULL),
		(80,8,-3,10,7,0,10,10,'2026-07-22 10:00:00',NULL)`)
	checker, err := NewChecker(db, DQOptions{CheckIDs: []string{"DQ-002"}, SampleLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	report, err := checker.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || report.Results[0].Violations != 8 {
		t.Fatalf("unexpected stock invariant report: %+v", report)
	}
	wantCodes := map[string]bool{
		"STOCK_NEGATIVE":                          true,
		"STOCK_RECORD_TOTAL_FIELDS_NULL":          true,
		"STOCK_AVAILABLE_EQUATION_MISMATCH":       true,
		"STOCK_TOTAL_EQUATION_MISMATCH":           true,
		"STOCK_AVAILABLE_DISCONTINUITY":           true,
		"STOCK_TOTAL_DISCONTINUITY":               true,
		"STOCK_LEDGER_CURRENT_AVAILABLE_MISMATCH": true,
		"STOCK_LEDGER_CURRENT_TOTAL_MISMATCH":     true,
	}
	for _, sample := range report.Results[0].Samples {
		delete(wantCodes, sample.Code)
	}
	if len(wantCodes) != 0 {
		t.Fatalf("missing stock invariant findings: %v; report=%+v", wantCodes, report)
	}
}

func TestReceiptCheckComparesImmutableAmountsAndItems(t *testing.T) {
	db := newDQDB(t,
		`CREATE TABLE print_tasks (id INTEGER PRIMARY KEY,order_id INTEGER,shop_id INTEGER,template_version TEXT,payload_schema_version TEXT,render_payload JSON)`,
		`CREATE TABLE orders (id INTEGER PRIMARY KEY,order_no TEXT,shop_id INTEGER,goods_amount INTEGER,discount_amount INTEGER,delivery_fee_amount INTEGER,payable_amount INTEGER,paid_amount INTEGER,deleted_at DATETIME)`,
		`CREATE TABLE order_items (id INTEGER PRIMARY KEY,order_id INTEGER,shop_product_id INTEGER,product_id INTEGER,product_snapshot JSON,quantity INTEGER,sale_price_amount INTEGER,total_amount INTEGER,deleted_at DATETIME)`,
	)
	mustDQExec(t, db, `INSERT INTO orders VALUES (1,'O1',10,1000,0,0,1000,1000,NULL)`)
	mustDQExec(t, db, `INSERT INTO order_items VALUES (2,1,20,30,'{"name":"下单商品","brand_name":"品牌","spec":"750ml"}',1,1000,1000,NULL)`)
	payload := map[string]any{
		"schema_version": "receipt.v1", "template_version": "v1",
		"order":   map[string]any{"order_id": "1", "order_no": "O1"},
		"shop":    map[string]any{"shop_id": "10"},
		"items":   []map[string]any{{"product_id": "30", "shop_product_id": "20", "name": "下单商品", "brand_name": "品牌", "spec": "750ml", "unit_price_amount": 1000, "quantity": 1, "total_amount": 1000}},
		"amounts": map[string]any{"goods": 1000, "discount": 0, "delivery_fee": 0, "payable": 999, "paid": 1000},
	}
	raw, _ := json.Marshal(payload)
	mustDQExec(t, db, `INSERT INTO print_tasks VALUES (3,1,10,'v1','receipt.v1',?)`, string(raw))
	checker, err := NewChecker(db, DQOptions{CheckIDs: []string{"DQ-005"}, SampleLimit: 10, BatchSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	report, err := checker.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || report.Results[0].Violations != 1 || report.Results[0].Samples[0].ObjectID != "3" {
		t.Fatalf("receipt mismatch was not reported: %+v", report)
	}
}

func TestVerificationCheckBlocksWithoutExplicitCutover(t *testing.T) {
	db := newDQDB(t)
	checker, err := NewChecker(db, DQOptions{CheckIDs: []string{"DQ-007"}})
	if err != nil {
		t.Fatal(err)
	}
	report, err := checker.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || report.Results[0].Status != "blocked" {
		t.Fatalf("missing cutover did not block DQ-007: %+v", report)
	}
}

func TestVerificationAuditCoverageRequiresDeliveryStage(t *testing.T) {
	audit := VerificationAudit{
		Completed: true,
		Mappings: []VerificationMapping{{
			DeliveryOrderID: 88,
			Stage:           "pickup",
			Action:          "mapped_to_observe",
		}},
	}
	if audit.containsDelivery(88) {
		t.Fatal("pickup-only mapping incorrectly covered a historical completion")
	}
	audit.Mappings = append(audit.Mappings, VerificationMapping{DeliveryOrderID: 88, Stage: "delivery", Action: "mapped_to_observe"})
	if !audit.containsDelivery(88) {
		t.Fatal("delivery-stage mapping did not cover a historical completion")
	}
}

func TestVerificationCheckBlocksAuditFromDifferentCutover(t *testing.T) {
	db := newDQDB(t)
	cutover := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	auditCutover := cutover.Add(-time.Minute)
	checker, err := NewChecker(db, DQOptions{
		CheckIDs:              []string{"DQ-007"},
		VerificationCutoverAt: &cutover,
		VerificationAudit: &VerificationAudit{
			SchemaVersion: "cp1.verification-migration-audit.v1",
			Completed:     true,
			CutoverAt:     auditCutover,
			MappingReason: "legacy evidence is unproven",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := checker.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || report.Results[0].Status != "blocked" {
		t.Fatalf("audit from a different cutover was accepted: %+v", report)
	}
}

func TestVerificationCheckReportsHistoricalOrphanAttempt(t *testing.T) {
	db := newDQDB(t,
		`CREATE TABLE delivery_orders (id INTEGER PRIMARY KEY,status TEXT,completed_at DATETIME,updated_at DATETIME,created_at DATETIME,deleted_at DATETIME)`,
		`CREATE TABLE delivery_verifications (id INTEGER PRIMARY KEY,delivery_order_id INTEGER,stage TEXT,status TEXT,mode_snapshot TEXT,verified_at DATETIME,created_at DATETIME)`,
		`CREATE TABLE delivery_verification_attempts (id INTEGER PRIMARY KEY,verification_id INTEGER,delivery_order_id INTEGER,mode_snapshot TEXT,created_at DATETIME)`,
		`CREATE TABLE admin_override_approvals (id INTEGER PRIMARY KEY,action TEXT,resource_type TEXT,resource_id INTEGER,status TEXT,approved_at DATETIME,maker_admin_id INTEGER,checker_admin_id INTEGER)`,
	)
	cutover := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	mustDQExec(t, db, `INSERT INTO delivery_verification_attempts VALUES (1,999,88,'observe',?)`, cutover.Add(-time.Hour))
	checker, err := NewChecker(db, DQOptions{CheckIDs: []string{"DQ-007"}, SampleLimit: 10, VerificationCutoverAt: &cutover})
	if err != nil {
		t.Fatal(err)
	}
	report, err := checker.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || report.Results[0].Violations != 1 || report.Results[0].Samples[0].Code != "HISTORICAL_ORPHAN_VERIFICATION_ATTEMPT" {
		t.Fatalf("historical orphan attempt was not reported: %+v", report)
	}
}

func TestPermissionMatrixDetectsExcessMerchantGrant(t *testing.T) {
	db := newDQDB(t,
		`CREATE TABLE roles (id INTEGER PRIMARY KEY,code TEXT,status TEXT,deleted_at DATETIME)`,
		`CREATE TABLE permissions (id INTEGER PRIMARY KEY,code TEXT,status TEXT,deleted_at DATETIME)`,
		`CREATE TABLE role_permissions (id INTEGER PRIMARY KEY,role_id INTEGER,permission_id INTEGER,deleted_at DATETIME)`,
	)
	roleIDs := map[string]uint64{"merchant_owner": 1, "merchant_order_operator": 2, "merchant_inventory_clerk": 3}
	for code, id := range roleIDs {
		mustDQExec(t, db, `INSERT INTO roles(id,code,status) VALUES (?,?, 'active')`, id, code)
	}
	permissionSet := map[string]struct{}{}
	for _, code := range phaseOnePermissionCodes {
		permissionSet[code] = struct{}{}
	}
	for _, codes := range merchantRoleMatrix {
		for _, code := range codes {
			permissionSet[code] = struct{}{}
		}
	}
	var codes []string
	for code := range permissionSet {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	permissionIDs := make(map[string]uint64, len(codes))
	for index, code := range codes {
		id := uint64(index + 100)
		permissionIDs[code] = id
		mustDQExec(t, db, `INSERT INTO permissions(id,code,status) VALUES (?,?, 'active')`, id, code)
	}
	linkID := uint64(1000)
	for roleCode, expected := range merchantRoleMatrix {
		for _, code := range expected {
			mustDQExec(t, db, `INSERT INTO role_permissions(id,role_id,permission_id) VALUES (?,?,?)`, linkID, roleIDs[roleCode], permissionIDs[code])
			linkID++
		}
	}
	checker, err := NewChecker(db, DQOptions{CheckIDs: []string{"DQ-010"}, SampleLimit: 20})
	if err != nil {
		t.Fatal(err)
	}
	report, err := checker.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed {
		t.Fatalf("exact matrix should pass: %+v", report)
	}
	mustDQExec(t, db, `INSERT INTO role_permissions(id,role_id,permission_id) VALUES (?,?,?)`, linkID, roleIDs["merchant_order_operator"], permissionIDs["inventory:adjust"])
	report, err = checker.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || report.Results[0].Violations != 1 || report.Results[0].Samples[0].Code != "ROLE_PERMISSION_EXCESS" {
		t.Fatalf("excess grant was not detected: %+v", report)
	}
}

func newDQDB(t *testing.T, statements ...string) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:cp1_dq_%d?mode=memory&cache=shared", time.Now().UnixNano())
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
	for _, statement := range statements {
		mustDQExec(t, db, statement)
	}
	return db
}

func mustDQExec(t *testing.T, db *gorm.DB, query string, args ...any) {
	t.Helper()
	if err := db.Exec(query, args...).Error; err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
