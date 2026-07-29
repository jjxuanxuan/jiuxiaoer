package gift

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

type GiftExpiryWorker struct {
	service  *GiftService
	log      *slog.Logger
	interval time.Duration
	batch    int
}

func NewGiftExpiryWorker(service *GiftService, log *slog.Logger) *GiftExpiryWorker {
	if log == nil {
		log = slog.Default()
	}
	return &GiftExpiryWorker{
		service: service, log: log, interval: time.Minute, batch: 100,
	}
}

func (w *GiftExpiryWorker) WithInterval(interval time.Duration) *GiftExpiryWorker {
	if interval > 0 {
		w.interval = interval
	}
	return w
}

func (w *GiftExpiryWorker) WithBatchSize(batch int) *GiftExpiryWorker {
	if batch > 0 && batch <= 1000 {
		w.batch = batch
	}
	return w
}

func (w *GiftExpiryWorker) Run(ctx context.Context) {
	if w == nil || w.service == nil {
		return
	}
	w.runOnce(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runOnce(ctx)
		}
	}
}

func (w *GiftExpiryWorker) runOnce(ctx context.Context) {
	if _, err := w.service.ExpireDue(ctx, w.batch); err != nil &&
		!errors.Is(err, context.Canceled) {
		w.log.Error("wine-ticket gift expiry worker failed", slog.Any("error", err))
	}
}
