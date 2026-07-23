package auditlog_test

import (
	"reflect"
	"testing"

	"jiuxiaoer-admin/backend-go/internal/modules/admin"
	"jiuxiaoer-admin/backend-go/internal/modules/aftersale"
	"jiuxiaoer-admin/backend-go/internal/modules/asset"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/delivery"
	"jiuxiaoer-admin/backend-go/internal/modules/deliveryincident"
	"jiuxiaoer-admin/backend-go/internal/modules/deliveryreturn"
	"jiuxiaoer-admin/backend-go/internal/modules/home"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/store"
)

// TestTypedAuditModelsExposeStructuredColumns prevents a module-local model
// from silently bypassing fields populated by the global GORM audit callback.
// Map-based writers do not need a model and are covered by auditlog's callback
// tests; every typed writer must expose this common column contract.
func TestTypedAuditModelsExposeStructuredColumns(t *testing.T) {
	models := map[string]any{
		"admin":            admin.AuditLog{},
		"aftersale":        aftersale.AuditLog{},
		"asset":            asset.AuditLog{},
		"auth":             auth.AuditLog{},
		"delivery":         delivery.AuditLog{},
		"deliveryincident": deliveryincident.AuditLog{},
		"deliveryreturn":   deliveryreturn.AuditLog{},
		"home":             home.AuditLog{},
		"order":            order.AuditLog{},
		"refund":           refund.Audit{},
		"store":            store.AuditLog{},
	}
	required := []string{
		"EventID", "AccountID", "ShopID", "OrderID", "DeliveryID",
		"ErrorCode", "ReasonCode", "BeforeStatus", "AfterStatus", "Version",
		"RequestID", "IP", "IPHash",
	}

	for name, model := range models {
		t.Run(name, func(t *testing.T) {
			typ := reflect.TypeOf(model)
			for _, field := range required {
				if _, ok := typ.FieldByName(field); !ok {
					t.Errorf("%s audit model is missing structured field %s", name, field)
				}
			}
		})
	}
}
