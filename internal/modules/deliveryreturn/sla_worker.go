package deliveryreturn

import (
	"context"
	"log/slog"
	"time"

	"gorm.io/gorm"
)

type SLAWorker struct {
	service *Service
	log     *slog.Logger
}

func NewSLAWorker(service *Service, log *slog.Logger) *SLAWorker {
	return &SLAWorker{service: service, log: log}
}

func (w *SLAWorker) Run(ctx context.Context) {
	if w == nil || w.service == nil || !w.service.cfg.DeliveryReturn.Enabled || !w.service.cfg.DeliveryReturn.SLAWorkerEnabled {
		return
	}
	ticker := time.NewTicker(w.service.cfg.DeliveryReturn.SLAWorkerInterval)
	defer ticker.Stop()
	for {
		w.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RunOnce records reminders and deadline breaches only. It never creates a
// receipt or inventory movement because elapsed time is not a physical fact.
func (w *SLAWorker) RunOnce(ctx context.Context) {
	service := w.service
	if service == nil || !service.cfg.DeliveryReturn.Enabled || !service.cfg.DeliveryReturn.SLAWorkerEnabled || service.repo.DB() == nil {
		return
	}
	now := service.now().UTC()
	closureIDs, err := service.repo.ClosureCandidates(ctx, service.cfg.DeliveryReturn.SLAWorkerBatchSize)
	if err != nil {
		w.logError("scan delivery return closure", err)
	} else {
		for _, afterSaleID := range closureIDs {
			if err := service.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				return service.TryCloseByAfterSaleWithTx(ctx, tx, afterSaleID)
			}); err != nil {
				w.logError("reconcile delivery return closure", err)
			}
		}
	}
	ids, err := service.repo.SLACandidates(ctx, now, service.cfg.DeliveryReturn.ReceiptReminderAfter, service.cfg.DeliveryReturn.SLAWorkerBatchSize)
	if err != nil {
		w.logError("scan delivery return SLA", err)
		return
	}
	for _, id := range ids {
		err := service.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			row, err := service.repo.ReturnByID(ctx, tx, id, true)
			if err != nil || row.ApprovedAt == nil || row.Status == StatusReceived || isTerminalStatus(row.Status) || row.Status == StatusDisputed {
				return err
			}
			deadlineReached := row.ReceiptDeadlineAt != nil && !now.Before(*row.ReceiptDeadlineAt)
			action := "sla_reminder"
			if deadlineReached {
				action = "sla_breach"
			}
			exists, err := service.repo.HistoryActionExists(ctx, tx, row.ID, action)
			if err != nil || exists {
				return err
			}
			from := row.Status
			to := row.Status
			if deadlineReached && row.Status != StatusException {
				updated, err := service.repo.UpdateReturnVersioned(ctx, tx, row, map[string]any{"status": StatusException})
				if err != nil {
					return err
				}
				if !updated {
					return nil
				}
				row.Status, row.Version, to = StatusException, row.Version+1, StatusException
			}
			return service.writeFacts(ctx, tx, row, "system", nil, action, from, to, "")
		})
		if err != nil {
			w.logError("process delivery return SLA", err)
		}
	}
}

func (w *SLAWorker) logError(message string, err error) {
	if w != nil && w.log != nil && err != nil {
		w.log.Error(message, slog.String("error", err.Error()))
	}
}
