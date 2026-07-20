package mq

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	rabbitinfra "jiuxiaoer-admin/backend-go/internal/infra/rabbitmq"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type mqAcceptanceFixture struct {
	ctx      context.Context
	db       *gorm.DB
	rabbit   *rabbitinfra.Manager
	registry *EventRegistry
	ids      *snowflake.Generator
	log      *slog.Logger
}

// TestRabbitMQAcceptanceFailureAndRecovery 验证Rabbit 消息队列验收 Failure And Recovery的预期行为。
func TestRabbitMQAcceptanceFailureAndRecovery(t *testing.T) {
	if os.Getenv("JXE_RUN_MQ_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_MQ_INTEGRATION=1 and use an isolated non-root RabbitMQ vhost")
	}
	fixture := newMQAcceptanceFixture(t)
	fixture.purgeQueues(t)
	defer fixture.purgeQueues(t)

	t.Run("ACC-RMQ-001-transaction-commits-without-broker", fixture.transactionWithoutBroker)
	t.Run("ACC-RMQ-002-old-backlog-drains-with-unique-event-ids", fixture.backlogDrains)
	t.Run("ACC-RMQ-003-missing-binding-routes-to-unrouted", fixture.missingBindingRoutesToUnrouted)
	t.Run("ACC-RMQ-004-two-publishers-claim-1000-once", fixture.twoPublishersClaim1000)
	t.Run("ACC-RMQ-012-max-attempt-enters-consumer-dead", fixture.maxAttemptEntersDead)
	t.Run("ACC-RMQ-013-replay-is-idempotent-and-audited", fixture.replayIsIdempotent)
	t.Run("ACC-RMQ-014-unauthorized-admin-cannot-observe-dead", fixture.unauthorizedAdminCannotObserveDead)
	t.Run("ACC-RMQ-016-succeeded-receipt-acks-redelivery", fixture.succeededReceiptAcksRedelivery)
	t.Run("ACC-RMQ-017-poison-schema-goes-directly-dead", fixture.poisonSchemaGoesDead)
	t.Run("ACC-RMQ-018-consumer-stops-on-cancellation", fixture.consumerStops)
}

