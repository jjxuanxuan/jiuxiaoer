package printjob

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestEnqueueAutoBuildsAndPersistsImmutableReceiptV1(t *testing.T) {
	db := newReceiptTestDB(t)
	now := time.Date(2026, 7, 22, 9, 30, 0, 0, time.UTC)
	addressSnapshot := `{
		"contact_name":"张三","contact_phone":"13800138000",
		"province":"上海市","city":"上海市","district":"浦东新区",
		"address_detail":"世纪大道100号","doorplate":"8楼801",
		"formatted_address":"上海市浦东新区世纪大道100号8楼801",
		"latitude":31.230416,"longitude":121.473701,
		"pickup_code":"654321","delivery_code":"123456","identity_number":"secret-id"
	}`
	productSnapshot := `{
		"name":"下单时商品名","brand_name":"下单时品牌","spec":"750ml",
		"image_url":"https://example.test/old.jpg","age_restricted":true,
		"provider_secret":"must-not-print"
	}`
	if err := db.Exec(`INSERT INTO shops (id,name,address,phone,deleted_at) VALUES (100,'下单门店','世纪大道1号','02112345678',NULL)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO orders
		(id,order_no,shop_id,goods_amount,discount_amount,delivery_fee_amount,payable_amount,paid_amount,remark,address_snapshot,paid_at,created_at,deleted_at)
		VALUES (1000,'O202607220001',100,20000,1000,500,19500,19500,'请轻放',?,?,?,NULL)`, addressSnapshot, now.Add(5*time.Minute), now).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO order_items
		(id,order_id,shop_product_id,product_id,product_snapshot,quantity,sale_price_amount,total_amount,deleted_at)
		VALUES (2000,1000,501,601,?,2,10000,20000,NULL)`, productSnapshot).Error; err != nil {
		t.Fatal(err)
	}
	template := Template{
		ID: 9001, TemplateCode: "store_receipt", Version: "v1", PaperWidthMM: 58,
		PayloadSchemaVersion: "receipt.v1", TemplateBody: "receipt", Status: "published", CreatedBy: 1,
	}
	if err := db.Create(&template).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Setting{
		ID: 8001, ShopID: 100, Provider: "fake", DeviceIDCiphertext: []byte("sealed"), DeviceIDMask: "de***ce",
		DeviceStatus: "online", TemplateID: 9001, Copies: 1,
		AutoPrintEvents: datatypes.JSON(`["order_accepted"]`), Enabled: true, Version: 1, CreatedBy: 77, UpdatedBy: 77,
	}).Error; err != nil {
		t.Fatal(err)
	}

	ids := snowflake.New(73)
	maliciousEventPayload := map[string]any{
		"contact_phone": "13999999999", "pickup_code": "malicious-code", "latitude": 99.999,
	}
	if err := EnqueueAuto(context.Background(), db, ids, 100, 1000, "event-1", "order_accepted", maliciousEventPayload); err != nil {
		t.Fatalf("EnqueueAuto() error = %v", err)
	}

	var task Task
	if err := db.First(&task).Error; err != nil {
		t.Fatal(err)
	}
	if task.PayloadSchemaVersion != "receipt.v1" || task.TemplateVersion != "v1" || task.Status != "pending" {
		t.Fatalf("unexpected task metadata: %+v", task)
	}
	dto := taskDTO(task, template.PaperWidthMM)
	if dto.PayloadSchemaVersion != "receipt.v1" || dto.RenderSummary == nil || dto.RenderSummary.ItemKindCount != 1 || dto.RenderSummary.TotalQuantity != 2 || dto.RenderSummary.PayableAmount != 19500 || dto.RenderSummary.PaperWidthMM != 58 || len(dto.RenderSummary.ContentHash) != 64 {
		t.Fatalf("unsafe or incomplete render summary: %+v", dto)
	}
	dtoPayload, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"render_payload", "13800138000", "pickup_code", "delivery_code", "identity_number"} {
		if strings.Contains(string(dtoPayload), forbidden) {
			t.Fatalf("task DTO leaked %q: %s", forbidden, dtoPayload)
		}
	}
	originalPayload := append([]byte(nil), task.RenderPayload...)
	assertReceiptSnapshot(t, originalPayload)
	encoded := string(originalPayload)
	for _, forbidden := range []string{
		"13800138000", "13999999999", "张三", "31.230416", "121.473701", "99.999",
		"654321", "123456", "malicious-code", "secret-id", "must-not-print", "provider_secret", "identity_number",
	} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("receipt payload leaked %q: %s", forbidden, encoded)
		}
	}

	// 任务创建后修改所有实时源记录，不得改写已经持久化的打印快照。
	if err := db.Exec(`UPDATE shops SET name='新门店名',address='新地址',phone='13900000000' WHERE id=100`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`UPDATE orders SET address_snapshot='{"contact_name":"李四","contact_phone":"13700000000","formatted_address":"新地址"}' WHERE id=1000`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`UPDATE order_items SET product_snapshot='{"name":"当前商品名"}' WHERE id=2000`).Error; err != nil {
		t.Fatal(err)
	}
	if err := EnqueueAuto(context.Background(), db, ids, 100, 1000, "event-2", "order_accepted", nil); err != nil {
		t.Fatalf("duplicate EnqueueAuto() error = %v", err)
	}
	var tasks, outbox int64
	if err := db.Model(&Task{}).Count(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("outbox_events").Count(&outbox).Error; err != nil {
		t.Fatal(err)
	}
	if tasks != 1 || outbox != 1 {
		t.Fatalf("same first-print event duplicated records: tasks=%d outbox=%d", tasks, outbox)
	}
	var audits int64
	if err := db.Table("audit_logs").Where("action='print_task.enqueued' AND resource_id=?", task.ID).Count(&audits).Error; err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("auto print enqueue audit count=%d, want 1", audits)
	}
	var persisted Task
	if err := db.First(&persisted, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if string(persisted.RenderPayload) != string(originalPayload) {
		t.Fatalf("immutable payload changed:\nold=%s\nnew=%s", originalPayload, persisted.RenderPayload)
	}
}

func assertReceiptSnapshot(t *testing.T, raw []byte) {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	if payload["schema_version"] != "receipt.v1" || payload["template_version"] != "v1" {
		t.Fatalf("unexpected receipt envelope: %#v", payload)
	}
	order, _ := payload["order"].(map[string]any)
	if order["order_no"] != "O202607220001" || order["remark"] != "请轻放" {
		t.Fatalf("unexpected order snapshot: %#v", order)
	}
	shop, _ := payload["shop"].(map[string]any)
	if shop["name"] != "下单门店" || shop["phone_mask"] != "021****5678" {
		t.Fatalf("unexpected shop snapshot: %#v", shop)
	}
	items, _ := payload["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("unexpected receipt items: %#v", items)
	}
	item, _ := items[0].(map[string]any)
	if item["name"] != "下单时商品名" || item["brand_name"] != "下单时品牌" || item["spec"] != "750ml" || item["quantity"] != float64(2) {
		t.Fatalf("receipt did not use order item snapshot: %#v", item)
	}
	recipient, _ := payload["recipient"].(map[string]any)
	if recipient["name_mask"] != "张**" || recipient["phone_mask"] != "138****8000" || recipient["formatted_address"] != "上海市浦东新区世纪大道100号8楼801" {
		t.Fatalf("unexpected recipient projection: %#v", recipient)
	}
	amounts, _ := payload["amounts"].(map[string]any)
	if amounts["paid"] != float64(19500) || amounts["currency"] != "CNY" {
		t.Fatalf("unexpected amounts: %#v", amounts)
	}
}

func newReceiptTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:print_receipt_%d?mode=memory&cache=shared", time.Now().UnixNano())
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
		`CREATE TABLE shops (id INTEGER PRIMARY KEY,name TEXT NOT NULL,address TEXT NOT NULL,phone TEXT,deleted_at DATETIME)`,
		`CREATE TABLE orders (id INTEGER PRIMARY KEY,order_no TEXT NOT NULL,shop_id INTEGER NOT NULL,goods_amount INTEGER NOT NULL,discount_amount INTEGER NOT NULL,delivery_fee_amount INTEGER NOT NULL,payable_amount INTEGER NOT NULL,paid_amount INTEGER NOT NULL,remark TEXT,address_snapshot JSON,paid_at DATETIME,created_at DATETIME NOT NULL,deleted_at DATETIME)`,
		`CREATE TABLE order_items (id INTEGER PRIMARY KEY,order_id INTEGER NOT NULL,shop_product_id INTEGER NOT NULL,product_id INTEGER NOT NULL,product_snapshot JSON NOT NULL,quantity INTEGER NOT NULL,sale_price_amount INTEGER NOT NULL,total_amount INTEGER NOT NULL,deleted_at DATETIME)`,
		`CREATE TABLE outbox_events (id INTEGER PRIMARY KEY,event_id TEXT NOT NULL UNIQUE,event_type TEXT NOT NULL,aggregate_type TEXT NOT NULL,aggregate_id INTEGER NOT NULL,payload JSON NOT NULL,status TEXT NOT NULL,request_id TEXT,created_at DATETIME)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("prepare receipt fixture with %q: %v", statement, err)
		}
	}
	if err := db.AutoMigrate(&Setting{}, &Task{}, &Template{}, &printAuditFixture{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE UNIQUE INDEX uk_test_print_task_event ON print_tasks(shop_id,order_id,event_type,template_version,reprint_seq)`).Error; err != nil {
		t.Fatal(err)
	}
	return db
}
