package reminder

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/modules/notification"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
)

const allocationStatusHeld = "held"

// reminderWorkerRepository 不提供备用连接。
// 调用方必须传入数据库句柄，或拥有当前后台任务状态转换的准确事务。
type reminderWorkerRepository struct{}

func (r *reminderWorkerRepository) materializationCandidates(
	ctx context.Context,
	db *gorm.DB,
	now time.Time,
	batch int,
	includeWeChat bool,
) ([]core.Lot, error) {
	var rows []core.Lot
	query := db.WithContext(ctx).
		Where(
			`(
				status = ?
				OR (
					status = ?
					AND (
						EXISTS (
							SELECT 1
							FROM wine_ticket_redemption_allocations redemption_allocation
							WHERE redemption_allocation.lot_id = wine_ticket_lots.id
							  AND redemption_allocation.status = ?
						)
						OR EXISTS (
							SELECT 1
							FROM wine_ticket_gift_allocations gift_allocation
							WHERE gift_allocation.source_lot_id = wine_ticket_lots.id
							  AND gift_allocation.status = ?
						)
						OR EXISTS (
							SELECT 1
							FROM wine_ticket_refund_allocations refund_allocation
							WHERE refund_allocation.lot_id = wine_ticket_lots.id
							  AND refund_allocation.status = ?
						)
					)
				)
			)
			AND expires_at > ? AND expires_at <= ? AND expiry_changed_at <= ?`,
			core.LotStatusActive,
			core.LotStatusDepleted,
			allocationStatusHeld,
			allocationStatusHeld,
			allocationStatusHeld,
			now,
			now.Add(expiryReminderLeadTime),
			now,
		)
	missingInbox := `NOT EXISTS (
		SELECT 1 FROM wine_ticket_reminders reminder
		WHERE reminder.lot_id = wine_ticket_lots.id
		  AND reminder.expires_at = wine_ticket_lots.expires_at
		  AND reminder.remind_days = ?
		  AND reminder.channel = 'inbox'
	)`
	if includeWeChat {
		query = query.Where(`(`+missingInbox+` OR NOT EXISTS (
			SELECT 1 FROM wine_ticket_reminders reminder
			WHERE reminder.lot_id = wine_ticket_lots.id
			  AND reminder.expires_at = wine_ticket_lots.expires_at
			  AND reminder.remind_days = ?
			  AND reminder.channel = 'wechat_subscription'
		))`, expiryReminderDays, expiryReminderDays)
	} else {
		query = query.Where(missingInbox, expiryReminderDays)
	}
	err := query.
		Order("expires_at ASC, id ASC").
		Limit(batch).
		Find(&rows).Error
	return rows, err
}

func (r *reminderWorkerRepository) lockLot(
	ctx context.Context,
	tx *gorm.DB,
	lotID uint64,
) (core.Lot, bool, error) {
	var row core.Lot
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", lotID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return core.Lot{}, false, nil
	}
	return row, err == nil, err
}

func (r *reminderWorkerRepository) createReminderFact(
	ctx context.Context,
	tx *gorm.DB,
	row *Reminder,
) (bool, error) {
	result := tx.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "lot_id"},
			{Name: "expires_at"},
			{Name: "remind_days"},
			{Name: "channel"},
		},
		DoNothing: true,
	}).Create(row)
	return result.RowsAffected == 1, result.Error
}

func (r *reminderWorkerRepository) createInboxMessage(
	ctx context.Context,
	tx *gorm.DB,
	row *notification.Message,
) error {
	return tx.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(row).Error
}

func (r *reminderWorkerRepository) createOutbox(
	ctx context.Context,
	tx *gorm.DB,
	values map[string]any,
) error {
	return tx.WithContext(ctx).
		Table("outbox_events").
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "event_id"}},
			DoNothing: true,
		}).
		Create(values).Error
}

func (r *reminderWorkerRepository) dispatchCandidates(
	ctx context.Context,
	db *gorm.DB,
	now time.Time,
	batch int,
) ([]Reminder, error) {
	var rows []Reminder
	err := db.WithContext(ctx).
		Where(
			`channel = ? AND status = ? AND scheduled_at <= ?
			 AND (attempts = 0 OR locked_until IS NULL OR locked_until <= ?)`,
			"wechat_subscription",
			"pending",
			now,
			now,
		).
		Order("scheduled_at ASC, id ASC").
		Limit(batch).
		Find(&rows).Error
	return rows, err
}

func (r *reminderWorkerRepository) lockReminder(
	ctx context.Context,
	tx *gorm.DB,
	reminderID uint64,
) (Reminder, bool, error) {
	var row Reminder
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", reminderID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Reminder{}, false, nil
	}
	return row, err == nil, err
}

func (r *reminderWorkerRepository) updatePendingReminder(
	ctx context.Context,
	tx *gorm.DB,
	reminderID uint64,
	values map[string]any,
) error {
	return tx.WithContext(ctx).
		Model(&Reminder{}).
		Where("id = ? AND status = 'pending'", reminderID).
		Updates(values).Error
}

func (r *reminderWorkerRepository) updateSendingConsentsByReminder(
	ctx context.Context,
	tx *gorm.DB,
	reminderID uint64,
	values map[string]any,
) error {
	return tx.WithContext(ctx).
		Model(&NotificationSubscriptionConsent{}).
		Where("claimed_by_reminder_id = ? AND status = 'sending'", reminderID).
		Updates(values).Error
}

