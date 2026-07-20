package reconciliation

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type repository struct{ db *gorm.DB }

var errDiscrepancyNotOpen = errors.New("discrepancy is not open")
var errRunLeaseLost = errors.New("bill reconciliation run lease was lost")

func newRepository(db *gorm.DB) *repository { return &repository{db: db} }

func (r *repository) acquireRun(ctx context.Context, id uint64, billDate time.Time, billType string, now time.Time, staleAfter time.Duration) (Run, bool, error) {
	var run Run
	acquired := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		candidate := Run{ID: id, BillDate: billDate, BillType: billType, Status: "pending", Version: 1}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&candidate).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("bill_date=? AND bill_type=?", billDate, billType).Take(&run).Error; err != nil {
			return err
		}
		if run.Status == "succeeded" || run.Status == "no_statement" {
			return nil
		}
		if run.Status == "running" && run.StartedAt != nil && run.StartedAt.After(now.Add(-staleAfter)) {
			return nil
		}
		if err := tx.Where("run_id=?", run.ID).Delete(&Observation{}).Error; err != nil {
			return err
		}
		if err := tx.Where("run_id=?", run.ID).Delete(&Discrepancy{}).Error; err != nil {
			return err
		}
		nextVersion := run.Version + 1
		if nextVersion == 0 {
			return errors.New("bill reconciliation run version overflow")
		}
		result := tx.Model(&Run{}).Where("id=? AND version=?", run.ID, run.Version).Updates(map[string]any{
			"status": "running", "started_at": now, "completed_at": nil,
			"hash_type": nil, "expected_hash": nil, "computed_hash": nil,
			"provider_request_id": nil, "download_request_id": nil,
			"row_count": 0, "discrepancy_count": 0, "stats_json": nil,
			"error_code": nil, "error_detail": nil, "version": nextVersion,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errRunLeaseLost
		}
		run.Status, run.StartedAt, run.Version = "running", &now, nextVersion
		acquired = true
		return nil
	})
	if err != nil {
		return Run{}, false, err
	}
	return run, acquired, nil
}

func (r *repository) markFailed(ctx context.Context, run Run, code, detail, providerRequestID, downloadRequestID string, now time.Time) error {
	if len(detail) > 512 {
		detail = detail[:512]
	}
	result := r.db.WithContext(ctx).Model(&Run{}).Where("id=? AND status='running' AND version=?", run.ID, run.Version).Updates(map[string]any{
		"status": "failed", "completed_at": now, "error_code": optionalString(code),
		"error_detail": optionalString(detail), "provider_request_id": optionalString(providerRequestID),
		"download_request_id": optionalString(downloadRequestID), "version": gorm.Expr("version+1"),
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errRunLeaseLost
	}
	return nil
}

func (r *repository) listRuns(ctx context.Context, limit int) ([]Run, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows []Run
	err := r.db.WithContext(ctx).Order("bill_date DESC, bill_type, id DESC").Limit(limit).Find(&rows).Error
	return rows, err
}

func (r *repository) listDiscrepancies(ctx context.Context, status string, limit int) ([]Discrepancy, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := r.db.WithContext(ctx)
	if status != "" {
		query = query.Where("status=?", status)
	}
	var rows []Discrepancy
	err := query.Order("bill_date DESC, id DESC").Limit(limit).Find(&rows).Error
	return rows, err
}

func (r *repository) resolveDiscrepancy(ctx context.Context, id, actorID uint64, note string, now time.Time) error {
	result := r.db.WithContext(ctx).Model(&Discrepancy{}).Where("id=? AND status='open'", id).Updates(map[string]any{
		"status": "resolved", "handling_note": note, "handled_by": actorID, "handled_at": now,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		var row Discrepancy
		if err := r.db.WithContext(ctx).Where("id=?", id).Take(&row).Error; err != nil {
			return err
		}
		if row.Status == "resolved" {
			return nil
		}
		return errDiscrepancyNotOpen
	}
	return nil
}

func optionalString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
