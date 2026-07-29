package ops

import (
	"context"

	"gorm.io/gorm"
)

// Repository 负责酒票运营边界所需的全部持久化操作，
// 不向父包暴露门面。
type Repository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

func (r *Repository) DB() *gorm.DB { return r.db }

func (r *Repository) CreateAudit(
	ctx context.Context,
	tx *gorm.DB,
	values map[string]any,
) error {
	return tx.WithContext(ctx).Table("audit_logs").Create(values).Error
}

func (r *Repository) CreateOutbox(
	ctx context.Context,
	tx *gorm.DB,
	values map[string]any,
) error {
	return tx.WithContext(ctx).Table("outbox_events").Create(values).Error
}
