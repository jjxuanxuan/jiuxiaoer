package printjob

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/securevalue"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type Worker struct {
	cfg      config.CP1Config
	db       *gorm.DB
	ids      *snowflake.Generator
	provider Provider
	owner    string
	log      *slog.Logger
}

// NewWorker 创建并初始化工作器。
func NewWorker(cfg config.CP1Config, db *gorm.DB, ids *snowflake.Generator, provider Provider, owner string, log *slog.Logger) *Worker {
	return &Worker{cfg: cfg, db: db, ids: ids, provider: provider, owner: owner, log: log}
}

// Run 运行当前实例的核心处理流程。
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.WorkerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runBatch(ctx)
		}
	}
}

// RunOnce 运行Once处理流程。
// RunOnce processes one bounded batch for tests, smoke checks, and manual
// recovery commands without starting a long-lived loop.
func (w *Worker) RunOnce(ctx context.Context) { w.runBatch(ctx) }

// RunTask 运行任务处理流程。
// RunTask immediately claims a specific MQ-woken task. If another worker has
// already claimed or completed it, the operation is a safe no-op; the regular
// DB worker remains the compensation path.
func (w *Worker) RunTask(ctx context.Context, taskID uint64) error {
	task, setting, ok, err := w.claimTask(ctx, taskID)
	if err != nil || !ok {
		return err
	}
	w.execute(ctx, task, setting)
	return nil
}

// runBatch 运行批次处理流程。
func (w *Worker) runBatch(ctx context.Context) {
	for i := 0; i < w.cfg.WorkerBatchSize; i++ {
		task, setting, ok, e := w.claim(ctx)
		if e != nil {
			w.log.Error("claim print task", slog.Any("error", e))
			return
		}
		if !ok {
			return
		}
		w.execute(ctx, task, setting)
	}
}

// claim 认领任务。
func (w *Worker) claim(ctx context.Context) (Task, Setting, bool, error) {
	return w.claimTask(ctx, 0)
}

// claimTask 认领任务。
func (w *Worker) claimTask(ctx context.Context, taskID uint64) (Task, Setting, bool, error) {
	var task Task
	var setting Setting
	err := w.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		until := now.Add(30 * time.Second)
		claimToken := w.owner + ":" + uuid.NewString()
		query := `
			UPDATE print_tasks SET status='processing', locked_by=?, locked_until=?
			WHERE ((status IN ('pending','retry_wait') AND (next_retry_at IS NULL OR next_retry_at<=?))
			    OR (status='processing' AND locked_until<?))
			  AND (locked_until IS NULL OR locked_until<?)
		`
		args := []any{claimToken, until, now, now, now}
		if taskID != 0 {
			query += " AND id=?"
			args = append(args, taskID)
		}
		query += " ORDER BY id LIMIT 1"
		result := tx.Exec(query, args...)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
		if e := tx.Where("status='processing' AND locked_by=?", claimToken).First(&task).Error; e != nil {
			return e
		}
		return tx.Where("shop_id = ?", task.ShopID).First(&setting).Error
	})
	if err != nil {
		return Task{}, Setting{}, false, err
	}
	return task, setting, task.ID != 0, nil
}

// execute 处理execute相关逻辑。
func (w *Worker) execute(ctx context.Context, task Task, setting Setting) {
	started := time.Now()
	device, e := securevalue.Open(w.cfg.DataEncryptionKey, setting.DeviceIDCiphertext)
	if e != nil {
		w.finish(ctx, task, started, "submit", PrintResult{}, &ProviderError{Code: "device_decrypt_failed", Retryable: false})
		return
	}
	providerID := fmt.Sprintf("print-%s", task.TaskNo)
	result, e := w.provider.Submit(ctx, PrintRequest{TaskNo: task.TaskNo, ProviderRequestID: providerID, DeviceID: device, Copies: setting.Copies, Payload: task.RenderPayload})
	var pe *ProviderError
	if errors.As(e, &pe) && pe.Unknown {
		w.recordAttempt(ctx, task, started, "submit", result, e)
		started = time.Now()
		result, e = w.provider.Query(ctx, providerID)
		w.finish(ctx, task, started, "query", result, e)
		return
	}
	w.finish(ctx, task, started, "submit", result, e)
}

