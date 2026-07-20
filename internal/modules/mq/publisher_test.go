package mq

import (
	"encoding/json"
	"testing"
	"time"

	"gorm.io/datatypes"
)

// TestExchangeNamesMatchPRD 验证Exchange Names Match PRD的预期行为。
func TestExchangeNamesMatchPRD(t *testing.T) {
	if exchangeName != "jxe.events.topic.v2" {
		t.Fatalf("expected consumer-scoped exchange jxe.events.topic.v2, got %s", exchangeName)
	}
	if deadExchangeName == "" || unroutedExchangeName == "" || unroutedQueueName == "" {
		t.Fatal("expected dead letter topology constants")
	}
	required := map[string]bool{"cache.invalidate": false, "payment.succeeded": false, "print.task.ready": false}
	for _, binding := range DefaultTopology().Bindings {
		if _, ok := required[binding.RoutingKey]; ok {
			required[binding.RoutingKey] = true
		}
	}
	for routingKey, found := range required {
		if !found {
			t.Fatalf("missing durable queue binding for %s", routingKey)
		}
	}
}

// TestEventBodyIncludesRequiredMetadata 验证事件请求体 Includes Required Metadata的预期行为。
func TestEventBodyIncludesRequiredMetadata(t *testing.T) {
	requestID := "req_test"
	body, err := eventBody(OutboxEvent{
		EventID:       "2f3321dd-6495-4af5-914c-78c97d02712e",
		EventType:     "order.paid",
		AggregateType: "order",
		AggregateID:   1,
		Payload:       datatypes.JSON([]byte(`{"order_id":"1","payment_id":"2"}`)),
		RequestID:     &requestID,
		CreatedAt:     time.Date(2026, 7, 10, 1, 2, 3, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("event body error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("invalid event body: %v", err)
	}
	for _, key := range []string{"spec_version", "event_id", "event_type", "event_version", "occurred_at", "request_id", "partition_key", "metadata"} {
		if got[key] == nil {
			t.Fatalf("expected %s in event body", key)
		}
	}
}
