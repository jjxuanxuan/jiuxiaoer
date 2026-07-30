package app

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"
)

type acceptanceCatalog struct {
	productID, shopB, shopProductA, shopProductB, stockA, stockB uint64
	name                                                         string
}

// createCatalog 创建测试商品目录。
func (f *l2AcceptanceFixture) createCatalog(t *testing.T, qtyA, qtyB int) acceptanceCatalog {
	t.Helper()
	v := acceptanceCatalog{
		productID: f.idGen.Next(), shopB: f.idGen.Next(), shopProductA: f.idGen.Next(), shopProductB: f.idGen.Next(), stockA: f.idGen.Next(), stockB: f.idGen.Next(), name: uniqueName("l2-catalog"),
	}
	if err := f.db.Exec(`INSERT INTO shops
		(id, merchant_id, name, city, city_code, district, address, latitude, longitude, status, business_status, service_mode, service_radius_m, service_area_version, delivery_fee_amount, delivery_eta_min, delivery_eta_max)
		VALUES (?, 4001, 'L2 B Shop', '深圳市', '440300', '南山区', '远端门店', 22.80, 114.20, 'active', 'open', 'radius', 1000, 1, 500, 15, 25)`, v.shopB).Error; err != nil {
		t.Fatalf("insert shop B: %v", err)
	}
	if err := f.db.Exec(`INSERT INTO products (id, category_id, name, sale_price_amount, original_price_amount, status)
		VALUES (?, 6001, ?, 1000, 1000, 'on_sale')`, v.productID, v.name).Error; err != nil {
		t.Fatalf("insert product: %v", err)
	}
	if err := f.db.Exec(`INSERT INTO shop_products (id, merchant_id, shop_id, product_id, sale_price_amount, status, sort_order) VALUES
		(?, 4001, 4201, ?, 1000, 'on_sale', 1), (?, 4001, ?, ?, 1200, 'on_sale', 1)`, v.shopProductA, v.productID, v.shopProductB, v.shopB, v.productID).Error; err != nil {
		t.Fatalf("insert shop products: %v", err)
	}
	if err := f.db.Exec(`INSERT INTO product_stocks (id, shop_product_id, shop_id, product_id, available_qty, reserved_qty, locked_qty, version) VALUES
		(?, ?, 4201, ?, ?, 0, 0, 0), (?, ?, ?, ?, ?, 0, 0, 0)`, v.stockA, v.shopProductA, v.productID, qtyA, v.stockB, v.shopProductB, v.shopB, v.productID, qtyB).Error; err != nil {
		t.Fatalf("insert stocks: %v", err)
	}
	return v
}

