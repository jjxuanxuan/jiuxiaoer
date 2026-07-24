package cp1metrics

import (
	"database/sql"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
)

// Register 注册cp 1 指标。
func Register(db *gorm.DB, registry *metrics.Registry) {
	if db == nil || registry == nil {
		return
	}
	registry.AddCollector(func() []metrics.Sample { return collect(db) })
}

type statusCount struct {
	Status   string
	Provider string
	Channel  string
	Count    int64
}

// collect 收集Sample列表。
func collect(db *gorm.DB) []metrics.Sample {
	samples := []metrics.Sample{}
	samples = appendStatusBy(samples, db, "print_tasks", "jxe_print_tasks_total", "Print tasks by current status.", "provider")
	samples = appendStatusBy(samples, db, "notification_deliveries", "jxe_notification_deliveries_total", "Notification deliveries by current status.", "channel")
	samples = appendStatus(samples, db, "provisioning_operations", "jxe_provisioning_operations_total", "Provisioning operations by current status.")
	samples = appendStatusBy(samples, db, "identity_verification_requests", "jxe_identity_verification_total", "Identity verification requests by current status.", "provider")
	now := time.Now()
	samples = append(samples,
		metrics.Sample{Name: "jxe_print_oldest_pending_seconds", Help: "Age of oldest pending print task.", Type: "gauge", Value: oldest(db, "print_tasks", "status IN ('pending','retry_wait','processing')", "created_at", now)},
		metrics.Sample{Name: "jxe_notification_oldest_pending_seconds", Help: "Age of oldest pending notification delivery.", Type: "gauge", Value: oldest(db, "notification_deliveries", "status IN ('pending','retry_wait','processing')", "created_at", now)},
	)
	var attempts []struct {
		Stage, Result string
		Count         int64
	}
	_ = db.Table("delivery_verification_attempts").Select("stage,result,COUNT(*) count").Group("stage,result").Scan(&attempts).Error
	for _, r := range attempts {
		samples = append(samples, metrics.Sample{Name: "jxe_delivery_verification_total", Help: "Delivery verification attempts by stage and result.", Type: "gauge", Labels: map[string]string{"stage": r.Stage, "result": r.Result}, Value: float64(r.Count)})
	}
	var overrides []struct {
		Action string
		Count  int64
	}
	_ = db.Table("admin_override_approvals").Select("action,COUNT(*) count").Where("status='approved'").Group("action").Scan(&overrides).Error
	for _, r := range overrides {
		samples = append(samples, metrics.Sample{Name: "jxe_admin_override_total", Help: "Approved admin override operations.", Type: "gauge", Labels: map[string]string{"action": r.Action}, Value: float64(r.Count)})
	}
	return samples
}

// appendStatusBy 按维度追加状态指标。
func appendStatusBy(samples []metrics.Sample, db *gorm.DB, table, name, help, dimension string) []metrics.Sample {
	var rows []statusCount
	_ = db.Table(table).Select("status," + dimension + ",COUNT(*) count").Group("status," + dimension).Scan(&rows).Error
	for _, r := range rows {
		value := r.Provider
		if dimension == "channel" {
			value = r.Channel
		}
		samples = append(samples, metrics.Sample{Name: name, Help: help, Type: "gauge", Labels: map[string]string{"status": r.Status, dimension: value}, Value: float64(r.Count)})
	}
	return samples
}

// appendStatus 追加状态。
func appendStatus(samples []metrics.Sample, db *gorm.DB, table, name, help string) []metrics.Sample {
	var rows []statusCount
	_ = db.Table(table).Select("status,COUNT(*) count").Group("status").Scan(&rows).Error
	for _, r := range rows {
		samples = append(samples, metrics.Sample{Name: name, Help: help, Type: "gauge", Labels: map[string]string{"status": r.Status}, Value: float64(r.Count)})
	}
	return samples
}

// oldest 返回最早记录。
func oldest(db *gorm.DB, table, where, column string, now time.Time) float64 {
	var value sql.NullTime
	if db.Table(table).Select("MIN("+column+")").Where(where).Scan(&value).Error != nil || !value.Valid {
		return 0
	}
	age := now.Sub(value.Time).Seconds()
	if age < 0 {
		return 0
	}
	return age
}
