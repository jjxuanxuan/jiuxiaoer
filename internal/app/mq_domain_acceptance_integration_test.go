package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
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
	"jiuxiaoer-admin/backend-go/internal/modules/mq"
	"jiuxiaoer-admin/backend-go/internal/modules/notification"
	"jiuxiaoer-admin/backend-go/internal/modules/printjob"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

const (
	acceptanceExchange          = "jxe.events.topic.v2"
	acceptanceNotificationQueue = "jxe.notification.v1.queue"
	acceptancePrintQueue        = "jxe.print.v1.queue"
	acceptanceSecurityQueue     = "jxe.security.v1.queue"
)

type mqDomainFixture struct {
	ctx      context.Context
	db       *gorm.DB
	rabbit   *rabbitinfra.Manager
	registry *mq.EventRegistry
	ids      *snowflake.Generator
	log      *slog.Logger
}

// TestRabbitMQDomainAcceptanceIntegration 验证Rabbit 消息队列 Domain 验收集成的预期行为。
func TestRabbitMQDomainAcceptanceIntegration(t *testing.T) {
	if os.Getenv("JXE_RUN_MQ_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_MQ_INTEGRATION=1 and use an isolated RabbitMQ vhost")
	}
	fixture := newMQDomainFixture(t)
	t.Run("ACC-RMQ-006-notification-full-lifecycle-at-most-once", fixture.notificationLifecycle)
	t.Run("ACC-RMQ-007-notification-db-fallback-converges", fixture.notificationFallback)
	t.Run("ACC-RMQ-008-print-wakeup-ACC-RMQ-009-duplicate-100", fixture.printWakeupAndDuplicates)
	t.Run("ACC-RMQ-010-provider-unknown-queries-without-resubmit", fixture.printUnknownReconciles)
	t.Run("identity-events-reach-security-consumer-once", fixture.identitySecurityObservation)
}

// newMQDomainFixture 创建并初始化消息队列 Domain 测试夹具。
func newMQDomainFixture(t *testing.T) *mqDomainFixture {
	t.Helper()
	ctx := context.Background()
	cfg := config.Load()
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
	if err := mq.DeclareTopology(channel, mq.DefaultTopology()); err != nil {
		_ = channel.Close()
		t.Fatal(err)
	}
	_ = channel.Close()
	return &mqDomainFixture{ctx: ctx, db: db, rabbit: rabbit, registry: mq.MustDefaultEventRegistry(), ids: snowflake.New(908), log: log}
}