// finish 完成printjob。
func (w *Worker) finish(ctx context.Context, task Task, started time.Time, operation string, result PrintResult, callErr error) {
	_ = w.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		attempt := task.Attempts + 1
		status := "succeeded"
		var code *string
		var safe *string
		var next *time.Time
		providerStatus := "succeeded"
		if callErr == nil && result.Status != "" && result.Status != "succeeded" {
			callErr = &ProviderError{Code: "provider_result_unknown", Retryable: true, Unknown: true}
		}
		if callErr != nil {
			c := "provider_failure"
			retryable := true
			var pe *ProviderError
			if errors.As(callErr, &pe) {
				c = pe.Code
				retryable = pe.Retryable
			}
			code = &c
			s := "print provider request failed"
			safe = &s
			providerStatus = "failed"
			if retryable && attempt < 5 {
				status = "retry_wait"
				n := now.Add(backoff(attempt))
				next = &n
			} else {
				status = "dead"
			}
		}
		providerID := result.ProviderRequestID
		if providerID == "" {
			providerID = fmt.Sprintf("print-%s", task.TaskNo)
		}
		updates := map[string]any{"status": status, "attempts": attempt, "provider_request_id": providerID, "next_retry_at": next, "locked_by": nil, "locked_until": nil, "last_error_code": code, "last_error_safe": safe}
		if status == "succeeded" {
			updates["succeeded_at"] = &now
		}
		if e := tx.Model(&Task{}).Where("id=? AND locked_by=?", task.ID, taskLeaseOwner(task, w.owner)).Updates(updates).Error; e != nil {
			return e
		}
		return tx.Create(&Attempt{ID: w.ids.Next(), PrintTaskID: task.ID, AttemptNo: attempt, Operation: operation, ProviderRequestID: &providerID, RequestHash: securevalue.Digest(string(task.RenderPayload)), Result: status, ProviderStatus: &providerStatus, ErrorCode: code, DurationMS: uint(time.Since(started).Milliseconds()), StartedAt: started, FinishedAt: now}).Error
	})
}

// recordAttempt 处理记录尝试相关逻辑。
func (w *Worker) recordAttempt(ctx context.Context, task Task, started time.Time, operation string, result PrintResult, callErr error) {
	now := time.Now()
	providerID := result.ProviderRequestID
	if providerID == "" {
		providerID = fmt.Sprintf("print-%s", task.TaskNo)
	}
	providerStatus := result.Status
	if providerStatus == "" {
		providerStatus = "unknown"
	}
	var code *string
	if callErr != nil {
		value := "provider_failure"
		var pe *ProviderError
		if errors.As(callErr, &pe) {
			value = pe.Code
		}
		code = &value
	}
	_ = w.db.WithContext(ctx).Create(&Attempt{ID: w.ids.Next(), PrintTaskID: task.ID, AttemptNo: task.Attempts + 1, Operation: operation, ProviderRequestID: &providerID, RequestHash: securevalue.Digest(string(task.RenderPayload)), Result: "unknown", ProviderStatus: &providerStatus, ErrorCode: code, DurationMS: uint(time.Since(started).Milliseconds()), StartedAt: started, FinishedAt: now}).Error
}

// taskLeaseOwner 返回任务租约 Owner。
func taskLeaseOwner(task Task, fallback string) string {
	if task.LockedBy != nil && *task.LockedBy != "" {
		return *task.LockedBy
	}
	return fallback
}

// backoff 返回backoff。
func backoff(attempt uint) time.Duration {
	values := []time.Duration{10 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute}
	if attempt == 0 {
		return values[0]
	}
	if int(attempt) > len(values) {
		return values[len(values)-1]
	}
	return values[attempt-1]
}
