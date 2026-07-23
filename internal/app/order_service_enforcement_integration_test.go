package app

import (
	"fmt"
	"net/http"
	"testing"
)

func TestOrderServiceEnforcementDominatesObservedCustomerLBS(t *testing.T) {
	f := newL2AcceptanceFixture(t)
	defer f.close()

	orderBody := func(shopID, shopProductID uint64) map[string]any {
		return map[string]any{
			"shop_id":    fmt.Sprint(shopID),
			"address_id": f.addressID,
			"items": []map[string]any{{
				"shop_product_id": fmt.Sprint(shopProductID),
				"quantity":        1,
			}},
		}
	}

	t.Run("observed cross-shop resolution is still rejected", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			catalog := f.createCatalog(t, 10, 10)
			if err := f.db.Exec(`UPDATE shops
				SET latitude = 22.54, longitude = 113.93, service_radius_m = 20000, priority = 100
				WHERE id = ?`, catalog.shopB).Error; err != nil {
				t.Fatalf("make shop B the resolved service shop: %v", err)
			}
			for day := 1; day <= 7; day++ {
				if err := f.db.Exec(`INSERT INTO shop_business_hours
					(id, shop_id, day_of_week, open_time, close_time, status)
					VALUES (?, ?, ?, '00:00:00', '23:59:59', 'active')`, f.idGen.Next(), catalog.shopB, day).Error; err != nil {
					t.Fatalf("insert shop B hours: %v", err)
				}
			}

			cfg := f.cfg
			cfg.CustomerLBS.Mode = "observe"
			router := f.routerWithConfig(cfg)

			var beforeOrders int64
			if err := f.db.Table("orders").Where("customer_id = ?", f.customerID).Count(&beforeOrders).Error; err != nil {
				t.Fatalf("count orders before cross-shop request: %v", err)
			}
			var beforeStock struct{ AvailableQty, ReservedQty int }
			if err := f.db.Table("product_stocks").Select("available_qty, reserved_qty").Where("id = ?", catalog.stockA).Scan(&beforeStock).Error; err != nil {
				t.Fatalf("read stock before cross-shop request: %v", err)
			}

			status, response := perform(t, router, http.MethodPost, "/api/v1/orders", f.customerToken, f.key("observe-enforce-cross-shop"), orderBody(4201, catalog.shopProductA))
			expectProblem(t, status, response, http.StatusConflict, "SERVICE_SHOP_CHANGED")

			var afterOrders int64
			if err := f.db.Table("orders").Where("customer_id = ?", f.customerID).Count(&afterOrders).Error; err != nil {
				t.Fatalf("count orders after cross-shop request: %v", err)
			}
			var afterStock struct{ AvailableQty, ReservedQty int }
			if err := f.db.Table("product_stocks").Select("available_qty, reserved_qty").Where("id = ?", catalog.stockA).Scan(&afterStock).Error; err != nil {
				t.Fatalf("read stock after cross-shop request: %v", err)
			}
			if afterOrders != beforeOrders || afterStock != beforeStock {
				t.Fatalf("cross-shop rejection had side effects: orders %d -> %d, stock %+v -> %+v", beforeOrders, afterOrders, beforeStock, afterStock)
			}
		})
	})

	t.Run("customer LBS failure falls back to enforced service resolution", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			if err := f.db.Table("customer_addresses").Where("id = ?", f.addressID).Update("geocode_status", "conflict").Error; err != nil {
				t.Fatalf("mark customer LBS address resolution invalid: %v", err)
			}
			cfg := f.cfg
			cfg.CustomerLBS.Mode = "observe"
			router := f.routerWithConfig(cfg)

			created := performOK(t, router, http.MethodPost, "/api/v1/orders", f.customerToken, f.key("observe-failed-enforce-fallback"), orderBody(4201, 8002))
			data := object(t, created["data"])
			if data["delivery_promise"] == nil {
				t.Fatalf("enforced fallback returned an empty delivery promise: %#v", data)
			}

			var row struct {
				DeliveryFeeAmount       int64
				DeliveryPromiseSnapshot []byte
			}
			if err := f.db.Table("orders").Select("delivery_fee_amount, delivery_promise_snapshot").Where("id = ?", stringValue(t, data["order_id"])).Scan(&row).Error; err != nil {
				t.Fatalf("read fallback order: %v", err)
			}
			if row.DeliveryFeeAmount != 500 || len(row.DeliveryPromiseSnapshot) == 0 {
				t.Fatalf("fallback order lost its enforced promise: fee=%d snapshot=%q", row.DeliveryFeeAmount, row.DeliveryPromiseSnapshot)
			}
		})
	})

	t.Run("observed same-shop resolution keeps a valid promise", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			cfg := f.cfg
			cfg.CustomerLBS.Mode = "observe"
			router := f.routerWithConfig(cfg)

			created := performOK(t, router, http.MethodPost, "/api/v1/orders", f.customerToken, f.key("observe-enforce-same-shop"), orderBody(4201, 8002))
			data := object(t, created["data"])
			promise := object(t, data["delivery_promise"])
			if promise["confirmed"] != true || int(l2Number(t, promise["eta_min_minutes"])) <= 0 || int(l2Number(t, promise["eta_max_minutes"])) < int(l2Number(t, promise["eta_min_minutes"])) {
				t.Fatalf("same-shop order returned an invalid delivery promise: %#v", promise)
			}

			var row struct{ DeliveryPromiseSnapshot []byte }
			if err := f.db.Table("orders").Select("delivery_promise_snapshot").Where("id = ?", stringValue(t, data["order_id"])).Scan(&row).Error; err != nil {
				t.Fatalf("read same-shop order promise: %v", err)
			}
			if len(row.DeliveryPromiseSnapshot) == 0 {
				t.Fatal("same-shop order persisted an empty delivery promise snapshot")
			}
		})
	})
}
