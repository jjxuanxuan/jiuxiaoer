package admin

import (
	"testing"

	"gorm.io/datatypes"
)

// TestAuditLogDTOIncludesBeforeAfter 验证审计日志DTO Includes Before 售后的预期行为。
func TestAuditLogDTOIncludesBeforeAfter(t *testing.T) {
	requestID := "req_test"
	row := AuditLog{
		ID:           1,
		ActorType:    "merchant",
		ActorID:      2,
		Action:       "stock.adjust",
		ResourceType: "shop_product",
		ResourceID:   3,
		BeforeData:   datatypes.JSON([]byte(`{"available_qty":10}`)),
		AfterData:    datatypes.JSON([]byte(`{"available_qty":12}`)),
		Result:       "success",
		RequestID:    &requestID,
	}

	dto := auditLogDTO(row)
	if len(dto.BeforeData) == 0 || len(dto.AfterData) == 0 {
		t.Fatal("expected before_data and after_data in audit dto")
	}
	if dto.RequestID != requestID {
		t.Fatalf("expected request_id %s, got %s", requestID, dto.RequestID)
	}
}
