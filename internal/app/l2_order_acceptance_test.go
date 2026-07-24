package app

import (
	"fmt"
	"net/http"
	"testing"
)

// TestL2OrderMissingAcceptanceScenarios 验证 L2 订单能力缺失项的验收场景。
func TestL2OrderMissingAcceptanceScenarios(t *testing.T) {
	f := newL2AcceptanceFixture(t)
	defer f.close()

	orderBody := func(shopID, shopProductID uint64) map[string]any {
		return map[string]any{"shop_id": fmt.Sprint(shopID), "address_id": f.addressID, "items": []map[string]any{{"shop_product_id": fmt.Sprint(shopProductID), "quantity": 1}}}
	}
	stock := func(t *testing.T, stockID uint64) (int, int) {
		t.Helper()
		var row struct{ AvailableQty, ReservedQty int }
		if err := f.db.Raw("SELECT available_qty, reserved_qty FROM product_stocks WHERE id = ?", stockID).Scan(&row).Error; err != nil {
			t.Fatalf("read stock: %v", err)
		}
		return row.AvailableQty, row.ReservedQty
	}
	orderCount := func(t *testing.T) int64 {
		t.Helper()
		var count int64
		if err := f.db.Table("orders").Where("customer_id = ?", f.customerID).Count(&count).Error; err != nil {
			t.Fatalf("count orders: %v", err)
		}
		return count
	}

	t.Run("ACC-L2-ORD-002-shop-closes-before-order", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			beforeAvailable, beforeReserved := stock(t, 8502)
			beforeOrders := orderCount(t)
			if err := f.db.Exec("UPDATE shops SET business_status = 'resting' WHERE id = 4201").Error; err != nil {
				t.Fatalf("rest shop: %v", err)
			}
			status, response := perform(t, f.router, http.MethodPost, "/api/v1/orders", f.customerToken, f.key("closed-shop"), orderBody(4201, 8002))
			expectProblem(t, status, response, http.StatusConflict, "NO_OPEN_SHOP")
			afterAvailable, afterReserved := stock(t, 8502)
			if beforeAvailable != afterAvailable || beforeReserved != afterReserved || orderCount(t) != beforeOrders {
				t.Fatalf("closed-shop rejection had side effects: stock %d/%d -> %d/%d orders %d -> %d", beforeAvailable, beforeReserved, afterAvailable, afterReserved, beforeOrders, orderCount(t))
			}
		})
	})

	t.Run("ACC-L2-ORD-003-service-shop-changed", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			catalog := f.createCatalog(t, 10, 10)
			if err := f.db.Exec("UPDATE shops SET latitude = 22.54, longitude = 113.93, service_radius_m = 20000 WHERE id = ?", catalog.shopB).Error; err != nil {
				t.Fatalf("move shop B: %v", err)
			}
			for day := 1; day <= 7; day++ {
				if err := f.db.Exec(`INSERT INTO shop_business_hours (id, shop_id, day_of_week, open_time, close_time, status)
					VALUES (?, ?, ?, '00:00:00', '23:59:59', 'active')`, f.idGen.Next(), catalog.shopB, day).Error; err != nil {
					t.Fatalf("insert shop B hours: %v", err)
				}
			}
			if err := f.db.Exec("UPDATE shops SET business_status = 'resting' WHERE id = 4201").Error; err != nil {
				t.Fatalf("rest old shop: %v", err)
			}
			cfg := f.cfg
			cfg.CustomerLBS.Mode = "enforce"
			enforceRouter := f.routerWithConfig(cfg)
			beforeAvailable, beforeReserved := stock(t, catalog.stockA)
			beforeOrders := orderCount(t)
			status, response := perform(t, enforceRouter, http.MethodPost, "/api/v1/orders", f.customerToken, f.key("shop-changed"), orderBody(4201, catalog.shopProductA))
			expectProblem(t, status, response, http.StatusConflict, "SERVICE_SHOP_CHANGED")
			data := object(t, response["data"])
			if object(t, data["service_shop"])["id"] != fmt.Sprint(catalog.shopB) {
				t.Fatalf("new service shop missing from conflict: %#v", response)
			}
			afterAvailable, afterReserved := stock(t, catalog.stockA)
			if beforeAvailable != afterAvailable || beforeReserved != afterReserved || orderCount(t) != beforeOrders {
				t.Fatal("service-shop conflict had order or stock side effects")
			}
		})
	})

	t.Run("ACC-L2-ORD-004-product-not-in-service-shop", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			catalog := f.createCatalog(t, 10, 7)
			beforeAvailable, beforeReserved := stock(t, catalog.stockB)
			beforeOrders := orderCount(t)
			status, response := perform(t, f.router, http.MethodPost, "/api/v1/orders", f.customerToken, f.key("wrong-shop-product"), orderBody(4201, catalog.shopProductB))
			expectProblem(t, status, response, http.StatusConflict, "PRODUCT_NOT_ON_SALE")
			afterAvailable, afterReserved := stock(t, catalog.stockB)
			if beforeAvailable != afterAvailable || beforeReserved != afterReserved || orderCount(t) != beforeOrders {
				t.Fatal("wrong-shop product rejection had side effects")
			}
		})
	})

	t.Run("ACC-L2-ORD-006-free-delivery-threshold-boundary", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			var price int64
			if err := f.db.Table("shop_products").Select("sale_price_amount").Where("id = 8002").Scan(&price).Error; err != nil || price <= 0 {
				t.Fatalf("read seed price: price=%d err=%v", price, err)
			}
			if err := f.db.Exec("UPDATE shops SET free_delivery_threshold_amount = ? WHERE id = 4201", price).Error; err != nil {
				t.Fatalf("set threshold: %v", err)
			}
			created := performOK(t, f.router, http.MethodPost, "/api/v1/orders", f.customerToken, f.key("free-threshold"), orderBody(4201, 8002))
			data := object(t, created["data"])
			if int64(l2Number(t, data["payable_amount"])) != price {
				t.Fatalf("threshold order charged delivery fee: %#v", data)
			}
			var fee int64
			if err := f.db.Table("orders").Select("delivery_fee_amount").Where("id = ?", stringValue(t, data["order_id"])).Scan(&fee).Error; err != nil || fee != 0 {
				t.Fatalf("expected zero persisted delivery fee: fee=%d err=%v", fee, err)
			}
		})
	})
}