func (r *reminderWorkerRepository) productName(
	ctx context.Context,
	tx *gorm.DB,
	productID uint64,
) (string, bool, error) {
	var row struct {
		Name string
	}
	err := tx.WithContext(ctx).
		Table("products").
		Select("name").
		Where("id = ?", productID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	return row.Name, true, err
}

func (r *reminderWorkerRepository) activeMiniAppIdentity(
	ctx context.Context,
	tx *gorm.DB,
	customerID uint64,
	appID string,
) (string, int64, error) {
	var row struct {
		OpenID        string
		IdentityCount int64
	}
	err := tx.WithContext(ctx).
		Table("customer_identities").
		Select("COALESCE(MIN(provider_subject), '') AS open_id, COUNT(*) AS identity_count").
		Where(
			"customer_id = ? AND provider = 'wechat_miniapp' AND app_id = ? AND status = 'active' AND deleted_at IS NULL",
			customerID,
			appID,
		).
		Scan(&row).Error
	return row.OpenID, row.IdentityCount, err
}

func (r *reminderWorkerRepository) expireAvailableConsents(
	ctx context.Context,
	tx *gorm.DB,
	customerID uint64,
	scene string,
	now time.Time,
) error {
	return tx.WithContext(ctx).
		Model(&NotificationSubscriptionConsent{}).
		Where(
			"customer_id = ? AND scene = ? AND status = 'available' AND expires_at IS NOT NULL AND expires_at <= ?",
			customerID,
			scene,
			now,
		).
		Updates(map[string]any{"status": "expired", "updated_at": now}).Error
}

func (r *reminderWorkerRepository) lockAvailableConsent(
	ctx context.Context,
	tx *gorm.DB,
	customerID uint64,
	scene string,
	now time.Time,
) (NotificationSubscriptionConsent, bool, error) {
	var row NotificationSubscriptionConsent
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
		Where(
			"customer_id = ? AND scene = ? AND status = 'available' AND (expires_at IS NULL OR expires_at > ?)",
			customerID,
			scene,
			now,
		).
		Order("consented_at ASC, id ASC").
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return NotificationSubscriptionConsent{}, false, nil
	}
	return row, err == nil, err
}

func (r *reminderWorkerRepository) claimConsent(
	ctx context.Context,
	tx *gorm.DB,
	consentID uint64,
	reminderID uint64,
	now time.Time,
) (bool, error) {
	result := tx.WithContext(ctx).
		Model(&NotificationSubscriptionConsent{}).
		Where(
			"id = ? AND status = 'available' AND claimed_by_reminder_id IS NULL",
			consentID,
		).
		Updates(map[string]any{
			"status":                 "sending",
			"claimed_by_reminder_id": reminderID,
			"claimed_at":             now,
			"updated_at":             now,
		})
	return result.RowsAffected == 1, result.Error
}

func (r *reminderWorkerRepository) claimReminder(
	ctx context.Context,
	tx *gorm.DB,
	reminderID uint64,
	providerRequestID string,
	lockOwner string,
	lockedUntil time.Time,
	now time.Time,
) (bool, error) {
	result := tx.WithContext(ctx).
		Model(&Reminder{}).
		Where("id = ? AND status = 'pending' AND attempts = 0", reminderID).
		Updates(map[string]any{
			"attempts":            1,
			"provider_message_id": providerRequestID,
			"locked_by":           lockOwner,
			"locked_until":        lockedUntil,
			"updated_at":          now,
		})
	return result.RowsAffected == 1, result.Error
}

func (r *reminderWorkerRepository) heldQuantity(
	ctx context.Context,
	tx *gorm.DB,
	lotID uint64,
) (uint64, error) {
	var row struct {
		Quantity uint64
	}
	err := tx.WithContext(ctx).Raw(`
		SELECT
			COALESCE((
				SELECT SUM(quantity)
				FROM wine_ticket_redemption_allocations
				WHERE lot_id = ? AND status = ?
			), 0) +
			COALESCE((
				SELECT SUM(quantity)
				FROM wine_ticket_gift_allocations
				WHERE source_lot_id = ? AND status = ?
			), 0) +
			COALESCE((
				SELECT SUM(quantity)
				FROM wine_ticket_refund_allocations
				WHERE lot_id = ? AND status = ?
			), 0) AS quantity`,
		lotID,
		allocationStatusHeld,
		lotID,
		allocationStatusHeld,
		lotID,
		allocationStatusHeld,
	).Scan(&row).Error
	return row.Quantity, err
}

func (r *reminderWorkerRepository) updateOwnedReminder(
	ctx context.Context,
	tx *gorm.DB,
	reminderID uint64,
	lockOwner string,
	values map[string]any,
) (bool, error) {
	result := tx.WithContext(ctx).
		Model(&Reminder{}).
		Where(
			"id = ? AND status = 'pending' AND attempts = 1 AND locked_by = ?",
			reminderID,
			lockOwner,
		).
		Updates(values)
	return result.RowsAffected == 1, result.Error
}

func (r *reminderWorkerRepository) updateSendingConsent(
	ctx context.Context,
	tx *gorm.DB,
	consentID uint64,
	reminderID uint64,
	values map[string]any,
) error {
	return tx.WithContext(ctx).
		Model(&NotificationSubscriptionConsent{}).
		Where(
			"id = ? AND status = 'sending' AND claimed_by_reminder_id = ?",
			consentID,
			reminderID,
		).
		Updates(values).Error
}
