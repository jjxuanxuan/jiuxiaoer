package mq

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/infra/rabbitmq"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
)

const maxPublisherRetries = 5

type OutboxEvent struct {
	ID              uint64
	EventID         string
	EventType       string
	EventVersion    uint
	SpecVersion     string
	AggregateType   string
	AggregateID     uint64
	Producer        string
	SchemaRef       string
	PartitionKey    string
	ReplayOfEventID string
	Payload         datatypes.JSON
	Status          string
	RetryCount      int
	NextRetryAt     *time.Time
	PublishedAt     *time.Time
	ExchangeName    *string
	RoutingKey      *string
	DispatchedAt    *time.Time
	RequestID       *string
	LockedBy        *string
	LockedUntil     *time.Time
	LastErrorCode   *string
	LastErrorDetail *string
	CreatedAt       time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (OutboxEvent) TableName() string { return "outbox_events" }

type idSource interface{ Next() uint64 }

type Publisher struct {
	db            *gorm.DB
	rabbit        *rabbitmq.Manager
	metrics       *metrics.Registry
	registry      *EventRegistry
	ids           idSource
	environment   string
	workerID      string
	leaseDuration time.Duration
	batchSize     int
	log           *slog.Logger
}

type PublisherOption func(*Publisher)

// WithPublisherRegistry 设置发布器注册表并返回更新后的值。
func WithPublisherRegistry(registry *EventRegistry) PublisherOption {
	return func(p *Publisher) { p.registry = registry }
}

// WithPublisherEnvironment 设置发布器 Environment并返回更新后的值。
func WithPublisherEnvironment(environment string) PublisherOption {
	return func(p *Publisher) { p.environment = environment }
}

// WithPublisherIDs 设置发布器 I Ds并返回更新后的值。
func WithPublisherIDs(ids idSource) PublisherOption {
	return func(p *Publisher) { p.ids = ids }
}

// WithPublisherBatchSize 设置发布器批次 Size并返回更新后的值。
func WithPublisherBatchSize(size int) PublisherOption {
	return func(p *Publisher) {
		if size >= 10 && size <= 500 {
			p.batchSize = size
		}
	}
}

// NewPublisher 创建并初始化发布器。
// NewPublisher publishes committed outbox facts outside the business
// transaction. RabbitMQ outages therefore never roll back core business data.
func NewPublisher(db *gorm.DB, rabbit *rabbitmq.Manager, metricRegistry *metrics.Registry, workerID string, log *slog.Logger, options ...PublisherOption) *Publisher {
	publisher := &Publisher{
		db: db, rabbit: rabbit, metrics: metricRegistry, registry: MustDefaultEventRegistry(),
		environment: "local", workerID: workerID + ":outbox", leaseDuration: 30 * time.Second,
		batchSize: 50, log: log,
	}
	for _, option := range options {
		option(publisher)
	}
	if metricRegistry != nil && db != nil {
		metricRegistry.AddCollector(publisher.collectMetrics)
	}
	return publisher
}

// Run 运行当前实例的核心处理流程。
func (p *Publisher) Run(ctx context.Context) {
	if p.db == nil || p.rabbit == nil {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	p.log.Info("outbox publisher started", slog.String("topology_version", topologyContractVersion))
	for {
		if err := p.publishBatch(ctx); err != nil && ctx.Err() == nil {
			p.log.Warn("outbox publish batch failed", slog.Any("error", err))
		}
		select {
		case <-ctx.Done():
			p.log.Info("outbox publisher stopped")
			return
		case <-ticker.C:
		}
	}
}

// publishBatch 发布批次。
func (p *Publisher) publishBatch(ctx context.Context) error {
	now := time.Now()
	events, err := p.claimBatch(ctx, now, p.batchSize)
	if err != nil || len(events) == 0 {
		return err
	}

	type dispatch struct {
		event      OutboxEvent
		definition EventDefinition
	}
	routable := make([]dispatch, 0, len(events))
	for _, event := range events {
		definition, registered := p.registry.Lookup(event.EventType)
		if !registered {
			p.markValidationDead(ctx, event, "MQ_EVENT_UNREGISTERED", "event type is not registered")
			continue
		}
		if _, err := BuildEnvelope(event, definition, p.environment); err != nil {
			p.markValidationDead(ctx, event, stableErrorCode(err), "event contract validation failed")
			continue
		}
		if !definition.Routable() {
			if err := p.markNoConsumer(ctx, event, definition); err != nil {
				p.log.Warn("mark no-consumer event failed", slog.Uint64("outbox_id", event.ID), slog.Any("error", err))
			}
			continue
		}
		routable = append(routable, dispatch{event: event, definition: definition})
	}
	if len(routable) == 0 {
		return nil
	}

	conn, err := p.rabbit.Connection(ctx)
	if err != nil {
		return err
	}
	channel, err := conn.Channel()
	if err != nil {
		return err
	}
	defer channel.Close()
	if err := DeclareTopology(channel, DefaultTopology()); err != nil {
		return err
	}
	if err := channel.Confirm(false); err != nil {
		return err
	}
	confirmations := channel.NotifyPublish(make(chan amqp.Confirmation, 1))
	returns := channel.NotifyReturn(make(chan amqp.Return, 1))

	for _, item := range routable {
		if err := p.publishOne(ctx, channel, confirmations, returns, item.event, item.definition); err != nil {
			p.incOutbox("failed")
			p.log.Warn("outbox publish failed", slog.Uint64("outbox_id", item.event.ID), slog.String("event_type", item.event.EventType), slog.Any("error", err))
			if markErr := p.markFailed(ctx, item.event); markErr != nil {
				p.log.Warn("mark outbox failed", slog.Uint64("outbox_id", item.event.ID), slog.Any("error", markErr))
			}
			continue
		}
		if err := p.markPublished(ctx, item.event, item.definition); err != nil {
			p.log.Warn("mark outbox published failed", slog.Uint64("outbox_id", item.event.ID), slog.Any("error", err))
			continue
		}
		p.incOutbox("published")
	}
	return nil
}

// claimBatch 认领批次。
func (p *Publisher) claimBatch(ctx context.Context, now time.Time, limit int) ([]OutboxEvent, error) {
	if limit <= 0 {
		return nil, nil
	}
	var events []OutboxEvent
	leaseUntil := now.Add(p.leaseDuration)
	claimToken := p.workerID + ":" + uuid.NewString()
	err := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Exec(`
			UPDATE outbox_events SET locked_by = ?, locked_until = ?
			WHERE status = 'pending'
			  AND (next_retry_at IS NULL OR next_retry_at <= ?)
			  AND (locked_until IS NULL OR locked_until <= ?)
			ORDER BY id ASC LIMIT ?
		`, claimToken, leaseUntil, now, now, limit)
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		if err := tx.Where("status='pending' AND locked_by=?", claimToken).Order("id ASC").Limit(limit).Find(&events).Error; err != nil {
			return err
		}
		if int64(len(events)) != result.RowsAffected {
			return fmt.Errorf("outbox claim mismatch: updated=%d loaded=%d", result.RowsAffected, len(events))
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	return events, err
}

// publishOne 发布One。
func (p *Publisher) publishOne(ctx context.Context, channel *amqp.Channel, confirmations <-chan amqp.Confirmation, returns <-chan amqp.Return, event OutboxEvent, definition EventDefinition) error {
	envelope, err := BuildEnvelope(event, definition, p.environment)
	if err != nil {
		return err
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	return publishConfirmed(ctx, channel, confirmations, returns, exchangeName, event.EventType, false, amqp.Publishing{
		Headers: amqp.Table{"x-jxe-topology-version": topologyContractVersion}, ContentType: "application/json",
		DeliveryMode: amqp.Persistent, MessageId: event.EventID, Timestamp: time.Now(), Type: event.EventType, Body: body,
	})
}

// eventBody 返回事件请求体。
// eventBody remains the small contract seam used by unit tests and tools.
func eventBody(event OutboxEvent) ([]byte, error) {
	definition, ok := MustDefaultEventRegistry().Lookup(event.EventType)
	if !ok {
		return nil, fmt.Errorf("MQ_EVENT_UNREGISTERED")
	}
	envelope, err := BuildEnvelope(event, definition, "local")
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope)
}

// publishConfirmed 发布已确认事件。
func publishConfirmed(ctx context.Context, channel *amqp.Channel, confirmations <-chan amqp.Confirmation, returns <-chan amqp.Return, exchange, routingKey string, mandatory bool, message amqp.Publishing) error {
	if err := channel.PublishWithContext(ctx, exchange, routingKey, mandatory, false, message); err != nil {
		return err
	}
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case returned := <-returns:
			if returned.MessageId == "" || returned.MessageId == message.MessageId {
				return fmt.Errorf("rabbitmq returned unroutable message: %s", returned.ReplyText)
			}
		case confirmation := <-confirmations:
			if !confirmation.Ack {
				return fmt.Errorf("rabbitmq negatively acknowledged message")
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return fmt.Errorf("rabbitmq publish confirmation timeout")
		}
	}
}

// markPublished 标记Published的状态。
func (p *Publisher) markPublished(ctx context.Context, event OutboxEvent, definition EventDefinition) error {
	now := time.Now()
	exchange, routingKey := exchangeName, event.EventType
	result := p.db.WithContext(ctx).Model(&OutboxEvent{}).
		Where("id = ? AND status = 'pending' AND locked_by = ?", event.ID, leaseOwner(event, p.workerID)).
		Updates(map[string]any{
			"status": "published", "published_at": &now, "dispatched_at": &now,
			"exchange_name": exchange, "routing_key": routingKey, "event_version": definition.Version,
			"spec_version": envelopeSpecVersion, "producer": definition.Owner,
			"schema_ref":    fmt.Sprintf("event://%s/%d", event.EventType, definition.Version),
			"partition_key": partitionKey(event), "locked_by": nil, "locked_until": nil,
			"last_error_code": nil, "last_error_detail": nil,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("outbox lease lost before marking published")
	}
	return nil
}

// markNoConsumer 标记无消费者的状态。
func (p *Publisher) markNoConsumer(ctx context.Context, event OutboxEvent, definition EventDefinition) error {
	result := p.db.WithContext(ctx).Model(&OutboxEvent{}).
		Where("id = ? AND status = 'pending' AND locked_by = ?", event.ID, leaseOwner(event, p.workerID)).
		Updates(map[string]any{
			"status": "no_consumer", "event_version": definition.Version, "spec_version": envelopeSpecVersion,
			"producer": definition.Owner, "schema_ref": fmt.Sprintf("event://%s/%d", event.EventType, definition.Version),
			"partition_key": partitionKey(event), "locked_by": nil, "locked_until": nil,
			"last_error_code": nil, "last_error_detail": nil,
		})
	if result.Error == nil && result.RowsAffected == 1 {
		p.incOutbox("no_consumer")
		return nil
	}
	if result.Error != nil {
		return result.Error
	}
	return fmt.Errorf("outbox lease lost before marking no_consumer")
}

// markValidationDead 标记校验死信的状态。
func (p *Publisher) markValidationDead(ctx context.Context, event OutboxEvent, code, safe string) {
	_ = p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&OutboxEvent{}).
			Where("id = ? AND status = 'pending' AND locked_by = ?", event.ID, leaseOwner(event, p.workerID)).
			Updates(map[string]any{"status": "dead", "retry_count": event.RetryCount + 1, "locked_by": nil, "locked_until": nil, "last_error_code": code, "last_error_detail": safe})
		if result.Error != nil || result.RowsAffected != 1 {
			return result.Error
		}
		return p.createDeadLetter(tx, event, "publisher", code, safe, event.RetryCount+1)
	})
	p.incOutbox("validation_dead")
}

