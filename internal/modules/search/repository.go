package search

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Repository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) *Repository { return &Repository{db: db} }

func (r *Repository) LockCustomer(ctx context.Context, tx *gorm.DB, customerID uint64) error {
	var row struct{ ID uint64 }
	return tx.WithContext(ctx).Table("customers").Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id").Where("id = ? AND status = 'active' AND deleted_at IS NULL", customerID).Take(&row).Error
}

func (r *Repository) UpsertHistory(ctx context.Context, tx *gorm.DB, row *History) (History, error) {
	err := tx.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "customer_id"}, {Name: "normalized_keyword"}},
		DoUpdates: clause.Assignments(map[string]any{
			"keyword":          row.Keyword,
			"search_count":     gorm.Expr("search_count + 1"),
			"last_searched_at": row.LastSearchedAt,
			"updated_at":       row.UpdatedAt,
		}),
	}).Create(row).Error
	if err != nil {
		return History{}, err
	}
	var stored History
	err = tx.WithContext(ctx).Where("customer_id = ? AND normalized_keyword = ?", row.CustomerID, row.NormalizedKeyword).Take(&stored).Error
	return stored, err
}

func (r *Repository) TrimHistory(ctx context.Context, tx *gorm.DB, customerID uint64, keep int) error {
	var count int64
	if err := tx.WithContext(ctx).Model(&History{}).Where("customer_id = ?", customerID).Count(&count).Error; err != nil {
		return err
	}
	excess := count - int64(keep)
	if excess <= 0 {
		return nil
	}
	var ids []uint64
	if err := tx.WithContext(ctx).Model(&History{}).Where("customer_id = ?", customerID).
		Order("last_searched_at DESC, id DESC").Offset(keep).Limit(int(excess)).Pluck("id", &ids).Error; err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	return tx.WithContext(ctx).Where("customer_id = ? AND id IN ?", customerID, ids).Delete(&History{}).Error
}

func (r *Repository) ListHistory(ctx context.Context, customerID uint64, since time.Time, limit int) ([]History, error) {
	if r.db == nil {
		return nil, errors.New("search database is unavailable")
	}
	var rows []History
	err := r.db.WithContext(ctx).Where("customer_id = ? AND last_searched_at >= ?", customerID, since).
		Order("last_searched_at DESC, id DESC").Limit(limit).Find(&rows).Error
	return rows, err
}

func (r *Repository) ClearHistory(ctx context.Context, tx *gorm.DB, customerID uint64) (int64, error) {
	result := tx.WithContext(ctx).Where("customer_id = ?", customerID).Delete(&History{})
	return result.RowsAffected, result.Error
}

func (r *Repository) UpsertDailyStat(ctx context.Context, tx *gorm.DB, row *DailyStat) error {
	return tx.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "stat_date"}, {Name: "scope_type"}, {Name: "scope_id"}, {Name: "normalized_keyword"}},
		DoUpdates: clause.Assignments(map[string]any{
			"display_keyword":  row.DisplayKeyword,
			"search_count":     gorm.Expr("search_count + 1"),
			"last_searched_at": row.LastSearchedAt,
			"updated_at":       row.UpdatedAt,
		}),
	}).Create(row).Error
}

func (r *Repository) HotKeywords(ctx context.Context, scopeType, scopeID string, fromDate, throughDate time.Time, limit int) ([]hotAggregate, error) {
	if r.db == nil {
		return nil, errors.New("search database is unavailable")
	}
	var rows []hotAggregate
	err := r.db.WithContext(ctx).Table("search_keyword_daily_stats").
		Select("normalized_keyword, MAX(display_keyword) AS display_keyword, SUM(search_count) AS search_count").
		Where("scope_type = ? AND scope_id = ? AND stat_date >= ? AND stat_date <= ?", scopeType, scopeID, fromDate, throughDate).
		Group("normalized_keyword").
		Order("search_count DESC, MAX(last_searched_at) DESC, normalized_keyword ASC").
		Limit(limit).Scan(&rows).Error
	return rows, err
}

func (r *Repository) ConfigStrings(ctx context.Context, key string) ([]string, error) {
	if r.db == nil {
		return nil, errors.New("search database is unavailable")
	}
	var row struct {
		ConfigValue string `gorm:"column:config_value"`
	}
	err := r.db.WithContext(ctx).Table("system_configs").Select("config_value").
		Where("config_key = ? AND status = 'active' AND deleted_at IS NULL", key).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var values []string
	if err := json.Unmarshal([]byte(row.ConfigValue), &values); err != nil {
		return nil, err
	}
	if len(values) > 200 {
		return nil, errors.New("search string configuration exceeds 200 entries")
	}
	return values, nil
}

func (r *Repository) CleanupHistory(ctx context.Context, before time.Time, limit int) (int64, error) {
	return r.cleanupByIDs(ctx, "customer_search_histories", "last_searched_at < ?", before, limit)
}

func (r *Repository) CleanupStats(ctx context.Context, beforeDate time.Time, limit int) (int64, error) {
	return r.cleanupByIDs(ctx, "search_keyword_daily_stats", "stat_date < ?", beforeDate, limit)
}

func (r *Repository) cleanupByIDs(ctx context.Context, table, predicate string, boundary any, limit int) (int64, error) {
	if r.db == nil {
		return 0, errors.New("search database is unavailable")
	}
	var ids []uint64
	if err := r.db.WithContext(ctx).Table(table).Where(predicate, boundary).Order("id ASC").Limit(limit).Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	var result *gorm.DB
	switch table {
	case "customer_search_histories":
		result = r.db.WithContext(ctx).Where("id IN ?", ids).Delete(&History{})
	case "search_keyword_daily_stats":
		result = r.db.WithContext(ctx).Where("id IN ?", ids).Delete(&DailyStat{})
	default:
		return 0, errors.New("unsupported search cleanup table")
	}
	return result.RowsAffected, result.Error
}
