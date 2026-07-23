package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestDeliveryDetailReturnsAssignedRiderFulfillmentView(t *testing.T) {
	service := newDeliveryDetailFixture(t)
	claims := &auth.Claims{
		AccountType: "rider",
		RiderID:     "40",
		Permissions: []string{"delivery:view_own"},
	}

	got, err := service.Detail(context.Background(), claims, "30")
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if got.ID != "30" || got.OrderID != "10" || got.ShopID != "20" || got.RiderID != "40" {
		t.Fatalf("unexpected delivery identity: %+v", got)
	}
	if got.Version != 7 || got.AssignmentVersion != 7 {
		t.Fatalf("version = %d, assignment_version = %d", got.Version, got.AssignmentVersion)
	}
	if got.Order.OrderNo != "ORDER-10" || got.Order.Version != 4 || got.Order.PayableAmount != 8800 {
		t.Fatalf("unexpected order detail: %+v", got.Order)
	}
	if got.Shop.Name != "当前门店名称" || got.Shop.Phone != "020-00000000" || got.Shop.CoordinateSystem != "gcj02" {
		t.Fatalf("unexpected shop detail: %+v", got.Shop)
	}
	if got.PickupContact.Name != "下单时门店名称" || got.PickupContact.Phone != "020-11111111" {
		t.Fatalf("pickup contact must come from snapshot: %+v", got.PickupContact)
	}
	if got.RecipientContact.Name != "李女士" || got.RecipientContact.Phone != "13800138000" || got.RecipientContact.FormattedAddress != "广州市天河区体育西路 1 号 1801" {
		t.Fatalf("unexpected recipient contact: %+v", got.RecipientContact)
	}
	if len(got.Items) != 1 || got.Items[0].Quantity != 2 || got.Items[0].ProductSnapshot.Name != "高端酒水 A" {
		t.Fatalf("unexpected item detail: %+v", got.Items)
	}
	payload, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"pickup_code", "delivery_code", "identity_number", "provider_secret"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("delivery detail leaked snapshot field %q: %s", forbidden, payload)
		}
	}
	if got.AcceptedAt == "" || got.StartedAt == "" || got.CreatedAt == "" || got.UpdatedAt == "" {
		t.Fatalf("status timestamps were not returned: %+v", got)
	}
}

func TestDeliveryDetailScopeIsIndistinguishableFromMissing(t *testing.T) {
	service := newDeliveryDetailFixture(t)

	tests := []struct {
		name       string
		riderID    string
		deliveryID string
	}{
		{name: "other rider", riderID: "41", deliveryID: "30"},
		{name: "unaccepted candidate", riderID: "40", deliveryID: "31"},
		{name: "revoked assignment", riderID: "40", deliveryID: "32"},
		{name: "another rider active assignment", riderID: "40", deliveryID: "33"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := &auth.Claims{AccountType: "rider", RiderID: tt.riderID, Permissions: []string{"delivery:view_own"}}
			_, err := service.Detail(context.Background(), claims, tt.deliveryID)
			assertDeliveryDetailProblem(t, err, http.StatusNotFound, "DELIVERY_NOT_FOUND")
		})
	}
}

func TestDeliveryDetailPermissionBoundary(t *testing.T) {
	service := newDeliveryDetailFixture(t)

	denied := []*auth.Claims{
		{AccountType: "rider", RiderID: "40", Permissions: []string{"delivery:list"}},
		{AccountType: "rider", RiderID: "40", Permissions: []string{"delivery:list", "delivery:accept"}},
		{AccountType: "rider", RiderID: "40", Permissions: []string{"delivery:update_status"}},
		{AccountType: "merchant", RiderID: "40", Permissions: []string{"delivery:view_own"}},
		{AccountType: "rider", RiderID: "", Permissions: []string{"delivery:view_own"}},
	}
	for _, claims := range denied {
		_, err := service.Detail(context.Background(), claims, "30")
		assertDeliveryDetailProblem(t, err, http.StatusForbidden, "PERM_FORBIDDEN")
	}
}

func TestRegisterRoutesIncludesDeliveryDetail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRoutes(router.Group("/api/v1/delivery"), NewHandler(nil))
	for _, route := range router.Routes() {
		if route.Method == http.MethodGet && route.Path == "/api/v1/delivery/orders/:id" {
			return
		}
	}
	t.Fatal("GET /api/v1/delivery/orders/:id was not registered")
}