// TestL2CatalogAndCartMissingAcceptanceScenarios 验证 L2 商品目录和购物车缺失项的验收场景。
func TestL2CatalogAndCartMissingAcceptanceScenarios(t *testing.T) {
	f := newL2AcceptanceFixture(t)
	defer f.close()

	t.Run("ACC-L2-LBS-007-manual-city-directory", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			response := performOK(t, f.router, http.MethodGet, "/api/v1/shops?city_code=440300&page_size=100", "", "", nil)
			items := array(t, object(t, response["data"])["items"])
			if len(items) == 0 {
				t.Fatal("expected city shop directory")
			}
			for _, item := range items {
				row := object(t, item)
				if _, claimed := row["serviceable"]; claimed {
					t.Fatalf("manual city directory must not claim serviceability: %#v", row)
				}
			}
		})
	})

	t.Run("ACC-L2-CATALOG-001-resolved-shop-scope", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			catalog := f.createCatalog(t, 5, 9)
			path := fmt.Sprintf("/api/v1/products?city_code=440300&lat=22.54&lng=113.93&keyword=%s&page_size=100", catalog.name)
			response := performOK(t, f.router, http.MethodGet, path, "", "", nil)
			items := array(t, object(t, response["data"])["items"])
			if len(items) != 1 {
				t.Fatalf("expected one resolved-shop item, got %#v", items)
			}
			row := object(t, items[0])
			if row["shop_id"] != "4201" || row["shop_product_id"] != fmt.Sprint(catalog.shopProductA) || int(l2Number(t, row["available_qty"])) != 5 {
				t.Fatalf("catalog escaped resolved shop: %#v", row)
			}
		})
	})

	t.Run("ACC-L2-CATALOG-002-requested-shop-conflict", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			catalog := f.createCatalog(t, 5, 9)
			path := fmt.Sprintf("/api/v1/products?shop_id=%d&city_code=440300&lat=22.54&lng=113.93", catalog.shopB)
			status, response := perform(t, f.router, http.MethodGet, path, "", "", nil)
			expectProblem(t, status, response, http.StatusConflict, "SERVICE_SHOP_CHANGED")
		})
	})

	t.Run("ACC-L2-CATALOG-003-no-cross-shop-stock-fallback", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			catalog := f.createCatalog(t, 0, 10)
			availableAtB := performOK(t, f.router, http.MethodGet, fmt.Sprintf("/api/v1/products/%d?shop_id=%d", catalog.productID, catalog.shopB), "", "", nil)
			if object(t, availableAtB["data"])["shop_id"] != fmt.Sprint(catalog.shopB) {
				t.Fatalf("fixture product is not available at shop B: %#v", availableAtB)
			}
			path := fmt.Sprintf("/api/v1/products/%d?city_code=440300&lat=22.54&lng=113.93", catalog.productID)
			response := performOK(t, f.router, http.MethodGet, path, "", "", nil)
			row := object(t, response["data"])
			if row["shop_id"] != "4201" || int(l2Number(t, row["available_qty"])) != 0 || row["purchasable"] != false || row["unavailable_reason"] != "out_of_stock" {
				t.Fatalf("product detail crossed shops or hid its stock state: %#v", row)
			}
		})
	})

	t.Run("ACC-L2-CART-001-cross-shop-add-preserves-cart", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			catalog := f.createCatalog(t, 10, 10)
			first := performOK(t, f.router, http.MethodPost, "/api/v1/cart/items", f.customerToken, f.key("cart-a"), map[string]any{"shop_product_id": fmt.Sprint(catalog.shopProductA), "quantity": 1})
			before := array(t, object(t, first["data"])["items"])
			status, response := perform(t, f.router, http.MethodPost, "/api/v1/cart/items", f.customerToken, f.key("cart-b"), map[string]any{"shop_product_id": fmt.Sprint(catalog.shopProductB), "quantity": 1})
			expectProblem(t, status, response, http.StatusConflict, "CART_SHOP_CONFLICT")
			after := array(t, object(t, performOK(t, f.router, http.MethodGet, "/api/v1/cart/items", f.customerToken, "", nil)["data"])["items"])
			if len(before) != 1 || len(after) != 1 || object(t, after[0])["shop_product_id"] != fmt.Sprint(catalog.shopProductA) {
				t.Fatalf("cross-shop conflict changed original cart: %#v", after)
			}
		})
	})

	t.Run("ACC-L2-CART-002-unavailable-item-cannot-order", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			catalog := f.createCatalog(t, 10, 0)
			added := performOK(t, f.router, http.MethodPost, "/api/v1/cart/items", f.customerToken, f.key("cart-unavailable"), map[string]any{"shop_product_id": fmt.Sprint(catalog.shopProductA), "quantity": 1})
			itemID := stringValue(t, object(t, array(t, object(t, added["data"])["items"])[0])["id"])
			performOK(t, f.router, http.MethodPatch, "/api/v1/cart/items/"+itemID+"/selection", f.customerToken, f.key("cart-unselect-before-off-sale"), map[string]any{"selected": false})
			if err := f.db.Exec("UPDATE shop_products SET status = 'off_sale' WHERE id = ?", catalog.shopProductA).Error; err != nil {
				t.Fatalf("off-sale product: %v", err)
			}
			cart := object(t, performOK(t, f.router, http.MethodGet, "/api/v1/cart/items", f.customerToken, "", nil)["data"])
			item := object(t, array(t, cart["items"])[0])
			if item["availability_status"] != "not_on_sale" || item["available"] != false || item["unavailable_reason"] != "not_on_sale" || int(l2Number(t, cart["unavailable_count"])) != 1 {
				t.Fatalf("unavailable cart item not marked: %#v", cart)
			}
			status, response := perform(t, f.router, http.MethodPatch, "/api/v1/cart/items/"+itemID+"/selection", f.customerToken, f.key("cart-reselect-off-sale"), map[string]any{"selected": true})
			expectProblem(t, status, response, http.StatusConflict, "PRODUCT_NOT_ON_SALE")
			status, response = perform(t, f.router, http.MethodPost, "/api/v1/cart/selection", f.customerToken, f.key("cart-select-shop-off-sale"), map[string]any{"shop_id": "4201", "selected": true})
			expectProblem(t, status, response, http.StatusConflict, "PRODUCT_NOT_ON_SALE")
			status, response = perform(t, f.router, http.MethodPut, "/api/v1/cart/items/"+itemID, f.customerToken, f.key("cart-update-off-sale"), map[string]any{"quantity": 2})
			expectProblem(t, status, response, http.StatusConflict, "PRODUCT_NOT_ON_SALE")
			if err := f.db.Exec("UPDATE shop_products SET deleted_at = CURRENT_TIMESTAMP(3) WHERE id = ?", catalog.shopProductA).Error; err != nil {
				t.Fatalf("soft-delete shop product: %v", err)
			}
			cart = object(t, performOK(t, f.router, http.MethodGet, "/api/v1/cart/items", f.customerToken, "", nil)["data"])
			items := array(t, cart["items"])
			if len(items) != 1 {
				t.Fatalf("soft-deleted shop product removed cart fact: %#v", cart)
			}
			item = object(t, items[0])
			if item["shop_product_id"] != fmt.Sprint(catalog.shopProductA) || item["available"] != false || item["unavailable_reason"] != "not_on_sale" || int(l2Number(t, item["sale_price_amount"])) != 0 || int(l2Number(t, item["total_amount"])) != 0 || item["selected"] != false {
				t.Fatalf("soft-deleted shop product cart projection is unsafe: %#v", item)
			}
			status, response = perform(t, f.router, http.MethodPost, "/api/v1/orders", f.customerToken, f.key("order-unavailable"), map[string]any{"shop_id": "4201", "address_id": f.addressID, "items": []map[string]any{{"shop_product_id": fmt.Sprint(catalog.shopProductA), "quantity": 1}}})
			expectProblem(t, status, response, http.StatusConflict, "PRODUCT_NOT_ON_SALE")
		})
	})

	t.Run("ACC-L2-CART-003-selection-and-clear-idempotency", func(t *testing.T) {
		f.subtest(t, func(t *testing.T) {
			catalog := f.createCatalog(t, 10, 10)
			first := performOK(t, f.router, http.MethodPost, "/api/v1/cart/items", f.customerToken, f.key("cart-select-a"), map[string]any{"shop_product_id": fmt.Sprint(catalog.shopProductA), "quantity": 2})
			itemID := stringValue(t, object(t, array(t, object(t, first["data"])["items"])[0])["id"])
			key := f.key("item-unselect")
			performOK(t, f.router, http.MethodPatch, "/api/v1/cart/items/"+itemID+"/selection", f.customerToken, key, map[string]any{"selected": false})
			performOK(t, f.router, http.MethodPatch, "/api/v1/cart/items/"+itemID+"/selection", f.customerToken, key, map[string]any{"selected": false})
			clearKey := f.key("cart-clear")
			for i := 0; i < 2; i++ {
				status := performStatusOnly(t, f.router, http.MethodDelete, "/api/v1/cart/items", f.customerToken, clearKey)
				if status != http.StatusOK {
					t.Fatalf("idempotent clear failed: status=%d", status)
				}
			}
			cart := object(t, performOK(t, f.router, http.MethodGet, "/api/v1/cart/items", f.customerToken, "", nil)["data"])
			if len(array(t, cart["items"])) != 0 || int(l2Number(t, cart["total_quantity"])) != 0 {
				t.Fatalf("cart was not cleared: %#v", cart)
			}
		})
	})
}

