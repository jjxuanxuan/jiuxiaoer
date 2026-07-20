package reconciliation

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
)

type Worker struct {
	cfg                 config.Config
	service             *Service
	log                 *slog.Logger
	lastSuccess         atomic.Int64
	consecutiveFailures atomic.Uint64
}

func NewWorker(cfg config.Config, service *Service, registry *metrics.Registry, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	worker := &Worker{cfg: cfg, service: service, log: log}
	if registry != nil {
		registry.AddCollector(worker.collectMetrics)
	}
	return worker
}

func (w *Worker) Run(ctx context.Context) {
	w.runDue(ctx, time.Now())
	ticker := time.NewTicker(w.cfg.Reconciliation.WorkerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			w.runDue(ctx, now)
		}
	}
}

// RunDue executes both official T+1 bill types once the configured China-local
// run hour is reached. It is exported for deterministic operational tests.
func (w *Worker) RunDue(ctx context.Context, now time.Time) []error {
	return w.runDue(ctx, now)
}

func (w *Worker) runDue(ctx context.Context, now time.Time) []error {
	startDate, endDate, due, err := w.billWindow(now, true)
	if err != nil {
		w.consecutiveFailures.Add(1)
		return []error{err}
	}
	if !due {
		return nil
	}
	billDates, err := w.missingBillDates(ctx, startDate, endDate, w.cfg.Reconciliation.BackfillDaysPerCycle)
	if err != nil {
		w.consecutiveFailures.Add(1)
		w.log.Error("find missing WeChat bill dates", "error", err)
		return []error{err}
	}
	errs := make([]error, 0, len(billDates)*2)
	for _, billDate := range billDates {
		for _, billType := range []string{BillTypeTradeAll, BillTypeFundflowBase} {
			requestCtx, cancel := context.WithTimeout(ctx, w.cfg.Reconciliation.RequestTimeout)
			result, runErr := w.service.RunBill(requestCtx, billDate, billType)
			cancel()
			if runErr != nil {
				errs = append(errs, runErr)
				w.log.Error("WeChat bill reconciliation failed", "bill_date", billDate.Format("2006-01-02"), "bill_type", billType, "error", runErr)
				continue
			}
			w.log.Info("WeChat bill reconciliation cycle result", "run_id", result.RunID, "bill_date", result.BillDate, "bill_type", result.BillType, "status", result.Status, "rows", result.Rows, "discrepancies", result.Discrepancies, "already_completed", result.AlreadyCompleted)
		}
	}
	if len(errs) == 0 {
		w.consecutiveFailures.Store(0)
		w.lastSuccess.Store(now.Unix())
	} else {
		w.consecutiveFailures.Add(1)
	}
	return errs
}

func (w *Worker) billWindow(now time.Time, requireRunHour bool) (time.Time, time.Time, bool, error) {
	localNow := now.In(chinaLocation())
	if requireRunHour && localNow.Hour() < w.cfg.Reconciliation.RunHour {
		return time.Time{}, time.Time{}, false, nil
	}
	latestDownloadable := normalizeBillDate(localNow.AddDate(0, 0, -1))
	if !requireRunHour && localNow.Hour() < w.cfg.Reconciliation.RunHour {
		latestDownloadable = latestDownloadable.AddDate(0, 0, -1)
	}
	endDate := latestDownloadable.AddDate(0, 0, -(w.cfg.Reconciliation.LagDays - 1))
	startDate := endDate
	if raw := w.cfg.Reconciliation.StartDate; raw != "" {
		parsed, err := time.ParseInLocation("2006-01-02", raw, chinaLocation())
		if err != nil {
			return time.Time{}, time.Time{}, false, fmt.Errorf("parse reconciliation start date: %w", err)
		}
		startDate = normalizeBillDate(parsed)
	}
	oldestDownloadable := latestDownloadable.AddDate(0, 0, -89)
	if startDate.Before(oldestDownloadable) {
		startDate = oldestDownloadable
	}
	if startDate.After(endDate) {
		return startDate, endDate, false, nil
	}
	return startDate, endDate, true, nil
}

