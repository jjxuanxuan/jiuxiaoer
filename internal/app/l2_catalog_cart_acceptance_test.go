package app

import (
	"fmt"
	"net/http"
	"testing"
)

type acceptanceCatalog struct {
	productID, shopB, shopProductA, shopProductB, stockA, stockB uint64
	name                                                         string
}

// createCatalog 创建Catalog。
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

// TestL2CatalogAndCartMissingAcceptanceScenarios 验证L 2 Catalog And 购物车 Missing 验收 Scenarios的预期行为。
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
