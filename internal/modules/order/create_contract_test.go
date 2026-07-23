package order

import (
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/modules/customerlocation"
	"jiuxiaoer-admin/backend-go/internal/modules/servicearea"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestAggregateItemsRejectsDuplicateShopProduct(t *testing.T) {
	_, err := aggregateItems([]OrderCreateItemReq{
		{ShopProductID: "101", Quantity: 1},
		{ShopProductID: "101", Quantity: 2},
	})
	if got := problem.FromError(err).ErrorCode; got != "ORDER_DUPLICATE_ITEM" {
		t.Fatalf("duplicate item error code = %q, want ORDER_DUPLICATE_ITEM", got)
	}
}

func TestContextServiceShopMatchesOnlySelectedServiceShop(t *testing.T) {
	value := customerlocation.LocationContext{
		ServiceShop: &servicearea.ShopDTO{ID: "101", Selectable: true},
		CandidateShops: []servicearea.ShopDTO{
			{ID: "101", Selectable: true},
			{ID: "202", Selectable: true},
		},
	}
	if !contextServiceShopMatches(value, "101") {
		t.Fatal("selected service shop must match")
	}
	if contextServiceShopMatches(value, "202") {
		t.Fatal("a selectable candidate is not the current service shop")
	}

	value.ServiceShop.Selectable = false
	if contextServiceShopMatches(value, "101") {
		t.Fatal("an unselectable service shop must not match")
	}
}

func TestValidateEnforcedOrderResolution(t *testing.T) {
	valid := func() *servicearea.ResolveDTO {
		return &servicearea.ResolveDTO{
			ResolvedAt: time.Now().UTC(),
			ServiceShop: servicearea.ShopDTO{
				ID: "101", Selectable: true, ServiceAreaVersion: 3,
				DeliveryPromise: servicearea.DeliveryPromiseDTO{
					DeliveryFeeAmount: 500, ETAMinMinutes: 15, ETAMaxMinutes: 25, Confirmed: true,
				},
			},
		}
	}

	tests := []struct {
		name     string
		resolved *servicearea.ResolveDTO
		shopID   string
		wantCode string
	}{
		{name: "empty resolution", resolved: nil, shopID: "101", wantCode: "SERVICE_AREA_UNAVAILABLE"},
		{name: "cross shop", resolved: valid(), shopID: "202", wantCode: "SERVICE_SHOP_CHANGED"},
		{name: "same shop", resolved: valid(), shopID: "101"},
	}
	invalidPromise := valid()
	invalidPromise.ServiceShop.DeliveryPromise.Confirmed = false
	tests = append(tests, struct {
		name     string
		resolved *servicearea.ResolveDTO
		shopID   string
		wantCode string
	}{name: "unconfirmed promise", resolved: invalidPromise, shopID: "101", wantCode: "DELIVERY_PROMISE_UNAVAILABLE"})
	invertedETA := valid()
	invertedETA.ServiceShop.DeliveryPromise.ETAMinMinutes = 30
	invertedETA.ServiceShop.DeliveryPromise.ETAMaxMinutes = 20
	tests = append(tests, struct {
		name     string
		resolved *servicearea.ResolveDTO
		shopID   string
		wantCode string
	}{name: "inverted promise ETA", resolved: invertedETA, shopID: "101", wantCode: "DELIVERY_PROMISE_UNAVAILABLE"})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateEnforcedOrderResolution(tt.resolved, tt.shopID)
			got := ""
			if err != nil {
				got = problem.FromError(err).ErrorCode
			}
			if got != tt.wantCode {
				t.Fatalf("error code = %q, want %q", got, tt.wantCode)
			}
		})
	}
}

func TestEnhanceResolvedShopKeepsTrustedCommercialPromise(t *testing.T) {
	distance, duration := uint64(1200), uint64(360)
	current := servicearea.ShopDTO{
		ID: "101", Selectable: true, ServiceAreaVersion: 7,
		DeliveryPromise: servicearea.DeliveryPromiseDTO{
			DeliveryFeeAmount: 500, ETAMinMinutes: 15, ETAMaxMinutes: 25, Confirmed: true,
		},
	}
	observed := servicearea.ShopDTO{
		ID: "101", ServiceAreaVersion: 7, SelectionSource: "automatic",
		DeliveryPromise: servicearea.DeliveryPromiseDTO{
			DeliveryFeeAmount: 0, ETAMinMinutes: 0, ETAMaxMinutes: 0,
			RouteDistanceM: &distance, RouteDurationSeconds: &duration, RouteSource: "amap",
		},
	}

	got := enhanceResolvedShop(current, observed)
	if got.DeliveryPromise.DeliveryFeeAmount != 500 || got.DeliveryPromise.ETAMinMinutes != 15 || got.DeliveryPromise.ETAMaxMinutes != 25 || !got.DeliveryPromise.Confirmed {
		t.Fatalf("observed LBS overwrote trusted commercial promise: %+v", got.DeliveryPromise)
	}
	if got.DeliveryPromise.RouteDistanceM != &distance || got.DeliveryPromise.RouteDurationSeconds != &duration || got.DeliveryPromise.RouteSource != "amap" {
		t.Fatalf("observed LBS route enhancement was not retained: %+v", got.DeliveryPromise)
	}
}
