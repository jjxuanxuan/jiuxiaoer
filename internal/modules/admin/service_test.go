package admin

import (
	"testing"

	"gorm.io/datatypes"
)

// TestAuditLogDTOIncludesBeforeAfter 验证审计日志 DTO 包含变更前后数据。
func TestAuditLogDTOIncludesBeforeAfter(t *testing.T) {
	requestID := "req_test"
	eventID := "audit_event_test"
	ipHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	accountID, shopID, orderID, deliveryID, version := uint64(9), uint64(10), uint64(11), uint64(12), uint64(3)
	errorCode, reasonCode, beforeStatus, afterStatus := "ORDER_STATE_CONFLICT", "manual", "paid", "accepted"
	row := AuditLog{
		ID:           1,
		EventID:      &eventID,
		ActorType:    "merchant",
		ActorID:      2,
		AccountID:    &accountID,
		Action:       "stock.adjust",
		ResourceType: "shop_product",
		ResourceID:   3,
		ShopID:       &shopID,
		OrderID:      &orderID,
		DeliveryID:   &deliveryID,
		BeforeData:   datatypes.JSON([]byte(`{"available_qty":10}`)),
		AfterData:    datatypes.JSON([]byte(`{"available_qty":12}`)),
		Result:       "success",
		ErrorCode:    &errorCode,
		ReasonCode:   &reasonCode,
		BeforeStatus: &beforeStatus,
		AfterStatus:  &afterStatus,
		Version:      &version,
		RequestID:    &requestID,
		IPHash:       &ipHash,
	}

	dto := auditLogDTO(row)
	if len(dto.BeforeData) == 0 || len(dto.AfterData) == 0 {
		t.Fatal("expected before_data and after_data in audit dto")
	}
	if dto.RequestID != requestID {
		t.Fatalf("expected request_id %s, got %s", requestID, dto.RequestID)
	}
	if dto.EventID != eventID || dto.AccountID != "9" || dto.ShopID != "10" || dto.OrderID != "11" || dto.DeliveryID != "12" || dto.Version != 3 || dto.IPHash != ipHash {
		t.Fatalf("structured audit fields missing: %+v", dto)
	}
	if dto.ErrorCode != errorCode || dto.ReasonCode != reasonCode || dto.BeforeStatus != beforeStatus || dto.AfterStatus != afterStatus {
		t.Fatalf("structured audit state/error fields missing: %+v", dto)
	}
}
