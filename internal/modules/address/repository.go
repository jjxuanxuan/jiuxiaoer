package address

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Repository struct {
	db *gorm.DB
}

// NewRepository 创建并初始化数据仓储。
func NewRepository(db *gorm.DB) *Repository { return &Repository{db: db} }

// DB 返回当前数据仓储使用的数据库连接。
func (r *Repository) DB() *gorm.DB { return r.db }

// LockCustomer 加锁并获取用户。
func (r *Repository) LockCustomer(ctx context.Context, tx *gorm.DB, customerID uint64) error {
	var row struct{ ID uint64 }
	return tx.WithContext(ctx).Table("customers").Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id").Where("id = ? AND deleted_at IS NULL", customerID).Take(&row).Error
}

// List 查询用户 Address列表列表。
func (r *Repository) List(ctx context.Context, db *gorm.DB, customerID uint64) ([]CustomerAddress, error) {
	var rows []CustomerAddress
	err := db.WithContext(ctx).Where("customer_id = ? AND deleted_at IS NULL", customerID).
		Order("is_default DESC, updated_at DESC, id DESC").Find(&rows).Error
	return rows, err
}

// Count 统计int 64的数量。
func (r *Repository) Count(ctx context.Context, tx *gorm.DB, customerID uint64) (int64, error) {
	var count int64
	err := tx.WithContext(ctx).Model(&CustomerAddress{}).
		Where("customer_id = ? AND deleted_at IS NULL", customerID).Count(&count).Error
	return count, err
}

// GetOwned 获取Owned。
func (r *Repository) GetOwned(ctx context.Context, db *gorm.DB, customerID, addressID uint64, lock bool, includeDeleted bool) (CustomerAddress, error) {
	query := db.WithContext(ctx).Where("customer_id = ? AND id = ?", customerID, addressID)
	if !includeDeleted {
		query = query.Where("deleted_at IS NULL")
	}
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row CustomerAddress
	err := query.Take(&row).Error
	return row, err
}

// ClearDefault 清空默认项。
func (r *Repository) ClearDefault(ctx context.Context, tx *gorm.DB, customerID uint64, exceptID uint64) error {
	query := tx.WithContext(ctx).Model(&CustomerAddress{}).
		Where("customer_id = ? AND is_default = 1 AND deleted_at IS NULL", customerID)
	if exceptID != 0 {
		query = query.Where("id <> ?", exceptID)
	}
	return query.Updates(map[string]any{"is_default": false, "version": gorm.Expr("version + 1")}).Error
}

// Create 创建地址。
func (r *Repository) Create(ctx context.Context, tx *gorm.DB, row *CustomerAddress) error {
	return tx.WithContext(ctx).Create(row).Error
}

// Update 更新地址。
func (r *Repository) Update(ctx context.Context, tx *gorm.DB, customerID, addressID uint64, version uint32, values map[string]any) (bool, error) {
	values["version"] = gorm.Expr("version + 1")
	result := tx.WithContext(ctx).Model(&CustomerAddress{}).
		Where("customer_id = ? AND id = ? AND version = ? AND deleted_at IS NULL", customerID, addressID, version).
		Updates(values)
	return result.RowsAffected == 1, result.Error
}

// SoftDelete 返回Soft Delete。
func (r *Repository) SoftDelete(ctx context.Context, tx *gorm.DB, customerID, addressID uint64) error {
	return tx.WithContext(ctx).Model(&CustomerAddress{}).
		Where("customer_id = ? AND id = ? AND deleted_at IS NULL", customerID, addressID).
		Updates(map[string]any{"deleted_at": time.Now().UTC(), "is_default": false, "version": gorm.Expr("version + 1")}).Error
}

// SetDefault 设置默认项。
func (r *Repository) SetDefault(ctx context.Context, tx *gorm.DB, customerID, addressID uint64) error {
	return tx.WithContext(ctx).Model(&CustomerAddress{}).
		Where("customer_id = ? AND id = ? AND deleted_at IS NULL", customerID, addressID).
		Updates(map[string]any{"is_default": true, "version": gorm.Expr("version + 1")}).Error
}
