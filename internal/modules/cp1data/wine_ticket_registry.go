package cp1data

import (
	"context"
	"time"

	"gorm.io/gorm"
)

type wineTicketPaymentBackfillRow struct {
	ID      uint64
	OrderID uint64
}

type wineTicketRefundBackfillRow struct {
	ID          uint64
	AfterSaleID uint64
}

type wineTicketReturnBackfillRow struct {
	ID          uint64
	AfterSaleID *uint64
	Status      string
	ClosedAt    *time.Time
}

// runWineTicketPayments 回填 EXPAND 阶段引入的可空注册字段。
// 它与 Goose 有意分离，以便在旧进程排空期间限速、续跑和重复执行。
func (b *Backfiller) runWineTicketPayments(ctx context.Context, report *BackfillReport, fingerprint string) error {
	for {
		batchStarted := time.Now()
		var rows []wineTicketPaymentBackfillRow
		err := b.db.WithContext(ctx).Table("payments").
			Select("id,order_id").
			Where("id>? AND id<=? AND (biz_type IS NULL OR biz_id IS NULL)", report.LastID, report.Range.Max).
			Order("id").Limit(b.options.BatchSize).Scan(&rows).Error
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		report.Progress.Scanned += int64(len(rows))
		report.Progress.Planned += int64(len(rows))
		report.LastID = rows[len(rows)-1].ID
		if b.options.Execute {
			var updated int64
			if err := b.retryTransaction(ctx, func(tx *gorm.DB) error {
				updated = 0
				for _, row := range rows {
					result := tx.Table("payments").Where(
						"id=? AND order_id=? AND (biz_type IS NULL OR biz_id IS NULL)",
						row.ID, row.OrderID,
					).Updates(map[string]any{"biz_type": "retail_order", "biz_id": row.OrderID})
					if result.Error != nil {
						return result.Error
					}
					updated += result.RowsAffected
				}
				return nil
			}); err != nil {
				return err
			}
			report.Progress.Updated += updated
		}
		if err := b.afterBatch(ctx, fingerprint, report, batchStarted, len(rows)); err != nil {
			return err
		}
	}
}

func (b *Backfiller) runWineTicketRefunds(ctx context.Context, report *BackfillReport, fingerprint string) error {
	for {
		batchStarted := time.Now()
		var rows []wineTicketRefundBackfillRow
		err := b.db.WithContext(ctx).Table("refunds").
			Select("id,after_sale_id").
			Where("id>? AND id<=? AND (biz_type IS NULL OR biz_id IS NULL)", report.LastID, report.Range.Max).
			Order("id").Limit(b.options.BatchSize).Scan(&rows).Error
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		report.Progress.Scanned += int64(len(rows))
		report.Progress.Planned += int64(len(rows))
		report.LastID = rows[len(rows)-1].ID
		if b.options.Execute {
			var updated int64
			if err := b.retryTransaction(ctx, func(tx *gorm.DB) error {
				updated = 0
				for _, row := range rows {
					result := tx.Table("refunds").Where(
						"id=? AND after_sale_id=? AND (biz_type IS NULL OR biz_id IS NULL)",
						row.ID, row.AfterSaleID,
					).Updates(map[string]any{"biz_type": "retail_after_sale", "biz_id": row.AfterSaleID})
					if result.Error != nil {
						return result.Error
					}
					updated += result.RowsAffected
				}
				return nil
			}); err != nil {
				return err
			}
			report.Progress.Updated += updated
		}
		if err := b.afterBatch(ctx, fingerprint, report, batchStarted, len(rows)); err != nil {
			return err
		}
	}
}

func (b *Backfiller) runWineTicketReturns(ctx context.Context, report *BackfillReport, fingerprint string) error {
	for {
		batchStarted := time.Now()
		var rows []wineTicketReturnBackfillRow
		err := b.db.WithContext(ctx).Table("delivery_returns").
			Select("id,after_sale_id,status,closed_at").
			Where("id>? AND id<=? AND (settlement_type IS NULL OR settlement_status IS NULL)", report.LastID, report.Range.Max).
			Order("id").Limit(b.options.BatchSize).Scan(&rows).Error
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		report.Progress.Scanned += int64(len(rows))
		report.Progress.Planned += int64(len(rows))
		report.LastID = rows[len(rows)-1].ID
		if b.options.Execute {
			var updated int64
			if err := b.retryTransaction(ctx, func(tx *gorm.DB) error {
				updated = 0
				for _, row := range rows {
					status := returnSettlementStatus(row)
					values := map[string]any{
						"settlement_type":   "retail_cash_refund",
						"settlement_biz_id": row.AfterSaleID,
						"settlement_status": status,
						"settled_at":        nil,
					}
					if status == "succeeded" {
						values["settled_at"] = row.ClosedAt
					}
					result := tx.Table("delivery_returns").Where(
						"id=? AND (settlement_type IS NULL OR settlement_status IS NULL)", row.ID,
					).Updates(values)
					if result.Error != nil {
						return result.Error
					}
					updated += result.RowsAffected
				}
				return nil
			}); err != nil {
				return err
			}
			report.Progress.Updated += updated
		}
		if err := b.afterBatch(ctx, fingerprint, report, batchStarted, len(rows)); err != nil {
			return err
		}
	}
}

func returnSettlementStatus(row wineTicketReturnBackfillRow) string {
	if row.Status == "closed" {
		return "succeeded"
	}
	if row.AfterSaleID == nil {
		return "not_started"
	}
	if row.Status == "disputed" || row.Status == "exception" {
		return "exception"
	}
	return "processing"
}
