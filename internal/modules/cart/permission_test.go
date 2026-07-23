package cart

import (
	"testing"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestCustomerIDFromClaimsRequiresExactCartPermission(t *testing.T) {
	claims := &auth.Claims{AccountType: "customer", CustomerID: "100", Permissions: []string{"cart:view"}}
	if id, err := customerIDFromClaims(claims, "cart:view"); err != nil || id != 100 {
		t.Fatalf("expected exact permission to pass, id=%d err=%v", id, err)
	}
	if _, err := customerIDFromClaims(claims, "cart:update"); problem.FromError(err).ErrorCode != "PERM_FORBIDDEN" {
		t.Fatalf("expected missing cart:update to fail closed, got %v", err)
	}
}

func TestCartAvailabilityDistinguishesShopProductAndStockFailures(t *testing.T) {
	base := CartItemRow{
		Quantity: 2, ProductStatus: "on_sale", CategoryStatus: "active",
		ShopProductStatus: "on_sale", ShopStatus: "active", BusinessStatus: "open", AvailableQty: 2,
	}
	if got := cartAvailability(base); got != "available" {
		t.Fatalf("availability=%q, want available", got)
	}
	shopClosed := base
	shopClosed.BusinessStatus = "resting"
	if got := cartAvailability(shopClosed); got != "shop_closed" {
		t.Fatalf("availability=%q, want shop_closed", got)
	}
	categoryOff := base
	categoryOff.CategoryStatus = "disabled"
	if got := cartAvailability(categoryOff); got != "not_on_sale" {
		t.Fatalf("availability=%q, want not_on_sale", got)
	}
	outOfStock := base
	outOfStock.AvailableQty = 1
	if got := cartAvailability(outOfStock); got != "out_of_stock" {
		t.Fatalf("availability=%q, want out_of_stock", got)
	}
	deletedProduct := base
	deletedProduct.ProductStatus = ""
	if got := cartAvailability(deletedProduct); got != "not_on_sale" {
		t.Fatalf("availability=%q, want not_on_sale for deleted product", got)
	}
	deletedShop := base
	deletedShop.ShopStatus = ""
	if got := cartAvailability(deletedShop); got != "shop_closed" {
		t.Fatalf("availability=%q, want shop_closed for deleted shop", got)
	}
}

func TestUnavailableSelectionUsesStableWriteErrors(t *testing.T) {
	cases := map[string]string{
		"not_on_sale":  "PRODUCT_NOT_ON_SALE",
		"out_of_stock": "STOCK_NOT_ENOUGH",
		"shop_closed":  "SHOP_CLOSED",
	}
	for reason, want := range cases {
		if got := problem.FromError(cartUnavailableMutationError(reason)).ErrorCode; got != want {
			t.Fatalf("reason=%q error_code=%q, want %q", reason, got, want)
		}
	}
}

func TestValidateCartProductChecksGlobalSaleabilityAndResultingQuantity(t *testing.T) {
	row := ShopProductRow{
		ProductStatus: "on_sale", CategoryStatus: "active", ShopProductStatus: "on_sale",
		ShopStatus: "active", BusinessStatus: "open", AvailableQty: 5,
	}
	if err := validateCartProduct(row, 5); err != nil {
		t.Fatalf("expected saleable quantity to pass: %v", err)
	}
	row.ProductStatus = "off_sale"
	if got := problem.FromError(validateCartProduct(row, 1)).ErrorCode; got != "PRODUCT_NOT_ON_SALE" {
		t.Fatalf("error_code=%q, want PRODUCT_NOT_ON_SALE", got)
	}
	row.ProductStatus = "on_sale"
	if got := problem.FromError(validateCartProduct(row, 6)).ErrorCode; got != "STOCK_NOT_ENOUGH" {
		t.Fatalf("error_code=%q, want STOCK_NOT_ENOUGH", got)
	}
}