func newDeliveryDetailFixture(t *testing.T) *Service {
	t.Helper()
	dsn := fmt.Sprintf("file:delivery_detail_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("database handle: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })

	statements := []string{
		`CREATE TABLE delivery_orders (
			id INTEGER PRIMARY KEY, order_id INTEGER NOT NULL, shop_id INTEGER NOT NULL, rider_id INTEGER,
			status TEXT NOT NULL, assignment_version INTEGER NOT NULL, dispatch_status TEXT NOT NULL,
			current_dispatch_job_id INTEGER, pickup_ready_status TEXT NOT NULL, pickup_ready_at DATETIME,
			pickup_snapshot JSON, recipient_snapshot JSON, accepted_at DATETIME, picked_up_at DATETIME,
			picked_up_verified_at DATETIME, started_at DATETIME, completed_at DATETIME,
			completed_verified_at DATETIME, cancelled_at DATETIME, created_at DATETIME, updated_at DATETIME,
			deleted_at DATETIME
		)`,
		`CREATE TABLE orders (
			id INTEGER PRIMARY KEY, order_no TEXT NOT NULL, customer_id INTEGER NOT NULL, merchant_id INTEGER NOT NULL,
			shop_id INTEGER NOT NULL, status TEXT NOT NULL, pay_status TEXT NOT NULL, delivery_status TEXT NOT NULL,
			goods_amount INTEGER NOT NULL, discount_amount INTEGER NOT NULL, delivery_fee_amount INTEGER NOT NULL,
			payable_amount INTEGER NOT NULL, paid_amount INTEGER NOT NULL, remark TEXT, address_snapshot JSON,
			version INTEGER NOT NULL, paid_at DATETIME, cancelled_at DATETIME, completed_at DATETIME,
			created_at DATETIME, updated_at DATETIME, deleted_at DATETIME
		)`,
		`CREATE TABLE shops (
			id INTEGER PRIMARY KEY, name TEXT NOT NULL, phone TEXT, province TEXT, city TEXT NOT NULL,
			district TEXT NOT NULL, address TEXT NOT NULL, latitude REAL, longitude REAL,
			coordinate_system TEXT, status TEXT NOT NULL, business_status TEXT NOT NULL, deleted_at DATETIME
		)`,
		`CREATE TABLE order_items (
			id INTEGER PRIMARY KEY, order_id INTEGER NOT NULL, shop_product_id INTEGER NOT NULL, product_id INTEGER NOT NULL,
			product_snapshot JSON NOT NULL, quantity INTEGER NOT NULL, sale_price_amount INTEGER NOT NULL,
			total_amount INTEGER NOT NULL, created_at DATETIME, updated_at DATETIME, deleted_at DATETIME
		)`,
		`CREATE TABLE delivery_assignments (
			id INTEGER PRIMARY KEY, delivery_order_id INTEGER NOT NULL, to_rider_id INTEGER NOT NULL, status TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("create fixture schema: %v", err)
		}
	}

	now := time.Date(2026, time.July, 22, 10, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	pickupSnapshot := `{"shop_id":"20","name":"下单时门店名称","phone":"020-11111111","province":"广东省","city":"广州市","district":"天河区","address":"天河北路 8 号","latitude":23.1,"longitude":113.3,"coordinate_system":"gcj02","pickup_code":"must-not-leak"}`
	recipientSnapshot := `{"contact_name":"李女士","contact_phone":"13800138000","province":"广东省","city":"广州市","district":"天河区","address_detail":"体育西路 1 号","doorplate":"1801","formatted_address":"广州市天河区体育西路 1 号 1801","latitude":23.2,"longitude":113.4,"coordinate_system":"gcj02","location_source":"amap_geocode","delivery_code":"must-not-leak","identity_number":"must-not-leak"}`
	if err := db.Exec(`INSERT INTO delivery_orders (
		id,order_id,shop_id,rider_id,status,assignment_version,dispatch_status,pickup_ready_status,pickup_ready_at,
		pickup_snapshot,recipient_snapshot,accepted_at,started_at,created_at,updated_at
	) VALUES (30,10,20,40,'delivering',7,'assigned','ready',?,?,?,?,?,?,?)`,
		now, pickupSnapshot, recipientSnapshot, now.Add(-30*time.Minute), now.Add(-20*time.Minute), now.Add(-time.Hour), now).Error; err != nil {
		t.Fatalf("insert owned delivery: %v", err)
	}
	if err := db.Exec(`INSERT INTO orders (
		id,order_no,customer_id,merchant_id,shop_id,status,pay_status,delivery_status,goods_amount,discount_amount,
		delivery_fee_amount,payable_amount,paid_amount,remark,address_snapshot,version,paid_at,created_at,updated_at
	) VALUES (10,'ORDER-10',1,2,20,'delivering','paid','delivering',9000,500,300,8800,8800,'请轻拿轻放',?,4,?,?,?)`,
		recipientSnapshot, now.Add(-50*time.Minute), now.Add(-time.Hour), now).Error; err != nil {
		t.Fatalf("insert order: %v", err)
	}
	if err := db.Exec(`INSERT INTO shops (
		id,name,phone,province,city,district,address,latitude,longitude,coordinate_system,status,business_status
	) VALUES (20,'当前门店名称','020-00000000','广东省','广州市','天河区','天河北路 10 号',23.1,113.3,'gcj02','active','open')`).Error; err != nil {
		t.Fatalf("insert shop: %v", err)
	}
	if err := db.Exec(`INSERT INTO order_items (
		id,order_id,shop_product_id,product_id,product_snapshot,quantity,sale_price_amount,total_amount,created_at,updated_at
	) VALUES (50,10,60,70,?,2,4500,9000,?,?)`, `{"name":"高端酒水 A","spec":"500ml","image_url":"https://example.test/a.png","age_restricted":true,"provider_secret":"must-not-leak","return_policy":{"eligible":true,"policy_code":"sealed-goods","policy_version":"1","sealed_package_required":true}}`, now, now).Error; err != nil {
		t.Fatalf("insert item: %v", err)
	}
	if err := db.Exec(`INSERT INTO delivery_assignments (id,delivery_order_id,to_rider_id,status) VALUES (80,30,40,'active')`).Error; err != nil {
		t.Fatalf("insert active assignment: %v", err)
	}

	if err := db.Exec(`INSERT INTO delivery_orders (id,order_id,shop_id,rider_id,status,assignment_version,dispatch_status,pickup_ready_status,created_at,updated_at)
		VALUES (31,11,20,NULL,'pending_assign',1,'grab_open','ready',?,?)`, now, now).Error; err != nil {
		t.Fatalf("insert candidate delivery: %v", err)
	}
	if err := db.Exec(`INSERT INTO delivery_orders (id,order_id,shop_id,rider_id,status,assignment_version,dispatch_status,pickup_ready_status,created_at,updated_at)
		VALUES (32,12,20,40,'accepted',2,'assigned','ready',?,?)`, now, now).Error; err != nil {
		t.Fatalf("insert revoked delivery: %v", err)
	}
	if err := db.Exec(`INSERT INTO delivery_assignments (id,delivery_order_id,to_rider_id,status) VALUES (81,32,40,'revoked')`).Error; err != nil {
		t.Fatalf("insert revoked assignment: %v", err)
	}
	if err := db.Exec(`INSERT INTO delivery_orders (id,order_id,shop_id,rider_id,status,assignment_version,dispatch_status,pickup_ready_status,created_at,updated_at)
		VALUES (33,13,20,41,'accepted',2,'assigned','ready',?,?)`, now, now).Error; err != nil {
		t.Fatalf("insert foreign delivery: %v", err)
	}
	if err := db.Exec(`INSERT INTO delivery_assignments (id,delivery_order_id,to_rider_id,status) VALUES (82,33,41,'active')`).Error; err != nil {
		t.Fatalf("insert foreign assignment: %v", err)
	}

	return NewService(db, nil)
}

func assertDeliveryDetailProblem(t *testing.T, err error, status int, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s", code)
	}
	details := problem.FromError(err)
	if details.Status != status || details.ErrorCode != code {
		t.Fatalf("problem = (%d, %s), want (%d, %s): %v", details.Status, details.ErrorCode, status, code, err)
	}
}
