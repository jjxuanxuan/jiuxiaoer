package integrity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	reconciliationCheckpointRunning   = "running"
	reconciliationCheckpointCompleted = CheckpointStatusCompleted

	CheckpointStatusCompleted = "completed"

	defaultIntegrityDailyStart  = 5 * time.Minute
	defaultIntegrityLease       = 5 * time.Minute
	maxIntegrityCheckpointOwner = 128
)

// Checkpoint 是上海时区单个业务日周期内持久化的单写者游标。
// 扫描器始终读取当前不可变事实，CycleKey 则明确标识重试和运维证据。
type Checkpoint struct {
	CycleKey       string `gorm:"primaryKey;size:10"`
	Status         string
	Phase          IntegrityPhase
	LastID         uint64
	HighWatermarks datatypes.JSON
	CheckedRows    uint64
	DetectedRows   uint64
	LeaseOwner     *string
	LeaseUntil     *time.Time
	StartedAt      time.Time
	LastBatchAt    *time.Time
	CompletedAt    *time.Time
	Version        uint
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (Checkpoint) TableName() string {
	return "wine_ticket_reconciliation_checkpoints"
}

type reconciliationCheckpointClaim struct {
	CycleKey       string
	Cursor         IntegrityCursor
	HighWatermarks map[IntegrityPhase]uint64
	Owner          string
	Version        uint
}

type reconciliationClaimResult struct {
	Claim     *reconciliationCheckpointClaim
	Cursor    IntegrityCursor
	WaitUntil time.Time
}

func reconciliationDueCycle(
	now time.Time,
	dailyStart time.Duration,
) (string, time.Time) {
	local := now.In(shanghaiLocation)
	midnight := time.Date(
		local.Year(),
		local.Month(),
		local.Day(),
		0,
		0,
		0,
		0,
		shanghaiLocation,
	)
	start := midnight.Add(dailyStart)
	cycleDate := midnight.AddDate(0, 0, -1)
	nextStart := start.AddDate(0, 0, 1)
	if local.Before(start) {
		cycleDate = midnight.AddDate(0, 0, -2)
		nextStart = start
	}
	return cycleDate.Format("2006-01-02"), nextStart
}

func normalizeIntegrityOwner(owner string) string {
	runes := []rune(owner)
	if len(runes) <= maxIntegrityCheckpointOwner {
		return owner
	}
	digest := sha256.Sum256([]byte(owner))
	suffix := ":" + hex.EncodeToString(digest[:8])
	return string(runes[:maxIntegrityCheckpointOwner-len(suffix)]) + suffix
}

func (r *reconciliationRepository) claimCheckpoint(
	ctx context.Context,
	cycleKey string,
	owner string,
	now time.Time,
	leaseDuration time.Duration,
	nextCycleStart time.Time,
	initialCursor IntegrityCursor,
) (reconciliationClaimResult, error) {
	if r == nil || r.db == nil {
		return reconciliationClaimResult{}, fmt.Errorf(
			"wine-ticket reconciliation checkpoint store is unavailable",
		)
	}
	owner = normalizeIntegrityOwner(owner)
	if cycleKey == "" || owner == "" || leaseDuration <= 0 {
		return reconciliationClaimResult{}, fmt.Errorf(
			"invalid wine-ticket reconciliation checkpoint claim",
		)
	}
	now = now.In(shanghaiLocation).Truncate(time.Millisecond)
	leaseUntil := now.Add(leaseDuration)
	initialCursor, err := normalizeIntegrityCursor(initialCursor)
	if err != nil {
		return reconciliationClaimResult{}, err
	}
	var existing Checkpoint
	findErr := r.db.WithContext(ctx).
		Where("cycle_key = ?", cycleKey).
		Take(&existing).Error
	if errors.Is(findErr, gorm.ErrRecordNotFound) {
		highWatermarks, captureErr := r.captureHighWatermarks(ctx)
		if captureErr != nil {
			return reconciliationClaimResult{}, captureErr
		}
		rawHighWatermarks, marshalErr := json.Marshal(highWatermarks)
		if marshalErr != nil {
			return reconciliationClaimResult{}, marshalErr
		}
		seed := Checkpoint{
			CycleKey:       cycleKey,
			Status:         reconciliationCheckpointRunning,
			Phase:          initialCursor.Phase,
			LastID:         initialCursor.LastID,
			HighWatermarks: datatypes.JSON(rawHighWatermarks),
			StartedAt:      now,
			Version:        1,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if err := r.db.WithContext(ctx).
			Clauses(clause.OnConflict{DoNothing: true}).
			Create(&seed).Error; err != nil {
			return reconciliationClaimResult{}, err
		}
	} else if findErr != nil {
		return reconciliationClaimResult{}, findErr
	}

	var claimed reconciliationClaimResult
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row Checkpoint
		if err := tx.WithContext(ctx).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("cycle_key = ?", cycleKey).
			Take(&row).Error; err != nil {
			return err
		}
		cursor, err := normalizeIntegrityCursor(
			IntegrityCursor{Phase: row.Phase, LastID: row.LastID},
		)
		if err != nil {
			return fmt.Errorf(
				"invalid persisted wine-ticket reconciliation cursor: %w",
				err,
			)
		}
		highWatermarks := make(map[IntegrityPhase]uint64)
		if err := json.Unmarshal(row.HighWatermarks, &highWatermarks); err != nil {
			return fmt.Errorf(
				"invalid wine-ticket reconciliation high watermarks: %w",
				err,
			)
		}
		upperID, exists := highWatermarks[cursor.Phase]
		if !exists {
			return fmt.Errorf(
				"wine-ticket reconciliation high watermark missing for %s",
				cursor.Phase,
			)
		}
		cursor.UpperID = &upperID
		claimed.Cursor = cursor
		if row.Status == reconciliationCheckpointCompleted {
			claimed.WaitUntil = nextCycleStart
			return nil
		}
		if row.LeaseOwner != nil &&
			*row.LeaseOwner != owner &&
			row.LeaseUntil != nil &&
			row.LeaseUntil.After(now) {
			claimed.WaitUntil = *row.LeaseUntil
			return nil
		}

		nextVersion := row.Version + 1
		result := tx.WithContext(ctx).Model(&Checkpoint{}).
			Where("cycle_key = ? AND version = ?", cycleKey, row.Version).
			Updates(map[string]any{
				"status":      reconciliationCheckpointRunning,
				"lease_owner": owner,
				"lease_until": leaseUntil,
				"version":     nextVersion,
				"updated_at":  now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf(
				"wine-ticket reconciliation checkpoint %s changed concurrently",
				cycleKey,
			)
		}
		claimed.Claim = &reconciliationCheckpointClaim{
			CycleKey:       cycleKey,
			Cursor:         cursor,
			HighWatermarks: highWatermarks,
			Owner:          owner,
			Version:        nextVersion,
		}
		return nil
	})
	return claimed, err
}

func (r *reconciliationRepository) captureHighWatermarks(
	ctx context.Context,
) (map[IntegrityPhase]uint64, error) {
	tables := map[IntegrityPhase]string{
		IntegrityPhasePayments:    "payments",
		IntegrityPhasePurchases:   "wine_ticket_purchases",
		IntegrityPhaseLots:        "wine_ticket_lots",
		IntegrityPhaseRedemptions: "wine_ticket_redemptions",
		IntegrityPhaseGifts:       "wine_ticket_gifts",
		IntegrityPhaseRenewals:    "wine_ticket_renewals",
		IntegrityPhaseRefunds:     "wine_ticket_refunds",
		IntegrityPhaseSlots:       "delivery_time_slots",
		IntegrityPhaseReminders:   "wine_ticket_reminders",
	}
	result := make(map[IntegrityPhase]uint64, len(tables))
	for _, phase := range reconciliationPhases {
		var highID uint64
		if err := r.db.WithContext(ctx).Table(tables[phase]).
			Select("COALESCE(MAX(id), 0)").
			Scan(&highID).Error; err != nil {
			return nil, fmt.Errorf(
				"capture wine-ticket reconciliation %s high watermark: %w",
				phase,
				err,
			)
		}
		result[phase] = highID
	}
	return result, nil
}

func (r *reconciliationRepository) persistClaimedBatch(
	ctx context.Context,
	claim reconciliationCheckpointClaim,
	result IntegrityBatchResult,
	rows []reconciliationDiscrepancy,
	detectedAt time.Time,
) error {
	if r == nil || r.db == nil {
		return fmt.Errorf(
			"wine-ticket reconciliation checkpoint store is unavailable",
		)
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var checkpoint Checkpoint
		err := tx.WithContext(ctx).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("cycle_key = ?", claim.CycleKey).
			Take(&checkpoint).Error
		if err != nil {
			return err
		}
		if checkpoint.Status != reconciliationCheckpointRunning ||
			checkpoint.LeaseOwner == nil ||
			*checkpoint.LeaseOwner != claim.Owner ||
			checkpoint.Version != claim.Version {
			return fmt.Errorf(
				"wine-ticket reconciliation checkpoint %s lease was lost",
				claim.CycleKey,
			)
		}
		if err := r.persistDiscrepanciesWithTx(
			ctx,
			tx,
			rows,
			detectedAt,
		); err != nil {
			return err
		}

		status := reconciliationCheckpointRunning
		var completedAt any
		if result.CycleCompleted {
			status = reconciliationCheckpointCompleted
			completedAt = detectedAt
		}
		update := map[string]any{
			"status":        status,
			"phase":         result.NextCursor.Phase,
			"last_id":       result.NextCursor.LastID,
			"checked_rows":  gorm.Expr("checked_rows + ?", result.Checked),
			"detected_rows": gorm.Expr("detected_rows + ?", result.Detected),
			"lease_owner":   nil,
			"lease_until":   nil,
			"last_batch_at": detectedAt,
			"completed_at":  completedAt,
			"version":       gorm.Expr("version + 1"),
			"updated_at":    detectedAt,
		}
		updated := tx.WithContext(ctx).Model(&Checkpoint{}).
			Where(
				"cycle_key = ? AND version = ? AND lease_owner = ?",
				claim.CycleKey,
				claim.Version,
				claim.Owner,
			).
			Updates(update)
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return fmt.Errorf(
				"wine-ticket reconciliation checkpoint %s advance was lost",
				claim.CycleKey,
			)
		}
		return nil
	})
}

func (r *reconciliationRepository) releaseCheckpoint(
	ctx context.Context,
	claim reconciliationCheckpointClaim,
	now time.Time,
) error {
	if r == nil || r.db == nil {
		return nil
	}
	result := r.db.WithContext(ctx).Model(&Checkpoint{}).
		Where(
			"cycle_key = ? AND version = ? AND lease_owner = ?",
			claim.CycleKey,
			claim.Version,
			claim.Owner,
		).
		Updates(map[string]any{
			"lease_owner": nil,
			"lease_until": nil,
			"version":     gorm.Expr("version + 1"),
			"updated_at":  now.In(shanghaiLocation).Truncate(time.Millisecond),
		})
	if result.Error != nil && !errors.Is(result.Error, context.Canceled) {
		return result.Error
	}
	return nil
}
