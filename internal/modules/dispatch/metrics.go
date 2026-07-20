package dispatch

import (
	"database/sql"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
)

// RegisterMetrics 注册指标。
// RegisterMetrics exposes low-cardinality dispatch health and invariant
// gauges. Query failures intentionally produce zero/partial samples because a
// metrics scrape must never make the API unavailable; dependency readiness is
// reported separately by the health module.
func RegisterMetrics(db *gorm.DB, registry *metrics.Registry) {
	if db == nil || registry == nil {
		return
	}
	registry.AddCollector(func() []metrics.Sample { return collectMetrics(db, time.Now()) })
}

// collectMetrics 收集指标。
func collectMetrics(db *gorm.DB, now time.Time) []metrics.Sample {
	type jobCount struct {
		Status string
		Mode   string
		Count  int64
	}
	var jobs []jobCount
	_ = db.Table("dispatch_jobs").Select("status,mode,COUNT(*) count").Group("status,mode").Scan(&jobs).Error
	samples := make([]metrics.Sample, 0, len(jobs)+7)
	for _, row := range jobs {
		samples = append(samples, metrics.Sample{
			Name: "jxe_dispatch_jobs", Help: "Dispatch jobs by current status and mode.", Type: "gauge",
			Labels: map[string]string{"status": row.Status, "mode": row.Mode}, Value: float64(row.Count),
		})
	}
	samples = append(samples,
		metrics.Sample{Name: "jxe_dispatch_oldest_overdue_seconds", Help: "Age past next_action_at of the oldest overdue actionable dispatch job.", Type: "gauge", Value: oldestOverdue(db, now)},
		metrics.Sample{Name: "jxe_dispatch_manual_required", Help: "Dispatch jobs currently requiring manual assignment.", Type: "gauge", Value: countWhere(db, "dispatch_jobs", "status='manual_required'", nil)},
		metrics.Sample{Name: "jxe_dispatch_expired_pending_offers", Help: "Pending dispatch offers whose server expiry has passed.", Type: "gauge", Value: countWhere(db, "dispatch_offers", "status='pending' AND expires_at<=?", []any{now})},
		metrics.Sample{Name: "jxe_dispatch_duplicate_active_assignments", Help: "Delivery orders with more than one active assignment; must remain zero.", Type: "gauge", Value: duplicateActiveAssignments(db)},
		metrics.Sample{Name: "jxe_dispatch_paid_orders_without_delivery", Help: "Paid orders missing a durable delivery order.", Type: "gauge", Value: paidOrdersWithoutDelivery(db)},
		metrics.Sample{Name: "jxe_dispatch_service_scope_mismatches", Help: "Differences between legacy rider service_scope JSON and normalized active service shops.", Type: "gauge", Value: serviceScopeMismatches(db)},
	)
	return samples
}

// serviceScopeMismatches 返回服务范围 Mismatches。
func serviceScopeMismatches(db *gorm.DB) float64 {
	var count int64
	err := db.Raw(`SELECT COUNT(*) FROM (
		SELECT r.id AS rider_id,CAST(legacy_scope.shop_id AS UNSIGNED) AS shop_id
		FROM riders r
		JOIN JSON_TABLE(
			COALESCE(r.service_scope,JSON_OBJECT('shop_ids',JSON_ARRAY())),
			'$.shop_ids[*]' COLUMNS(shop_id VARCHAR(32) PATH '$')
		) legacy_scope
		LEFT JOIN rider_service_shops rss
			ON rss.rider_id=r.id AND rss.shop_id=CAST(legacy_scope.shop_id AS UNSIGNED) AND rss.status='active'
		WHERE r.deleted_at IS NULL
			AND legacy_scope.shop_id REGEXP '^[1-9][0-9]{0,19}$'
			AND rss.id IS NULL
		UNION ALL
		SELECT rss.rider_id,rss.shop_id
		FROM rider_service_shops rss
		JOIN riders r ON r.id=rss.rider_id AND r.deleted_at IS NULL
		WHERE rss.status='active' AND NOT (
			JSON_CONTAINS(COALESCE(r.service_scope,JSON_OBJECT('shop_ids',JSON_ARRAY())),JSON_QUOTE(CAST(rss.shop_id AS CHAR)),'$.shop_ids')
			OR JSON_CONTAINS(COALESCE(r.service_scope,JSON_OBJECT('shop_ids',JSON_ARRAY())),JSON_ARRAY(rss.shop_id),'$.shop_ids')
		)
	) scope_diff`).Scan(&count).Error
	if err != nil {
		return 0
	}
	return float64(count)
}

// oldestOverdue 返回最早记录 Overdue。
func oldestOverdue(db *gorm.DB, now time.Time) float64 {
	var oldest sql.NullTime
	err := db.Table("dispatch_jobs").Select("MIN(next_action_at)").
		Where("status IN ? AND next_action_at<=?", []string{"pending", "scoring", "offering", "grab_open"}, now).
		Scan(&oldest).Error
	if err != nil || !oldest.Valid {
		return 0
	}
	age := now.Sub(oldest.Time).Seconds()
	if age < 0 {
		return 0
	}
	return age
}

// countWhere 统计Where的数量。
func countWhere(db *gorm.DB, table, condition string, args []any) float64 {
	var count int64
	query := db.Table(table)
	if condition != "" {
		query = query.Where(condition, args...)
	}
	if query.Count(&count).Error != nil {
		return 0
	}
	return float64(count)
}

// duplicateActiveAssignments 返回重复项启用状态 Assignments。
func duplicateActiveAssignments(db *gorm.DB) float64 {
	var count int64
	err := db.Raw(`SELECT COUNT(*) FROM (
		SELECT delivery_order_id FROM delivery_assignments
		WHERE status='active' GROUP BY delivery_order_id HAVING COUNT(*)>1
	) duplicate_assignments`).Scan(&count).Error
	if err != nil {
		return 0
	}
	return float64(count)
}

// paidOrdersWithoutDelivery 返回paid 订单 Without 配送。
func paidOrdersWithoutDelivery(db *gorm.DB) float64 {
	var count int64
	err := db.Table("orders o").Joins("LEFT JOIN delivery_orders d ON d.order_id=o.id AND d.deleted_at IS NULL").
		Where("o.pay_status='succeeded' AND o.status IN ? AND o.deleted_at IS NULL AND d.id IS NULL", []string{"paid", "accepted", "preparing", "ready_for_pickup", "delivering"}).Count(&count).Error
	if err != nil {
		return 0
	}
	return float64(count)
}
