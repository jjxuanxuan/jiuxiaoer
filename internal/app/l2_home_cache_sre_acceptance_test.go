package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	adminmodule "jiuxiaoer-admin/backend-go/internal/modules/admin"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
)

// TestL2HomeCacheAndSREMissingAcceptanceScenarios 验证 L2 首页缓存和可靠性缺失的验收场景。
func TestL2HomeCacheAndSREMissingAcceptanceScenarios(t *testing.T) {
	f := newL2AcceptanceFixture(t)
	defer f.close()

	createAndPublish := func(t *testing.T, body map[string]any) string {
		t.Helper()
		created := performOK(t, f.router, http.MethodPost, "/api/v1/admin/home-slots", f.adminToken, f.key("home-create"), body)
		data := object(t, created["data"])
		id := stringValue(t, data["id"])
		performOK(t, f.router, http.MethodPost, "/api/v1/admin/home-slots/"+id+"/status", f.adminToken, f.key("home-publish"), map[string]any{"status": "published", "version": int(l2Number(t, data["version"]))})
		return id
	}

	t.Run("ACC-L2-HOME-001-no-location-public-content", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			response := performOK(t, f.router, http.MethodGet, "/api/v1/home?city_code=440300", "", "", nil)
			data := object(t, response["data"])
			serviceability := object(t, data["serviceability"])
			if serviceability["serviceable"] != false || serviceability["reason_code"] != "LOCATION_REQUIRED" || len(array(t, data["categories"])) == 0 {
				t.Fatalf("no-location home response is incomplete: %#v", data)
			}
		})
	})

	t.Run("ACC-L2-HOME-002-location-resolves-service-shop", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			response := performOK(t, f.router, http.MethodGet, "/api/v1/home?city_code=440300&lat=22.54&lng=113.93", "", "", nil)
			data := object(t, response["data"])
			if object(t, data["serviceability"])["serviceable"] != true || object(t, data["service_shop"])["id"] != "4201" || data["delivery_promise"] == nil {
				t.Fatalf("serviceable home response is incomplete: %#v", data)
			}
		})
	})

	t.Run("ACC-L2-HOME-003-admin-rbac", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			status, response := perform(t, f.router, http.MethodGet, "/api/v1/admin/home-slots", f.merchantToken, "", nil)
			expectProblem(t, status, response, http.StatusForbidden, "PERM_FORBIDDEN")
		})
	})

	t.Run("ACC-L2-HOME-004-invalid-payload-and-reference", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			status, response := perform(t, f.router, http.MethodPost, "/api/v1/admin/home-slots", f.adminToken, f.key("invalid-payload"), map[string]any{
				"city_code": "440300", "slot_type": "product_block", "slot_key": uniqueName("invalid"), "title": "invalid", "payload": map[string]any{"product_ids": "7001"},
			})
			expectProblem(t, status, response, http.StatusUnprocessableEntity, "HOME_SLOT_INVALID_PAYLOAD")

			created := performOK(t, f.router, http.MethodPost, "/api/v1/admin/home-slots", f.adminToken, f.key("missing-ref"), map[string]any{
				"city_code": "440300", "slot_type": "product_block", "slot_key": uniqueName("missing-ref"), "title": "missing ref", "payload": map[string]any{"product_ids": []string{"999999999999"}},
			})
			data := object(t, created["data"])
			status, response = perform(t, f.router, http.MethodPost, "/api/v1/admin/home-slots/"+stringValue(t, data["id"])+"/status", f.adminToken, f.key("publish-missing-ref"), map[string]any{"status": "published", "version": int(l2Number(t, data["version"]))})
			expectProblem(t, status, response, http.StatusUnprocessableEntity, "HOME_SLOT_INVALID_PAYLOAD")
		})
	})

	t.Run("ACC-L2-HOME-005-effective-window", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			now := time.Now().UTC()
			activeKey, futureKey, expiredKey := uniqueName("active"), uniqueName("future"), uniqueName("expired")
			for _, slot := range []struct {
				key        string
				start, end time.Time
			}{
				{activeKey, now.Add(-time.Hour), now.Add(time.Hour)},
				{futureKey, now.Add(time.Hour), now.Add(2 * time.Hour)},
				{expiredKey, now.Add(-2 * time.Hour), now.Add(-time.Hour)},
			} {
				createAndPublish(t, map[string]any{"city_code": "440300", "slot_type": "banner", "slot_key": slot.key, "title": slot.key, "payload": map[string]any{"content": slot.key}, "start_at": slot.start.Format(time.RFC3339), "end_at": slot.end.Format(time.RFC3339)})
			}
			response := performOK(t, f.router, http.MethodGet, "/api/v1/home?city_code=440300", "", "", nil)
			items := array(t, object(t, response["data"])["slots"])
			if !containsID(items, "slot_key", activeKey) || containsID(items, "slot_key", futureKey) || containsID(items, "slot_key", expiredKey) {
				t.Fatalf("effective window filtering failed: %#v", items)
			}
		})
	})

	t.Run("ACC-L2-CACHE-001-shop-status-invalidates-dependent-reads", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			cacheKey := "product:detail:7001:4201"
			if err := f.redis.Set(f.ctx, cacheKey, `{"id":"stale"}`, time.Minute).Err(); err != nil {
				t.Fatalf("seed product cache: %v", err)
			}
			before, _ := f.redis.Get(f.ctx, "service_city_version:440300").Int64()
			performOK(t, f.router, http.MethodPatch, "/api/v1/store/shops/4201/business-status", f.merchantToken, f.key("rest-shop"), map[string]any{"business_status": "resting"})
			after, err := f.redis.Get(f.ctx, "service_city_version:440300").Int64()
			if err != nil || after <= before {
				t.Fatalf("service city version was not advanced: before=%d after=%d err=%v", before, after, err)
			}
			if exists, _ := f.redis.Exists(f.ctx, cacheKey).Result(); exists != 0 {
				t.Fatal("shop status change left product detail cache behind")
			}
			home := object(t, performOK(t, f.router, http.MethodGet, "/api/v1/home?city_code=440300&lat=22.54&lng=113.93", "", "", nil)["data"])
			if object(t, home["serviceability"])["reason_code"] != "NO_OPEN_SHOP" {
				t.Fatalf("home retained stale serviceability: %#v", home)
			}
		})
	})

	t.Run("ACC-L2-CACHE-002-product-change-invalidates-product-and-home", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			cacheKey := "product:detail:7001:4201"
			if err := f.redis.Set(f.ctx, cacheKey, `{"id":"stale"}`, time.Minute).Err(); err != nil {
				t.Fatalf("seed product cache: %v", err)
			}
			before, _ := f.redis.Get(f.ctx, "home_version:global").Int64()
			performOK(t, f.router, http.MethodPost, "/api/v1/admin/products/7001/off-sale", f.adminToken, f.key("off-sale"), nil)
			if exists, _ := f.redis.Exists(f.ctx, cacheKey).Result(); exists != 0 {
				t.Fatal("product status change left detail cache behind")
			}
			after, err := f.redis.Get(f.ctx, "home_version:global").Int64()
			if err != nil || after <= before {
				t.Fatalf("product status change did not invalidate home: before=%d after=%d err=%v", before, after, err)
			}
		})
	})

	t.Run("ACC-L2-SRE-001-observe-does-not-block", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			if err := f.db.Exec("UPDATE shops SET service_radius_m = 100 WHERE id = 4201").Error; err != nil {
				t.Fatalf("shrink service radius: %v", err)
			}
			farAddress := performOK(t, f.router, http.MethodPost, "/api/v1/addresses", f.customerToken, f.key("far-address"), map[string]any{
				"contact_name": "observe", "contact_phone": "13600000001", "province": "广东省", "city": "深圳市", "city_code": "440300", "district": "南山区", "district_code": "440305",
				"address_detail": "far", "location_source": "map_pin", "latitude": 22.55, "longitude": 113.94, "coordinate_system": "gcj02",
			})
			cfg := f.cfg
			cfg.Service.EnforcementMode = "observe"
			cfg.CustomerLBS.Mode = "observe"
			observeRouter := f.routerWithConfig(cfg)
			created := performOK(t, observeRouter, http.MethodPost, "/api/v1/orders", f.customerToken, f.key("observe-order"), map[string]any{
				"shop_id": "4201", "address_id": stringValue(t, object(t, farAddress["data"])["id"]), "items": []map[string]any{{"shop_product_id": "8002", "quantity": 1}},
			})
			if object(t, created["data"])["status"] != "pending_payment" {
				t.Fatalf("observe mode changed L1 behavior: %#v", created)
			}
		})
	})

	t.Run("ACC-L2-SRE-003-redis-and-mq-degradation", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			badRedis := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1", DialTimeout: 20 * time.Millisecond, ReadTimeout: 20 * time.Millisecond, WriteTimeout: 20 * time.Millisecond, MaxRetries: 0})
			defer badRedis.Close()
			publicRouter := NewRouter(Dependencies{Config: f.cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: f.db, Redis: badRedis})
			response := performOK(t, publicRouter, http.MethodGet, "/api/v1/products?shop_id=4201&page_size=2", "", "", nil)
			if len(array(t, object(t, response["data"])["items"])) == 0 {
				t.Fatal("DB read failed when Redis was unavailable")
			}

			productID := f.idGen.Next()
			if err := f.db.Exec("INSERT INTO products (id, category_id, name, sale_price_amount, original_price_amount, status) VALUES (?, 6001, ?, 100, 100, 'on_sale')", productID, uniqueName("degraded-product")).Error; err != nil {
				t.Fatalf("insert product: %v", err)
			}
			service := adminmodule.NewService(f.db, badRedis, f.idGen)
			claims := &auth.Claims{AccountType: "admin", AdminUserID: "3001", Permissions: []string{"product:update"}}
			if _, err := service.SetProductStatus(context.Background(), claims, "POST", "/api/v1/admin/products/:id/off-sale", f.key("degraded-off-sale"), fmt.Sprint(productID), "off_sale"); err != nil {
				t.Fatalf("DB write failed when Redis/MQ were unavailable: %v", err)
			}
			var status string
			if err := f.db.Table("products").Select("status").Where("id = ?", productID).Scan(&status).Error; err != nil || status != "off_sale" {
				t.Fatalf("product write did not commit: status=%s err=%v", status, err)
			}
			var fallback int64
			if err := f.db.Table("outbox_events").Where("event_type = 'cache.invalidate' AND aggregate_id = ?", productID).Count(&fallback).Error; err != nil || fallback != 1 {
				t.Fatalf("cache failure was not persisted for retry: count=%d err=%v", fallback, err)
			}
		})
	})
}
