package servicearea

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

type Repository struct {
	db *gorm.DB
}

// NewRepository 创建并初始化数据仓储。
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

// Resolve 返回Resolve。
func (r *Repository) Resolve(ctx context.Context, db *gorm.DB, input ResolveInput, now time.Time) (ResolvedShop, error) {
	rows, err := r.Candidates(ctx, db, input, now, 1)
	if err != nil {
		return ResolvedShop{}, err
	}
	if len(rows) == 0 {
		return ResolvedShop{}, gorm.ErrRecordNotFound
	}
	return rows[0], nil
}

// Candidates 返回当前可服务门店的稳定有界集合。
// 路线服务商的精细筛选刻意在此仓储层之外执行。
func (r *Repository) Candidates(ctx context.Context, db *gorm.DB, input ResolveInput, now time.Time, limit int) ([]ResolvedShop, error) {
	if db == nil {
		db = r.db
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 50 {
		limit = 50
	}
	var rows []ResolvedShop
	weekday := int(now.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	clock := now.Format("15:04:05")
	err := db.WithContext(ctx).Raw(`
		SELECT
			s.id, s.merchant_id, s.name, s.city_code, s.district, s.address,
			CAST(s.latitude AS DOUBLE) AS latitude, CAST(s.longitude AS DOUBLE) AS longitude,
			ROUND(ST_Distance_Sphere(POINT(s.longitude, s.latitude), POINT(?, ?))) AS distance_m,
			s.service_area_version, s.service_radius_m, s.priority, s.delivery_fee_amount,
			s.free_delivery_threshold_amount, s.delivery_eta_min, s.delivery_eta_max,
			s.overtime_policy_code, s.overtime_policy_version,
			p.title AS policy_title, p.summary AS policy_summary, p.terms_url AS policy_terms_url
		FROM shops s
		LEFT JOIN delivery_promise_policies p
		  ON p.policy_code = s.overtime_policy_code
		 AND p.version = s.overtime_policy_version
		 AND p.status = 'published'
		 AND (p.effective_from IS NULL OR p.effective_from <= UTC_TIMESTAMP(3))
		 AND (p.effective_to IS NULL OR p.effective_to > UTC_TIMESTAMP(3))
		WHERE s.city_code = ?
		  AND s.status = 'active'
		  AND s.business_status = 'open'
		  AND s.service_mode = 'radius'
		  AND s.service_radius_m > 0
		  AND s.latitude IS NOT NULL
		  AND s.longitude IS NOT NULL
		  AND s.deleted_at IS NULL
		  AND EXISTS (
			SELECT 1 FROM shop_business_hours h
			WHERE h.shop_id = s.id
			  AND h.day_of_week = ?
			  AND h.status = 'active'
			  AND h.deleted_at IS NULL
			  AND ? >= h.open_time
			  AND ? < h.close_time
		  )
		HAVING distance_m <= service_radius_m
		ORDER BY distance_m ASC, s.priority DESC, s.id ASC
		LIMIT ?
	`, input.Longitude, input.Latitude, input.CityCode, weekday, clock, clock, limit).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// HasConfiguredCity 判断是否存在Configured City。
func (r *Repository) HasConfiguredCity(ctx context.Context, db *gorm.DB, cityCode string) (bool, error) {
	if db == nil {
		db = r.db
	}
	var count int64
	err := db.WithContext(ctx).Table("shops").Where(
		"city_code = ? AND status = 'active' AND service_mode = 'radius' AND service_radius_m > 0 AND deleted_at IS NULL",
		cityCode,
	).Count(&count).Error
	return count > 0, err
}

// HasOpenShop 判断是否存在打开门店。
func (r *Repository) HasOpenShop(ctx context.Context, db *gorm.DB, cityCode string, now time.Time) (bool, error) {
	if db == nil {
		db = r.db
	}
	weekday := int(now.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	clock := now.Format("15:04:05")
	var count int64
	err := db.WithContext(ctx).Raw(`
		SELECT COUNT(DISTINCT s.id)
		FROM shops s
		JOIN shop_business_hours h ON h.shop_id = s.id
		WHERE s.city_code = ? AND s.status = 'active' AND s.business_status = 'open'
		  AND s.service_mode = 'radius' AND s.service_radius_m > 0 AND s.deleted_at IS NULL
		  AND h.day_of_week = ? AND h.status = 'active' AND h.deleted_at IS NULL
		  AND ? >= h.open_time AND ? < h.close_time
	`, cityCode, weekday, clock, clock).Scan(&count).Error
	return count > 0, err
}

// IsNotFound 判断不 Found是否成立。
func IsNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}
