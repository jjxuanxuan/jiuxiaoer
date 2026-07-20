package mq

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"

	"jiuxiaoer-admin/backend-go/internal/config"
)

// TestRabbitMQAcceptanceContractAndOperations 验证Rabbit 消息队列验收 Contract And Operations的预期行为。
func TestRabbitMQAcceptanceContractAndOperations(t *testing.T) {
	t.Run("ACC-RMQ-011-retry-schedule-is-10s-1m-10m", func(t *testing.T) {
		spec, err := DefaultConsumerSpec("notification")
		if err != nil {
			t.Fatal(err)
		}
		want := []time.Duration{10 * time.Second, time.Minute, 10 * time.Minute}
		if !slices.Equal(spec.RetryDelays, want) {
			t.Fatalf("notification retry schedule=%v want=%v", spec.RetryDelays, want)
		}
		queues := map[time.Duration]bool{}
		dead := false
		for _, queue := range DefaultTopology().Queues {
			if queue.Consumer == "notification" && queue.RetryDelay > 0 {
				arguments := queueArguments(queue)
				if arguments["x-dead-letter-exchange"] != exchangeName {
					t.Fatalf("retry queue %s does not dead-letter to main exchange", queue.Name)
				}
				queues[queue.RetryDelay] = true
			}
			if queue.Name == "jxe.notification.dead.v1.queue" {
				dead = true
			}
		}
		for _, delay := range want {
			if !queues[delay] {
				t.Fatalf("missing retry queue for %v", delay)
			}
		}
		if !dead {
			t.Fatal("notification dead queue is missing")
		}
	})

	t.Run("ACC-RMQ-015-sensitive-payload-is-rejected", func(t *testing.T) {
		definition, _ := MustDefaultEventRegistry().Lookup("cache.invalidate")
		for _, payload := range []string{
			`{"keys":["x"],"access_token":"secret"}`,
			`{"keys":["x"],"document_no":"11010519491231002X"}`,
			`{"keys":["x"],"pickup_code":"123456"}`,
		} {
			event := OutboxEvent{EventID: uuid.NewString(), EventType: "cache.invalidate", AggregateType: "product", AggregateID: 1, Payload: datatypes.JSON(payload), CreatedAt: time.Now()}
			if _, err := BuildEnvelope(event, definition, "acceptance"); err == nil || !strings.Contains(err.Error(), "MQ_SENSITIVE_FIELD_FORBIDDEN") {
				t.Fatalf("sensitive payload was accepted: payload=%s err=%v", payload, err)
			}
		}
	})

	t.Run("ACC-RMQ-019-alerts-link-queue-consumer-and-runbook", func(t *testing.T) {
		body, err := os.ReadFile("../../../deploy/prometheus/alerts.yml")
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		for _, required := range []string{"jxe_mq_queue_ready", "jxe_mq_queue_consumers", "jxe_mq_topology_drift", "rabbitmq-event-backbone"} {
			if !strings.Contains(text, required) {
				t.Fatalf("MQ alert evidence is missing %q", required)
			}
		}
	})

	t.Run("ACC-RMQ-020-db-only-rollback-switches-are-explicit", func(t *testing.T) {
		t.Setenv("JXE_MQ_PUBLISH_ENABLED", "false")
		t.Setenv("JXE_MQ_CONSUMER_NOTIFICATION_ENABLED", "false")
		t.Setenv("JXE_MQ_CONSUMER_PRINT_ENABLED", "false")
		t.Setenv("JXE_MQ_CONSUMER_CACHE_ENABLED", "false")
		t.Setenv("JXE_MQ_CONSUMER_SECURITY_ENABLED", "false")
		t.Setenv("JXE_MQ_DB_FALLBACK_ENABLED", "true")
		cfg := config.Load()
		if cfg.Feature.MQPublisherEnabled || cfg.MQ.ConsumerNotificationEnabled || cfg.MQ.ConsumerPrintEnabled || cfg.MQ.ConsumerCacheEnabled || cfg.MQ.ConsumerSecurityEnabled || !cfg.MQ.DBFallbackEnabled {
			t.Fatalf("DB-only rollback configuration is not isolated: %+v", cfg.MQ)
		}
	})

	t.Run("ACC-RMQ-021-all-outbox-events-are-registered", func(t *testing.T) {
		registry := MustDefaultEventRegistry()
		for eventType := range collectLiteralOutboxEvents(t, "..") {
			if _, ok := registry.Lookup(eventType); !ok {
				t.Fatalf("unregistered outbox event: %s", eventType)
			}
		}
		for _, eventType := range []string{"member.tier.changed", "asset.transaction.posted", "asset.hold.created"} {
			definition, ok := registry.Lookup(eventType)
			if !ok || definition.Status != EventNoConsumer {
				t.Fatalf("phase-two event %s is not no_consumer", eventType)
			}
		}
	})

	t.Run("ACC-RMQ-022-unregistered-cross-domain-event-is-blocked", func(t *testing.T) {
		if _, ok := MustDefaultEventRegistry().Lookup("marketing.campaign.started"); ok {
			t.Fatal("undeclared phase-two event unexpectedly exists")
		}
		_, err := eventBody(OutboxEvent{EventID: uuid.NewString(), EventType: "marketing.campaign.started", AggregateType: "campaign", AggregateID: 1, Payload: datatypes.JSON(`{}`), CreatedAt: time.Now()})
		if err == nil || !strings.Contains(err.Error(), "MQ_EVENT_UNREGISTERED") {
			t.Fatalf("unregistered event was not blocked: %v", err)
		}
	})
}
