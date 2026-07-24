package realtime

import (
	"context"
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/modules/mq"
)

// TestRecipientMappingAssignmentClosesOnlyLosingRiders 验证分配事件只关闭未获分配骑手的映射。
func TestRecipientMappingAssignmentClosesOnlyLosingRiders(t *testing.T) {
	db := realtimeSQLite(t)
	if err := db.Exec("CREATE TABLE dispatch_candidates (id INTEGER PRIMARY KEY,job_id INTEGER,rider_id INTEGER,rank_no INTEGER,eligible INTEGER)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE dispatch_offers (id INTEGER PRIMARY KEY,job_id INTEGER,rider_id INTEGER)").Error; err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO dispatch_candidates(id,job_id,rider_id,rank_no,eligible) VALUES (1,700,101,1,1),(2,700,202,2,1),(3,700,303,3,1),(4,700,404,4,0)",
		"INSERT INTO dispatch_offers(id,job_id,rider_id) VALUES (11,700,303)",
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	handler := &MQHandler{}
	targets, err := handler.targets(context.Background(), db, mq.EventEnvelope{EventType: "delivery.assigned", OccurredAt: time.Now()}, map[string]any{
		"delivery_order_id": "800", "dispatch_job_id": "700", "rider_id": "202", "order_id": "900",
	})
	if err != nil {
		t.Fatal(err)
	}
	winnerAssigned := 0
	for _, target := range targets {
		if target.riderID == 404 {
			t.Fatal("ineligible rider must not receive realtime delivery")
		}
		if target.riderID == 202 {
			if target.clientEvent != "delivery.assigned" {
				t.Fatalf("winner received close event %s", target.clientEvent)
			}
			winnerAssigned++
		}
		if target.clientEvent == "dispatch.grab.closed" || target.clientEvent == "dispatch.offer.closed" {
			if target.payload["reason_code"] != "assigned_elsewhere" {
				t.Fatalf("unexpected close reason: %+v", target.payload)
			}
		}
		for key := range target.payload {
			switch key {
			case "delivery_order_id", "dispatch_job_id", "offer_id", "shop_id", "reason_code":
			default:
				t.Fatalf("non-whitelisted realtime payload key %q", key)
			}
		}
	}
	if winnerAssigned != 1 {
		t.Fatalf("winner assignment count=%d targets=%+v", winnerAssigned, targets)
	}
}

// TestOfferSourceMapsToOpenedClientEvent 验证派单来源映射为已打开的客户端事件。
func TestOfferSourceMapsToOpenedClientEvent(t *testing.T) {
	handler := &MQHandler{}
	targets, err := handler.targets(context.Background(), nil, mq.EventEnvelope{EventType: "dispatch.offer.created", OccurredAt: time.Now()}, map[string]any{
		"offer_id": "501", "delivery_order_id": "601", "rider_id": "101", "expires_at": time.Now().Add(time.Minute).Format(time.RFC3339Nano), "sound_key": "new_delivery_offer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].clientEvent != "dispatch.offer.opened" || targets[0].soundKey != "new_delivery_offer" {
		t.Fatalf("unexpected offer mapping: %+v", targets)
	}
}