// TestOrderCreationRemovesOnlyPurchasedCartItems 验证创建订单只删除已购买的购物车明细。
func TestOrderCreationRemovesOnlyPurchasedCartItems(t *testing.T) {
	f := newL2AcceptanceFixture(t)
	defer f.close()

	f.subtest(t, func(t *testing.T) {
		performOK(t, f.router, http.MethodPost, "/api/v1/cart/items", f.customerToken, f.key("cart-purchased"), map[string]any{
			"shop_product_id": "8001", "quantity": 2,
		})
		secondCart := performOK(t, f.router, http.MethodPost, "/api/v1/cart/items", f.customerToken, f.key("cart-unselected"), map[string]any{
			"shop_product_id": "8002", "quantity": 1,
		})
		performOK(t, f.router, http.MethodPost, "/api/v1/cart/items", f.customerToken, f.key("cart-purchased-second"), map[string]any{
			"shop_product_id": "8003", "quantity": 1,
		})
		performOK(t, f.router, http.MethodPost, "/api/v1/cart/items", f.otherToken, f.key("other-customer-cart"), map[string]any{
			"shop_product_id": "8001", "quantity": 1,
		})
		var unselectedItemID string
		for _, raw := range array(t, object(t, secondCart["data"])["items"]) {
			item := object(t, raw)
			if item["shop_product_id"] == "8002" {
				unselectedItemID = stringValue(t, item["id"])
				break
			}
		}
		if unselectedItemID == "" {
			t.Fatal("cart item 8002 missing before checkout")
		}
		performOK(t, f.router, http.MethodPatch, "/api/v1/cart/items/"+unselectedItemID+"/selection", f.customerToken, f.key("cart-deselect"), map[string]any{"selected": false})

		orderBody := map[string]any{
			"shop_id": "4201", "address_id": f.addressID,
			"items": []map[string]any{
				{"shop_product_id": "8001", "quantity": 2},
				{"shop_product_id": "8003", "quantity": 1},
			},
		}
		orderKey := f.key("cart-checkout")
		created := performOK(t, f.router, http.MethodPost, "/api/v1/orders", f.customerToken, orderKey, orderBody)
		orderID := stringValue(t, object(t, created["data"])["order_id"])

		cartAfterOrder := performOK(t, f.router, http.MethodGet, "/api/v1/cart/items", f.customerToken, "", nil)
		remaining := array(t, object(t, cartAfterOrder["data"])["items"])
		if len(remaining) != 1 {
			t.Fatalf("expected only the unselected item to remain, got %#v", remaining)
		}
		remainingItem := object(t, remaining[0])
		if remainingItem["shop_product_id"] != "8002" || remainingItem["selected"] != false {
			t.Fatalf("unexpected remaining cart item: %#v", remainingItem)
		}
		otherCart := performOK(t, f.router, http.MethodGet, "/api/v1/cart/items", f.otherToken, "", nil)
		if !containsID(array(t, object(t, otherCart["data"])["items"]), "shop_product_id", "8001") {
			t.Fatal("checkout removed the same product from another customer's cart")
		}

		performOK(t, f.router, http.MethodPost, "/api/v1/cart/items", f.customerToken, f.key("cart-readd"), map[string]any{
			"shop_product_id": "8001", "quantity": 1,
		})
		replayed := performOK(t, f.router, http.MethodPost, "/api/v1/orders", f.customerToken, orderKey, orderBody)
		if stringValue(t, object(t, replayed["data"])["order_id"]) != orderID {
			t.Fatal("idempotent replay returned a different order")
		}
		cartAfterReplay := performOK(t, f.router, http.MethodGet, "/api/v1/cart/items", f.customerToken, "", nil)
		if !containsID(array(t, object(t, cartAfterReplay["data"])["items"]), "shop_product_id", "8001") {
			t.Fatal("idempotent replay removed an item added after the original order")
		}
	})
}

