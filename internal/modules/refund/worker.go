package refund

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
)

type Worker struct {
	cfg      config.Config
	service  *Service
	provider Provider
	instance string
	log      *slog.Logger
}

// NewWorker 创建并初始化工作器。
func NewWorker(cfg config.Config, service *Service, provider Provider, log *slog.Logger) *Worker {
	return &Worker{cfg: cfg, service: service, provider: provider, instance: cfg.App.InstanceID, log: log}
}

// Run 运行当前实例的核心处理流程。
func (w *Worker) Run(ctx context.Context) {
	if w.provider == nil {
		return
	}
	ticker := time.NewTicker(w.cfg.AfterSale.WorkerInterval)
	defer ticker.Stop()
	for {
		w.runBatch(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runBatch 运行批次处理流程。
func (w *Worker) runBatch(ctx context.Context) {
	for i := 0; i < w.cfg.AfterSale.WorkerBatchSize; i++ {
		row, err := w.service.repo.Claim(ctx, w.instance, time.Now())
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return
		}
		if err != nil {
			w.log.Error("claim refund task", slog.String("error", err.Error()))
			return
		}
		callCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		var state State
		if row.Status == "creating" {
			payment, paymentErr := w.service.repo.LockPayment(callCtx, w.service.repo.DB(), row.PaymentID)
			if paymentErr != nil {
				err = paymentErr
			} else {
				state, err = w.provider.Refund(callCtx, Input{RefundNo: row.RefundNo, PaymentNo: payment.PaymentNo, Reason: "after-sale refund", NotifyURL: w.cfg.WeChat.RefundNotifyURL, Currency: row.Currency, Amount: row.Amount, TotalAmount: row.TotalAmount})
			}
		} else {
			state, err = w.provider.QueryRefund(callCtx, row.RefundNo)
		}
		cancel()
		if err != nil {
			_ = w.service.MarkAttemptError(ctx, row.ID, err)
			continue
		}
		if err := w.service.ApplyProviderState(ctx, row.ID, state); err != nil {
			_ = w.service.MarkAttemptError(ctx, row.ID, err)
		}
	}
}
