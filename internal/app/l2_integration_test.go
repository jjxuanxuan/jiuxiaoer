package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
)

// TestL2ServiceAreaOrderAndHomeIntegration 验证 L2 服务区、订单和首页集成。
func TestL2ServiceAreaOrderAndHomeIntegration(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run local L2 integration test")
	}
	ctx := context.Background()
	cfg := config.Load()
	cfg.Service.EnforcementMode = "enforce"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := mysql.Open(ctx, cfg.MySQL, log)
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatalf("begin transaction: %v", tx.Error)
	}
	defer tx.Rollback()

	redisClient := goredis.NewClient(&goredis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: 13})
	if err := redisClient.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flush redis: %v", err)
	}
	defer redisClient.Close()
	defer redisClient.FlushDB(ctx)
	router := NewRouter(Dependencies{Config: cfg, Log: log, DB: tx, Redis: redisClient})

	resolved := performOK(t, router, http.MethodGet, "/api/v1/service-shops/resolve?city_code=440300&lat=22.54&lng=113.93", "", "", nil)
	serviceShop := object(t, object(t, resolved["data"])["service_shop"])
	if got := stringValue(t, serviceShop["id"]); got != "4201" {
		t.Fatalf("expected seeded service shop 4201, got %s", got)
	}

	phone := fmt.Sprintf("136%08d", time.Now().UnixNano()%100000000)
	seedCustomerReadyForSMSLogin(t, tx, cfg, phone)
	performOK(t, router, http.MethodPost, "/api/v1/auth/customer/send-code", "", "", map[string]any{"phone": phone})
	login := performOK(t, router, http.MethodPost, "/api/v1/auth/customer/sms-login", "", "", map[string]any{"phone": phone, "code": "123456"})
	token := stringValue(t, object(t, login["data"])["access_token"])
	address := performOK(t, router, http.MethodPost, "/api/v1/addresses", token, "l2-test-address-01", map[string]any{
		"contact_name": "L2 test", "contact_phone": phone,
		"province": "广东省", "city": "深圳市", "city_code": "440300", "district": "南山区", "district_code": "440305",
		"address_detail": "科技园", "doorplate": "1号", "location_source": "map_pin",
		"latitude": 22.54, "longitude": 113.93, "coordinate_system": "gcj02",
	})
	addressID := stringValue(t, object(t, address["data"])["id"])
	created := performOK(t, router, http.MethodPost, "/api/v1/orders", token, "l2-test-order-0001", map[string]any{
		"shop_id": "4201", "address_id": addressID,
		"items": []map[string]any{{"shop_product_id": "8002", "quantity": 1}},
	})
	orderData := object(t, created["data"])
	if got := int64(l2Number(t, orderData["payable_amount"])); got != 6400 {
		t.Fatalf("expected payable amount 6400, got %d", got)
	}
	orderID := stringValue(t, orderData["order_id"])
	var snapshot string
	if err := tx.Table("orders").Select("delivery_promise_snapshot").Where("id = ?", orderID).Scan(&snapshot).Error; err != nil {
		t.Fatalf("read delivery snapshot: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(snapshot), &decoded); err != nil || decoded["service_area_version"] == nil {
		t.Fatalf("invalid delivery snapshot: %s err=%v", snapshot, err)
	}

	adminLogin := performOK(t, router, http.MethodPost, "/api/v1/auth/admin/login", "", "", map[string]any{"username": "admin", "password": cfg.Security.AdminBootstrapPassword})
	adminToken := stringValue(t, object(t, adminLogin["data"])["access_token"])
	slot := performOK(t, router, http.MethodPost, "/api/v1/admin/home-slots", adminToken, "l2-home-create-01", map[string]any{
		"city_code": "440300", "slot_type": "product_block", "slot_key": "l2_test", "title": "L2 test",
		"payload": map[string]any{"product_ids": []string{"7001"}}, "sort_order": 1,
	})
	slotData := object(t, slot["data"])
	slotID := stringValue(t, slotData["id"])
	version := int(l2Number(t, slotData["version"]))
	performOK(t, router, http.MethodPost, "/api/v1/admin/home-slots/"+slotID+"/status", adminToken, "l2-home-publish-1", map[string]any{"status": "published", "version": version})
	home := performOK(t, router, http.MethodGet, "/api/v1/home?city_code=440300", "", "", nil)
	homeData := object(t, home["data"])
	if len(array(t, homeData["slots"])) == 0 || len(array(t, homeData["product_blocks"])) == 0 {
		t.Fatal("published product block was not returned by home aggregation")
	}
}

// l2Number 返回 L2 测试编号。
func l2Number(t *testing.T, value any) float64 {
	t.Helper()
	number, ok := value.(float64)
	if !ok {
		t.Fatalf("expected JSON number, got %T", value)
	}
	return number
}