// TestL2FrequentPurchaseRepurchaseAcceptance 验证常购历史只使用当前客户的已完成订单，
// 并覆盖部分成功、目标数量语义、幂等重放和跨门店选中替换。
func TestL2FrequentPurchaseRepurchaseAcceptance(t *testing.T) {
	f := newL2AcceptanceFixture(t)
	defer f.close()

	f.subtest(t, func(t *testing.T) {
		cfg := f.cfg
		cfg.CustomerLBS.Mode = "enforce"
		cfg.CustomerLBS.Provider = "fake"
		cfg.CustomerLBS.RegeocodeEnabled = true
		cfg.CustomerLBS.RouteRefineEnabled = true
		cfg.CustomerLBS.CacheHMACSecret = "repurchase-acceptance-lbs-secret-123456789"
		router := f.routerWithConfig(cfg)

		catalog := f.createCatalog(t, 10, 10)
		historicalOnlyProductID := f.idGen.Next()
		if err := f.db.Exec(`INSERT INTO products (id, category_id, name, sale_price_amount, original_price_amount, status)
			VALUES (?, 6001, ?, 800, 800, 'on_sale')`, historicalOnlyProductID, uniqueName("history-only")).Error; err != nil {
			t.Fatalf("insert historical-only product: %v", err)
		}
		olderOrderID, latestOrderID := f.idGen.Next(), f.idGen.Next()
		if err := f.db.Exec(`INSERT INTO orders
			(id, order_no, customer_id, merchant_id, shop_id, status, pay_status, delivery_status, goods_amount, payable_amount, paid_amount, address_snapshot, completed_at)
			VALUES
			(?, ?, ?, 4001, 4201, 'completed', 'paid', 'completed', 2000, 2000, 2000, '{}', ?),
			(?, ?, ?, 4001, 4201, 'completed', 'paid', 'completed', 4400, 4400, 4400, '{}', ?)`,
			olderOrderID, fmt.Sprintf("REP-%d", olderOrderID), f.customerID, time.Now().Add(-20*24*time.Hour),
			latestOrderID, fmt.Sprintf("REP-%d", latestOrderID), f.customerID, time.Now().Add(-2*24*time.Hour),
		).Error; err != nil {
			t.Fatalf("insert completed repurchase orders: %v", err)
		}
		if err := f.db.Exec(`INSERT INTO order_items
			(id, order_id, shop_product_id, product_id, product_snapshot, quantity, sale_price_amount, total_amount)
			VALUES
			(?, ?, ?, ?, '{}', 2, 1000, 2000),
			(?, ?, ?, ?, '{}', 3, 1200, 3600),
			(?, ?, 0, ?, '{}', 1, 800, 800)`,
			f.idGen.Next(), olderOrderID, catalog.shopProductA, catalog.productID,
			f.idGen.Next(), latestOrderID, catalog.shopProductA, catalog.productID,
			f.idGen.Next(), latestOrderID, historicalOnlyProductID,
		).Error; err != nil {
			t.Fatalf("insert completed repurchase items: %v", err)
		}

		resolved := lbsHTTP(t, router, http.MethodPost, "/api/v1/location-contexts", map[string]string{
			"Authorization": "Bearer " + f.customerToken,
		}, map[string]any{
			"source": "device_location", "latitude": 22.54, "longitude": 113.93,
			"coordinate_system": "gcj02", "accuracy_m": 20, "captured_at": time.Now().Format(time.RFC3339Nano),
		})
		if resolved.status != http.StatusOK {
			t.Fatalf("resolve customer location: status=%d body=%#v", resolved.status, resolved.body)
		}
		contextID := stringValue(t, object(t, resolved.body["data"])["location_context_id"])
		commonHeaders := map[string]string{
			"Authorization":      "Bearer " + f.customerToken,
			"X-Location-Context": contextID,
		}
		frequent := lbsHTTP(t, router, http.MethodGet, "/api/v1/frequent-purchases", commonHeaders, nil)
		if frequent.status != http.StatusOK {
			t.Fatalf("list frequent purchases: status=%d body=%#v", frequent.status, frequent.body)
		}
		frequentItems := array(t, object(t, frequent.body["data"])["items"])
		if len(frequentItems) != 2 {
			t.Fatalf("unexpected frequent purchase items: %#v", frequentItems)
		}
		first := object(t, frequentItems[0])
		if first["product_id"] != fmt.Sprint(catalog.productID) || int(l2Number(t, first["purchase_count"])) != 2 ||
			int(l2Number(t, first["recommended_quantity"])) != 3 || first["sale_price_amount"] != float64(1000) {
			t.Fatalf("frequent aggregation mismatch: %#v", first)
		}

		body := map[string]any{"items": []map[string]any{
			{"product_id": fmt.Sprint(catalog.productID), "quantity": 4},
			{"product_id": fmt.Sprint(historicalOnlyProductID), "quantity": 1},
		}}
		key := f.key("repurchase-partial")
		headers := map[string]string{
			"Authorization":      "Bearer " + f.customerToken,
			"X-Location-Context": contextID,
			"Idempotency-Key":    key,
		}
		firstRepurchase := lbsHTTP(t, router, http.MethodPost, "/api/v1/cart/repurchase", headers, body)
		replayed := lbsHTTP(t, router, http.MethodPost, "/api/v1/cart/repurchase", headers, body)
		if firstRepurchase.status != http.StatusOK || replayed.status != http.StatusOK ||
			!reflect.DeepEqual(firstRepurchase.body["data"], replayed.body["data"]) {
			t.Fatalf("repurchase replay mismatch: first=%d %#v replay=%d %#v", firstRepurchase.status, firstRepurchase.body, replayed.status, replayed.body)
		}
		data := object(t, firstRepurchase.body["data"])
		if int(l2Number(t, data["succeeded_count"])) != 1 || int(l2Number(t, data["failed_count"])) != 1 {
			t.Fatalf("partial repurchase counts mismatch: %#v", data)
		}
		results := array(t, data["results"])
		if object(t, results[0])["status"] != "added" || object(t, results[1])["error_code"] != "PRODUCT_NOT_AVAILABLE_IN_SHOP" {
			t.Fatalf("partial repurchase results mismatch: %#v", results)
		}
		cartItems := array(t, object(t, data["cart"])["items"])
		if len(cartItems) != 1 || int(l2Number(t, object(t, cartItems[0])["quantity"])) != 4 {
			t.Fatalf("repurchase cart quantity mismatch: %#v", cartItems)
		}

		unchangedHeaders := map[string]string{
			"Authorization":      "Bearer " + f.customerToken,
			"X-Location-Context": contextID,
			"Idempotency-Key":    f.key("repurchase-max-target"),
		}
		unchanged := lbsHTTP(t, router, http.MethodPost, "/api/v1/cart/repurchase", unchangedHeaders, map[string]any{
			"items": []map[string]any{{"product_id": fmt.Sprint(catalog.productID), "quantity": 2}},
		})
		if unchanged.status != http.StatusOK || object(t, array(t, object(t, unchanged.body["data"])["results"])[0])["status"] != "unchanged" {
			t.Fatalf("repurchase target quantity was not stable: status=%d body=%#v", unchanged.status, unchanged.body)
		}

		performOK(t, router, http.MethodDelete, "/api/v1/cart/items", f.customerToken, f.key("repurchase-clear"), nil)
		performOK(t, router, http.MethodPost, "/api/v1/cart/items", f.customerToken, f.key("repurchase-other-shop"), map[string]any{
			"shop_product_id": fmt.Sprint(catalog.shopProductB), "quantity": 1,
		})
		conflictHeaders := map[string]string{
			"Authorization":      "Bearer " + f.customerToken,
			"X-Location-Context": contextID,
			"Idempotency-Key":    f.key("repurchase-shop-conflict"),
		}
		conflict := lbsHTTP(t, router, http.MethodPost, "/api/v1/cart/repurchase", conflictHeaders, map[string]any{
			"items": []map[string]any{{"product_id": fmt.Sprint(catalog.productID), "quantity": 1}},
		})
		if conflict.status != http.StatusConflict || conflict.body["error_code"] != "CART_SHOP_CONFLICT" {
			t.Fatalf("cross-shop repurchase must fail closed: status=%d body=%#v", conflict.status, conflict.body)
		}
		replaceHeaders := map[string]string{
			"Authorization":      "Bearer " + f.customerToken,
			"X-Location-Context": contextID,
			"Idempotency-Key":    f.key("repurchase-replace-shop"),
		}
		replaced := lbsHTTP(t, router, http.MethodPost, "/api/v1/cart/repurchase", replaceHeaders, map[string]any{
			"items":             []map[string]any{{"product_id": fmt.Sprint(catalog.productID), "quantity": 1}},
			"replace_selection": true,
		})
		if replaced.status != http.StatusOK {
			t.Fatalf("replace-selection repurchase failed: status=%d body=%#v", replaced.status, replaced.body)
		}
		replacedItems := array(t, object(t, object(t, replaced.body["data"])["cart"])["items"])
		if len(replacedItems) != 2 {
			t.Fatalf("replace-selection must preserve unselected other-shop item: %#v", replacedItems)
		}
		for _, raw := range replacedItems {
			item := object(t, raw)
			if item["shop_id"] == fmt.Sprint(catalog.shopB) && item["selected"] != false {
				t.Fatalf("other shop remained selected: %#v", item)
			}
			if item["shop_id"] == "4201" && item["selected"] != true {
				t.Fatalf("service shop was not selected: %#v", item)
			}
		}
	})
}
