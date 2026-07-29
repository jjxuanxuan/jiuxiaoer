package mq

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

func TestWineTicketOperationalEventsMatchProducerPayloads(t *testing.T) {
	tests := []struct {
		eventType     string
		aggregateType string
		required      []string
		payload       map[string]any
		notification  bool
	}{
		{
			eventType: "wine_ticket.renewed", aggregateType: "wine_ticket_renewal",
			required: []string{
				"renewal_no", "lot_no", "customer_id",
				"old_expires_at", "new_expires_at",
			},
			payload: map[string]any{
				"renewal_no": "WTRN1", "lot_no": "WTL1", "customer_id": "1",
				"old_expires_at": "2026-08-01T00:00:00+08:00",
				"new_expires_at": "2026-08-31T00:00:00+08:00",
			},
			notification: true,
		},
		{
			eventType: "wine_ticket.refund_created", aggregateType: "wine_ticket_refund",
			required: []string{"refund_no", "purchase_no", "customer_id", "status"},
			payload: map[string]any{
				"refund_no": "WTRF1", "purchase_no": "WTPU1",
				"customer_id": "1", "status": "holding",
			},
		},
		{
			eventType: "wine_ticket.refund_retry_pending", aggregateType: "wine_ticket_refund",
			required: []string{"refund_no", "purchase_no", "common_refund_id"},
			payload: map[string]any{
				"refund_no": "WTRF1", "purchase_no": "WTPU1",
				"common_refund_id": "2",
			},
		},
		{
			eventType: "wine_ticket.refund_exception", aggregateType: "wine_ticket_refund",
			required: []string{"refund_no", "purchase_no"},
			payload: map[string]any{
				"refund_no": "WTRF1", "purchase_no": "WTPU1",
				"error_code": "PROVIDER_REJECTED",
			},
		},
		{
			eventType: "wine_ticket.refund_succeeded", aggregateType: "wine_ticket_refund",
			required: []string{"refund_no", "purchase_no", "customer_id", "amount"},
			payload: map[string]any{
				"refund_no": "WTRF1", "purchase_no": "WTPU1",
				"customer_id": "1", "amount": int64(12800),
			},
			notification: true,
		},
		{
			eventType:     "wine_ticket.delivery_time_slot_changed",
			aggregateType: "delivery_time_slot",
			required: []string{
				"slot_id", "shop_id", "action", "status",
				"service_date", "version",
			},
			payload: map[string]any{
				"slot_id": "11", "shop_id": "22", "action": "created",
				"status": "open", "service_date": "2026-07-28",
				"version": uint(1),
			},
		},
	}

	registry := MustDefaultEventRegistry()
	for _, test := range tests {
		t.Run(test.eventType, func(t *testing.T) {
			definition, ok := registry.Lookup(test.eventType)
			if !ok {
				t.Fatalf("event is not registered")
			}
			expectedStatus := EventNoConsumer
			var expectedConsumers []string
			if test.notification {
				expectedStatus = EventActive
				expectedConsumers = []string{"notification"}
			}
			if definition.Status != expectedStatus ||
				definition.AggregateType != test.aggregateType ||
				!slices.Equal(definition.Consumers, expectedConsumers) ||
				!slices.Equal(definition.RequiredFields, test.required) {
				t.Fatalf("unexpected definition: %+v", definition)
			}
			payload, err := json.Marshal(test.payload)
			if err != nil {
				t.Fatal(err)
			}
			event := OutboxEvent{
				EventID: uuid.NewString(), EventType: test.eventType,
				AggregateType: test.aggregateType, AggregateID: 1,
				Payload: datatypes.JSON(payload), CreatedAt: time.Now(),
			}
			if _, err := BuildEnvelope(event, definition, "test"); err != nil {
				t.Fatalf("producer payload does not satisfy registry: %v", err)
			}

			delete(test.payload, test.required[0])
			payload, err = json.Marshal(test.payload)
			if err != nil {
				t.Fatal(err)
			}
			event.Payload = datatypes.JSON(payload)
			if _, err := BuildEnvelope(event, definition, "test"); err == nil ||
				!strings.Contains(err.Error(), "MQ_PAYLOAD_REQUIRED_FIELD_MISSING") {
				t.Fatalf("missing required payload field was accepted: %v", err)
			}
		})
	}
}

func TestWineTicketSettlementEventsRouteToNotification(t *testing.T) {
	registry := MustDefaultEventRegistry()
	topology := DefaultTopology()
	for _, eventType := range []string{
		"wine_ticket.renewed",
		"wine_ticket.refund_succeeded",
	} {
		definition, ok := registry.Lookup(eventType)
		if !ok {
			t.Fatalf("event %s is not registered", eventType)
		}
		if definition.Status != EventActive ||
			!slices.Equal(definition.Consumers, []string{"notification"}) {
			t.Fatalf("%s notification contract mismatch: %+v", eventType, definition)
		}
		bound := false
		for _, binding := range topology.Bindings {
			if binding.Queue == notificationQueueName &&
				topicMatches(binding.RoutingKey, eventType) {
				bound = true
				break
			}
		}
		if !bound {
			t.Fatalf("%s is not bound to the notification queue", eventType)
		}
	}
}
