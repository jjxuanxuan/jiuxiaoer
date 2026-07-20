package order

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/paygateway"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type ExpiryWorker struct {
	cfg                  config.OrderConfig
	service              *Service
	metrics              *metrics.Registry
	log                  *slog.Logger
	lastReconcileSuccess atomic.Int64
}

// NewExpiryWorker 创建并初始化过期工作器。
func NewExpiryWorker(cfg config.Config, db *gorm.DB, idGen *snowflake.Generator, registry *metrics.Registry, log *slog.Logger, providers ...PaymentProvider) *ExpiryWorker {
	service := NewService(cfg, db, idGen)
	service.WithLogger(log)
	if len(providers) > 0 {
		service.WithPaymentProvider(providers[0], registry)
	}
	worker := &ExpiryWorker{
		cfg:     cfg.Order,
		service: service,
		metrics: registry,
		log:     log,
	}
	if registry != nil && db != nil {
		registry.AddCollector(worker.collectMetrics)
	}
	return worker
}

// Run 运行当前实例的核心处理流程。
func (w *ExpiryWorker) Run(ctx context.Context) {
	if w == nil || w.service == nil || w.service.repo.DB() == nil {
		return
	}
	ticker := time.NewTicker(w.cfg.ExpiryScanInterval)
	defer ticker.Stop()
	w.log.Info("order expiry worker started")
	defer w.log.Info("order expiry worker stopped")
	for {
		reconciled, reconcileErr := w.ReconcileCreatingBatch(ctx, time.Now(), w.cfg.ExpiryBatchSize)
		if reconcileErr != nil {
			w.metrics.IncOrderExpiry("creating_reconcile_failed")
			w.log.Warn("creating payment reconciliation failed", slog.Any("error", reconcileErr))
		} else if reconciled > 0 {
			w.log.Info("reconciled creating payments", slog.Int("count", reconciled))
		}
		if reconcileErr == nil {
			w.lastReconcileSuccess.Store(time.Now().Unix())
		}
		processed, err := w.ExpireBatch(ctx, time.Now(), w.cfg.ExpiryBatchSize)
		if err != nil {
			w.metrics.IncOrderExpiry("failed")
			w.log.Warn("order expiry batch failed", slog.Any("error", err))
		} else if processed > 0 {
			w.log.Info("expired unpaid orders", slog.Int("count", processed))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ReconcileCreatingBatch 执行创建中批次对账。
func (w *ExpiryWorker) ReconcileCreatingBatch(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit <= 0 || w.service.payment == nil {
		return 0, nil
	}
	processed := 0
	staleBefore := now.Add(-w.cfg.CreatingReconcileAge)
	for processed < limit {
		payment, err := w.service.repo.ClaimNextReconcilablePayment(ctx, w.service.payment.Code(), now, staleBefore)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			break
		}
		if err != nil {
			return processed, err
		}
		providerCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		state, queryErr := w.service.payment.Query(providerCtx, payment.PaymentNo)
		cancel()
		if queryErr != nil {
			if markErr := w.service.MarkPaymentReconcileError(ctx, payment, queryErr); markErr != nil {
				return processed, markErr
			}
			w.metrics.IncOrderExpiry("payment_provider_query_failed")
			w.log.Warn("payment reconciliation query failed", slog.String("payment_no", payment.PaymentNo), slog.String("provider_code", paygateway.Code(queryErr, "PROVIDER_UNAVAILABLE")), slog.String("provider_request_id", paygateway.RequestID(queryErr)), slog.Bool("retryable", paygateway.Retryable(queryErr)))
			processed++
			continue
		}
		if _, err := w.service.ApplyProviderPaymentState(ctx, payment.PaymentNo, payment.Provider, state, "system", 0, "reconcile:"+payment.PaymentNo); err != nil {
			w.metrics.IncOrderExpiry("payment_provider_state_failed")
			return processed, err
		}
		w.metrics.IncOrderExpiry("payment_reconciled_" + strings.ToLower(state.Status))
		w.log.Info("payment reconciliation completed", slog.String("payment_no", payment.PaymentNo), slog.String("provider_status", state.Status), slog.String("provider_request_id", state.RequestID))
		processed++
	}
	return processed, nil
}

// ExpireBatch 将批次标记为过期。
func (w *ExpiryWorker) ExpireBatch(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	processed := 0
	for processed < limit {
		candidate, err := w.service.repo.NextExpiredOrder(ctx, now)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			break
		}
		if err != nil {
			return processed, err
		}
		handled, retryLater, err := w.expireCandidate(ctx, candidate, now)
		if err != nil {
			return processed, err
		}
		if retryLater {
			break
		}
		if !handled {
			continue
		}
		processed++
	}
	return processed, nil
}

// expireCandidate 将Candidate标记为过期。
func (w *ExpiryWorker) expireCandidate(ctx context.Context, candidate Order, now time.Time) (bool, bool, error) {
	if w.service.payment != nil {
		payment, err := w.service.repo.GetPaymentByOrderProvider(ctx, candidate.ID, w.service.payment.Code())
		if err == nil && (payment.Status == "pending" || payment.Status == "creating") {
			providerCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			state, queryErr := w.service.payment.Query(providerCtx, payment.PaymentNo)
			cancel()
			if queryErr != nil {
				w.metrics.IncOrderExpiry("provider_query_failed")
				w.log.Warn("final payment query failed", slog.String("payment_no", payment.PaymentNo), slog.String("provider_code", paygateway.Code(queryErr, "PROVIDER_UNAVAILABLE")), slog.String("provider_request_id", paygateway.RequestID(queryErr)), slog.Bool("retryable", paygateway.Retryable(queryErr)))
				if paygateway.IsCode(queryErr, "ORDER_NOT_EXIST") || paygateway.IsCode(queryErr, "ORDER_NOT_EXISTS") {
					// The provider explicitly confirms that no transaction exists, so
					// there is nothing to close and local stock can be released.
					goto cancelLocal
				}
				return false, true, queryErr
			}
			w.log.Info("payment provider call completed", slog.String("operation", "payment.query"), slog.String("payment_no", payment.PaymentNo), slog.String("provider_status", state.Status), slog.String("provider_request_id", state.RequestID))
			if state.Status == "SUCCESS" {
				_, err := w.service.ApplyProviderPaymentState(ctx, payment.PaymentNo, payment.Provider, state, "system", 0, "expiry-query:"+payment.PaymentNo)
				if err != nil {
					return false, false, err
				}
				w.metrics.IncOrderExpiry("payment_reconciled")
				return true, false, nil
			}
			if state.Status == "USERPAYING" {
				if _, err := w.service.ApplyProviderPaymentState(ctx, payment.PaymentNo, payment.Provider, state, "system", 0, "expiry-query:"+payment.PaymentNo); err != nil {
					return false, false, err
				}
				w.metrics.IncOrderExpiry("provider_user_paying")
				return false, true, nil
			}
			if _, err := w.service.ApplyProviderPaymentState(ctx, payment.PaymentNo, payment.Provider, state, "system", 0, "expiry-query:"+payment.PaymentNo); err != nil {
				return false, false, err
			}
			if state.Status != "CLOSED" && state.Status != "REVOKED" && state.Status != "PAYERROR" {
				providerCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
				closeResult, closeErr := w.service.payment.Close(providerCtx, payment.PaymentNo)
				cancel()
				if closeErr != nil {
					w.service.logPaymentProviderFailure(payment.PaymentNo, closeErr, "retry_before_local_expiry")
					w.metrics.IncOrderExpiry("provider_close_failed")
					return false, true, closeErr
				}
				w.log.Info("payment provider call completed", slog.String("operation", "payment.close"), slog.String("payment_no", payment.PaymentNo), slog.String("provider_request_id", closeResult.RequestID))
			}
		} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return false, false, err
		}
	}

cancelLocal:
	handled := false
	err := w.service.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := w.service.repo.LockOrder(ctx, tx, candidate.ID)
		if err != nil {
			return err
		}
		if row.Status != "pending_payment" || row.ExpiresAt == nil || row.ExpiresAt.After(now) {
			return nil
		}
		key := fmt.Sprintf("expiry:%d", row.ID)
		if _, err := w.service.cancelPendingOrder(ctx, tx, row, "system", 0, "system", "PAYMENT_TIMEOUT", "payment window expired", key); err != nil {
			return err
		}
		handled = true
		return nil
	})
	if err != nil {
		return false, false, err
	}
	if handled {
		w.metrics.IncOrderExpiry("expired")
	}
	return handled, false, nil
}