// newMQAcceptanceFixture 创建并初始化消息队列验收测试夹具。
func newMQAcceptanceFixture(t *testing.T) *mqAcceptanceFixture {
	t.Helper()
	ctx := context.Background()
	cfg := config.Load()
	parsed, err := parseRabbitVHost(cfg.RabbitMQ.URL)
	if err != nil || parsed == "" || parsed == "/" {
		t.Fatalf("MQ acceptance requires an isolated non-root vhost: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := mysql.Open(ctx, cfg.MySQL, log)
	if err != nil || db == nil {
		t.Fatalf("open MySQL: %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	rabbit, err := rabbitinfra.Open(ctx, cfg.RabbitMQ, log)
	if err != nil || rabbit == nil {
		t.Fatalf("open RabbitMQ: %v", err)
	}
	t.Cleanup(func() { _ = rabbit.Close() })
	connection, err := rabbit.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := connection.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if err := DeclareTopology(channel, DefaultTopology()); err != nil {
		_ = channel.Close()
		t.Fatal(err)
	}
	_ = channel.Close()
	return &mqAcceptanceFixture{ctx: ctx, db: db, rabbit: rabbit, registry: MustDefaultEventRegistry(), ids: snowflake.New(906), log: log}
}

// parseRabbitVHost 解析Rabbit V Host。
func parseRabbitVHost(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	return parsed.Path, nil
}

// channel 返回通道。
func (f *mqAcceptanceFixture) channel(t *testing.T) *amqp.Channel {
	t.Helper()
	connection, err := f.rabbit.Connection(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := connection.Channel()
	if err != nil {
		t.Fatal(err)
	}
	return channel
}

// purgeQueues 彻底清理Queues。
func (f *mqAcceptanceFixture) purgeQueues(t *testing.T) {
	t.Helper()
	channel := f.channel(t)
	defer channel.Close()
	for _, queue := range DefaultTopology().Queues {
		if _, err := channel.QueuePurge(queue.Name, false); err != nil {
			t.Fatalf("purge %s: %v", queue.Name, err)
		}
	}
}

// transactionWithoutBroker 处理交易 Without Broker相关逻辑。
func (f *mqAcceptanceFixture) transactionWithoutBroker(t *testing.T) {
	eventID := uuid.NewString()
	id := f.ids.Next()
	err := f.db.Transaction(func(tx *gorm.DB) error {
		return tx.Create(&OutboxEvent{ID: id, EventID: eventID, EventType: "cache.invalidate", AggregateType: "product", AggregateID: id, Payload: datatypes.JSON(`{"keys":["acceptance:broker-down"]}`), Status: "pending"}).Error
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.db.Where("event_id=?", eventID).Delete(&OutboxEvent{})
	var row OutboxEvent
	if err := f.db.Where("event_id=?", eventID).First(&row).Error; err != nil || row.Status != "pending" {
		t.Fatalf("committed outbox fact was not pending: status=%s err=%v", row.Status, err)
	}
}

// backlogDrains 处理backlog Drains相关逻辑。
func (f *mqAcceptanceFixture) backlogDrains(t *testing.T) {
	f.purgeQueues(t)
	const count = 100
	ids := priorityOutboxIDs(t, f.db, count, f.ids)
	rows := make([]OutboxEvent, 0, count)
	eventIDs := make([]string, 0, count)
	createdAt := time.Now().Add(-10 * time.Minute)
	for index, id := range ids {
		eventID := uuid.NewString()
		eventIDs = append(eventIDs, eventID)
		rows = append(rows, OutboxEvent{ID: id, EventID: eventID, EventType: "cache.invalidate", AggregateType: "product", AggregateID: id, Payload: datatypes.JSON(fmt.Sprintf(`{"keys":["acceptance:backlog:%d"]}`, index)), Status: "pending", CreatedAt: createdAt})
	}
	if err := f.db.CreateInBatches(rows, 100).Error; err != nil {
		t.Fatal(err)
	}
	defer f.db.Where("event_id IN ?", eventIDs).Delete(&OutboxEvent{})
	publisher := NewPublisher(f.db, f.rabbit, nil, "acc-rmq-002", f.log, WithPublisherRegistry(f.registry), WithPublisherEnvironment("acceptance"), WithPublisherIDs(f.ids), WithPublisherBatchSize(500))
	started := time.Now()
	if err := publisher.publishBatch(f.ctx); err != nil {
		t.Fatal(err)
	}
	var published, distinct int64
	f.db.Model(&OutboxEvent{}).Where("event_id IN ? AND status='published'", eventIDs).Count(&published)
	f.db.Model(&OutboxEvent{}).Where("event_id IN ?", eventIDs).Distinct("event_id").Count(&distinct)
	queue, err := f.channelInspect(cacheQueueName)
	if err != nil || published != count || distinct != count || queue.Messages != count {
		t.Fatalf("backlog result published=%d distinct=%d queued=%d err=%v", published, distinct, queue.Messages, err)
	}
	if time.Since(started) >= 15*time.Minute {
		t.Fatalf("backlog recovery exceeded 15 minutes: %v", time.Since(started))
	}
	f.purgeQueues(t)
}

// missingBindingRoutesToUnrouted 处理missing Binding Routes To 未路由消息相关逻辑。
func (f *mqAcceptanceFixture) missingBindingRoutesToUnrouted(t *testing.T) {
	f.purgeQueues(t)
	channel := f.channel(t)
	if err := channel.QueueUnbind(cacheQueueName, "cache.invalidate", exchangeName, nil); err != nil {
		_ = channel.Close()
		t.Fatal(err)
	}
	defer func() {
		restore := f.channel(t)
		defer restore.Close()
		if err := restore.QueueBind(cacheQueueName, "cache.invalidate", exchangeName, false, nil); err != nil {
			t.Errorf("restore cache binding: %v", err)
		}
	}()
	event := f.envelope(t, "cache.invalidate", `{"keys":["acceptance:unrouted"]}`)
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := channel.PublishWithContext(f.ctx, exchangeName, event.EventType, false, false, amqp.Publishing{ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageId: event.EventID, Type: event.EventType, Body: body}); err != nil {
		_ = channel.Close()
		t.Fatal(err)
	}
	_ = channel.Close()
	waitFor(t, 5*time.Second, func() bool {
		queue, inspectErr := f.channelInspect(unroutedQueueName)
		return inspectErr == nil && queue.Messages == 1
	})
}

// twoPublishersClaim1000 处理two Publishers 认领 1000相关逻辑。
func (f *mqAcceptanceFixture) twoPublishersClaim1000(t *testing.T) {
	const count = 1000
	ids := priorityOutboxIDs(t, f.db, count, f.ids)
	prefix := "acc-rmq-004-" + uuid.NewString()
	rows := make([]OutboxEvent, 0, count)
	for index, id := range ids {
		rows = append(rows, OutboxEvent{ID: id, EventID: fmt.Sprintf("%s-%04d", prefix, index), EventType: "member.tier.changed", AggregateType: "member", AggregateID: id, Payload: datatypes.JSON(`{"customer_id":"1"}`), Status: "pending", CreatedAt: time.Now().Add(-time.Hour)})
	}
	if err := f.db.CreateInBatches(rows, 100).Error; err != nil {
		t.Fatal(err)
	}
	defer f.db.Where("event_id LIKE ?", prefix+"%").Delete(&OutboxEvent{})
	publishers := []*Publisher{NewPublisher(f.db, nil, nil, "acc-rmq-004-a", f.log), NewPublisher(f.db, nil, nil, "acc-rmq-004-b", f.log)}
	claimed := make(chan []OutboxEvent, 2)
	errors := make(chan error, 2)
	var group sync.WaitGroup
	for _, publisher := range publishers {
		group.Add(1)
		go func(item *Publisher) {
			defer group.Done()
			result, err := item.claimBatch(f.ctx, time.Now(), count/2)
			if err != nil {
				errors <- err
				return
			}
			claimed <- result
		}(publisher)
	}
	group.Wait()
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	close(claimed)
	unique := map[uint64]bool{}
	for batch := range claimed {
		for _, event := range batch {
			if unique[event.ID] {
				t.Fatalf("event %d had two active claims", event.ID)
			}
			unique[event.ID] = true
		}
	}
	if len(unique) != count {
		t.Fatalf("claimed %d events, want %d", len(unique), count)
	}
}

// maxAttemptEntersDead 处理max 尝试 Enters 死信相关逻辑。
func (f *mqAcceptanceFixture) maxAttemptEntersDead(t *testing.T) {
	f.purgeQueues(t)
	event := f.envelope(t, "cache.invalidate", `{"keys":["acceptance:max-retry"]}`)
	handler := ConsumerHandlerFunc(func(context.Context, *gorm.DB, EventEnvelope) (ConsumerResult, error) {
		return ConsumerResult{}, TemporaryConsumerError("ACC_TEMPORARY", "temporary acceptance failure", nil)
	})
	runtimeCtx, cancelRuntime := context.WithCancel(f.ctx)
	defer cancelRuntime()
	runtimeDone := f.startConsumer(runtimeCtx, "cache", handler)
	sinkCtx, cancelSink := context.WithCancel(f.ctx)
	defer cancelSink()
	sinkDone := f.startDeadSink(sinkCtx, "cache")
	f.publishEnvelope(t, event, amqp.Table{"x-retry-count": int32(3)})
	waitFor(t, 5*time.Second, func() bool {
		var dead DeadLetter
		var receipt ConsumerReceipt
		deadErr := f.db.Where("consumer_name='cache' AND event_id=? AND error_code='ACC_TEMPORARY'", event.EventID).First(&dead).Error
		receiptErr := f.db.Where("consumer_name='cache' AND event_id=? AND status='dead'", event.EventID).First(&receipt).Error
		return deadErr == nil && receiptErr == nil && dead.RetryCount == 3
	})
	cancelRuntime()
	cancelSink()
	f.awaitStop(t, runtimeDone)
	f.awaitStop(t, sinkDone)
	f.cleanupEventEvidence(event.EventID)
}

// replayIsIdempotent 处理replay Is Idempotent相关逻辑。
func (f *mqAcceptanceFixture) replayIsIdempotent(t *testing.T) {
	source := OutboxEvent{ID: f.ids.Next(), EventID: uuid.NewString(), EventType: "cache.invalidate", EventVersion: 1, AggregateType: "product", AggregateID: f.ids.Next(), Payload: datatypes.JSON(`{"keys":["acceptance:replay"]}`), Status: "published", CreatedAt: time.Now()}
	if err := f.db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}
	dead := DeadLetter{ID: f.ids.Next(), DeadNo: fmt.Sprintf("MD%d", f.ids.Next()), ConsumerName: "cache", EventID: source.EventID, EventType: source.EventType, EventVersion: 1, ErrorCode: "ACC_REPLAY", PayloadHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: "open", Version: 1, FirstFailedAt: time.Now(), DeadAt: time.Now()}
	dead.DeadNo = fmt.Sprintf("MD%d", dead.ID)
	if err := f.db.Create(&dead).Error; err != nil {
		t.Fatal(err)
	}
	claims := &auth.Claims{AccountType: "admin", AdminUserID: "1001", Permissions: []string{"mq:dead_letter:replay"}}
	service := NewAdminService(f.db, f.rabbit, f.registry, f.ids)
	request := ReplayRequest{ReasonCode: "FIXED_DEPENDENCY", Reason: "acceptance replay after dependency recovery", ExpectedVersion: 1}
	first, err := service.Replay(f.ctx, claims, fmt.Sprint(dead.ID), "acc-rmq-013-"+source.EventID, request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Replay(f.ctx, claims, fmt.Sprint(dead.ID), "acc-rmq-013-"+source.EventID, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.ID != second.ID || first.ReplayEventID != second.ReplayEventID {
		t.Fatalf("idempotent replay mismatch: first=%+v second=%+v", first, second)
	}
	var replayCount, outboxCount, auditCount int64
	f.db.Model(&DeadLetterReplay{}).Where("dead_letter_id=?", dead.ID).Count(&replayCount)
	f.db.Model(&OutboxEvent{}).Where("replay_of_event_id=?", source.EventID).Count(&outboxCount)
	f.db.Table("audit_logs").Where("action='mq.dead_letter.replay' AND resource_id=?", dead.ID).Count(&auditCount)
	if replayCount != 1 || outboxCount != 1 || auditCount != 1 {
		t.Fatalf("replay evidence replay=%d outbox=%d audit=%d", replayCount, outboxCount, auditCount)
	}
	f.db.Exec("DELETE FROM audit_logs WHERE action='mq.dead_letter.replay' AND resource_id=?", dead.ID)
	f.db.Model(&DeadLetter{}).Where("id=?", dead.ID).Update("last_replay_id", nil)
	f.db.Where("dead_letter_id=?", dead.ID).Delete(&DeadLetterReplay{})
	f.db.Delete(&dead)
	f.db.Where("replay_of_event_id=? OR event_id=?", source.EventID, source.EventID).Delete(&OutboxEvent{})
}

// unauthorizedAdminCannotObserveDead 处理未认证管理端 Cannot Observe 死信相关逻辑。
func (f *mqAcceptanceFixture) unauthorizedAdminCannotObserveDead(t *testing.T) {
	service := NewAdminService(f.db, f.rabbit, f.registry, f.ids)
	claims := &auth.Claims{AccountType: "admin", AdminUserID: "1001"}
	_, getErr := service.GetDeadLetter(f.ctx, claims, "1")
	_, replayErr := service.Replay(f.ctx, claims, "1", "unauthorized", ReplayRequest{ReasonCode: "TEST", Reason: "must be rejected", ExpectedVersion: 1})
	if problem.FromError(getErr).Status != 403 || problem.FromError(replayErr).Status != 403 {
		t.Fatalf("unauthorized MQ admin status get=%v replay=%v", getErr, replayErr)
	}
}

// succeededReceiptAcksRedelivery 处理succeeded Receipt Acks Redelivery相关逻辑。
func (f *mqAcceptanceFixture) succeededReceiptAcksRedelivery(t *testing.T) {
	f.purgeQueues(t)
	event := f.envelope(t, "cache.invalidate", `{"keys":["acceptance:ack-crash"]}`)
	now := time.Now()
	if err := f.db.Create(&ConsumerReceipt{ID: f.ids.Next(), ConsumerName: "cache", EventID: event.EventID, EventType: event.EventType, EventVersion: 1, Status: "succeeded", Attempts: 1, FirstReceivedAt: now, ProcessedAt: &now}).Error; err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	handler := ConsumerHandlerFunc(func(context.Context, *gorm.DB, EventEnvelope) (ConsumerResult, error) {
		calls.Add(1)
		return ConsumerResult{}, nil
	})
	runtimeCtx, cancel := context.WithCancel(f.ctx)
	done := f.startConsumer(runtimeCtx, "cache", handler)
	f.publishEnvelope(t, event, nil)
	waitFor(t, 5*time.Second, func() bool {
		queue, err := f.channelInspect(cacheQueueName)
		return err == nil && queue.Messages == 0
	})
	if calls.Load() != 0 {
		t.Fatalf("handler ran %d times for succeeded receipt", calls.Load())
	}
	cancel()
	f.awaitStop(t, done)
	f.cleanupEventEvidence(event.EventID)
}

// poisonSchemaGoesDead 处理poison Schema Goes 死信相关逻辑。
func (f *mqAcceptanceFixture) poisonSchemaGoesDead(t *testing.T) {
	f.purgeQueues(t)
	eventID := uuid.NewString()
	runtimeCtx, cancelRuntime := context.WithCancel(f.ctx)
	defer cancelRuntime()
	runtimeDone := f.startConsumer(runtimeCtx, "cache", ConsumerHandlerFunc(func(context.Context, *gorm.DB, EventEnvelope) (ConsumerResult, error) {
		t.Fatal("poison schema reached business handler")
		return ConsumerResult{}, nil
	}))
	sinkCtx, cancelSink := context.WithCancel(f.ctx)
	defer cancelSink()
	sinkDone := f.startDeadSink(sinkCtx, "cache")
	channel := f.channel(t)
	err := channel.PublishWithContext(f.ctx, exchangeName, "cache.invalidate", false, false, amqp.Publishing{ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageId: eventID, Type: "cache.invalidate", Body: []byte(`{"spec_version":"broken"}`)})
	_ = channel.Close()
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		var count int64
		f.db.Model(&DeadLetter{}).Where("consumer_name='cache' AND event_id=? AND error_code LIKE 'MQ_%'", eventID).Count(&count)
		return count == 1
	})
	cancelRuntime()
	cancelSink()
	f.awaitStop(t, runtimeDone)
	f.awaitStop(t, sinkDone)
	f.cleanupEventEvidence(eventID)
}

// consumerStops 处理消费者 Stops相关逻辑。
func (f *mqAcceptanceFixture) consumerStops(t *testing.T) {
	runtimeCtx, cancel := context.WithCancel(f.ctx)
	done := f.startConsumer(runtimeCtx, "cache", ConsumerHandlerFunc(func(context.Context, *gorm.DB, EventEnvelope) (ConsumerResult, error) {
		return ConsumerResult{}, nil
	}))
	started := time.Now()
	cancel()
	f.awaitStop(t, done)
	if time.Since(started) > 5*time.Second {
		t.Fatalf("consumer shutdown took %v", time.Since(started))
	}
}

// envelope 返回消息信封。
func (f *mqAcceptanceFixture) envelope(t *testing.T, eventType, payload string) EventEnvelope {
	t.Helper()
	definition, ok := f.registry.Lookup(eventType)
	if !ok {
		t.Fatalf("missing event definition %s", eventType)
	}
	event := OutboxEvent{ID: f.ids.Next(), EventID: uuid.NewString(), EventType: eventType, AggregateType: "product", AggregateID: f.ids.Next(), Payload: datatypes.JSON(payload), CreatedAt: time.Now()}
	envelope, err := BuildEnvelope(event, definition, "acceptance")
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

// publishEnvelope 发布消息信封。
func (f *mqAcceptanceFixture) publishEnvelope(t *testing.T, event EventEnvelope, headers amqp.Table) {
	t.Helper()
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	channel := f.channel(t)
	defer channel.Close()
	if err := channel.PublishWithContext(f.ctx, exchangeName, event.EventType, false, false, amqp.Publishing{Headers: headers, ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageId: event.EventID, Type: event.EventType, Body: body}); err != nil {
		t.Fatal(err)
	}
}

// startConsumer 启动消费者处理流程。
func (f *mqAcceptanceFixture) startConsumer(ctx context.Context, name string, handler ConsumerHandler) <-chan struct{} {
	spec, _ := DefaultConsumerSpec(name)
	runtime := NewConsumerRuntime(spec, f.db, f.rabbit, f.registry, handler, f.ids, nil, "acceptance", f.log)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.Run(ctx)
	}()
	return done
}

// startDeadSink 启动死信接收端处理流程。
func (f *mqAcceptanceFixture) startDeadSink(ctx context.Context, consumer string) <-chan struct{} {
	sink := NewDeadSink(f.db, f.rabbit, f.ids, f.registry, nil, consumer, f.log)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sink.Run(ctx)
	}()
	return done
}

// awaitStop 处理await Stop相关逻辑。
func (f *mqAcceptanceFixture) awaitStop(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("MQ runtime did not stop in five seconds")
	}
}

// channelInspect 返回通道 Inspect。
func (f *mqAcceptanceFixture) channelInspect(queue string) (amqp.Queue, error) {
	connection, err := f.rabbit.Connection(f.ctx)
	if err != nil {
		return amqp.Queue{}, err
	}
	channel, err := connection.Channel()
	if err != nil {
		return amqp.Queue{}, err
	}
	defer channel.Close()
	return channel.QueueInspect(queue)
}

// cleanupEventEvidence 清理事件 Evidence。
func (f *mqAcceptanceFixture) cleanupEventEvidence(eventID string) {
	f.db.Where("event_id=?", eventID).Delete(&ConsumerReceipt{})
	f.db.Where("event_id=?", eventID).Delete(&DeadLetter{})
}
