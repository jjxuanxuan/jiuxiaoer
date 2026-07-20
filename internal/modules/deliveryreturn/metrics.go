package deliveryreturn

import (
	"database/sql"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
)

// RegisterMetrics exposes low-cardinality backlog, branch-divergence, and
// accounting-invariant gauges. Scrape failures never affect business traffic.
func RegisterMetrics(db *gorm.DB, registry *metrics.Registry) {
	if db == nil || registry == nil {
		return
	}
	registry.AddCollector(func() []metrics.Sample { return collectMetrics(db, time.Now().UTC()) })
}

func collectMetrics(db *gorm.DB, now time.Time) []metrics.Sample {
	type statusCount struct {
		Status string
		Count  int64
	}
	var statuses []statusCount
	_ = db.Table("delivery_returns").Select("status,COUNT(*) count").Group("status").Scan(&statuses).Error
	samples := make([]metrics.Sample, 0, len(statuses)+8)
	for _, row := range statuses {
		samples = append(samples, metrics.Sample{Name: "jxe_delivery_return_tasks", Help: "Delivery returns by orchestration status.", Type: "gauge",
			Labels: map[string]string{"status": row.Status}, Value: float64(row.Count)})
	}
	samples = append(samples,
		metrics.Sample{Name: "jxe_delivery_return_oldest_pending_receipt_seconds", Help: "Age since approval of the oldest return still awaiting physical receipt.", Type: "gauge", Value: oldestReturnAge(db, now)},
		metrics.Sample{Name: "jxe_delivery_return_sla_breached", Help: "Approved delivery returns past receipt deadline without a receipt.", Type: "gauge", Value: countMetric(db, `SELECT COUNT(*) FROM delivery_returns dr
			LEFT JOIN return_receipts rr ON rr.after_sale_id=dr.after_sale_id
			WHERE dr.status IN ('returning','arrived','exception') AND dr.receipt_deadline_at IS NOT NULL
			  AND dr.receipt_deadline_at<=? AND rr.id IS NULL`, now)},
		metrics.Sample{Name: "jxe_delivery_return_refund_succeeded_unreceived", Help: "Returns whose refund succeeded while physical receipt is still absent.", Type: "gauge", Value: countMetric(db, `SELECT COUNT(*) FROM delivery_returns dr
			JOIN refunds r ON r.after_sale_id=dr.after_sale_id AND r.deleted_at IS NULL AND r.status='succeeded'
			LEFT JOIN return_receipts rr ON rr.after_sale_id=dr.after_sale_id
			WHERE rr.id IS NULL AND dr.status NOT IN ('closed','cancelled')`)},
		metrics.Sample{Name: "jxe_delivery_return_received_refund_problem", Help: "Physically received returns whose refund is failed or exceptional.", Type: "gauge", Value: countMetric(db, `SELECT COUNT(*) FROM delivery_returns dr
			JOIN return_receipts rr ON rr.after_sale_id=dr.after_sale_id
			JOIN refunds r ON r.after_sale_id=dr.after_sale_id AND r.deleted_at IS NULL
			WHERE r.status IN ('failed','exception') AND dr.status<>'closed'`)},
		metrics.Sample{Name: "jxe_delivery_return_inconsistent_closed", Help: "Closed returns missing a succeeded refund, receipt item, or required restock ledger fact.", Type: "gauge", Value: countMetric(db, `SELECT COUNT(*) FROM delivery_returns dr
			WHERE dr.status='closed' AND (
			  NOT EXISTS (SELECT 1 FROM refunds r WHERE r.after_sale_id=dr.after_sale_id AND r.status='succeeded' AND r.deleted_at IS NULL)
			  OR NOT EXISTS (SELECT 1 FROM return_receipts rr JOIN return_receipt_items rri ON rri.return_receipt_id=rr.id WHERE rr.after_sale_id=dr.after_sale_id)
			  OR EXISTS (SELECT 1 FROM return_receipts rr JOIN return_receipt_items rri ON rri.return_receipt_id=rr.id
			    WHERE rr.after_sale_id=dr.after_sale_id AND rri.received_quantity<>rri.expected_quantity)
			  OR EXISTS (SELECT 1 FROM return_receipts rr JOIN return_receipt_items rri ON rri.return_receipt_id=rr.id
			    WHERE rr.after_sale_id=dr.after_sale_id AND rri.disposition='restock'
			      AND NOT EXISTS (SELECT 1 FROM stock_records sr WHERE sr.business_action_key=CONCAT('delivery_return:',dr.id,':',rri.after_sale_item_id,':restock')))
			)`)},
		metrics.Sample{Name: "jxe_delivery_return_handoff_rejections_5m", Help: "Rejected handoff-code checks in the last five minutes.", Type: "gauge", Value: countMetric(db, `SELECT COUNT(*) FROM delivery_return_history WHERE action='handoff_rejected' AND created_at>=?`, now.Add(-5*time.Minute))},
		metrics.Sample{Name: "jxe_delivery_return_requested_customer_notifications", Help: "Customer notifications incorrectly created for unapproved return requests; must remain zero.", Type: "gauge", Value: countMetric(db, `SELECT
			(SELECT COUNT(*) FROM notification_deliveries WHERE event_type='delivery.return_requested' AND recipient_type='customer') +
			(SELECT COUNT(*) FROM message_inboxes WHERE type='delivery.return_requested')`)},
	)
	return samples
}

func oldestReturnAge(db *gorm.DB, now time.Time) float64 {
	var oldest sql.NullTime
	err := db.Table("delivery_returns dr").Select("MIN(dr.approved_at)").
		Joins("LEFT JOIN return_receipts rr ON rr.after_sale_id=dr.after_sale_id").
		Where("dr.status IN ? AND dr.approved_at IS NOT NULL AND rr.id IS NULL", []string{StatusReturning, StatusArrived, StatusException}).Scan(&oldest).Error
	if err != nil || !oldest.Valid {
		return 0
	}
	age := now.Sub(oldest.Time).Seconds()
	if age < 0 {
		return 0
	}
	return age
}

func countMetric(db *gorm.DB, query string, args ...any) float64 {
	var count int64
	if db.Raw(query, args...).Scan(&count).Error != nil {
		return 0
	}
	return float64(count)
}