// collectMetrics 收集指标。
func (w *ExpiryWorker) collectMetrics() []metrics.Sample {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var pending int64
	var paymentPending int64
	var oldest struct {
		ExpiresAt sql.NullTime `gorm:"column:expires_at"`
	}
	var oldestPayment struct {
		DueAt sql.NullTime `gorm:"column:due_at"`
	}
	now := time.Now()
	_ = w.service.repo.DB().WithContext(ctx).Model(&Order{}).
		Where("status = 'pending_payment' AND expires_at IS NOT NULL AND expires_at <= ?", now).
		Count(&pending).Error
	_ = w.service.repo.DB().WithContext(ctx).Model(&Order{}).
		Where("status = 'pending_payment' AND expires_at IS NOT NULL AND expires_at <= ?", now).
		Select("MIN(expires_at) AS expires_at").Scan(&oldest).Error
	_ = w.service.repo.DB().WithContext(ctx).Model(&Payment{}).
		Where("provider=? AND status IN ? AND deleted_at IS NULL", w.service.paymentCode(), []string{"creating", "pending"}).
		Count(&paymentPending).Error
	_ = w.service.repo.DB().WithContext(ctx).Model(&Payment{}).
		Where("provider=? AND status IN ? AND deleted_at IS NULL", w.service.paymentCode(), []string{"creating", "pending"}).
		Select("MIN(COALESCE(next_reconcile_at,updated_at)) AS due_at").Scan(&oldestPayment).Error
	lag := float64(0)
	if oldest.ExpiresAt.Valid {
		lag = now.Sub(oldest.ExpiresAt.Time).Seconds()
		if lag < 0 {
			lag = 0
		}
	}
	paymentLag := float64(0)
	if oldestPayment.DueAt.Valid {
		paymentLag = now.Sub(oldestPayment.DueAt.Time).Seconds()
		if paymentLag < 0 {
			paymentLag = 0
		}
	}
	return []metrics.Sample{
		{Name: "jxe_order_expiry_pending", Help: "Expired unpaid orders awaiting closure.", Type: "gauge", Value: float64(pending)},
		{Name: "jxe_order_expiry_lag_seconds", Help: "Age of the oldest expired unpaid order.", Type: "gauge", Value: lag},
		{Name: "jxe_payment_reconcile_active", Help: "Creating or pending payments awaiting provider reconciliation.", Type: "gauge", Value: float64(paymentPending)},
		{Name: "jxe_payment_reconcile_oldest_due_seconds", Help: "Age past schedule for the oldest active payment reconciliation.", Type: "gauge", Value: paymentLag},
		{Name: "jxe_payment_reconcile_last_success_unixtime", Help: "Unix timestamp of the last successful payment reconciliation worker cycle.", Type: "gauge", Value: float64(w.lastReconcileSuccess.Load())},
	}
}
