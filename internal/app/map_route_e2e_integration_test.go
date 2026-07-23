package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/modules/routeplanning"
)

// TestRiderLoginAcceptAndAmapNavigationE2E exercises the real rider-facing
// chain with a non-production Amap key. MySQL writes are wrapped in a rollback
// transaction and Redis uses an isolated test database.
func TestRiderLoginAcceptAndAmapNavigationE2E(t *testing.T) {
	if os.Getenv("JXE_RUN_AMAP_E2E") != "1" {
		t.Skip("set JXE_RUN_AMAP_E2E=1 with a non-production Amap Web Service key")
	}
	ctx := context.Background()
	cfg := config.Load()
	if cfg.MapRoute.AmapKey == "" {
		t.Fatal("JXE_MAP_ROUTE_AMAP_KEY is not configured")
	}
	cfg.MapRoute.Enabled = true
	cfg.MapRoute.Provider = "amap"
	cfg.MapRoute.CanaryRiderIDs = nil
	cfg.Dispatch.Enabled = true
	cfg.Dispatch.ModeOverride = "grab"
	cfg.Feature.SMSMockEnabled = true
	cfg.Feature.PaymentMockEnabled = true
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	db, err := mysql.Open(ctx, cfg.MySQL, log)
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()

	redisClient := goredis.NewClient(&goredis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: 11})
	if err := redisClient.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping Redis: %v", err)
	}
	defer redisClient.Close()
	if err := redisClient.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	defer redisClient.FlushDB(ctx)

	provider, err := routeplanning.NewAmapProvider(cfg.MapRoute.AmapBaseURL, cfg.MapRoute.AmapKey, cfg.MapRoute.Timeout)
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(Dependencies{Config: cfg, Log: log, DB: tx, Redis: redisClient, RouteProvider: provider})
	runID := fmt.Sprintf("%d", time.Now().UnixNano())

	phone := fmt.Sprintf("137%08d", time.Now().UnixNano()%100000000)
	performOK(t, router, http.MethodPost, "/api/v1/auth/customer/send-code", "", "", map[string]any{"phone": phone})
	customerLogin := performOK(t, router, http.MethodPost, "/api/v1/auth/customer/sms-login", "", "", map[string]any{"phone": phone, "code": "123456"})
	customerToken := stringValue(t, object(t, customerLogin["data"])["access_token"])
	address := performOK(t, router, http.MethodPost, "/api/v1/addresses", customerToken, "amap-e2e-address-"+runID, map[string]any{
		"contact_name": "Amap E2E", "contact_phone": phone,
		"province": "广东省", "city": "深圳市", "city_code": "440300", "district": "南山区", "district_code": "440305",
		"address_detail": "非生产固定测试地址", "location_source": "map_pin", "latitude": 22.552000, "longitude": 113.942000,
		"coordinate_system": "gcj02", "is_default": true,
	})
	addressID := stringValue(t, object(t, address["data"])["id"])
	created := performOK(t, router, http.MethodPost, "/api/v1/orders", customerToken, "amap-e2e-order-"+runID, map[string]any{
		"shop_id": "4201", "address_id": addressID,
		"items": []map[string]any{{"shop_product_id": "8001", "quantity": 1}},
	})
	orderID := stringValue(t, object(t, created["data"])["order_id"])
	performOK(t, router, http.MethodPost, "/api/v1/orders/"+orderID+"/pay/mock", customerToken, "amap-e2e-pay-"+runID, map[string]any{"channel": "mock"})
	openOrderGrab(t, cfg, tx, redisClient, log, orderID)

	performOK(t, router, http.MethodPost, "/api/v1/auth/rider/send-code", "", "", map[string]any{"phone": "13800000003"})
	riderLogin := performOK(t, router, http.MethodPost, "/api/v1/auth/rider/sms-login", "", "", map[string]any{"phone": "13800000003", "code": "123456"})
	riderData := object(t, riderLogin["data"])
	riderToken := stringValue(t, riderData["access_token"])
	assertPermission(t, object(t, riderData["profile"])["permissions"], "delivery:route")
	performOK(t, router, http.MethodPost, "/api/v1/delivery/riders/me/heartbeat", riderToken, "", map[string]any{
		"device_id": "amap-e2e-rider", "sequence": uint64(time.Now().UnixNano()), "captured_at": time.Now().Format(time.RFC3339Nano),
		"latitude": 22.541000, "longitude": 113.931000, "coordinate_system": "gcj02", "accuracy_m": 10,
	})
	deliveries := performOK(t, router, http.MethodGet, "/api/v1/delivery/orders?page_size=100", riderToken, "", nil)
	deliveryID := findDeliveryID(t, array(t, object(t, deliveries["data"])["items"]), orderID)
	accepted := performOK(t, router, http.MethodPost, "/api/v1/delivery/orders/"+deliveryID+"/accept", riderToken, "amap-e2e-accept-"+runID, map[string]any{"expected_assignment_version": 1})
	if status := stringValue(t, object(t, accepted["data"])["status"]); status != "accepted" {
		t.Fatalf("accepted delivery status=%s", status)
	}
	assertAmapRoute(t, router, riderToken, deliveryID, "pickup", 22.540000, 113.930000)

	merchantLogin := performOK(t, router, http.MethodPost, "/api/v1/auth/merchant/login", "", "", map[string]any{"username": "merchant_demo", "password": "merchant123"})
	merchantToken := stringValue(t, object(t, merchantLogin["data"])["access_token"])
	performStoreOrderAction(t, router, tx, orderID, "accept", merchantToken, "amap-e2e-store-accept-"+runID)
	performStoreOrderAction(t, router, tx, orderID, "start-preparing", merchantToken, "amap-e2e-store-start-"+runID)
	performStoreOrderAction(t, router, tx, orderID, "prepare", merchantToken, "amap-e2e-store-ready-"+runID)
	performOK(t, router, http.MethodPost, "/api/v1/delivery/orders/"+deliveryID+"/pickup", riderToken, "amap-e2e-pickup-"+runID, nil)
	assertAmapRoute(t, router, riderToken, deliveryID, "delivery", 22.552000, 113.942000)

	performOK(t, router, http.MethodPost, "/api/v1/delivery/orders/"+deliveryID+"/complete", riderToken, "amap-e2e-complete-"+runID, nil)
	status, body := perform(t, router, http.MethodGet, "/api/v1/delivery/orders/"+deliveryID+"/route", riderToken, "", nil)
	if status != http.StatusConflict || body["error_code"] != "DELIVERY_ROUTE_NOT_AVAILABLE" {
		t.Fatalf("completed delivery route status=%d body=%#v", status, body)
	}
}

