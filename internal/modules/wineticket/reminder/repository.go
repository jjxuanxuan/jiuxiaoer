package reminder

import (
	"context"

	"gorm.io/gorm"
)

type reminderRepository struct {
	db *gorm.DB
}

func newReminderRepository(db *gorm.DB) *reminderRepository {
	return &reminderRepository{db: db}
}

func (r *reminderRepository) dbConn() *gorm.DB {
	return r.db
}

func (r *reminderRepository) latestConsent(
	ctx context.Context,
	customerID uint64,
	scene string,
) (NotificationSubscriptionConsent, error) {
	var row NotificationSubscriptionConsent
	err := r.db.WithContext(ctx).
		Where("customer_id = ? AND scene = ?", customerID, scene).
		Order("consented_at DESC, id DESC").
		Take(&row).Error
	return row, err
}

func (r *reminderRepository) createConsent(
	ctx context.Context,
	tx *gorm.DB,
	row *NotificationSubscriptionConsent,
) error {
	return tx.WithContext(ctx).Create(row).Error
}