// TestFailedOrderKeepsPurchasedCartItems 验证下单失败时保留已选择的购物车明细。
func TestFailedOrderKeepsPurchasedCartItems(t *testing.T) {
	f := newL2AcceptanceFixture(t)
	defer f.close()

	f.subtest(t, func(t *testing.T) {
		performOK(t, f.router, http.MethodPost, "/api/v1/cart/items", f.customerToken, f.key("cart-before-failed-order"), map[string]any{
			"shop_product_id": "8001", "quantity": 1,
		})
		if err := f.db.Exec("UPDATE product_stocks SET available_qty = 0 WHERE shop_product_id = 8001").Error; err != nil {
			t.Fatalf("make product unavailable: %v", err)
		}
		status, response := perform(t, f.router, http.MethodPost, "/api/v1/orders", f.customerToken, f.key("failed-cart-checkout"), map[string]any{
			"shop_id": "4201", "address_id": f.addressID,
			"items": []map[string]any{{"shop_product_id": "8001", "quantity": 1}},
		})
		expectProblem(t, status, response, http.StatusConflict, "STOCK_NOT_ENOUGH")

		cartAfterFailure := performOK(t, f.router, http.MethodGet, "/api/v1/cart/items", f.customerToken, "", nil)
		if !containsID(array(t, object(t, cartAfterFailure["data"])["items"]), "shop_product_id", "8001") {
			t.Fatal("failed order removed the purchased cart item")
		}
	})
}