func assertPermission(t *testing.T, value any, expected string) {
	t.Helper()
	for _, permission := range array(t, value) {
		if text, ok := permission.(string); ok && text == expected {
			return
		}
	}
	t.Fatalf("permission %s is missing: %#v", expected, value)
}

func assertAmapRoute(t *testing.T, router http.Handler, token, deliveryID, stage string, wantLatitude, wantLongitude float64) {
	t.Helper()
	response := performOK(t, router, http.MethodGet, "/api/v1/delivery/orders/"+deliveryID+"/route", token, "", nil)
	data := object(t, response["data"])
	if stringValue(t, data["stage"]) != stage || stringValue(t, data["provider"]) != "amap" || stringValue(t, data["source"]) != "provider" {
		t.Fatalf("unexpected %s route: %#v", stage, data)
	}
	if stringValue(t, data["coordinate_system"]) != "gcj02" || data["degraded"] != false {
		t.Fatalf("unsafe %s route coordinate/degraded state: %#v", stage, data)
	}
	destination := object(t, data["destination"])
	latitude, latOK := destination["latitude"].(float64)
	longitude, lngOK := destination["longitude"].(float64)
	if !latOK || !lngOK || math.Abs(latitude-wantLatitude) > 0.000001 || math.Abs(longitude-wantLongitude) > 0.000001 {
		t.Fatalf("%s route destination=%#v want %.6f,%.6f", stage, destination, wantLatitude, wantLongitude)
	}
	distance, ok := data["distance_m"].(float64)
	if !ok || distance <= 0 {
		t.Fatalf("invalid %s route distance: %#v", stage, data["distance_m"])
	}
	duration, ok := data["duration_seconds"].(float64)
	if !ok || duration <= 0 {
		t.Fatalf("invalid %s route duration: %#v", stage, data["duration_seconds"])
	}
	steps := len(array(t, data["steps"]))
	if steps == 0 || stringValue(t, data["polyline"]) == "" {
		t.Fatalf("%s route contains no steps", stage)
	}
	t.Logf("%s Amap route passed: distance_m=%.0f duration_seconds=%.0f steps=%d", stage, distance, duration, steps)
}
