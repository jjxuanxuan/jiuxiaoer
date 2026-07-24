package mq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/infra/rabbitmq"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
)

type ConsumerFailureClass string

const (
	FailureValidation       ConsumerFailureClass = "validation"
	FailureUnauthorized     ConsumerFailureClass = "unauthorized"
	FailureBusinessTerminal ConsumerFailureClass = "business_terminal"
	FailureTemporary        ConsumerFailureClass = "temporary"
	FailureRateLimited      ConsumerFailureClass = "rate_limited"
	FailureUnknown          ConsumerFailureClass = "unknown"
)

type ConsumerError struct {
	Code  string
	Safe  string
	Class ConsumerFailureClass
	Err   error
}

// Error 返回当前错误的文本描述。
func (e *ConsumerError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil {
		return e.Code + ": " + e.Err.Error()
	}
	return e.Code
}

// Unwrap 返回Unwrap。
func (e *ConsumerError) Unwrap() error { return e.Err }

// TemporaryConsumerError 返回Temporary 消费者错误。
func TemporaryConsumerError(code, safe string, err error) error {
	return &ConsumerError{Code: code, Safe: safe, Class: FailureTemporary, Err: err}
}

// TerminalConsumerError 返回Terminal 消费者错误。
func TerminalConsumerError(code, safe string, err error) error {
	return &ConsumerError{Code: code, Safe: safe, Class: FailureBusinessTerminal, Err: err}
}

type ConsumerResult struct {
	RefType string
	RefID   uint64
}

type ConsumerHandler interface {
	Handle(context.Context, *gorm.DB, EventEnvelope) (ConsumerResult, error)
}

type AfterCommitConsumerHandler interface {
	AfterCommit(context.Context, EventEnvelope, ConsumerResult) error
}

// ReliableAfterCommitConsumerHandler 在临时扇出真正成功前保持消费者回执未完成。
// 只有尚无持久数据库中继或降级机制的处理器才应启用。
type ReliableAfterCommitConsumerHandler interface {
	AfterCommitConsumerHandler
	RequiresSuccessfulAfterCommit(EventEnvelope, ConsumerResult) bool
}

type ConsumerHandlerFunc func(context.Context, *gorm.DB, EventEnvelope) (ConsumerResult, error)

// Handle 处理消费者结果请求。
func (f ConsumerHandlerFunc) Handle(ctx context.Context, tx *gorm.DB, envelope EventEnvelope) (ConsumerResult, error) {
	return f(ctx, tx, envelope)
}

type ConsumerSpec struct {
	Name           string
	Queue          string
	Prefetch       int
	HandlerTimeout time.Duration
	RetryDelays    []time.Duration
}

// DefaultConsumerSpec 返回默认项消费者规则。
func DefaultConsumerSpec(name string) (ConsumerSpec, error) {
	switch name {
	case "notification":
		return ConsumerSpec{Name: name, Queue: notificationQueueName, Prefetch: 50, HandlerTimeout: 5 * time.Second, RetryDelays: []time.Duration{10 * time.Second, time.Minute, 10 * time.Minute}}, nil
	case "print":
		return ConsumerSpec{Name: name, Queue: printQueueName, Prefetch: 10, HandlerTimeout: 30 * time.Second, RetryDelays: []time.Duration{10 * time.Second, time.Minute, 10 * time.Minute}}, nil
	case "cache":
		return ConsumerSpec{Name: name, Queue: cacheQueueName, Prefetch: 100, HandlerTimeout: 3 * time.Second, RetryDelays: []time.Duration{5 * time.Second, 30 * time.Second, 5 * time.Minute}}, nil
	case "security":
		return ConsumerSpec{Name: name, Queue: securityQueueName, Prefetch: 20, HandlerTimeout: 5 * time.Second, RetryDelays: []time.Duration{time.Minute, 10 * time.Minute}}, nil
	case "dispatch":
		return ConsumerSpec{Name: name, Queue: dispatchQueueName, Prefetch: 100, HandlerTimeout: 5 * time.Second, RetryDelays: []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}}, nil
	case "realtime":
		return ConsumerSpec{Name: name, Queue: realtimeQueueName, Prefetch: 100, HandlerTimeout: 5 * time.Second, RetryDelays: []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}}, nil
	default:
		return ConsumerSpec{}, fmt.Errorf("unknown consumer %q", name)
	}
}

