package auditlog

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/securevalue"
)

type auditFixture struct {
	ID           uint64 `gorm:"primaryKey"`
	EventID      *string
	ActorType    string
	ActorID      uint64
	AccountID    *uint64
	Action       string
	ResourceType string
	ResourceID   uint64
	ShopID       *uint64
	OrderID      *uint64
	DeliveryID   *uint64
	BeforeData   datatypes.JSON
	AfterData    datatypes.JSON
	Result       string
	ErrorCode    *string
	ReasonCode   *string
	BeforeStatus *string
	AfterStatus  *string
	Version      *uint64
	RequestID    *string
	IP           *string
	IPHash       *string
	UserAgent    *string
	CreatedAt    time.Time
}

func (auditFixture) TableName() string { return "audit_logs" }

func TestRegisterEnrichesTypedAndMapAuditWrites(t *testing.T) {
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&auditFixture{}); err != nil {
		t.Fatal(err)
	}
	if err := Register(db); err != nil {
		t.Fatal(err)
	}
	// 注册操作具备幂等性，因为应用测试可能基于同一个数据库句柄
	// 构造多个路由器。
	if err := Register(db); err != nil {
		t.Fatal(err)
	}

	ctx := requestctx.WithHTTPMeta(context.Background(), "203.0.113.9", "test-agent")
	ctx = requestctx.WithRequestID(ctx, "request-1")
	ctx = requestctx.WithAccountID(ctx, "42")
	rawIP := "198.51.100.7"
	wrongAccountID := uint64(999)
	row := auditFixture{
		ID: 1, ActorType: "merchant", ActorID: 7, Action: "store.order.accept",
		AccountID: &wrongAccountID, ResourceType: "order", ResourceID: 88,
		BeforeData: datatypes.JSON(`{"Status":"paid","Version":1,"ShopID":"77","AddressSnapshot":{"ContactPhone":"13800138000","AddressDetail":"secret address"}}`),
		AfterData:  datatypes.JSON(`{"Status":"accepted","Version":2,"ErrorCode":"ORDER_STATE_CONFLICT","ReasonCode":"manual","DeliveryCode":"123456","Reason":"customer phone 13800138000","ReviewRemark":"call customer","FailureDetail":"provider secret","LicenseNo":"license-1","Address":"exact address","ClientIP":"192.0.2.9"}`),
		Result:     "failed", IP: &rawIP, IPHash: &rawIP,
	}
	if err := db.WithContext(ctx).Create(&row).Error; err != nil {
		t.Fatal(err)
	}

	var got auditFixture
	if err := db.First(&got, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.EventID == nil || *got.EventID == "" || got.RequestID == nil || *got.RequestID != "request-1" {
		t.Fatalf("event/request identity not stamped: %+v", got)
	}
	if got.AccountID == nil || *got.AccountID != 42 || got.OrderID == nil || *got.OrderID != 88 {
		t.Fatalf("account/order dimensions not stamped: %+v", got)
	}
	if got.ShopID == nil || *got.ShopID != 77 {
		t.Fatalf("CamelCase shop dimension not extracted: %+v", got)
	}
	if got.BeforeStatus == nil || *got.BeforeStatus != "paid" || got.AfterStatus == nil || *got.AfterStatus != "accepted" || got.Version == nil || *got.Version != 2 {
		t.Fatalf("state dimensions not extracted: %+v", got)
	}
	if got.ErrorCode == nil || *got.ErrorCode != "ORDER_STATE_CONFLICT" || got.ReasonCode == nil || *got.ReasonCode != "manual" {
		t.Fatalf("error dimensions not extracted: %+v", got)
	}
	if got.IP != nil || got.IPHash == nil || *got.IPHash != securevalue.Digest("203.0.113.9") {
		t.Fatalf("raw IP was not replaced by request IP hash: %+v", got)
	}
	combinedAuditJSON := string(got.BeforeData) + string(got.AfterData)
	for _, forbidden := range []string{"13800138000", "secret address", "123456", "customer phone", "AddressSnapshot", "DeliveryCode", "ReviewRemark", "FailureDetail", "LicenseNo", "exact address", "192.0.2.9"} {
		if strings.Contains(combinedAuditJSON, forbidden) {
			t.Fatalf("sensitive audit value/key %q leaked: %s", forbidden, combinedAuditJSON)
		}
	}

	ctx = requestctx.WithAccountID(requestctx.WithHTTPMeta(context.Background(), "192.0.2.4", "test-agent"), "55")
	if err := db.WithContext(ctx).Table("audit_logs").Create(map[string]any{
		"id": uint64(2), "actor_type": "rider", "actor_id": uint64(9),
		"action": "delivery.complete", "resource_type": "delivery_order", "resource_id": uint64(99),
		"after_data": datatypes.JSON(`{"status":"completed"}`), "result": "success",
	}).Error; err != nil {
		t.Fatal(err)
	}
	got = auditFixture{}
	if err := db.First(&got, 2).Error; err != nil {
		t.Fatal(err)
	}
	if got.EventID == nil || *got.EventID == "" || got.AccountID == nil || *got.AccountID != 55 || got.DeliveryID == nil || *got.DeliveryID != 99 || got.IPHash == nil || *got.IPHash != securevalue.Digest("192.0.2.4") {
		t.Fatalf("map audit write was not enriched: %+v", got)
	}
}

func TestRegisterRejectsBatchAuditWrites(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&auditFixture{}); err != nil {
		t.Fatal(err)
	}
	if err := Register(db); err != nil {
		t.Fatal(err)
	}
	err = db.Create(&[]auditFixture{
		{ID: 1, ActorType: "admin", Action: "one", ResourceType: "test", Result: "success"},
		{ID: 2, ActorType: "admin", Action: "two", ResourceType: "test", Result: "success"},
	}).Error
	if err == nil || !strings.Contains(err.Error(), "independent audit event") {
		t.Fatalf("batch audit error=%v", err)
	}
}

func TestNumericHTTPStatusIsNotPromotedToBusinessStatus(t *testing.T) {
	if got := findString(map[string]any{"status": json.Number("409")}, nil, "status"); got != "" {
		t.Fatalf("numeric HTTP status was promoted to business status: %q", got)
	}
}
