package realtime

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/config"
)

type RelayWorker struct {
	cfg     config.RealtimeConfig
	db      *gorm.DB
	service *Service
	owner   string
	log     *slog.Logger
	runs    uint64
}

// NewRelayWorker 创建并初始化转发工作器。
func NewRelayWorker(cfg config.RealtimeConfig, db *gorm.DB, service *Service, owner string, log *slog.Logger) *RelayWorker {
	return &RelayWorker{cfg: cfg, db: db, service: service, owner: owner, log: log}
}

// Run 运行当前实例的核心处理流程。
func (w *RelayWorker) Run(ctx context.Context) {
	if !w.cfg.Enabled || !w.cfg.RelayEnabled || w.db == nil || w.service == nil {
		return
	}
	ticker := time.NewTicker(w.cfg.RelayInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.RunOnce(ctx); err != nil && w.log != nil {
				w.log.Error("realtime relay sweep failed", slog.Any("error", err))
			}
		}
	}
}

// RunOnce 运行Once处理流程。
func (w *RelayWorker) RunOnce(ctx context.Context) error {
	rows, err := w.claim(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.ExpiresAt.Before(time.Now()) {
			_ = w.db.WithContext(ctx).Model(&Delivery{}).Where("id=?", row.ID).Update("relay_status", relayExpired).Error
			w.service.metrics.inc(w.service.metrics.relays, relayExpired)
			continue
		}
		if err := w.service.PublishWakeup(ctx, row); err != nil {
			w.recordFailure(ctx, row, err)
		}
	}
	w.runs++
	if w.runs%300 == 0 {
		w.cleanup(ctx)
	}
	return nil
}

// claim 认领Delivery列表。
func (w *RelayWorker) claim(ctx context.Context) ([]Delivery, error) {
	rows := make([]Delivery, 0, w.cfg.RelayBatchSize)
	err := w.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("relay_status=? AND next_relay_at<=?", relayPending, now).
			Order("next_relay_at,id").Limit(w.cfg.RelayBatchSize).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		ids := make([]uint64, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		return tx.Model(&Delivery{}).Where("id IN ? AND relay_status=?", ids, relayPending).
			Updates(map[string]any{"next_relay_at": now.Add(30 * time.Second), "relay_attempts": gorm.Expr("relay_attempts+1")}).Error
	})
	return rows, err
}

// recordFailure 处理记录 Failure相关逻辑。
func (w *RelayWorker) recordFailure(ctx context.Context, row Delivery, cause error) {
	status := relayPending
	attempt := row.RelayAttempts + 1
	if attempt >= 10 {
		status = relayDead
	}
	backoff := time.Second * time.Duration(1<<min(attempt, 8))
	code := "REALTIME_RELAY_FAILED"
	_ = w.db.WithContext(ctx).Model(&Delivery{}).Where("id=?", row.ID).Updates(map[string]any{
		"relay_status": status, "next_relay_at": time.Now().Add(backoff), "last_error_code": code,
	}).Error
	w.service.metrics.inc(w.service.metrics.relays, status)
	if w.log != nil && !errors.Is(cause, context.Canceled) {
		w.log.Warn("realtime relay publish failed", slog.Uint64("delivery_id", row.ID), slog.Any("error", cause))
	}
}

// cleanup 清理实时消息。
func (w *RelayWorker) cleanup(ctx context.Context) {
	now := time.Now().UTC()
	_ = w.db.WithContext(ctx).Where("received_at<?", now.Add(-w.cfg.AcknowledgementRetention)).Limit(500).Delete(&Acknowledgement{}).Error
	_ = w.db.WithContext(ctx).Where("expires_at<?", now.Add(-w.cfg.DeliveryRetention)).Limit(500).Delete(&Delivery{}).Error
}