type ConsumerReceipt struct {
	ID              uint64
	ConsumerName    string
	EventID         string
	EventType       string
	EventVersion    uint
	Status          string
	Attempts        uint
	LockedBy        *string
	LockedUntil     *time.Time
	LastErrorCode   *string
	ResultRefType   *string
	ResultRefID     *uint64
	FirstReceivedAt time.Time
	ProcessedAt     *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (ConsumerReceipt) TableName() string { return "mq_consumer_receipts" }

type ConsumerRuntime struct {
	spec       ConsumerSpec
	db         *gorm.DB
	rabbit     *rabbitmq.Manager
	registry   *EventRegistry
	handler    ConsumerHandler
	ids        idSource
	metrics    *metrics.Registry
	instanceID string
	log        *slog.Logger
}

// NewConsumerRuntime 创建并初始化消费者运行时。
func NewConsumerRuntime(spec ConsumerSpec, db *gorm.DB, rabbit *rabbitmq.Manager, registry *EventRegistry, handler ConsumerHandler, ids idSource, metricRegistry *metrics.Registry, instanceID string, log *slog.Logger) *ConsumerRuntime {
	return &ConsumerRuntime{spec: spec, db: db, rabbit: rabbit, registry: registry, handler: handler, ids: ids, metrics: metricRegistry, instanceID: instanceID + ":" + spec.Name, log: log}
}

// Run 运行当前实例的核心处理流程。
func (r *ConsumerRuntime) Run(ctx context.Context) {
	if r.db == nil || r.rabbit == nil || r.registry == nil || r.handler == nil || r.ids == nil {
		return
	}
	for ctx.Err() == nil {
		conn, err := r.rabbit.Connection(ctx)
		if err != nil {
			return
		}
		if err := r.consumeSession(ctx, conn); err != nil && ctx.Err() == nil {
			r.log.Warn("mq consumer session ended", slog.String("consumer", r.spec.Name), slog.Any("error", err))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// consumeSession 消费并处理会话。
func (r *ConsumerRuntime) consumeSession(ctx context.Context, conn *amqp.Connection) error {
	channel, err := conn.Channel()
	if err != nil {
		return err
	}
	defer channel.Close()
	if err := DeclareTopology(channel, DefaultTopology()); err != nil {
		return err
	}
	if err := channel.Qos(r.spec.Prefetch, 0, false); err != nil {
		return err
	}
	if err := channel.Confirm(false); err != nil {
		return err
	}
	confirmations := channel.NotifyPublish(make(chan amqp.Confirmation, 1))
	returns := channel.NotifyReturn(make(chan amqp.Return, 1))
	// 在当前协程中负责取消。ConsumeWithContext 会启动内部协程，
	// 在 ctx 结束时发送 basic.cancel；该 RPC 与延迟执行的 channel.Close
	// 发生竞争时，可能因 AMQP 通道互斥锁阻塞关闭流程。此处关闭通道仍会让
	// RabbitMQ 重新入队所有未确认投递，同时确保取消与通道所有权串行化。
	deliveries, err := channel.Consume(r.spec.Queue, "jxe-"+r.spec.Name+"-"+r.instanceID, false, false, false, false, nil)
	if err != nil {
		return err
	}
	r.log.Info("mq consumer started", slog.String("consumer", r.spec.Name), slog.String("queue", r.spec.Queue))
	for {
		select {
		case <-ctx.Done():
			// 关闭下方通道会重新入队 RabbitMQ 已发送但本进程尚未确认的所有投递。
			// 不要把关闭流程转化为人为的消费者失败或重试风暴。
			return nil
		case delivery, ok := <-deliveries:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("delivery channel closed")
			}
			if err := r.handleDelivery(ctx, channel, confirmations, returns, delivery); err != nil && ctx.Err() == nil {
				r.log.Warn("mq delivery failed", slog.String("consumer", r.spec.Name), slog.String("message_id", delivery.MessageId), slog.Any("error", err))
			}
			if ctx.Err() != nil {
				return nil
			}
		}
	}
}

// handleDelivery 处理配送请求。
func (r *ConsumerRuntime) handleDelivery(ctx context.Context, channel *amqp.Channel, confirmations <-chan amqp.Confirmation, returns <-chan amqp.Return, delivery amqp.Delivery) error {
	envelope, _, err := DecodeEnvelope(delivery.Body, r.registry)
	if err != nil {
		return r.deadLetter(ctx, channel, confirmations, returns, delivery, stableErrorCode(err), "message contract validation failed", FailureValidation)
	}
	if !r.consumerAllowed(envelope.EventType) {
		return r.deadLetter(ctx, channel, confirmations, returns, delivery, "MQ_CONSUMER_ROUTE_FORBIDDEN", "event is not registered for this consumer", FailureUnauthorized)
	}

	handlerCtx, cancel := context.WithTimeout(ctx, r.spec.HandlerTimeout)
	defer cancel()
	duplicate, handlerResult, err := r.processEnvelope(handlerCtx, envelope)
	if err == nil && !duplicate {
		err = r.finalizeAfterCommit(handlerCtx, envelope, handlerResult)
	}
	if err == nil {
		r.incConsume(map[bool]string{true: "duplicate", false: "succeeded"}[duplicate])
		return delivery.Ack(false)
	}
	if ctx.Err() != nil {
		// 会话返回时会关闭通道，从而重新入队这条未确认投递。
		// ConsumeWithContext 并发取消时发送 Nack，可能阻塞在 AMQP 通道互斥锁上。
		return ctx.Err()
	}

	failure := classifyConsumerError(err)
	attempt := headerInt(delivery.Headers, "x-retry-count") + 1
	if recordErr := r.recordFailure(ctx, envelope, attempt, failure); recordErr != nil {
		_ = delivery.Nack(false, true)
		return fmt.Errorf("record consumer failure: %w", recordErr)
	}
	if failure.Class == FailureValidation || failure.Class == FailureUnauthorized || failure.Class == FailureBusinessTerminal || attempt > len(r.spec.RetryDelays) {
		return r.deadLetter(ctx, channel, confirmations, returns, delivery, failure.Code, failure.Safe, failure.Class)
	}
	return r.retry(ctx, channel, confirmations, returns, delivery, attempt, failure)
}

// processEnvelope 处理消息信封。
// processEnvelope 是 RabbitMQ 投递和低频数据库补偿路径共用的事务单元。
func (r *ConsumerRuntime) processEnvelope(ctx context.Context, envelope EventEnvelope) (bool, ConsumerResult, error) {
	duplicate := false
	var handlerResult ConsumerResult
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		seed := ConsumerReceipt{ID: r.ids.Next(), ConsumerName: r.spec.Name, EventID: envelope.EventID, EventType: envelope.EventType, EventVersion: envelope.EventVersion, Status: "processing", FirstReceivedAt: now}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&seed).Error; err != nil {
			return err
		}
		var receipt ConsumerReceipt
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("consumer_name=? AND event_id=?", r.spec.Name, envelope.EventID).Take(&receipt).Error; err != nil {
			return err
		}
		if receipt.Status == "succeeded" {
			duplicate = true
			return nil
		}
		leaseUntil := now.Add(r.spec.HandlerTimeout + 5*time.Second)
		if err := tx.Model(&ConsumerReceipt{}).Where("id=?", receipt.ID).Updates(map[string]any{"status": "processing", "attempts": gorm.Expr("attempts + 1"), "locked_by": r.instanceID, "locked_until": leaseUntil, "last_error_code": nil}).Error; err != nil {
			return err
		}
		result, err := r.handler.Handle(ctx, tx, envelope)
		if err != nil {
			return err
		}
		status := "succeeded"
		processedAt := any(now)
		if r.requiresSuccessfulAfterCommit(envelope, result) {
			status = "post_commit"
			processedAt = nil
		}
		updates := map[string]any{"status": status, "processed_at": processedAt, "last_error_code": nil}
		if status == "succeeded" {
			updates["locked_by"] = nil
			updates["locked_until"] = nil
		}
		if result.RefType != "" {
			updates["result_ref_type"] = result.RefType
		}
		if result.RefID != 0 {
			updates["result_ref_id"] = result.RefID
		}
		handlerResult = result
		return tx.Model(&ConsumerReceipt{}).Where("id=?", receipt.ID).Updates(updates).Error
	})
	return duplicate, handlerResult, err
}

