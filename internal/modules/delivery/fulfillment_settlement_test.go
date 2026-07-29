package delivery

import (
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestFulfillmentSettlementRegistryPreservesLegacyRetailAndFailsClosed(t *testing.T) {
	t.Parallel()
	service := NewService(nil, snowflake.New(43))

	if err := service.applyFulfillmentSettlement(
		t.Context(),
		nil,
		FulfillmentDelivered,
		DeliveryOrder{ID: 1, OrderID: 2},
		Order{ID: 2},
		time.Now(),
		"rider",
		3,
	); err != nil {
		t.Fatalf("legacy retail route must remain a no-op: %v", err)
	}

	err := service.applyFulfillmentSettlement(
		t.Context(),
		nil,
		FulfillmentDelivered,
		DeliveryOrder{ID: 1, OrderID: 2},
		Order{ID: 2, OrderType: "future_order", SettlementMode: "points"},
		time.Now(),
		"rider",
		3,
	)
	if err == nil {
		t.Fatal("unregistered non-retail fulfillment route must fail closed")
	}
	detail := problem.FromError(err)
	if detail.Status != 503 ||
		detail.ErrorCode != "FULFILLMENT_SETTLEMENT_HANDLER_NOT_FOUND" {
		t.Fatalf("unexpected problem: %+v", detail)
	}
}