func (w *Worker) missingBillDates(ctx context.Context, startDate, endDate time.Time, limit int) ([]time.Time, error) {
	type completedRun struct {
		BillDate time.Time
		BillType string
	}
	var completed []completedRun
	err := w.service.repo.db.WithContext(ctx).Model(&Run{}).
		Select("bill_date,bill_type").
		Where("bill_date>=? AND bill_date<=? AND status IN ?", startDate, endDate, []string{"succeeded", "no_statement"}).
		Find(&completed).Error
	if err != nil {
		return nil, err
	}
	maskByDate := make(map[string]uint8, len(completed))
	for _, row := range completed {
		key := normalizeBillDate(row.BillDate).Format("2006-01-02")
		switch row.BillType {
		case BillTypeTradeAll:
			maskByDate[key] |= 1
		case BillTypeFundflowBase:
			maskByDate[key] |= 2
		}
	}
	missing := make([]time.Time, 0)
	for date := startDate; !date.After(endDate); date = date.AddDate(0, 0, 1) {
		if maskByDate[date.Format("2006-01-02")] == 3 {
			continue
		}
		missing = append(missing, date)
		if limit > 0 && len(missing) >= limit {
			break
		}
	}
	return missing, nil
}

func (w *Worker) collectMetrics() []metrics.Sample {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	db := w.service.repo.db.WithContext(ctx)
	var open int64
	var openByType []struct {
		DiscrepancyType string
		Count           int64
	}
	var oldest sql.NullTime
	var lastCompleted sql.NullTime
	_ = db.Model(&Discrepancy{}).Where("status='open'").Count(&open).Error
	_ = db.Model(&Discrepancy{}).Select("discrepancy_type, COUNT(*) AS count").Where("status='open'").Group("discrepancy_type").Scan(&openByType).Error
	_ = db.Model(&Discrepancy{}).Where("status='open'").Select("MIN(created_at)").Scan(&oldest).Error
	_ = db.Model(&Run{}).Where("status IN ?", []string{"succeeded", "no_statement"}).Select("MAX(completed_at)").Scan(&lastCompleted).Error
	oldestSeconds := float64(0)
	if oldest.Valid {
		oldestSeconds = time.Since(oldest.Time).Seconds()
		if oldestSeconds < 0 {
			oldestSeconds = 0
		}
	}
	lastCompletedUnix := float64(0)
	if lastCompleted.Valid {
		lastCompletedUnix = float64(lastCompleted.Time.Unix())
	}
	missingDates := []time.Time(nil)
	if startDate, endDate, due, err := w.billWindow(time.Now(), false); err == nil && due {
		missingDates, _ = w.missingBillDates(ctx, startDate, endDate, 90)
	}
	oldestMissingUnix := float64(0)
	if len(missingDates) > 0 {
		oldestMissingUnix = float64(missingDates[0].Unix())
	}
	samples := []metrics.Sample{
		{Name: "jxe_wechat_bill_reconciliation_last_success_unixtime", Help: "Unix timestamp of the last successful complete reconciliation worker cycle.", Type: "gauge", Value: float64(w.lastSuccess.Load())},
		{Name: "jxe_wechat_bill_reconciliation_last_completed_bill_unixtime", Help: "Unix timestamp of the latest successfully completed bill run.", Type: "gauge", Value: lastCompletedUnix},
		{Name: "jxe_wechat_bill_reconciliation_consecutive_failures", Help: "Consecutive failed reconciliation cycles.", Type: "gauge", Value: float64(w.consecutiveFailures.Load())},
		{Name: "jxe_wechat_bill_reconciliation_missing_dates", Help: "Bill dates missing a completed trade or fund-flow reconciliation.", Type: "gauge", Value: float64(len(missingDates))},
		{Name: "jxe_wechat_bill_reconciliation_oldest_missing_bill_unixtime", Help: "Unix timestamp of the oldest bill date with an incomplete reconciliation pair.", Type: "gauge", Value: oldestMissingUnix},
		{Name: "jxe_wechat_bill_discrepancies_open", Help: "Open WeChat bill discrepancies requiring review.", Type: "gauge", Value: float64(open)},
		{Name: "jxe_wechat_bill_discrepancy_oldest_open_seconds", Help: "Age of the oldest open WeChat bill discrepancy.", Type: "gauge", Value: oldestSeconds},
	}
	for _, row := range openByType {
		samples = append(samples, metrics.Sample{Name: "jxe_wechat_bill_discrepancies_open_by_type", Help: "Open WeChat bill discrepancies by type.", Type: "gauge", Labels: map[string]string{"type": row.DiscrepancyType}, Value: float64(row.Count)})
	}
	return samples
}
