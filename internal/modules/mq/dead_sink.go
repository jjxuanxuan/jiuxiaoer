package mq

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/infra/rabbitmq"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
)

type DeadSink struct {
	db       *gorm.DB
	rabbit   *rabbitmq.Manager
	ids      idSource
	registry *EventRegistry
	metrics  *metrics.Registry
	consumer string
	queue    string
	log      *slog.Logger
}

// NewDeadSink 创建并初始化死信接收端。
func NewDeadSink(db *gorm.DB, rabbit *rabbitmq.Manager, ids idSource, registry *EventRegistry, metricRegistry *metrics.Registry, consumer string, log *slog.Logger) *DeadSink {
	queue := fmt.Sprintf("jxe.%s.dead.v1.queue", consumer)
	if consumer == "unrouted" {
		queue = unroutedQueueName
	}
	return &DeadSink{db: db, rabbit: rabbit, ids: ids, registry: registry, metrics: metricRegistry, consumer: consumer, queue: queue, log: log}
}

// Run 运行当前实例的核心处理流程。
func (s *DeadSink) Run(ctx context.Context) {
	if s.db == nil || s.rabbit == nil || s.ids == nil {
		return
	}
	for ctx.Err() == nil {
		conn, err := s.rabbit.Connection(ctx)
		if err != nil {
			return
		}
		if err := s.consumeSession(ctx, conn); err != nil && ctx.Err() == nil {
			s.log.Warn("dead sink session ended", slog.String("consumer", s.consumer), slog.Any("error", err))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// consumeSession 消费并处理会话。
func (s *DeadSink) consumeSession(ctx context.Context, conn *amqp.Connection) error {
	channel, err := conn.Channel()
	if err != nil {
		return err
	}
	defer channel.Close()
	if err := DeclareTopology(channel, DefaultTopology()); err != nil {
		return err
	}
	if err := channel.Qos(20, 0, false); err != nil {
		return err
	}
	deliveries, err := channel.ConsumeWithContext(ctx, s.queue, "jxe-dead-sink-"+s.consumer, false, false, false, false, nil)
	if err != nil {
		return err
	}
	for delivery := range deliveries {
		if err := s.persist(ctx, delivery); err != nil {
			_ = delivery.Nack(false, true)
			s.log.Warn("persist dead letter failed", slog.String("consumer", s.consumer), slog.Any("error", err))
			continue
		}
		if err := delivery.Ack(false); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return fmt.Errorf("dead sink delivery channel closed")
}

// persist 返回persist。
func (s *DeadSink) persist(ctx context.Context, delivery amqp.Delivery) error {
	now := time.Now()
	consumer := headerString(delivery.Headers, "x-jxe-consumer")
	if consumer == "" {
		consumer = s.consumer
	}
	code := headerString(delivery.Headers, "x-jxe-error-code")
	if code == "" {
		code = "MQ_EVENT_UNROUTED"
	}
	safe := headerString(delivery.Headers, "x-jxe-error-safe")
	if safe == "" {
		safe = "event had no approved route"
	}
	var envelope EventEnvelope
	_ = json.Unmarshal(delivery.Body, &envelope)
	eventID := envelope.EventID
	if eventID == "" {
		eventID = delivery.MessageId
	}
	hash := sha256.Sum256(delivery.Body)
	if eventID == "" {
		eventID = "invalid:" + hex.EncodeToString(hash[:8])
	}
	eventType := envelope.EventType
	if eventType == "" {
		eventType = delivery.Type
	}
	if eventType == "" {
		eventType = "_unknown"
	}
	created := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing int64
		if err := tx.Table("mq_dead_letters").Where("consumer_name=? AND event_id=? AND error_code=? AND status='open'", consumer, eventID, code).Count(&existing).Error; err != nil || existing > 0 {
			return err
		}
		id := s.ids.Next()
		row := DeadLetter{
			ID: id, DeadNo: fmt.Sprintf("MD%d", id), ConsumerName: consumer,
			EventID: eventID, EventType: eventType, EventVersion: maxUint(envelope.EventVersion, 1),
			AggregateType: stringPointer(envelope.AggregateType), AggregateID: stringPointer(envelope.AggregateID),
			ErrorCode: code, ErrorSafe: stringPointer(safe), PayloadHash: hex.EncodeToString(hash[:]),
			RetryCount: uint(headerInt(delivery.Headers, "x-retry-count")), Status: "open", Version: 1,
			FirstFailedAt: now, DeadAt: now,
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if result.Error != nil {
			return result.Error
		}
		created = result.RowsAffected == 1
		return nil
	})
	if err == nil && created && consumer == "unrouted" && s.metrics != nil {
		label := eventType
		if _, ok := s.registry.Lookup(eventType); !ok {
			label = "_unknown"
		}
		s.metrics.IncMQUnrouted(label)
	}
	return err
}

// headerString 返回header 字符串。
func headerString(headers amqp.Table, key string) string {
	if headers == nil {
		return ""
	}
	switch value := headers[key].(type) {
	case string:
		return value
	case []byte:
		return string(value)
	default:
		return ""
	}
}

// stringPointer 返回字符串 Pointer。
func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