// notificationLifecycle 处理通知 Lifecycle相关逻辑。
func (f *mqDomainFixture) notificationLifecycle(t *testing.T) {
	f.purge(t, acceptanceNotificationQueue)
	orderID, deliveryOrderID := f.createNotificationOrder(t)
	eventIDs := make([]string, 0, 8)
	templateIDs := make([]uint64, 0, 8)
	defer func() {
		f.db.Where("event_id IN ?", eventIDs).Delete(&notification.Delivery{})
		f.db.Where("source_event_id IN ?", eventIDs).Delete(&notification.Message{})
		f.db.Where("consumer_name='notification' AND event_id IN ?", eventIDs).Delete(&mq.ConsumerReceipt{})
		f.db.Where("id IN ?", templateIDs).Delete(&notification.Template{})
		f.db.Exec("DELETE FROM delivery_orders WHERE id=?", deliveryOrderID)
		f.db.Exec("DELETE FROM orders WHERE id=?", orderID)
		f.purge(t, acceptanceNotificationQueue)
	}()

	types := []string{
		"payment.succeeded", "store.order.accepted", "store.order.prepared", "delivery.assigned",
		"delivery.reassigned", "delivery.picked_up", "delivery.started", "delivery.completed",
	}
	for _, eventType := range types {
		templateID := f.ids.Next()
		templateIDs = append(templateIDs, templateID)
		template := notification.Template{
			ID: templateID, TemplateCode: fmt.Sprintf("acc_%d", templateID), EventType: eventType,
			Channel: "wechat", Version: "acc-v1", TitleTemplate: "acceptance", BodyTemplate: "acceptance",
			AllowedFields: datatypes.JSON(`[]`), Status: "published", CreatedBy: 3101,
		}
		if err := f.db.Create(&template).Error; err != nil {
			t.Fatal(err)
		}
	}

	cfg := config.Load()
	cfg.CP1.NotificationEnabled = true
	cfg.CP1.WorkerBatchSize = 1000
	worker := notification.NewWorker(cfg.CP1, f.db, f.ids, &notification.FakeProvider{}, "acc-rmq-notification", f.log)
	ctx, cancel := context.WithCancel(f.ctx)
	done := f.startConsumer(ctx, "notification", notification.NewMQHandler(worker))
	f.waitConsumers(t, acceptanceNotificationQueue, 1)

	for _, eventType := range types {
		eventID := uuid.NewString()
		eventIDs = append(eventIDs, eventID)
		aggregateType, aggregateID := "order", orderID
		payload := map[string]any{"order_id": fmt.Sprint(orderID)}
		switch eventType {
		case "payment.succeeded":
			aggregateType, aggregateID = "payment", f.ids.Next()
			payload["payment_id"] = fmt.Sprint(aggregateID)
		case "store.order.accepted", "store.order.prepared":
			payload["shop_id"] = "4201"
		default:
			aggregateType, aggregateID = "delivery_order", deliveryOrderID
			payload["delivery_order_id"] = fmt.Sprint(deliveryOrderID)
			if eventType == "delivery.assigned" || eventType == "delivery.reassigned" {
				payload["rider_id"] = "5001"
			}
		}
		envelope := f.envelope(t, eventID, eventType, aggregateType, aggregateID, payload)
		// Every lifecycle node is deliberately delivered twice. The receipt and
		// inbox/delivery unique keys must collapse the redelivery.
		f.publish(t, envelope, 2)
	}

	wantMessages := int64(len(types))
	waitDomain(t, 10*time.Second, func() bool {
		var receipts, messages int64
		f.db.Model(&mq.ConsumerReceipt{}).Where("consumer_name='notification' AND event_id IN ? AND status='succeeded'", eventIDs).Count(&receipts)
		f.db.Model(&notification.Message{}).Where("source_event_id IN ? AND customer_id=?", eventIDs, orderID).Count(&messages)
		return receipts == wantMessages && messages == wantMessages
	})

	// Delivery is intentionally a DB worker after MQ materialization. Run it
	// twice to prove normal MQ and fallback polling converge without duplicates.
	worker.RunOnce(f.ctx)
	worker.RunOnce(f.ctx)
	waitDomain(t, 10*time.Second, func() bool {
		var pending, failed int64
		f.db.Model(&notification.Delivery{}).Where("event_id IN ? AND status IN ('pending','processing','retry_wait')", eventIDs).Count(&pending)
		f.db.Model(&notification.Delivery{}).Where("event_id IN ? AND status='dead'", eventIDs).Count(&failed)
		return pending == 0 && failed == 0
	})
	var uniqueMessages, uniqueReceipts int64
	f.db.Model(&notification.Message{}).Where("source_event_id IN ?", eventIDs).Count(&uniqueMessages)
	f.db.Model(&mq.ConsumerReceipt{}).Where("consumer_name='notification' AND event_id IN ?", eventIDs).Count(&uniqueReceipts)
	if uniqueMessages != wantMessages || uniqueReceipts != wantMessages {
		t.Fatalf("notification duplicate side effects: messages=%d receipts=%d want=%d", uniqueMessages, uniqueReceipts, wantMessages)
	}
	cancel()
	f.awaitStop(t, done)
}

// notificationFallback 处理通知降级相关逻辑。
func (f *mqDomainFixture) notificationFallback(t *testing.T) {
	orderID, deliveryOrderID := f.createNotificationOrder(t)
	eventID := uuid.NewString()
	outboxID := f.priorityOutboxID(t)
	row := mq.OutboxEvent{
		ID: outboxID, EventID: eventID, EventType: "order.cancelled", AggregateType: "order", AggregateID: orderID,
		Payload: datatypes.JSON(fmt.Sprintf(`{"order_id":"%d"}`, orderID)), Status: "pending", CreatedAt: time.Now(),
	}
	if err := f.db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	defer func() {
		f.db.Where("consumer_name='notification' AND event_id=?", eventID).Delete(&mq.ConsumerReceipt{})
		f.db.Where("source_event_id=?", eventID).Delete(&notification.Message{})
		f.db.Where("event_id=?", eventID).Delete(&notification.Delivery{})
		f.db.Where("event_id=?", eventID).Delete(&mq.OutboxEvent{})
		f.db.Exec("DELETE FROM delivery_orders WHERE id=?", deliveryOrderID)
		f.db.Exec("DELETE FROM orders WHERE id=?", orderID)
	}()

	cfg := config.Load()
	cfg.CP1.NotificationEnabled = false
	cfg.CP1.WorkerBatchSize = 1
	worker := notification.NewWorker(cfg.CP1, f.db, f.ids, &notification.FakeProvider{}, "acc-rmq-fallback", f.log)
	worker.RunOnce(f.ctx)
	worker.RunOnce(f.ctx)
	var messages, receipts int64
	f.db.Model(&notification.Message{}).Where("source_event_id=?", eventID).Count(&messages)
	f.db.Model(&mq.ConsumerReceipt{}).Where("consumer_name='notification' AND event_id=? AND status='succeeded'", eventID).Count(&receipts)
	if messages != 1 || receipts != 1 {
		t.Fatalf("DB fallback did not converge: messages=%d receipts=%d", messages, receipts)
	}
}

