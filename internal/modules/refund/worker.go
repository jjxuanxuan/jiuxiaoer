package refund

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/paygateway"
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
		state, apply, err := w.process(callCtx, row)
		cancel()
		if err != nil {
			decision := "scheduled"
			var markErr error
			if paygateway.Retryable(err) {
				markErr = w.service.MarkAttemptError(ctx, row.ID, err)
			} else {
				decision = "exception"
				markErr = w.service.MarkPermanentError(ctx, row.ID, paygateway.Code(err, "PROVIDER_REJECTED"), err)
			}
			if markErr != nil {
				w.log.Error("mark refund provider failure", slog.String("refund_no", row.RefundNo), slog.String("error", markErr.Error()))
			}
			w.logProviderFailure(row, err, decision)
			continue
		}
		if !apply {
			continue
		}
		w.logProviderSuccess(row, state)
		if err := w.service.ApplyClaimedProviderState(ctx, row.ID, row.Version+1, state); err != nil {
			if markErr := w.service.MarkAttemptError(ctx, row.ID, err); markErr != nil {
				w.log.Error("mark refund state failure", slog.String("refund_no", row.RefundNo), slog.String("error", markErr.Error()))
			}
		}
	}
}

func (w *Worker) process(ctx context.Context, row Row) (State, bool, error) {
	switch row.Status {
	case "creating":
		state, err := w.submit(ctx, row)
		return state, err == nil, err
	case "submission_unknown":
		state, err := w.provider.QueryRefund(ctx, row.RefundNo)
		if err == nil {
			return state, true, nil
		}
		if !paygateway.IsCode(err, "RESOURCE_NOT_EXISTS") {
			return State{}, false, err
		}
		originalCode := ""
		if row.FailureCode != nil {
			originalCode = *row.FailureCode
		}
		if !submissionRetryAllowed(originalCode) {
			if markErr := w.service.MarkPermanentError(ctx, row.ID, originalCode, err); markErr != nil {
				return State{}, false, markErr
			}
			w.logProviderFailure(row, err, "exception")
			return State{}, false, nil
		}
		state, err = w.submit(ctx, row)
		return state, err == nil, err
	case "pending":
		state, err := w.provider.QueryRefund(ctx, row.RefundNo)
		if paygateway.IsCode(err, "RESOURCE_NOT_EXISTS") {
			if markErr := w.service.MarkPermanentError(ctx, row.ID, "REFUND_PROVIDER_RECORD_MISSING", err); markErr != nil {
				return State{}, false, markErr
			}
			w.logProviderFailure(row, err, "exception")
			return State{}, false, nil
		}
		return state, err == nil, err
	default:
		return State{}, false, nil
	}
}

func (w *Worker) submit(ctx context.Context, row Row) (State, error) {
	payment, err := w.service.repo.LockPayment(ctx, w.service.repo.DB(), row.PaymentID)
	if err != nil {
		return State{}, err
	}
	reason := row.Reason
	if reason == "" {
		reason = "after-sale refund"
	}
	notifyURL := w.cfg.WeChat.RefundNotifyURL
	if row.NotifyURL != nil && *row.NotifyURL != "" {
		notifyURL = *row.NotifyURL
	}
	return w.provider.Refund(ctx, Input{RefundNo: row.RefundNo, PaymentNo: payment.PaymentNo, Reason: reason, NotifyURL: notifyURL, Currency: row.Currency, Amount: row.Amount, TotalAmount: row.TotalAmount})
}

func submissionRetryAllowed(code string) bool {
	switch code {
	case "", "PROVIDER_UNAVAILABLE", "SYSTEM_ERROR", "FREQUENCY_LIMITED", "NOT_ENOUGH_MANUAL_RETRY":
		return true
	default:
		return false
	}
}

func (w *Worker) logProviderFailure(row Row, err error, decision string) {
	providerErr, _ := paygateway.As(err)
	attrs := []any{slog.String("refund_no", row.RefundNo), slog.String("status", row.Status), slog.String("error", err.Error()), slog.String("retry_decision", decision)}
	if providerErr != nil {
		attrs = append(attrs, slog.String("operation", providerErr.Operation), slog.Int("http_status", providerErr.HTTPStatus), slog.String("provider_code", providerErr.Code), slog.String("provider_request_id", providerErr.RequestID), slog.Bool("provider_retryable", providerErr.Retryable))
	}
	w.log.Warn("refund provider call failed", attrs...)
}

func (w *Worker) logProviderSuccess(row Row, state State) {
	w.log.Info("refund provider call completed", slog.String("refund_no", row.RefundNo), slog.String("local_status", row.Status), slog.String("provider_status", state.Status), slog.String("provider_request_id", state.RequestID))
}