// markFailed 标记Failed的状态。
func (p *Publisher) markFailed(ctx context.Context, event OutboxEvent) error {
	retryCount := event.RetryCount + 1
	status := "pending"
	var nextRetryAt *time.Time
	if retryCount >= maxPublisherRetries {
		status = "dead"
	} else {
		delaySeconds := int(math.Min(60, math.Pow(2, float64(retryCount))))
		next := time.Now().Add(time.Duration(delaySeconds) * time.Second)
		nextRetryAt = &next
	}
	detail := "event publish failed"
	return p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&OutboxEvent{}).
			Where("id = ? AND status = 'pending' AND locked_by = ?", event.ID, leaseOwner(event, p.workerID)).
			Updates(map[string]any{"status": status, "retry_count": retryCount, "next_retry_at": nextRetryAt, "locked_by": nil, "locked_until": nil, "last_error_code": "MQ_PUBLISH_FAILED", "last_error_detail": detail})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("outbox lease lost before marking failed")
		}
		if status == "dead" {
			return p.createDeadLetter(tx, event, "publisher", "MQ_PUBLISH_FAILED", detail, retryCount)
		}
		return nil
	})
}

// createDeadLetter 创建死信 Letter。
func (p *Publisher) createDeadLetter(tx *gorm.DB, event OutboxEvent, consumer, code, safe string, retries int) error {
	now := time.Now()
	id := event.ID
	if p.ids != nil {
		id = p.ids.Next()
	}
	hash := sha256.Sum256(event.Payload)
	aggregateType, aggregateID, safeMessage := event.AggregateType, fmt.Sprint(event.AggregateID), safe
	row := DeadLetter{
		ID: id, DeadNo: fmt.Sprintf("MD%d", id), ConsumerName: consumer,
		EventID: event.EventID, EventType: event.EventType, EventVersion: maxUint(event.EventVersion, 1),
		AggregateType: &aggregateType, AggregateID: &aggregateID,
		ErrorCode: code, ErrorSafe: &safeMessage, PayloadHash: hex.EncodeToString(hash[:]),
		RetryCount: uint(retries), Status: "open", Version: 1, FirstFailedAt: now, DeadAt: now,
	}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error
}

