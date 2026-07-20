package shop

import (
	"context"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
)

type Repository struct {
	db *gorm.DB
}

// NewRepository 创建并初始化数据仓储。
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

// ListPublicShops 查询公开数据 Shops列表。
func (r *Repository) ListPublicShops(ctx context.Context, query ListQuery) ([]Shop, error) {
	now := time.Now().In(time.FixedZone("Asia/Shanghai", 8*60*60))
	weekday := int(now.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	clock := now.Format("15:04:05")
	selectSQL := `s.id, s.merchant_id, s.name, s.phone, s.city, s.city_code, s.district, s.address,
		s.latitude, s.longitude, s.status, s.business_status, s.service_radius_m, s.priority,
		s.delivery_fee_amount, s.free_delivery_threshold_amount, s.delivery_eta_min, s.delivery_eta_max,
		s.overtime_policy_code`
	selectArgs := []any{}
	if query.Latitude != nil && query.Longitude != nil {
		selectSQL += `, ROUND(ST_Distance_Sphere(POINT(s.longitude, s.latitude), POINT(?, ?))) AS distance_m,
			(ROUND(ST_Distance_Sphere(POINT(s.longitude, s.latitude), POINT(?, ?))) <= s.service_radius_m) AS serviceable`
		selectArgs = append(selectArgs, *query.Longitude, *query.Latitude, *query.Longitude, *query.Latitude)
	}
	db := r.db.WithContext(ctx).Table("shops s").Select(selectSQL, selectArgs...).
		Where("s.status = 'active' AND s.business_status = 'open' AND s.service_mode = 'radius' AND s.deleted_at IS NULL").
		Where(`EXISTS (SELECT 1 FROM shop_business_hours h WHERE h.shop_id = s.id AND h.day_of_week = ? AND h.status = 'active' AND h.deleted_at IS NULL AND ? >= h.open_time AND ? < h.close_time)`, weekday, clock, clock)

	if query.City != "" {
		db = db.Where("s.city = ?", query.City)
	}
	if query.District != "" {
		db = db.Where("s.district = ?", query.District)
	}
	if query.Keyword != "" {
		db = db.Where("s.name LIKE ?", "%"+query.Keyword+"%")
	}
	if query.CityCode != "" {
		db = db.Where("s.city_code = ?", query.CityCode)
	}
	db, err := pagination.ApplyFilter(db, query.Filter, shopFilterColumns)
	if err != nil {
		return nil, err
	}
	defaultOrder := "s.priority DESC, s.id ASC"
	if query.Latitude != nil {
		defaultOrder = "distance_m ASC, s.id ASC"
	}
	db, err = pagination.ApplyOrder(db, query.OrderBy, shopOrderColumns, defaultOrder)
	if err != nil {
		return nil, err
	}
	var shops []Shop
	err = db.Offset(query.Offset).Limit(query.PageSize + 1).Scan(&shops).Error
	return shops, err
}

var shopOrderColumns = map[string]string{
	"id":         "s.id",
	"created_at": "s.created_at",
	"updated_at": "s.updated_at",
	"name":       "s.name",
	"priority":   "s.priority",
	"distance_m": "distance_m",
}

var shopFilterColumns = map[string]string{
	"id":          "s.id",
	"merchant_id": "s.merchant_id",
	"name":        "s.name",
	"created_at":  "s.created_at",
}
