package ops

import (
	"database/sql"
	"strings"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/integrity"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
)

// RegisterMetrics 暴露从事实推导出的酒票健康信号。
// 采集器被限定为只读：不变量问题仍归完整性扫描器负责，
// 指标采集期间绝不会执行“修复”。
func RegisterMetrics(db *gorm.DB, registry *metrics.Registry) {
	if db == nil || registry == nil {
		return
	}
	registry.AddCollector(func() []metrics.Sample {
		return collectWineTicketMetrics(db, time.Now())
	})
}

func collectWineTicketMetrics(db *gorm.DB, now time.Time) []metrics.Sample {
	samples := make([]metrics.Sample, 0, 24)

	appendCount := func(
		name, help string,
		labels map[string]string,
		query string,
		args ...any,
	) {
		var count int64
		if err := db.Raw(query, args...).Scan(&count).Error; err != nil {
			return
		}
		samples = append(samples, metrics.Sample{
			Name: name, Help: help, Type: "gauge",
			Labels: labels, Value: float64(count),
		})
	}

	appendCount(
		"jxe_wine_ticket_issue_total",
		"Durable wine-ticket purchase issuance facts by result.",
		map[string]string{"result": "succeeded"},
		`SELECT COUNT(DISTINCT biz_id)
		   FROM wine_ticket_transactions
		  WHERE transaction_type = 'purchase_issue'
		    AND biz_type = 'purchase'`,
	)
	appendCount(
		"jxe_wine_ticket_issue_total",
		"Durable wine-ticket purchase issuance facts by result.",
		map[string]string{"result": "compensating_refund"},
		`SELECT COUNT(*)
		   FROM wine_ticket_refunds
		  WHERE refund_kind = 'issuance_compensation'`,
	)

	type groupedCount struct {
		Label string
		Count int64
	}
	appendGrouped := func(
		name, help, labelName, query string,
		args ...any,
	) {
		var rows []groupedCount
		if err := db.Raw(query, args...).Scan(&rows).Error; err != nil {
			return
		}
		for _, row := range rows {
			samples = append(samples, metrics.Sample{
				Name: name, Help: help, Type: "gauge",
				Labels: map[string]string{labelName: row.Label},
				Value:  float64(row.Count),
			})
		}
	}

	appendGrouped(
		"jxe_wine_ticket_redemption_total",
		"Wine-ticket redemptions by durable result state.",
		"result",
		`SELECT status AS label, COUNT(*) AS count
		   FROM wine_ticket_redemptions
		  GROUP BY status`,
	)
	appendGrouped(
		"jxe_wine_ticket_gift_claim_total",
		"Wine-ticket gifts by durable claim result state.",
		"result",
		`SELECT status AS label, COUNT(*) AS count
		   FROM wine_ticket_gifts
		  GROUP BY status`,
	)
	appendGrouped(
		"jxe_wine_ticket_reconcile_diff_total",
		"Active wine-ticket reconciliation differences by stable rule.",
		"type",
		`SELECT COALESCE(correlation_id, exception_type) AS label,
		        COUNT(*) AS count
		   FROM wine_ticket_exceptions
		  WHERE source_type = 'wine_ticket_reconciliation'
		    AND status IN ('investigating','awaiting_external_fact','pending_review')
		  GROUP BY COALESCE(correlation_id, exception_type)`,
	)

	appendCount(
		"jxe_wine_ticket_lot_invariant_violation_total",
		"Active lot replay or allocation-view invariant violations.",
		nil,
		`SELECT COUNT(*)
		   FROM wine_ticket_exceptions
		  WHERE source_type = 'wine_ticket_reconciliation'
		    AND correlation_id IN ('REC-WT-003','REC-WT-003A')
		    AND status IN ('investigating','awaiting_external_fact','pending_review')`,
	)

	type oldestByLabel struct {
		Label  string
		Oldest sql.NullString
	}
	appendOldestAge := func(
		name, help, labelName, query string,
		args ...any,
	) {
		var rows []oldestByLabel
		if err := db.Raw(query, args...).Scan(&rows).Error; err != nil {
			return
		}
		for _, row := range rows {
			samples = append(samples, metrics.Sample{
				Name: name, Help: help, Type: "gauge",
				Labels: map[string]string{labelName: strings.TrimSpace(row.Label)},
				Value:  wineTicketAgeSecondsText(now, row.Oldest),
			})
		}
	}

	appendOldestAge(
		"jxe_wine_ticket_settlement_lag_seconds",
		"Age of the oldest provider-confirmed wine-ticket payment awaiting local settlement.",
		"biz_type",
		`SELECT biz_type AS label,
		        MIN(COALESCE(paid_at, updated_at)) AS oldest
		   FROM payments
		  WHERE biz_type IN ('wine_ticket_purchase','wine_ticket_renewal')
		    AND provider_status = 'SUCCESS'
		    AND status <> 'succeeded'
		    AND deleted_at IS NULL
		  GROUP BY biz_type`,
	)
	appendOldestAge(
		"jxe_wine_ticket_reminder_lag_seconds",
		"Age past schedule of the oldest pending wine-ticket reminder.",
		"channel",
		`SELECT channel AS label, MIN(scheduled_at) AS oldest
		   FROM wine_ticket_reminders
		  WHERE status = 'pending' AND scheduled_at <= ?
		  GROUP BY channel`,
		now,
	)
	appendOldestAge(
		"jxe_wine_ticket_refund_hold_age_seconds",
		"Age of the oldest wine-ticket refund still holding entitlement.",
		"provider_status",
		`SELECT COALESCE(common_refund.provider_status, common_refund.status, business.status) AS label,
		        MIN(business.requested_at) AS oldest
		   FROM wine_ticket_refunds business
		   LEFT JOIN refunds common_refund
		     ON common_refund.id = business.current_refund_id
		    AND common_refund.deleted_at IS NULL
		  WHERE business.status IN (
		        'holding','submitting','processing','submission_unknown',
		        'retry_pending','exception'
		  )
		  GROUP BY COALESCE(common_refund.provider_status, common_refund.status, business.status)`,
	)

	var oldestGuard struct {
		Oldest sql.NullString
	}
	if err := db.Raw(
		`SELECT MIN(created_at) AS oldest
		   FROM wine_ticket_renewals
		  WHERE status IN (
		        'pending_payment','payment_unknown','applying',
		        'compensating_refund','refund_exception'
		  )`,
	).Scan(&oldestGuard).Error; err == nil {
		samples = append(samples, metrics.Sample{
			Name: "jxe_wine_ticket_renewal_guard_age_seconds",
			Help: "Age of the oldest active renewal guard.",
			Type: "gauge", Value: wineTicketAgeSecondsText(now, oldestGuard.Oldest),
		})
	}

	localNow := now.In(shanghaiLocation)
	deadline := time.Date(
		localNow.Year(),
		localNow.Month(),
		localNow.Day(),
		6,
		0,
		0,
		0,
		shanghaiLocation,
	)
	deadlineMissed := float64(0)
	cycleKey := deadline.AddDate(0, 0, -1).Format("2006-01-02")
	var completedByDeadline int64
	if err := db.Model(&integrity.Checkpoint{}).
		Where(
			"cycle_key = ? AND status = ? AND completed_at <= ?",
			cycleKey,
			integrity.CheckpointStatusCompleted,
			deadline,
		).
		Count(&completedByDeadline).Error; err == nil {
		if !localNow.Before(deadline) && completedByDeadline == 0 {
			deadlineMissed = 1
		}
		samples = append(samples, metrics.Sample{
			Name: "jxe_wine_ticket_reconciliation_deadline_missed",
			Help: "Whether the prior Shanghai business-day reconciliation missed 06:00.",
			Type: "gauge", Value: deadlineMissed,
		})
	}

	return samples
}

func wineTicketAgeSeconds(now time.Time, oldest *time.Time) float64 {
	if oldest == nil || oldest.IsZero() || oldest.After(now) {
		return 0
	}
	return now.Sub(*oldest).Seconds()
}

func wineTicketAgeSecondsText(now time.Time, oldest sql.NullString) float64 {
	if !oldest.Valid || strings.TrimSpace(oldest.String) == "" {
		return 0
	}
	value := strings.TrimSpace(oldest.String)
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		parsed, err := time.ParseInLocation(layout, value, now.Location())
		if err == nil {
			return wineTicketAgeSeconds(now, &parsed)
		}
	}
	return 0
}
