package integrity

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const (
	defaultIntegrityBatchInterval = 100 * time.Millisecond
	defaultIntegritySweepInterval = 15 * time.Minute
	defaultIntegrityErrorBackoff  = 5 * time.Second
)

// IntegrityWorker 持续推进一个有界主键游标。
// 游标及其租约会持久化，因此进程重启或存在多个维护副本时，
// 会恢复同一个每日周期，而不是各自启动全量扫描。
type IntegrityWorker struct {
	service *IntegrityService
	log     *slog.Logger

	mu            sync.Mutex
	owner         string
	cursor        IntegrityCursor
	batchSize     int
	batchInterval time.Duration
	sweepInterval time.Duration
	errorBackoff  time.Duration
	dailyStart    time.Duration
	leaseDuration time.Duration
}

func NewIntegrityWorker(
	service *IntegrityService,
	log *slog.Logger,
) *IntegrityWorker {
	if log == nil {
		log = slog.Default()
	}
	owner := "wine-ticket-reconciliation"
	if service != nil && service.repo != nil && service.repo.ids != nil {
		owner += ":" + idString(service.repo.ids.Next())
	}
	return &IntegrityWorker{
		service: service,
		log:     log,
		owner:   owner,
		cursor: IntegrityCursor{
			Phase: IntegrityPhasePayments,
		},
		batchSize:     defaultIntegrityBatchSize,
		batchInterval: defaultIntegrityBatchInterval,
		sweepInterval: defaultIntegritySweepInterval,
		errorBackoff:  defaultIntegrityErrorBackoff,
		dailyStart:    defaultIntegrityDailyStart,
		leaseDuration: defaultIntegrityLease,
	}
}

// ConfigureBounds 提供不影响正确性的运维调参能力。
// 无效值会被忽略，ScanBatch 仍保留自身的硬性上限。
func (w *IntegrityWorker) ConfigureBounds(
	batchSize int,
	batchInterval time.Duration,
	sweepInterval time.Duration,
) *IntegrityWorker {
	w.mu.Lock()
	defer w.mu.Unlock()
	if batchSize > 0 && batchSize <= maxIntegrityBatchSize {
		w.batchSize = batchSize
	}
	if batchInterval > 0 {
		w.batchInterval = batchInterval
	}
	if sweepInterval > 0 {
		w.sweepInterval = sweepInterval
	}
	return w
}

// ConfigureSchedule 与批次调参有意分离。
// dailyStart 表示上海时区午夜后的时长，
// leaseDuration 限制单个批次的持有窗口。
func (w *IntegrityWorker) ConfigureSchedule(
	owner string,
	dailyStart time.Duration,
	leaseDuration time.Duration,
) *IntegrityWorker {
	w.mu.Lock()
	defer w.mu.Unlock()
	if owner != "" {
		w.owner = normalizeIntegrityOwner(owner + ":" + w.owner)
	}
	if dailyStart >= 0 && dailyStart < 24*time.Hour {
		w.dailyStart = dailyStart
	}
	if leaseDuration > 0 {
		w.leaseDuration = leaseDuration
	}
	return w
}

func (w *IntegrityWorker) WithOwner(owner string) *IntegrityWorker {
	w.mu.Lock()
	defer w.mu.Unlock()
	if owner != "" {
		w.owner = normalizeIntegrityOwner(owner + ":" + w.owner)
	}
	return w
}

func (w *IntegrityWorker) RunOnce(
	ctx context.Context,
) (IntegrityBatchResult, error) {
	if w == nil || w.service == nil {
		return IntegrityBatchResult{}, fmt.Errorf(
			"wine-ticket reconciliation worker is unavailable",
		)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.service.now().In(shanghaiLocation).Truncate(time.Millisecond)
	cycleKey, nextCycleStart := reconciliationDueCycle(now, w.dailyStart)
	claimed, err := w.service.repo.claimCheckpoint(
		ctx,
		cycleKey,
		w.owner,
		now,
		w.leaseDuration,
		nextCycleStart,
		w.cursor,
	)
	if err != nil {
		return IntegrityBatchResult{}, err
	}
	w.cursor = claimed.Cursor
	if claimed.Claim == nil {
		waitUntil := claimed.WaitUntil
		return IntegrityBatchResult{
			Phase:         claimed.Cursor.Phase,
			NextCursor:    claimed.Cursor,
			CycleKey:      cycleKey,
			LeaseAcquired: false,
			NextRunAt:     &waitUntil,
		}, nil
	}

	result, discrepancies, detectedAt, err := w.service.scanBatchFacts(
		ctx,
		claimed.Cursor,
		w.batchSize,
	)
	if err != nil {
		_ = w.service.repo.releaseCheckpoint(
			context.WithoutCancel(ctx),
			*claimed.Claim,
			now,
		)
		return IntegrityBatchResult{}, err
	}
	if !result.CycleCompleted {
		upperID, exists := claimed.Claim.HighWatermarks[result.NextCursor.Phase]
		if !exists {
			_ = w.service.repo.releaseCheckpoint(
				context.WithoutCancel(ctx),
				*claimed.Claim,
				now,
			)
			return IntegrityBatchResult{}, fmt.Errorf(
				"wine-ticket reconciliation high watermark missing for %s",
				result.NextCursor.Phase,
			)
		}
		result.NextCursor.UpperID = &upperID
	}
	if err := w.service.repo.persistClaimedBatch(
		ctx,
		*claimed.Claim,
		result,
		discrepancies,
		detectedAt,
	); err != nil {
		_ = w.service.repo.releaseCheckpoint(
			context.WithoutCancel(ctx),
			*claimed.Claim,
			now,
		)
		return IntegrityBatchResult{}, err
	}
	w.cursor = result.NextCursor
	result.CycleKey = cycleKey
	result.LeaseAcquired = true
	if result.CycleCompleted {
		result.NextRunAt = &nextCycleStart
	}
	return result, nil
}

func (w *IntegrityWorker) Cursor() IntegrityCursor {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cursor
}

func (w *IntegrityWorker) Run(ctx context.Context) {
	if w == nil || w.service == nil {
		return
	}
	for {
		result, err := w.RunOnce(ctx)
		w.mu.Lock()
		batchInterval := w.batchInterval
		sweepInterval := w.sweepInterval
		errorBackoff := w.errorBackoff
		w.mu.Unlock()
		waitFor := batchInterval
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.log.Error(
				"wine-ticket reconciliation batch failed",
				slog.String("error", err.Error()),
			)
			waitFor = errorBackoff
		} else {
			if result.LeaseAcquired && result.Detected > 0 {
				w.log.Warn(
					"wine-ticket reconciliation differences detected",
					slog.String("phase", string(result.Phase)),
					slog.Int("checked", result.Checked),
					slog.Int("detected", result.Detected),
				)
			}
			if result.NextRunAt != nil {
				waitFor = time.Until(*result.NextRunAt)
				if waitFor <= 0 {
					waitFor = batchInterval
				}
				if sweepInterval > 0 && waitFor > sweepInterval {
					waitFor = sweepInterval
				}
			}
		}
		timer := time.NewTimer(waitFor)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}
