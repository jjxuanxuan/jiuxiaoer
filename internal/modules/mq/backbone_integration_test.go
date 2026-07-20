package mq

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/datatypes"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	rabbitinfra "jiuxiaoer-admin/backend-go/internal/infra/rabbitmq"
	redisinfra "jiuxiaoer-admin/backend-go/internal/infra/redis"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestCacheBackbonePublishConsumeAndDuplicate100 验证缓存主干 Publish Consume And 重复项 100的预期行为。
func TestCacheBackbonePublishConsumeAndDuplicate100(t *testing.T) {
	if os.Getenv("JXE_RUN_MQ_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_MQ_INTEGRATION=1 and use a clean RabbitMQ test vhost")
	}
	cfg := config.Load()
	parsedRabbitURL, err := url.Parse(cfg.RabbitMQ.URL)
	if err != nil || parsedRabbitURL.Path == "" || parsedRabbitURL.Path == "/" {
		t.Fatal("MQ integration must use an isolated non-root RabbitMQ vhost")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := mysql.Open(ctx, cfg.MySQL, log)
	if err != nil || db == nil {
		t.Fatalf("open MySQL: %v", err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	redisClient, err := redisinfra.Open(ctx, cfg.Redis, log)
	if err != nil || redisClient == nil {
		t.Fatalf("open Redis: %v", err)
	}
	defer redisClient.Close()
	rabbit, err := rabbitinfra.Open(ctx, cfg.RabbitMQ, log)
	if err != nil || rabbit == nil {
		t.Fatalf("open RabbitMQ: %v", err)
	}
	defer rabbit.Close()

	generator := snowflake.New(904)
	eventID := uuid.NewString()
	cacheKey := "mq:integration:" + eventID
	if err := redisClient.Set(ctx, cacheKey, "stale", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	defer redisClient.Del(context.Background(), cacheKey)
	priorityID := priorityOutboxIDs(t, db, 1, generator)[0]
	event := OutboxEvent{
		ID: priorityID, EventID: eventID, EventType: "cache.invalidate", AggregateType: "product", AggregateID: priorityID,
		Payload: datatypes.JSON([]byte(`{"keys":["` + cacheKey + `"],"reason":"integration"}`)), Status: "pending",
		CreatedAt: time.Date(1998, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := db.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Where("consumer_name='cache' AND event_id=?", eventID).Delete(&ConsumerReceipt{})
	defer db.Where("event_id=?", eventID).Delete(&OutboxEvent{})

	registry := MustDefaultEventRegistry()
	spec, _ := DefaultConsumerSpec("cache")
	runtime := NewConsumerRuntime(spec, db, rabbit, registry, NewCacheInvalidationHandler(redisClient), generator, nil, "mq-integration", log)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.Run(ctx)
	}()
	waitFor(t, 5*time.Second, func() bool {
		connection, connectionErr := rabbit.Connection(ctx)
		if connectionErr != nil {
			return false
		}
		channel, channelErr := connection.Channel()
		if channelErr != nil {
			return false
		}
		defer channel.Close()
		queue, inspectErr := channel.QueueInspect(cacheQueueName)
		return inspectErr == nil && queue.Consumers == 1
	})

	publisher := NewPublisher(db, rabbit, nil, "mq-integration", log, WithPublisherRegistry(registry), WithPublisherEnvironment("integration"), WithPublisherIDs(generator))
	publisher.batchSize = 1
	if err := publisher.publishBatch(ctx); err != nil {
		t.Fatalf("publish Outbox event: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		var receipt ConsumerReceipt
		if err := db.Where("consumer_name='cache' AND event_id=?", eventID).First(&receipt).Error; err != nil || receipt.Status != "succeeded" {
			return false
		}
		exists, redisErr := redisClient.Exists(ctx, cacheKey).Result()
		return redisErr == nil && exists == 0
	})

	definition, _ := registry.Lookup(event.EventType)
	envelope, err := BuildEnvelope(event, definition, "integration")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(envelope)
	connection, err := rabbit.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := connection.Channel()
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 100; index++ {
		if err := channel.PublishWithContext(ctx, exchangeName, event.EventType, false, false, amqp.Publishing{ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageId: eventID, Type: event.EventType, Body: body}); err != nil {
			_ = channel.Close()
			t.Fatal(err)
		}
	}
	_ = channel.Close()
	waitFor(t, 10*time.Second, func() bool {
		var count int64
		var receipt ConsumerReceipt
		if err := db.Model(&ConsumerReceipt{}).Where("consumer_name='cache' AND event_id=?", eventID).Count(&count).Error; err != nil || count != 1 {
			return false
		}
		if err := db.Where("consumer_name='cache' AND event_id=?", eventID).First(&receipt).Error; err != nil || receipt.Status != "succeeded" || receipt.Attempts != 1 {
			return false
		}
		_, queues, complete := VerifyManagedTopology(ctx, rabbit, DefaultTopology())
		queue, ok := queues[cacheQueueName]
		return complete && ok && queue.Ready == 0 && queue.Unacknowledged == 0
	})
	t.Run("ACC-RMQ-005-duplicate-100-has-one-receipt", func(t *testing.T) {
		var count int64
		var receipt ConsumerReceipt
		if err := db.Model(&ConsumerReceipt{}).Where("consumer_name='cache' AND event_id=?", eventID).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Where("consumer_name='cache' AND event_id=?", eventID).First(&receipt).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 || receipt.Attempts != 1 || receipt.Status != "succeeded" {
			t.Fatalf("duplicate receipt count=%d attempts=%d status=%s", count, receipt.Attempts, receipt.Status)
		}
	})
	t.Run("ACC-RMQ-023-target-cache-invalidated-only", func(t *testing.T) {
		exists, err := redisClient.Exists(ctx, cacheKey).Result()
		if err != nil || exists != 0 {
			t.Fatalf("target cache still exists: exists=%d err=%v", exists, err)
		}
	})

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cache consumer did not stop after context cancellation")
	}

	t.Run("ACC-RMQ-024-consumer-stop-and-recovery-drains-cache-event", func(t *testing.T) {
		if err := redisClient.Set(context.Background(), cacheKey, "stale-after-stop", time.Minute).Err(); err != nil {
			t.Fatal(err)
		}
		recoveryID := uuid.NewString()
		defer db.Where("consumer_name='cache' AND event_id=?", recoveryID).Delete(&ConsumerReceipt{})
		recoveryEvent := OutboxEvent{
			ID: generator.Next(), EventID: recoveryID, EventType: "cache.invalidate", AggregateType: "product", AggregateID: generator.Next(),
			Payload: datatypes.JSON([]byte(`{"keys":["` + cacheKey + `"],"reason":"consumer-recovery"}`)), Status: "published", CreatedAt: time.Now(),
		}
		recoveryEnvelope, err := BuildEnvelope(recoveryEvent, definition, "integration")
		if err != nil {
			t.Fatal(err)
		}
		recoveryBody, _ := json.Marshal(recoveryEnvelope)
		connection, err := rabbit.Connection(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		channel, err := connection.Channel()
		if err != nil {
			t.Fatal(err)
		}
		if err := channel.Confirm(false); err != nil {
			_ = channel.Close()
			t.Fatal(err)
		}
		confirmations := channel.NotifyPublish(make(chan amqp.Confirmation, 1))
		if err := channel.PublishWithContext(context.Background(), exchangeName, recoveryEvent.EventType, false, false, amqp.Publishing{
			ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageId: recoveryID, Type: recoveryEvent.EventType, Body: recoveryBody,
		}); err != nil {
			_ = channel.Close()
			t.Fatal(err)
		}
		select {
		case confirmation := <-confirmations:
			if !confirmation.Ack {
				_ = channel.Close()
				t.Fatal("RabbitMQ rejected recovery event")
			}
		case <-time.After(5 * time.Second):
			_ = channel.Close()
			t.Fatal("RabbitMQ did not confirm recovery event")
		}
		_ = channel.Close()
		var observed bool
		var lastQueue amqp.Queue
		var lastExists int64
		var lastInspectErr, lastRedisErr error
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			connection, connectErr := rabbit.Connection(context.Background())
			if connectErr != nil {
				lastInspectErr = connectErr
				time.Sleep(20 * time.Millisecond)
				continue
			}
			inspect, channelErr := connection.Channel()
			if channelErr != nil {
				lastInspectErr = channelErr
				time.Sleep(20 * time.Millisecond)
				continue
			}
			lastQueue, lastInspectErr = inspect.QueueInspect(cacheQueueName)
			_ = inspect.Close()
			lastExists, lastRedisErr = redisClient.Exists(context.Background(), cacheKey).Result()
			if lastInspectErr == nil && lastQueue.Consumers == 0 && lastQueue.Messages > 0 && lastRedisErr == nil && lastExists == 1 {
				observed = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !observed {
			t.Fatalf("event was not durably queued while consumer stopped: queue=%+v exists=%d inspect_err=%v redis_err=%v", lastQueue, lastExists, lastInspectErr, lastRedisErr)
		}

		recoveryCtx, recoveryCancel := context.WithCancel(context.Background())
		recoveryRuntime := NewConsumerRuntime(spec, db, rabbit, registry, NewCacheInvalidationHandler(redisClient), generator, nil, "mq-integration-recovery", log)
		recoveryDone := make(chan struct{})
		go func() {
			defer close(recoveryDone)
			recoveryRuntime.Run(recoveryCtx)
		}()
		waitFor(t, 10*time.Second, func() bool {
			var receipt ConsumerReceipt
			if err := db.Where("consumer_name='cache' AND event_id=?", recoveryID).First(&receipt).Error; err != nil || receipt.Status != "succeeded" {
				return false
			}
			exists, redisErr := redisClient.Exists(context.Background(), cacheKey).Result()
			return redisErr == nil && exists == 0
		})
		recoveryCancel()
		select {
		case <-recoveryDone:
		case <-time.After(5 * time.Second):
			t.Fatal("recovered cache consumer did not stop")
		}
	})
}

// waitFor 处理wait For相关逻辑。
func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}
