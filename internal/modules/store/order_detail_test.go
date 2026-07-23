package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestRegisterRoutesIncludesStoreOrderDetail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRoutes(router.Group("/api/v1/store"), NewHandler(nil))
	for _, route := range router.Routes() {
		if route.Method == "GET" && route.Path == "/api/v1/store/orders/:id" {
			return
		}
	}
	t.Fatal("GET /api/v1/store/orders/:id was not registered")
}

func TestDetailOrderReturnsSafeMerchantProjection(t *testing.T) {
	service, db := storeDetailTestService(t)
	now := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	remark := "请轻放"
	phone := "13800138000"
	address := datatypes.JSON(fmt.Sprintf(`{
		"contact_name":"张三","contact_phone":%q,"province":"上海市","city":"上海市",
		"city_code":"310000","district":"浦东新区","district_code":"310115",
		"address_detail":"世纪大道100号","doorplate":"8楼801","formatted_address":"上海市浦东新区世纪大道100号",
		"latitude":31.1234567,"longitude":121.7654321,"coordinate_system":"gcj02",
		"location_source":"amap_poi","poi_id":"sensitive-poi","address_version":3
	}`, phone))
	promise := datatypes.JSON(`{
		"schema_version":2,"service_area_version":7,"selection_source":"automatic","resolved_at":"2026-07-22T09:50:00Z",
		"route":{"provider":"amap","raw_response":"must-not-leak"},
		"delivery_promise":{"delivery_fee_amount":500,"eta_min_minutes":20,"eta_max_minutes":40,
		"route_distance_m":2800,"route_duration_seconds":1500,"route_source":"amap","confirmed":true,
		"policy":{"code":"standard","version":1,"title":"标准配送","summary":"预计40分钟内送达"}}
	}`)
	compliance := datatypes.JSON(`{
		"policy_version":"cp1-v2","verification_id":"secret-verification-id","status":"verified",
		"adult_result":"adult","verification_level":"identity_and_liveness","checked_at":"2026-07-22T09:55:00Z",
		"age_restricted_shop_product_ids":["501"],"mode":"enforce","would_allow":true
	}`)
	if err := db.Create(&Shop{ID: 100, MerchantID: 1, Name: "浦东门店", City: "上海市", District: "浦东新区", Address: "世纪大道1号"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Order{
		ID: 1000, OrderNo: "O202607220001", CustomerID: 9, MerchantID: 1, ShopID: 100,
		Status: "accepted", PayStatus: "paid", DeliveryStatus: "accepted",
		GoodsAmount: 20000, DiscountAmount: 1000, DeliveryFeeAmount: 500, PayableAmount: 19500, PaidAmount: 19500,
		Remark: &remark, AddressSnapshot: address, DeliveryPromiseSnapshot: promise, ComplianceSnapshot: compliance,
		Version: 4, PaidAt: timePointer(now.Add(-5 * time.Minute)), CreatedAt: now.Add(-10 * time.Minute), UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	items := []OrderItem{
		{ID: 2001, OrderID: 1000, ShopProductID: 501, ProductID: 601, ProductSnapshot: datatypes.JSON(`{"name":"高端红酒","brand_name":"示例酒庄","spec":"750ml","image_url":"https://example.com/wine.jpg","age_restricted":true}`), Quantity: 2, SalePriceAmount: 8000, TotalAmount: 16000},
		{ID: 2002, OrderID: 1000, ShopProductID: 502, ProductID: 602, ProductSnapshot: datatypes.JSON(`{"name":"礼品袋","spec":"标准"}`), Quantity: 1, SalePriceAmount: 4000, TotalAmount: 4000},
	}
	if err := db.Create(&items).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Payment{ID: 3001, PaymentNo: "P202607220001", OrderID: 1000, Channel: "miniapp", Provider: "wechat", Status: "succeeded", Amount: 19500, Currency: "CNY", PaidAt: timePointer(now.Add(-5 * time.Minute)), CreatedAt: now.Add(-6 * time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	riderID := uint64(88)
	if err := db.Create(&DeliveryOrder{ID: 4001, OrderID: 1000, ShopID: 100, RiderID: &riderID, Status: "accepted", AssignmentVersion: 2, PickupReadyStatus: "waiting_store"}).Error; err != nil {
		t.Fatal(err)
	}
	rawLogRemark := "raw-log-remark-must-not-leak"
	rawRequestID := "request-id-must-not-leak"
	logs := []OrderLog{
		{ID: 5001, OrderID: 1000, ActorType: "customer", ActorID: 9, Action: "create", ToStatus: stringPtr("pending_payment"), Remark: &rawLogRemark, RequestID: &rawRequestID, CreatedAt: now.Add(-10 * time.Minute)},
		{ID: 5002, OrderID: 1000, ActorType: "merchant", ActorID: 77, Action: "store_accept", FromStatus: stringPtr("paid"), ToStatus: stringPtr("accepted"), CreatedAt: now.Add(-time.Minute)},
	}
	if err := db.Create(&logs).Error; err != nil {
		t.Fatal(err)
	}

	got, err := service.DetailOrder(context.Background(), merchantDetailClaims("1", []string{"100"}, true), "1000")
	if err != nil {
		t.Fatalf("DetailOrder() error = %v", err)
	}
	if got.ID != "1000" || got.Version != 4 || got.Status != "accepted" || got.PayStatus != "paid" || got.DeliveryStatus != "accepted" {
		t.Fatalf("unexpected order projection: %+v", got)
	}
	if got.ItemKindCount != 2 || got.TotalQuantity != 3 || len(got.Items) != 2 || got.Items[0].ImageURL == "" {
		t.Fatalf("unexpected item projection: %+v", got.Items)
	}
	if got.AddressSnapshot.SnapshotQuality != "complete" || got.AddressSnapshot.ContactNameMask != "张*" || got.CustomerContactMask != "138****8000" {
		t.Fatalf("unexpected address projection: %+v contact=%q", got.AddressSnapshot, got.CustomerContactMask)
	}
	if got.PaymentSummary == nil || got.PaymentSummary.PaymentNo != "P202607220001" || got.PaymentSummary.Status != "succeeded" {
		t.Fatalf("unexpected payment summary: %+v", got.PaymentSummary)
	}
	if got.DeliverySummary == nil || got.DeliverySummary.DeliveryOrderID != "4001" || got.DeliverySummary.RiderID != "88" || got.DeliverySummary.AssignmentVersion != 2 {
		t.Fatalf("unexpected delivery summary: %+v", got.DeliverySummary)
	}
	if got.DeliveryPromise == nil || got.DeliveryPromise.RouteSource != "amap" || got.ComplianceSummary.Status != "verified" {
		t.Fatalf("unexpected promise/compliance: promise=%+v compliance=%+v", got.DeliveryPromise, got.ComplianceSummary)
	}
	if len(got.RecentLogs) != 2 || got.RecentLogs[0].ID != "5002" {
		t.Fatalf("recent logs must be safe and newest first: %+v", got.RecentLogs)
	}

	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	response := string(encoded)
	for _, forbidden := range []string{phone, "31.1234567", "121.7654321", "sensitive-poi", "secret-verification-id", "raw_response", rawLogRemark, rawRequestID, `"actor_id"`} {
		if strings.Contains(response, forbidden) {
			t.Fatalf("merchant detail leaked %q: %s", forbidden, response)
		}
	}
}

func TestDetailOrderAuthorization(t *testing.T) {
	service, db := storeDetailTestService(t)
	if err := db.Create(&Shop{ID: 100, MerchantID: 1, Name: "A店", City: "上海市", District: "浦东新区", Address: "地址"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Order{ID: 1000, OrderNo: "O1", CustomerID: 9, MerchantID: 1, ShopID: 100, Status: "paid", PayStatus: "paid", DeliveryStatus: "pending_assign", CreatedAt: time.Now(), UpdatedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name   string
		claims *auth.Claims
		status int
		code   string
	}{
		{name: "missing view permission", claims: merchantDetailClaims("1", []string{"100"}, false), status: 403, code: "PERM_FORBIDDEN"},
		{name: "other merchant", claims: merchantDetailClaims("2", []string{"100"}, true), status: 404, code: "ORDER_NOT_FOUND"},
		{name: "unauthorized shop", claims: merchantDetailClaims("1", []string{"200"}, true), status: 404, code: "ORDER_NOT_FOUND"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.DetailOrder(context.Background(), test.claims, "1000")
			detail := problem.FromError(err)
			if detail == nil || detail.Status != test.status || detail.ErrorCode != test.code {
				t.Fatalf("unexpected error: %#v", detail)
			}
		})
	}
}

func storeDetailTestService(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	dsn := fmt.Sprintf("file:store_order_detail_%d?mode=memory&cache=shared", time.Now().UnixNano())
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
	if err := db.AutoMigrate(&Order{}, &OrderItem{}, &OrderLog{}, &Shop{}, &Payment{}, &DeliveryOrder{}); err != nil {
		t.Fatal(err)
	}
	return NewService(db, nil, snowflake.New(63)), db
}

func merchantDetailClaims(merchantID string, shopIDs []string, withView bool) *auth.Claims {
	permissions := []string(nil)
	if withView {
		permissions = []string{"store_order:view"}
	}
	return &auth.Claims{
		AccountType: "merchant", MerchantUserID: "77", MerchantID: merchantID,
		AuthorizedShopIDs: shopIDs, Permissions: permissions,
	}
}

func timePointer(value time.Time) *time.Time { return &value }
