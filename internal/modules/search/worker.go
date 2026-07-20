package search

import (
	"context"
	"hash/fnv"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
)

type Worker struct {
	cfg      config.SearchConfig
	repo     *Repository
	metrics  *searchMetrics
	log      *slog.Logger
	instance string
	now      func() time.Time
}

func NewWorker(cfg config.SearchConfig, db *gorm.DB, registry *metrics.Registry, instance string, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{cfg: cfg, repo: NewRepository(db), metrics: newSearchMetrics(registry), log: log, instance: instance, now: time.Now}
}

func (w *Worker) Run(ctx context.Context) {
	delay := w.initialDelay()
	timer := time.NewTimer(delay)
	select {
	case <-ctx.Done():
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		return
	case <-timer.C:
	}
	w.runAndLog(ctx)
	ticker := time.NewTicker(w.cfg.CleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runAndLog(ctx)
		}
	}
}

func (w *Worker) RunOnce(ctx context.Context) error {
	now := w.now()
	historyBefore := now.Add(-w.cfg.HistoryRetention)
	local := now.In(chinaStandardTime)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, chinaStandardTime)
	statsBefore := today.AddDate(0, 0, -w.cfg.StatsRetentionDays)
	if err := w.cleanup(ctx, "history", func() (int64, error) {
		return w.repo.CleanupHistory(ctx, historyBefore, w.cfg.CleanupBatchSize)
	}); err != nil {
		return err
	}
	return w.cleanup(ctx, "stats", func() (int64, error) {
		return w.repo.CleanupStats(ctx, statsBefore, w.cfg.CleanupBatchSize)
	})
}

func (w *Worker) cleanup(ctx context.Context, table string, deleteBatch func() (int64, error)) error {
	for {
		deleted, err := deleteBatch()
		if err != nil {
			w.metrics.addCleanup(table, "error", 0)
			return err
		}
		w.metrics.addCleanup(table, "success", deleted)
		if deleted < int64(w.cfg.CleanupBatchSize) {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (w *Worker) initialDelay() time.Duration {
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(w.instance))
	_, _ = hasher.Write([]byte(w.now().In(chinaStandardTime).Format("2006-01-02")))
	return time.Duration(hasher.Sum64() % uint64(time.Minute))
}

func (w *Worker) runAndLog(ctx context.Context) {
	started := w.now()
	if err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
		w.log.ErrorContext(ctx, "search retention cleanup failed", slog.String("instance_id", w.instance), slog.Any("error", err))
		return
	}
	if ctx.Err() == nil {
		w.log.InfoContext(ctx, "search retention cleanup completed", slog.String("instance_id", w.instance), slog.Duration("duration", w.now().Sub(started)))
	}
}
