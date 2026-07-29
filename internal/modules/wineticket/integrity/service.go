package integrity

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

const (
	defaultIntegrityBatchSize = 250
	maxIntegrityBatchSize     = 2000
)

// IntegrityService 执行时点只读事实扫描，随后只写入有效异常观测。
// 它绝不会对业务台账执行平衡性更新。
type IntegrityService struct {
	db       *gorm.DB
	repo     *reconciliationRepository
	snapshot reconciliationSnapshotRunner
	now      func() time.Time
}

type reconciliationSnapshotRunner func(context.Context, *gorm.DB, func(*gorm.DB) error) error

func mysqlIntegritySnapshot(
	ctx context.Context,
	db *gorm.DB,
	read func(*gorm.DB) error,
) error {
	return db.WithContext(ctx).Transaction(
		read,
		&sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true},
	)
}

func NewIntegrityService(
	db *gorm.DB,
	ids *snowflake.Generator,
) *IntegrityService {
	return &IntegrityService{
		db:       db,
		repo:     newIntegrityRepository(db, ids),
		snapshot: mysqlIntegritySnapshot,
		now:      time.Now,
	}
}

func (s *IntegrityService) ScanBatch(
	ctx context.Context,
	cursor IntegrityCursor,
	batchSize int,
) (IntegrityBatchResult, error) {
	result, discrepancies, detectedAt, err := s.scanBatchFacts(
		ctx,
		cursor,
		batchSize,
	)
	if err != nil {
		return IntegrityBatchResult{}, err
	}
	if err := s.repo.persistDiscrepancies(
		ctx,
		discrepancies,
		detectedAt,
	); err != nil {
		return IntegrityBatchResult{}, err
	}
	return result, nil
}

// scanBatchFacts 读取一个有界、可重复的快照，且不执行任何写入。
// 定时任务会在同一个事务中持久化观测结果和检查点，
// 因此进程崩溃不会只推进批次中的一侧。
func (s *IntegrityService) scanBatchFacts(
	ctx context.Context,
	cursor IntegrityCursor,
	batchSize int,
) (
	IntegrityBatchResult,
	[]reconciliationDiscrepancy,
	time.Time,
	error,
) {
	cursor, err := normalizeIntegrityCursor(cursor)
	if err != nil {
		return IntegrityBatchResult{}, nil, time.Time{}, err
	}
	if s == nil || s.db == nil || s.repo == nil || s.snapshot == nil {
		return IntegrityBatchResult{}, nil, time.Time{}, fmt.Errorf(
			"wine-ticket reconciliation service is unavailable",
		)
	}
	if batchSize == 0 {
		batchSize = defaultIntegrityBatchSize
	}
	if batchSize < 1 || batchSize > maxIntegrityBatchSize {
		return IntegrityBatchResult{}, nil, time.Time{}, fmt.Errorf(
			"wine-ticket reconciliation batch size must be between 1 and %d",
			maxIntegrityBatchSize,
		)
	}

	var (
		checked       int
		lastID        = cursor.LastID
		discrepancies []reconciliationDiscrepancy
	)
	read := func(tx *gorm.DB) error {
		var scanErr error
		switch cursor.Phase {
		case IntegrityPhasePayments:
			checked, lastID, discrepancies, scanErr =
				s.scanPayments(ctx, tx, cursor.LastID, cursor.UpperID, batchSize)
		case IntegrityPhasePurchases:
			checked, lastID, discrepancies, scanErr =
				s.scanPurchases(ctx, tx, cursor.LastID, cursor.UpperID, batchSize)
		case IntegrityPhaseLots:
			checked, lastID, discrepancies, scanErr =
				s.scanLots(ctx, tx, cursor.LastID, cursor.UpperID, batchSize)
		case IntegrityPhaseRedemptions:
			checked, lastID, discrepancies, scanErr =
				s.scanRedemptions(ctx, tx, cursor.LastID, cursor.UpperID, batchSize)
		case IntegrityPhaseGifts:
			checked, lastID, discrepancies, scanErr =
				s.scanGifts(ctx, tx, cursor.LastID, cursor.UpperID, batchSize)
		case IntegrityPhaseRenewals:
			checked, lastID, discrepancies, scanErr =
				s.scanRenewals(ctx, tx, cursor.LastID, cursor.UpperID, batchSize)
		case IntegrityPhaseRefunds:
			checked, lastID, discrepancies, scanErr =
				s.scanRefunds(ctx, tx, cursor.LastID, cursor.UpperID, batchSize)
		case IntegrityPhaseSlots:
			checked, lastID, discrepancies, scanErr =
				s.scanSlots(ctx, tx, cursor.LastID, cursor.UpperID, batchSize)
		case IntegrityPhaseReminders:
			checked, lastID, discrepancies, scanErr =
				s.scanReminders(ctx, tx, cursor.LastID, cursor.UpperID, batchSize)
		default:
			scanErr = fmt.Errorf(
				"unsupported wine-ticket reconciliation phase %q",
				cursor.Phase,
			)
		}
		return scanErr
	}
	err = s.snapshot(ctx, s.db, read)
	if err != nil {
		return IntegrityBatchResult{}, nil, time.Time{}, err
	}

	discrepancies = deduplicateIntegrityDiscrepancies(discrepancies)
	detectedAt := s.now().In(shanghaiLocation).Truncate(time.Millisecond)

	phaseCompleted := checked < batchSize ||
		(cursor.UpperID != nil && lastID >= *cursor.UpperID)
	next, cycleCompleted := nextIntegrityCursor(
		cursor.Phase,
		lastID,
		phaseCompleted,
		cursor.UpperID,
	)
	return IntegrityBatchResult{
		Phase:          cursor.Phase,
		Checked:        checked,
		Detected:       len(discrepancies),
		NextCursor:     next,
		PhaseCompleted: phaseCompleted,
		CycleCompleted: cycleCompleted,
	}, discrepancies, detectedAt, nil
}

func (r *reconciliationRepository) idWindow(
	query *gorm.DB,
	column string,
	afterID uint64,
	upperID *uint64,
) *gorm.DB {
	query = query.Where(column+" > ?", afterID)
	if upperID != nil {
		query = query.Where(column+" <= ?", *upperID)
	}
	return query
}

func deduplicateIntegrityDiscrepancies(
	rows []reconciliationDiscrepancy,
) []reconciliationDiscrepancy {
	if len(rows) < 2 {
		return rows
	}
	result := make([]reconciliationDiscrepancy, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		key := row.key()
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, row)
	}
	return result
}
