package asset

import (
	"database/sql"
	"gorm.io/gorm"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"time"
)

// RegisterMetrics 注册指标。
func RegisterMetrics(db *gorm.DB, registry *metrics.Registry) {
	if db == nil || registry == nil {
		return
	}
	registry.AddCollector(func() []metrics.Sample { return collectAssetMetrics(db) })
}

// collectAssetMetrics 收集资产指标。
func collectAssetMetrics(db *gorm.DB) []metrics.Sample {
	samples := []metrics.Sample{}
	var unbalanced, negative, mismatches, compensationMismatch, staleHolds, expiryBacklog int64
	_ = db.Raw("SELECT COUNT(*) FROM (SELECT t.id FROM asset_transactions t JOIN asset_entries e ON e.transaction_id=t.id GROUP BY t.id HAVING SUM(e.delta)<>0) x").Scan(&unbalanced).Error
	_ = db.Table("asset_balances b").Joins("JOIN asset_accounts a ON a.id=b.account_id AND a.owner_type='customer'").Where("b.amount<0").Count(&negative).Error
	_ = db.Raw("SELECT COUNT(*) FROM asset_balances b LEFT JOIN (SELECT account_id,bucket,SUM(delta) amount FROM asset_entries GROUP BY account_id,bucket) e ON e.account_id=b.account_id AND e.bucket=b.bucket WHERE b.amount<>COALESCE(e.amount,0)").Scan(&mismatches).Error
	_ = db.Raw("SELECT COUNT(*) FROM compensation_ledger c LEFT JOIN asset_transactions t ON t.id=c.asset_transaction_id WHERE (c.status='issued' AND (t.id IS NULL OR t.amount<>c.amount OR t.asset_type<>'balance')) OR (c.status<>'issued' AND t.id IS NOT NULL)").Scan(&compensationMismatch).Error
	now := time.Now().UTC()
	_ = db.Table("asset_holds").Where("status IN ('active','partially_committed') AND expires_at IS NOT NULL AND expires_at<=?", now).Count(&staleHolds).Error
	_ = db.Table("asset_lots").Where("available_amount>0 AND expires_at IS NOT NULL AND expires_at<=?", now).Count(&expiryBacklog).Error
	samples = append(samples, metrics.Sample{Name: "jxe_asset_unbalanced_transactions", Help: "Posted asset transactions whose entries do not sum to zero.", Type: "gauge", Value: float64(unbalanced)}, metrics.Sample{Name: "jxe_asset_negative_customer_balances", Help: "Customer asset balance buckets below zero.", Type: "gauge", Value: float64(negative)}, metrics.Sample{Name: "jxe_asset_projection_mismatches", Help: "Asset balance projections that differ from immutable entries.", Type: "gauge", Value: float64(mismatches)}, metrics.Sample{Name: "jxe_asset_compensation_mismatches", Help: "Compensation and asset transaction mismatches.", Type: "gauge", Value: float64(compensationMismatch)}, metrics.Sample{Name: "jxe_asset_holds_stale", Help: "Active asset holds past expiry.", Type: "gauge", Value: float64(staleHolds)}, metrics.Sample{Name: "jxe_asset_expiry_backlog", Help: "Expired asset lots awaiting processing.", Type: "gauge", Value: float64(expiryBacklog)}, metrics.Sample{Name: "jxe_compensation_issue_oldest_seconds", Help: "Age of the oldest approved or issuing compensation.", Type: "gauge", Value: oldestCompensation(db, now)})
	return samples
}

// oldestCompensation 返回最早记录 Compensation。
func oldestCompensation(db *gorm.DB, now time.Time) float64 {
	var oldest sql.NullTime
	if db.Table("compensation_ledger").Select("MIN(created_at)").Where("status IN ('approved','issuing')").Scan(&oldest).Error != nil || !oldest.Valid {
		return 0
	}
	age := now.Sub(oldest.Time).Seconds()
	if age < 0 {
		return 0
	}
	return age
}