// printWakeupAndDuplicates 处理打印 Wakeup And Duplicates相关逻辑。
func (f *mqDomainFixture) printWakeupAndDuplicates(t *testing.T) {
	f.purge(t, acceptancePrintQueue)
	provider := &countingPrintProvider{}
	task := f.createPrintTask(t, "duplicate")
	defer f.cleanupPrint(t, task.ID, task.EventID)

	cfg := config.Load()
	worker := printjob.NewWorker(cfg.CP1, f.db, f.ids, provider, "acc-rmq-print", f.log)
	ctx, cancel := context.WithCancel(f.ctx)
	done := f.startConsumer(ctx, "print", printjob.NewMQHandler(worker))
	f.waitConsumers(t, acceptancePrintQueue, 1)
	envelope := f.envelope(t, task.EventID, "print.task.ready", "print_task", task.ID, map[string]any{"print_task_id": fmt.Sprint(task.ID), "shop_id": "4201"})
	f.publish(t, envelope, 100)
	waitDomain(t, 10*time.Second, func() bool {
		var current printjob.Task
		var receipt mq.ConsumerReceipt
		return f.db.First(&current, task.ID).Error == nil && current.Status == "succeeded" &&
			f.db.Where("consumer_name='print' AND event_id=?", task.EventID).First(&receipt).Error == nil && receipt.Status == "succeeded"
	})
	var attempts, receipts int64
	f.db.Model(&printjob.Attempt{}).Where("print_task_id=?", task.ID).Count(&attempts)
	f.db.Model(&mq.ConsumerReceipt{}).Where("consumer_name='print' AND event_id=?", task.EventID).Count(&receipts)
	if provider.submit.Load() != 1 || attempts != 1 || receipts != 1 {
		t.Fatalf("duplicate print side effect: submit=%d attempts=%d receipts=%d", provider.submit.Load(), attempts, receipts)
	}
	cancel()
	f.awaitStop(t, done)
}

// printUnknownReconciles 处理打印 Unknown Reconciles相关逻辑。
func (f *mqDomainFixture) printUnknownReconciles(t *testing.T) {
	f.purge(t, acceptancePrintQueue)
	provider := &countingPrintProvider{unknownFirst: true}
	task := f.createPrintTask(t, "unknown")
	defer f.cleanupPrint(t, task.ID, task.EventID)

	cfg := config.Load()
	worker := printjob.NewWorker(cfg.CP1, f.db, f.ids, provider, "acc-rmq-print-unknown", f.log)
	ctx, cancel := context.WithCancel(f.ctx)
	done := f.startConsumer(ctx, "print", printjob.NewMQHandler(worker))
	f.waitConsumers(t, acceptancePrintQueue, 1)
	envelope := f.envelope(t, task.EventID, "print.task.ready", "print_task", task.ID, map[string]any{"print_task_id": fmt.Sprint(task.ID), "shop_id": "4201"})
	f.publish(t, envelope, 1)
	waitDomain(t, 10*time.Second, func() bool {
		var current printjob.Task
		return f.db.First(&current, task.ID).Error == nil && current.Status == "succeeded"
	})
	var attempts int64
	f.db.Model(&printjob.Attempt{}).Where("print_task_id=?", task.ID).Count(&attempts)
	if provider.submit.Load() != 1 || provider.query.Load() != 1 || attempts != 2 {
		t.Fatalf("unknown reconciliation resubmitted or missed query: submit=%d query=%d attempts=%d", provider.submit.Load(), provider.query.Load(), attempts)
	}
	cancel()
	f.awaitStop(t, done)
}

