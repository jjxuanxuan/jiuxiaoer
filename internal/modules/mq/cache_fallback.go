package mq

import (
	"context"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
)

const (
	cacheFallbackInterval = time.Minute
	cacheFallbackBatch    = 100
)

// CacheFallbackWorker 对账已提交但没有成功缓存回执的 cache.invalidate
// 发件箱事实。Redis 删除具备幂等性，因此在 DEL 后、回执提交前崩溃也可安全重试。
type CacheFallbackWorker struct {
	db       *gorm.DB
	registry *EventRegistry
	runtime  *ConsumerRuntime
	metrics  *metrics.Registry
	log      *slog.Logger
}

// NewCacheFallbackWorker 创建并初始化缓存降级工作器。
func NewCacheFallbackWorker(db *gorm.DB, registry *EventRegistry, handler *CacheInvalidationHandler, ids idSource, metricRegistry *metrics.Registry, instanceID string, log *slog.Logger) *CacheFallbackWorker {
	spec, _ := DefaultConsumerSpec("cache")
	runtime := &ConsumerRuntime{spec: spec, db: db, registry: registry, handler: handler, ids: ids, metrics: metricRegistry, instanceID: instanceID + ":cache-fallback", log: log}
	return &CacheFallbackWorker{db: db, registry: registry, runtime: runtime, metrics: metricRegistry, log: log}
}

// Run 运行当前实例的核心处理流程。
func (w *CacheFallbackWorker) Run(ctx context.Context) {
	if w == nil || w.db == nil || w.registry == nil || w.runtime == nil || w.runtime.handler == nil || w.runtime.ids == nil {
		return
	}
	w.reconcile(ctx)
	ticker := time.NewTicker(cacheFallbackInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.reconcile(ctx)
		}
	}
}

// reconcile 执行消息队列对账。
func (w *CacheFallbackWorker) reconcile(ctx context.Context) {
	var events []OutboxEvent
	err := w.db.WithContext(ctx).Table("outbox_events AS o").
		Select("o.*").
		Joins("LEFT JOIN mq_consumer_receipts AS r ON r.consumer_name='cache' AND r.event_id=o.event_id AND r.status='succeeded'").
		Where("o.event_type='cache.invalidate' AND r.id IS NULL").
		Order("o.id ASC").Limit(cacheFallbackBatch).Scan(&events).Error
	if err != nil {
		w.log.Warn("scan cache fallback events failed", slog.Any("error", err))
		return
	}
	definition, ok := w.registry.Lookup("cache.invalidate")
	if !ok {
		w.log.Error("cache invalidation contract is missing")
		return
	}
	for _, event := range events {
		envelope, buildErr := BuildEnvelope(event, definition, "db-fallback")
		if buildErr != nil {
			w.log.Warn("cache fallback contract validation failed", slog.String("event_id", event.EventID), slog.Any("error", buildErr))
			continue
		}
		handlerCtx, cancel := context.WithTimeout(ctx, w.runtime.spec.HandlerTimeout)
		duplicate, _, processErr := w.runtime.processEnvelope(handlerCtx, envelope)
		cancel()
		if processErr != nil {
			failure := classifyConsumerError(processErr)
			if recordErr := w.runtime.recordFailure(ctx, envelope, 1, failure); recordErr != nil {
				w.log.Warn("record cache fallback failure failed", slog.String("event_id", event.EventID), slog.Any("error", recordErr))
			}
			continue
		}
		if w.metrics != nil {
			result := "fallback_succeeded"
			if duplicate {
				result = "fallback_duplicate"
			}
			w.metrics.IncMQConsume("cache", result)
		}
	}
}
