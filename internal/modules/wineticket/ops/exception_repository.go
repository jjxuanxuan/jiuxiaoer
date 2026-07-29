package ops

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/integrity"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
)

func (r *Repository) ListAdminExceptions(
	ctx context.Context,
	query pagination.Query,
	filter ExceptionAdminFilter,
) ([]integrity.Exception, error) {
	db := r.db.WithContext(ctx).Model(&integrity.Exception{})
	if filter.Status != "" {
		db = db.Where("status = ?", filter.Status)
	}
	if filter.Severity != "" {
		db = db.Where("severity = ?", filter.Severity)
	}
	var err error
	db, err = pagination.ApplyIDCursor(db, query, "id", "desc")
	if err != nil {
		return nil, err
	}
	var rows []integrity.Exception
	err = db.Order("id DESC").Limit(query.PageSize + 1).Find(&rows).Error
	return rows, err
}

func (r *Repository) AdminExceptionByNo(
	ctx context.Context,
	tx *gorm.DB,
	exceptionNo string,
) (integrity.Exception, error) {
	db := tx
	if db == nil {
		db = r.db
	}
	var row integrity.Exception
	err := db.WithContext(ctx).
		Where("exception_no = ?", exceptionNo).
		Take(&row).Error
	return row, err
}

func (r *Repository) AdminExceptionByNoLocked(
	ctx context.Context,
	tx *gorm.DB,
	exceptionNo string,
) (integrity.Exception, error) {
	var row integrity.Exception
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("exception_no = ?", exceptionNo).
		Take(&row).Error
	return row, err
}

func (r *Repository) ResolveException(
	ctx context.Context,
	tx *gorm.DB,
	row integrity.Exception,
	action string,
	reason string,
	reviewTicketNo string,
	actorID uint64,
	resultData []byte,
	now time.Time,
) (bool, error) {
	result := tx.WithContext(ctx).Model(&integrity.Exception{}).
		Where(
			"id = ? AND version = ? AND status IN ?",
			row.ID,
			row.Version,
			[]string{
				ExceptionStatusInvestigating,
				ExceptionStatusAwaitingExternalFact,
				ExceptionStatusPendingReview,
			},
		).
		Updates(map[string]any{
			"status":            ExceptionStatusResolved,
			"proposed_action":   action,
			"proposed_reason":   reason,
			"review_ticket_no":  reviewTicketNo,
			"proposed_by":       actorID,
			"proposed_at":       now,
			"review_decision":   nil,
			"review_note":       nil,
			"reviewed_by":       nil,
			"reviewed_at":       nil,
			"resolution_result": resultData,
			"resolved_at":       now,
			"version":           row.Version + 1,
			"updated_at":        now,
		})
	return result.RowsAffected == 1, result.Error
}
