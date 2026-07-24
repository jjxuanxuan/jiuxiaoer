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

// TestTypedAuditModelsExposeStructuredColumns 防止模块内模型悄然绕过
// 全局 GORM 审计回调所填充的字段。基于映射的写入器无需模型，
// 并由 auditlog 的回调测试覆盖；所有强类型写入器都必须公开这套通用列契约。
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
