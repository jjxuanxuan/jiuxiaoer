package refund

import (
	"database/sql"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
)

// RegisterMetrics 注册指标。
func RegisterMetrics(db *gorm.DB, registry *metrics.Registry) {
	if db == nil || registry == nil {
		return
	}
	registry.AddCollector(func() []metrics.Sample { return collectMetrics(db) })
}

// collectMetrics 收集指标。
func collectMetrics(db *gorm.DB) []metrics.Sample {
	type statusCount struct {
		Status string
		Count  int64
	}
	var counts []statusCount
	_ = db.Table("refunds").Select("status, COUNT(*) count").Where("deleted_at IS NULL").Group("status").Scan(&counts).Error
	samples := make([]metrics.Sample, 0, len(counts)+4)
	for _, row := range counts {
		samples = append(samples, metrics.Sample{Name: "jxe_refund_tasks", Help: "Refund tasks by current status.", Type: "gauge", Labels: map[string]string{"status": row.Status}, Value: float64(row.Count)})
	}
	now := time.Now()
	samples = append(samples,
		metrics.Sample{Name: "jxe_refund_oldest_pending_seconds", Help: "Age of the oldest creating or pending refund.", Type: "gauge", Value: oldestAge(db, "refunds", "status IN ('creating','pending') AND deleted_at IS NULL", "requested_at", now)},
		metrics.Sample{Name: "jxe_after_sale_review_oldest_seconds", Help: "Age of the oldest after-sale awaiting review.", Type: "gauge", Value: oldestAge(db, "after_sales", "status IN ('submitted','shop_reviewing','platform_reviewing') AND deleted_at IS NULL", "submitted_at", now)},
	)
	var failedCallbacks int64
	_ = db.Table("refund_callbacks").Where("process_status='failed' AND received_at>=?", now.Add(-5*time.Minute)).Count(&failedCallbacks).Error
	samples = append(samples, metrics.Sample{Name: "jxe_refund_callback_failures_5m", Help: "Failed refund callbacks received in the last five minutes.", Type: "gauge", Value: float64(failedCallbacks)})
	return samples
}

// oldestAge 返回最早记录 Age。
func oldestAge(db *gorm.DB, table, condition, column string, now time.Time) float64 {
	var oldest sql.NullTime
	if db.Table(table).Select("MIN("+column+")").Where(condition).Scan(&oldest).Error != nil || !oldest.Valid {
		return 0
	}
	age := now.Sub(oldest.Time).Seconds()
	if age < 0 {
		return 0
	}
	return age
}
