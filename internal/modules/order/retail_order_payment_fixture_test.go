package order_test

import (
	"testing"

	"jiuxiaoer-admin/backend-go/internal/modules/order"
)

func retailOrderPaymentFixture(
	t *testing.T,
	orderID uint64,
	payment order.Payment,
) order.Payment {
	t.Helper()
	if orderID == 0 {
		t.Fatal("retail payment fixture requires a non-zero order id")
	}
	bizType := order.RetailOrderPaymentBusiness
	payment.BizType = &bizType
	payment.BizID = &orderID
	payment.OrderID = &orderID
	return payment
}

func stringPointer(value string) *string {
	return &value
}