func (r *ConsumerRuntime) requiresSuccessfulAfterCommit(envelope EventEnvelope, result ConsumerResult) bool {
	handler, ok := r.handler.(ReliableAfterCommitConsumerHandler)
	return ok && handler.RequiresSuccessfulAfterCommit(envelope, result)
}

// finalizeAfterCommit 仅在可靠的临时扇出成功后关闭回执。失败会返回常规 MQ
// 重试或死信路径，其中 recordFailure 状态变更会让回执重新可处理。
func (r *ConsumerRuntime) finalizeAfterCommit(ctx context.Context, envelope EventEnvelope, result ConsumerResult) error {
	postCommit, ok := r.handler.(AfterCommitConsumerHandler)
	if !ok {
		return nil
	}
	postErr := postCommit.AfterCommit(ctx, envelope, result)
	if !r.requiresSuccessfulAfterCommit(envelope, result) {
		if postErr != nil && r.log != nil {
			r.log.Warn("mq consumer post-commit action failed; DB fallback will reconcile", slog.String("consumer", r.spec.Name), slog.String("event_id", envelope.EventID), slog.Any("error", postErr))
		}
		return nil
	}
	if postErr != nil {
		return TemporaryConsumerError("MQ_POST_COMMIT_FAILED", "post-commit delivery temporarily failed", postErr)
	}
	now := time.Now()
	resultDB := r.db.WithContext(ctx).Model(&ConsumerReceipt{}).
		Where("consumer_name=? AND event_id=? AND status<>'dead'", r.spec.Name, envelope.EventID).
		Updates(map[string]any{"status": "succeeded", "processed_at": now, "locked_by": nil, "locked_until": nil, "last_error_code": nil})
	if resultDB.Error != nil {
		return TemporaryConsumerError("MQ_POST_COMMIT_RECEIPT_FAILED", "post-commit receipt update temporarily failed", resultDB.Error)
	}
	if resultDB.RowsAffected != 1 {
		return TemporaryConsumerError("MQ_POST_COMMIT_RECEIPT_MISSING", "post-commit receipt is unavailable", nil)
	}
	return nil
}

