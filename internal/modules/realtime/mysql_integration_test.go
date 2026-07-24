package realtime

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestMySQLRealtimeAcknowledgementAndResumeContract 验证 MySQL 实时确认和续传契约。
func TestMySQLRealtimeAcknowledgementAndResumeContract(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run realtime MySQL integration test")
	}
	cfg := config.Load()
	if cfg.MySQL.DSN == "" {
		t.Fatal("JXE_MYSQL_DSN is required")
	}
	db, err := gorm.Open(mysql.Open(cfg.MySQL.DSN), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ids := snowflake.New(777)
	riderID, otherRiderID := ids.Next(), ids.Next()
	sourceID := uuid.NewString()
	now := time.Now().UTC()
	openDelivery := Delivery{
		ID: ids.Next(), SourceEventID: sourceID, SourceEventType: "dispatch.offer.created", ClientEventType: "dispatch.offer.opened",
		RecipientType: recipientRider, RecipientID: riderID, AggregateType: "dispatch_offer", AggregateID: ids.Next(),
		PayloadSnapshot: datatypes.JSON(`{"offer_id":"integration"}`), OccurredAt: now, ExpiresAt: now.Add(time.Hour), RelayStatus: relayPending, NextRelayAt: now,
	}
	closedDelivery := Delivery{
		ID: ids.Next(), SourceEventID: uuid.NewString(), SourceEventType: "dispatch.offer.expired", ClientEventType: "dispatch.offer.closed",
		RecipientType: recipientRider, RecipientID: riderID, AggregateType: "dispatch_offer", AggregateID: openDelivery.AggregateID,
		PayloadSnapshot: datatypes.JSON(`{"offer_id":"integration","reason_code":"expired"}`), OccurredAt: now, ExpiresAt: now.Add(time.Hour), RelayStatus: relayPending, NextRelayAt: now,
	}
	if err := db.Create(&[]Delivery{openDelivery, closedDelivery}).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Where("realtime_delivery_id IN ?", []uint64{openDelivery.ID, closedDelivery.ID}).Delete(&Acknowledgement{}).Error
		_ = db.Where("id IN ?", []uint64{openDelivery.ID, closedDelivery.ID}).Delete(&Delivery{}).Error
	})
	duplicate := openDelivery
	duplicate.ID = ids.Next()
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&duplicate).Error; err != nil {
		t.Fatal(err)
	}
	var deliveryCount int64
	if err := db.Model(&Delivery{}).Where("source_event_id=?", sourceID).Count(&deliveryCount).Error; err != nil || deliveryCount != 1 {
		t.Fatalf("delivery unique contract failed count=%d err=%v", deliveryCount, err)
	}

	service := NewService(cfg, db, nil, ids, nil)
	owner := TicketInfo{RiderID: riderID, DeviceHash: hashString("mysql-device"), Platform: "test", ClientVersion: "1.0.0"}
	other := owner
	other.RiderID = otherRiderID
	if err := service.Acknowledge(context.Background(), other, ClientFrame{DeliveryID: idStringForTest(openDelivery.ID), Outcome: "displayed"}); err == nil || problem.FromError(err).ErrorCode != "REALTIME_DELIVERY_FORBIDDEN" {
		t.Fatalf("cross-rider ACK must fail, got %v", err)
	}
	for range 2 {
		if err := service.Acknowledge(context.Background(), owner, ClientFrame{DeliveryID: idStringForTest(openDelivery.ID), Outcome: "displayed"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.Acknowledge(context.Background(), owner, ClientFrame{DeliveryID: idStringForTest(closedDelivery.ID), Outcome: "closed"}); err != nil {
		t.Fatal(err)
	}
	var ackCount int64
	if err := db.Model(&Acknowledgement{}).Where("realtime_delivery_id=? AND ack_type='displayed'", openDelivery.ID).Count(&ackCount).Error; err != nil || ackCount != 1 {
		t.Fatalf("ACK unique contract failed count=%d err=%v", ackCount, err)
	}
	resumed, _, err := service.Resume(context.Background(), owner, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range resumed {
		if row.ID == closedDelivery.ID {
			t.Fatal("ACKed close delivery was replayed")
		}
	}
}

// idStringForTest 返回测试使用的 ID 字符串。
func idStringForTest(id uint64) string { return snowflake.String(id) }