// identitySecurityObservation 处理身份 Security Observation相关逻辑。
func (f *mqDomainFixture) identitySecurityObservation(t *testing.T) {
	f.purge(t, acceptanceSecurityQueue)
	ctx, cancel := context.WithCancel(f.ctx)
	handler := mq.ConsumerHandlerFunc(func(context.Context, *gorm.DB, mq.EventEnvelope) (mq.ConsumerResult, error) {
		return mq.ConsumerResult{RefType: "security_observation"}, nil
	})
	done := f.startConsumer(ctx, "security", handler)
	f.waitConsumers(t, acceptanceSecurityQueue, 1)

	eventIDs := make([]string, 0, 3)
	events := []struct {
		eventType string
		payload   map[string]any
	}{
		{eventType: "identity.verification.updated", payload: map[string]any{"verification_request_id": "iv-1", "customer_id": "customer-1", "status": "verified", "adult_result": "adult"}},
		{eventType: "identity.verification.failed", payload: map[string]any{"verification_request_id": "iv-2", "customer_id": "customer-2", "error_code": "PROVIDER_REJECTED"}},
		{eventType: "identity.verification.revoked", payload: map[string]any{"verification_request_id": "iv-3", "customer_id": "customer-3", "error_code": "PROVIDER_REVOKED"}},
	}
	for _, event := range events {
		eventID := uuid.NewString()
		eventIDs = append(eventIDs, eventID)
		f.publish(t, f.envelope(t, eventID, event.eventType, "identity_verification", f.ids.Next(), event.payload), 2)
	}
	t.Cleanup(func() {
		f.db.Where("consumer_name='security' AND event_id IN ?", eventIDs).Delete(&mq.ConsumerReceipt{})
		f.purge(t, acceptanceSecurityQueue)
	})

	want := int64(len(events))
	waitDomain(t, 10*time.Second, func() bool {
		var receipts int64
		f.db.Model(&mq.ConsumerReceipt{}).
			Where("consumer_name='security' AND event_id IN ? AND status='succeeded' AND result_ref_type='security_observation'", eventIDs).
			Count(&receipts)
		return receipts == want
	})
	var receipts int64
	f.db.Model(&mq.ConsumerReceipt{}).Where("consumer_name='security' AND event_id IN ?", eventIDs).Count(&receipts)
	if receipts != want {
		t.Fatalf("identity event redelivery created %d security receipts, want %d", receipts, want)
	}
	cancel()
	f.awaitStop(t, done)
}

type countingPrintProvider struct {
	submit       atomic.Int32
	query        atomic.Int32
	unknownFirst bool
}

// Submit 返回Submit。
func (p *countingPrintProvider) Submit(_ context.Context, request printjob.PrintRequest) (printjob.PrintResult, error) {
	count := p.submit.Add(1)
	result := printjob.PrintResult{ProviderRequestID: request.ProviderRequestID, Status: "succeeded"}
	if p.unknownFirst && count == 1 {
		result.Status = "unknown"
		return result, &printjob.ProviderError{Code: "PROVIDER_TIMEOUT_UNKNOWN", Retryable: true, Unknown: true}
	}
	return result, nil
}

// Query 查询打印结果。
func (p *countingPrintProvider) Query(_ context.Context, providerRequestID string) (printjob.PrintResult, error) {
	p.query.Add(1)
	return printjob.PrintResult{ProviderRequestID: providerRequestID, Status: "succeeded"}, nil
}

// createNotificationOrder 创建通知订单。
func (f *mqDomainFixture) createNotificationOrder(t *testing.T) (uint64, uint64) {
	t.Helper()
	orderID, deliveryID := f.ids.Next(), f.ids.Next()
	order := map[string]any{
		"id": orderID, "order_no": fmt.Sprintf("ACC%d", orderID), "customer_id": orderID,
		"merchant_id": uint64(4001), "shop_id": uint64(4201), "status": "paid", "pay_status": "succeeded",
		"delivery_status": "pending", "address_snapshot": datatypes.JSON(`{}`), "version": 1,
	}
	if err := f.db.Table("orders").Create(order).Error; err != nil {
		t.Fatal(err)
	}
	delivery := map[string]any{"id": deliveryID, "order_id": orderID, "shop_id": uint64(4201), "rider_id": uint64(5001), "status": "assigned", "assignment_version": 1}
	if err := f.db.Table("delivery_orders").Create(delivery).Error; err != nil {
		f.db.Exec("DELETE FROM orders WHERE id=?", orderID)
		t.Fatal(err)
	}
	return orderID, deliveryID
}