// consumerAllowed 判断消费者允许状态。
func (r *ConsumerRuntime) consumerAllowed(eventType string) bool {
	definition, ok := r.registry.Lookup(eventType)
	if !ok {
		return false
	}
	for _, consumer := range definition.Consumers {
		if consumer == r.spec.Name {
			return true
		}
	}
	return false
}

// recordFailure 记录消费失败。
func (r *ConsumerRuntime) recordFailure(ctx context.Context, envelope EventEnvelope, attempt int, failure *ConsumerError) error {
	now := time.Now()
	row := ConsumerReceipt{ID: r.ids.Next(), ConsumerName: r.spec.Name, EventID: envelope.EventID, EventType: envelope.EventType, EventVersion: envelope.EventVersion, Status: "processing", Attempts: uint(attempt), LastErrorCode: &failure.Code, FirstReceivedAt: now}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "consumer_name"}, {Name: "event_id"}}, DoUpdates: clause.Assignments(map[string]any{"attempts": attempt, "last_error_code": failure.Code, "locked_by": nil, "locked_until": nil, "status": "processing"})}).Create(&row).Error
}

// retry 重试消息队列。
func (r *ConsumerRuntime) retry(ctx context.Context, channel *amqp.Channel, confirmations <-chan amqp.Confirmation, returns <-chan amqp.Return, delivery amqp.Delivery, attempt int, failure *ConsumerError) error {
	delay := r.spec.RetryDelays[attempt-1]
	headers := cloneHeaders(delivery.Headers)
	headers["x-retry-count"] = int32(attempt)
	headers["x-original-routing-key"] = delivery.RoutingKey
	headers["x-last-error-code"] = failure.Code
	if _, ok := headers["x-first-failed-at"]; !ok {
		headers["x-first-failed-at"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	message := publishingFromDelivery(delivery, headers)
	if err := publishConfirmed(ctx, channel, confirmations, returns, retryExchangeName(r.spec.Name, delay), delivery.RoutingKey, true, message); err != nil {
		_ = delivery.Nack(false, true)
		return err
	}
	r.incRetry(strconv.Itoa(attempt))
	if err := delivery.Ack(false); err != nil {
		return err
	}
	return failure
}

// deadLetter 返回死信 Letter。
func (r *ConsumerRuntime) deadLetter(ctx context.Context, channel *amqp.Channel, confirmations <-chan amqp.Confirmation, returns <-chan amqp.Return, delivery amqp.Delivery, code, safe string, class ConsumerFailureClass) error {
	headers := cloneHeaders(delivery.Headers)
	headers["x-jxe-consumer"] = r.spec.Name
	headers["x-jxe-error-code"] = code
	headers["x-jxe-error-safe"] = safe
	headers["x-jxe-error-class"] = string(class)
	message := publishingFromDelivery(delivery, headers)
	if err := publishConfirmed(ctx, channel, confirmations, returns, deadExchangeName, r.spec.Name+".dead", true, message); err != nil {
		_ = delivery.Nack(false, true)
		return err
	}
	if err := r.db.WithContext(ctx).Model(&ConsumerReceipt{}).Where("consumer_name=? AND event_id=?", r.spec.Name, delivery.MessageId).Updates(map[string]any{"status": "dead", "locked_by": nil, "locked_until": nil, "last_error_code": code}).Error; err != nil {
		// 已确认的死信副本具有持久性。在回执状态同样持久化前不要确认源消息；
		// 重复死信副本会通过死信接收端的数据库唯一约束合并。
		_ = delivery.Nack(false, true)
		return fmt.Errorf("mark consumer receipt dead: %w", err)
	}
	r.incDead()
	return delivery.Ack(false)
}

// classifyConsumerError 对消费者错误分类。
func classifyConsumerError(err error) *ConsumerError {
	var failure *ConsumerError
	if errors.As(err, &failure) {
		if failure.Code == "" {
			failure.Code = "MQ_CONSUMER_FAILED"
		}
		if failure.Safe == "" {
			failure.Safe = "consumer failed"
		}
		return failure
	}
	return &ConsumerError{Code: "MQ_CONSUMER_TEMPORARY", Safe: "consumer temporarily unavailable", Class: FailureTemporary, Err: err}
}

// cloneHeaders 克隆Headers。
func cloneHeaders(source amqp.Table) amqp.Table {
	result := amqp.Table{}
	for key, value := range source {
		result[key] = value
	}
	return result
}

// publishingFromDelivery 根据投递内容构造发布状态。
func publishingFromDelivery(delivery amqp.Delivery, headers amqp.Table) amqp.Publishing {
	return amqp.Publishing{Headers: headers, ContentType: delivery.ContentType, DeliveryMode: amqp.Persistent, MessageId: delivery.MessageId, Timestamp: time.Now(), Type: delivery.Type, Body: delivery.Body}
}

// headerInt 返回消息头整数。
func headerInt(headers amqp.Table, key string) int {
	if headers == nil {
		return 0
	}
	switch value := headers[key].(type) {
	case int:
		return value
	case int32:
		return int(value)
	case int64:
		return int(value)
	case string:
		parsed, _ := strconv.Atoi(value)
		return parsed
	default:
		return 0
	}
}

// incConsume 递增消费指标计数。
func (r *ConsumerRuntime) incConsume(result string) {
	if r.metrics != nil {
		r.metrics.IncMQConsume(r.spec.Name, result)
	}
}

// incRetry 递增重试指标计数。
func (r *ConsumerRuntime) incRetry(tier string) {
	if r.metrics != nil {
		r.metrics.IncMQRetry(r.spec.Name, tier)
	}
}

// incDead 递增死信指标计数。
func (r *ConsumerRuntime) incDead() {
	if r.metrics != nil {
		r.metrics.IncMQDead(r.spec.Name)
	}
}
