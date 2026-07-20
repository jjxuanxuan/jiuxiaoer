package home

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
)

type Repository struct{ db *gorm.DB }

// NewRepository 创建并初始化数据仓储。
func NewRepository(db *gorm.DB) *Repository { return &Repository{db: db} }

// DB 返回当前数据仓储使用的数据库连接。
func (r *Repository) DB() *gorm.DB { return r.db }

// PublicSlots 返回公开数据时段。
func (r *Repository) PublicSlots(ctx context.Context, cityCode string, now time.Time) ([]Slot, error) {
	var rows []Slot
	err := r.db.WithContext(ctx).Where(`deleted_at IS NULL AND status = 'published' AND (city_code IS NULL OR city_code = ?) AND (start_at IS NULL OR start_at <= ?) AND (end_at IS NULL OR end_at > ?)`, cityCode, now, now).
		Order("sort_order ASC, id ASC").Find(&rows).Error
	return rows, err
}

// Categories 返回Categories。
func (r *Repository) Categories(ctx context.Context) ([]Category, error) {
	var rows []Category
	err := r.db.WithContext(ctx).Table("categories").Select("id, name, sort_order").Where("status = 'active' AND deleted_at IS NULL").Order("sort_order ASC, id ASC").Scan(&rows).Error
	return rows, err
}

// List 查询Slot列表列表。
func (r *Repository) List(ctx context.Context, cityCode, slotType, status string, query pagination.Query) ([]Slot, error) {
	db := r.db.WithContext(ctx).Where("deleted_at IS NULL")
	if cityCode != "" {
		db = db.Where("city_code = ?", cityCode)
	}
	if slotType != "" {
		db = db.Where("slot_type = ?", slotType)
	}
	if status != "" {
		db = db.Where("status = ?", status)
	}
	db, err := pagination.ApplyOrder(db, query.OrderBy, map[string]string{"id": "id", "sort_order": "sort_order", "created_at": "created_at", "updated_at": "updated_at"}, "sort_order ASC, id ASC")
	if err != nil {
		return nil, err
	}
	var rows []Slot
	err = db.Offset(query.Offset).Limit(query.PageSize + 1).Find(&rows).Error
	return rows, err
}

// Create 创建首页。
func (r *Repository) Create(ctx context.Context, tx *gorm.DB, row *Slot) error {
	return tx.WithContext(ctx).Create(row).Error
}

// Lock 加锁并获取时段。
func (r *Repository) Lock(ctx context.Context, tx *gorm.DB, id uint64) (Slot, error) {
	var row Slot
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND deleted_at IS NULL", id).Take(&row).Error
	return row, err
}

// Update 更新首页。
func (r *Repository) Update(ctx context.Context, tx *gorm.DB, id uint64, version uint32, values map[string]any) (bool, error) {
	values["version"] = gorm.Expr("version + 1")
	result := tx.WithContext(ctx).Model(&Slot{}).Where("id = ? AND version = ? AND deleted_at IS NULL", id, version).Updates(values)
	return result.RowsAffected == 1, result.Error
}

// ReferencesExist 返回引用存在。
func (r *Repository) ReferencesExist(ctx context.Context, tx *gorm.DB, table string, ids []uint64) (bool, error) {
	if len(ids) == 0 {
		return true, nil
	}
	var count int64
	err := tx.WithContext(ctx).Table(table).Where("id IN ? AND deleted_at IS NULL", ids).Count(&count).Error
	return count == int64(len(ids)), err
}

// CreateAudit 创建审计。
func (r *Repository) CreateAudit(ctx context.Context, tx *gorm.DB, row AuditLog) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// CreateOutbox 创建发件箱事件。
func (r *Repository) CreateOutbox(ctx context.Context, tx *gorm.DB, row OutboxEvent) error {
	return tx.WithContext(ctx).Create(&row).Error
}