// createPrintTask 创建打印任务。
func (f *mqDomainFixture) createPrintTask(t *testing.T, suffix string) printjob.Task {
	t.Helper()
	id := f.ids.Next()
	task := printjob.Task{
		ID: id, TaskNo: fmt.Sprintf("ACC-%s-%d", suffix, id), EventID: uuid.NewString(), OrderID: id,
		ShopID: 4201, EventType: "order_accepted", TemplateID: 9001, TemplateVersion: "acc-v1",
		RenderPayload: datatypes.JSON(`{"order":"acceptance"}`), Provider: "fake", Status: "pending",
	}
	if err := f.db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	return task
}

// cleanupPrint 清理打印。
func (f *mqDomainFixture) cleanupPrint(t *testing.T, taskID uint64, eventID string) {
	t.Helper()
	f.db.Where("print_task_id=?", taskID).Delete(&printjob.Attempt{})
	f.db.Where("consumer_name='print' AND event_id=?", eventID).Delete(&mq.ConsumerReceipt{})
	f.db.Where("id=?", taskID).Delete(&printjob.Task{})
	f.purge(t, acceptancePrintQueue)
}

// priorityOutboxID 返回priority 发件箱事件ID。
func (f *mqDomainFixture) priorityOutboxID(t *testing.T) uint64 {
	t.Helper()
	var minimum uint64
	if err := f.db.Model(&mq.OutboxEvent{}).Select("COALESCE(MIN(id),0)").Scan(&minimum).Error; err != nil {
		t.Fatal(err)
	}
	if minimum > 1 {
		return minimum - 1
	}
	for {
		candidate := f.ids.Next()
		var count int64
		f.db.Model(&mq.OutboxEvent{}).Where("id=?", candidate).Count(&count)
		if count == 0 {
			return candidate
		}
	}
}

// envelope 返回消息信封。
func (f *mqDomainFixture) envelope(t *testing.T, eventID, eventType, aggregateType string, aggregateID uint64, payload any) mq.EventEnvelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := f.registry.Lookup(eventType)
	if !ok {
		t.Fatalf("missing registry event %s", eventType)
	}
	event := mq.OutboxEvent{ID: f.ids.Next(), EventID: eventID, EventType: eventType, AggregateType: aggregateType, AggregateID: aggregateID, Payload: datatypes.JSON(raw), CreatedAt: time.Now()}
	envelope, err := mq.BuildEnvelope(event, definition, "acceptance")
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

// publish 发布应用。
func (f *mqDomainFixture) publish(t *testing.T, envelope mq.EventEnvelope, count int) {
	t.Helper()
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := f.rabbit.Connection(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := connection.Channel()
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	for index := 0; index < count; index++ {
		if err := channel.PublishWithContext(f.ctx, acceptanceExchange, envelope.EventType, false, false, amqp.Publishing{
			ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageId: envelope.EventID,
			Type: envelope.EventType, Timestamp: time.Now(), Body: body,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// startConsumer 启动消费者处理流程。
func (f *mqDomainFixture) startConsumer(ctx context.Context, name string, handler mq.ConsumerHandler) <-chan struct{} {
	spec, _ := mq.DefaultConsumerSpec(name)
	runtime := mq.NewConsumerRuntime(spec, f.db, f.rabbit, f.registry, handler, f.ids, nil, "domain-acceptance", f.log)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.Run(ctx)
	}()
	return done
}

// waitConsumers 处理wait Consumers相关逻辑。
func (f *mqDomainFixture) waitConsumers(t *testing.T, queue string, want int) {
	t.Helper()
	waitDomain(t, 5*time.Second, func() bool {
		connection, err := f.rabbit.Connection(f.ctx)
		if err != nil {
			return false
		}
		channel, err := connection.Channel()
		if err != nil {
			return false
		}
		defer channel.Close()
		state, err := channel.QueueInspect(queue)
		return err == nil && state.Consumers == want
	})
}

// purge 彻底清理应用。
func (f *mqDomainFixture) purge(t *testing.T, queue string) {
	t.Helper()
	connection, err := f.rabbit.Connection(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := connection.Channel()
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	if _, err := channel.QueuePurge(queue, false); err != nil {
		t.Fatal(err)
	}
}

// awaitStop 处理await Stop相关逻辑。
func (f *mqDomainFixture) awaitStop(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("MQ domain consumer did not stop within five seconds")
	}
}

// waitDomain 处理wait Domain相关逻辑。
func waitDomain(t *testing.T, timeout time.Duration, condition func() bool) {
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