// leaseOwner 返回租约 Owner。
func leaseOwner(event OutboxEvent, fallback string) string {
	if event.LockedBy != nil && *event.LockedBy != "" {
		return *event.LockedBy
	}
	return fallback
}

// partitionKey 返回partition 密钥。
func partitionKey(event OutboxEvent) string {
	if event.PartitionKey != "" {
		return event.PartitionKey
	}
	return fmt.Sprintf("%s:%d", event.AggregateType, event.AggregateID)
}

// stableErrorCode 返回stable 错误代码。
func stableErrorCode(err error) string {
	if err == nil {
		return "MQ_CONTRACT_INVALID"
	}
	code := strings.SplitN(err.Error(), ":", 2)[0]
	if strings.HasPrefix(code, "MQ_") {
		return code
	}
	return "MQ_CONTRACT_INVALID"
}

// maxUint 返回max Uint。
func maxUint(value, fallback uint) uint {
	if value == 0 {
		return fallback
	}
	return value
}

// incOutbox 递增发件箱事件指标计数。
func (p *Publisher) incOutbox(result string) {
	if p.metrics != nil {
		p.metrics.IncOutbox(result)
	}
}

// collectMetrics 收集指标。
func (p *Publisher) collectMetrics() []metrics.Sample {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var pending, dead, noConsumer int64
	var oldest struct {
		CreatedAt sql.NullTime `gorm:"column:created_at"`
	}
	_ = p.db.WithContext(ctx).Model(&OutboxEvent{}).Where("status = 'pending'").Count(&pending).Error
	_ = p.db.WithContext(ctx).Model(&OutboxEvent{}).Where("status = 'dead'").Count(&dead).Error
	_ = p.db.WithContext(ctx).Model(&OutboxEvent{}).Where("status = 'no_consumer'").Count(&noConsumer).Error
	_ = p.db.WithContext(ctx).Model(&OutboxEvent{}).Where("status = 'pending'").Select("MIN(created_at) AS created_at").Scan(&oldest).Error
	return []metrics.Sample{
		{Name: "jxe_outbox_pending", Help: "Pending outbox events.", Type: "gauge", Value: float64(pending)},
		{Name: "jxe_outbox_dead", Help: "Dead outbox events.", Type: "gauge", Value: float64(dead)},
		{Name: "jxe_outbox_no_consumer", Help: "Registered events intentionally retained without a consumer.", Type: "gauge", Value: float64(noConsumer)},
		{Name: "jxe_outbox_oldest_pending_seconds", Help: "Age of the oldest pending outbox event.", Type: "gauge", Value: pendingAgeSeconds(time.Now(), oldest.CreatedAt)},
	}
}

// pendingAgeSeconds 返回待处理 Age Seconds。
func pendingAgeSeconds(now time.Time, oldest sql.NullTime) float64 {
	if !oldest.Valid {
		return 0
	}
	age := now.Sub(oldest.Time).Seconds()
	if age < 0 {
		return 0
	}
	return age
}
