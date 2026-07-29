package integrity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

var reconciliationActiveExceptionStatuses = []string{
	ExceptionStatusInvestigating,
	ExceptionStatusAwaitingExternalFact,
	ExceptionStatusPendingReview,
}

type reconciliationRepository struct {
	db  *gorm.DB
	ids *snowflake.Generator
}

func newIntegrityRepository(
	db *gorm.DB,
	ids *snowflake.Generator,
) *reconciliationRepository {
	return &reconciliationRepository{db: db, ids: ids}
}

// persistDiscrepancies 是扫描器唯一的写入边界。
// 它不持有批次、分配、订单、配送时段或库存的模型及表句柄。
func (r *reconciliationRepository) persistDiscrepancies(
	ctx context.Context,
	rows []reconciliationDiscrepancy,
	detectedAt time.Time,
) error {
	if len(rows) == 0 {
		return nil
	}
	if r.db == nil || r.ids == nil {
		return fmt.Errorf("wine-ticket reconciliation exception store is unavailable")
	}

	write := func(tx *gorm.DB) error {
		return r.persistDiscrepanciesWithTx(ctx, tx, rows, detectedAt)
	}
	err := r.db.WithContext(ctx).Transaction(write)
	if err == nil {
		return nil
	}

	// 两个扫描器可能同时观察到有效记录不存在。
	// 生成的 active_exception_key 保证只有一次插入成功。
	// 重试整个有界集合会把该竞争转为正常的加锁更新路径；
	// 无关失败在重试时仍会按失败处理。
	retryErr := r.db.WithContext(ctx).Transaction(write)
	if retryErr != nil {
		return errors.Join(err, retryErr)
	}
	return nil
}

func (r *reconciliationRepository) persistDiscrepanciesWithTx(
	ctx context.Context,
	tx *gorm.DB,
	rows []reconciliationDiscrepancy,
	detectedAt time.Time,
) error {
	for _, row := range rows {
		if err := r.upsertActiveException(ctx, tx, row, detectedAt); err != nil {
			return err
		}
	}
	return nil
}

func (r *reconciliationRepository) upsertActiveException(
	ctx context.Context,
	tx *gorm.DB,
	fact reconciliationDiscrepancy,
	detectedAt time.Time,
) error {
	if fact.BizID == 0 || fact.BizType == "" ||
		fact.Rule == "" || fact.Kind == "" {
		return fmt.Errorf("invalid wine-ticket reconciliation discrepancy")
	}
	expected, err := reconciliationSnapshot(fact.Expected)
	if err != nil {
		return err
	}
	actual, err := reconciliationSnapshot(fact.Actual)
	if err != nil {
		return err
	}
	severity := fact.Severity
	if severity == "" {
		severity = "P1"
	}
	exceptionType := fact.exceptionType()

	var existing Exception
	err = tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"exception_type = ? AND biz_type = ? AND biz_id = ? AND status IN ?",
			exceptionType,
			fact.BizType,
			fact.BizID,
			reconciliationActiveExceptionStatuses,
		).
		Order("id").
		Take(&existing).Error
	if err == nil {
		result := tx.WithContext(ctx).Model(&Exception{}).
			Where(
				"id = ? AND version = ? AND status IN ?",
				existing.ID,
				existing.Version,
				reconciliationActiveExceptionStatuses,
			).
			Updates(map[string]any{
				"biz_no":             fact.BizNo,
				"issuer_merchant_id": fact.IssuerMerchantID,
				"source_type":        "wine_ticket_reconciliation",
				"source_id":          fact.BizID,
				"correlation_id":     fact.Rule,
				"severity":           severity,
				"expected_snapshot":  expected,
				"actual_snapshot":    actual,
				"occurrence_count":   gorm.Expr("occurrence_count + 1"),
				"last_detected_at":   detectedAt,
				"version":            gorm.Expr("version + 1"),
				"updated_at":         detectedAt,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf(
				"wine-ticket active exception %d changed concurrently",
				existing.ID,
			)
		}
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}

	id := r.ids.Next()
	sourceID := fact.BizID
	correlationID := fact.Rule
	return tx.WithContext(ctx).Create(&Exception{
		ID:                 id,
		ExceptionNo:        "WTEX" + idString(id),
		ExceptionType:      exceptionType,
		BizType:            fact.BizType,
		BizID:              fact.BizID,
		BizNo:              fact.BizNo,
		IssuerMerchantID:   fact.IssuerMerchantID,
		SourceType:         "wine_ticket_reconciliation",
		SourceID:           &sourceID,
		CorrelationID:      &correlationID,
		Severity:           severity,
		Status:             ExceptionStatusInvestigating,
		ExpectedSnapshot:   expected,
		ActualSnapshot:     actual,
		OccurrenceCount:    1,
		FirstDetectedAt:    detectedAt,
		LastDetectedAt:     detectedAt,
		Version:            1,
		CreatedAt:          detectedAt,
		UpdatedAt:          detectedAt,
		ResolutionResult:   datatypes.JSON([]byte(`null`)),
		ActiveExceptionKey: nil,
	}).Error
}

func reconciliationSnapshot(value any) (datatypes.JSON, error) {
	if value == nil {
		return datatypes.JSON([]byte(`null`)), nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal reconciliation snapshot: %w", err)
	}
	return datatypes.JSON(raw), nil
}
