package customerlocation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
)

type Repository struct{ db *gorm.DB }

func NewRepository(db *gorm.DB) *Repository { return &Repository{db: db} }

func (r *Repository) DB() *gorm.DB { return r.db }

func (r *Repository) PublishedCities(ctx context.Context, keyword string, query pagination.Query) ([]ServiceCity, error) {
	if r.db == nil {
		return nil, errors.New("database unavailable")
	}
	db := r.db.WithContext(ctx).Where("status = ?", "published")
	if value := strings.TrimSpace(keyword); value != "" {
		db = db.Where("name LIKE ? OR pinyin LIKE ?", value+"%", strings.ToLower(value)+"%")
	}
	var rows []ServiceCity
	err := db.Order("sort_order ASC, city_code ASC").Offset(query.Offset).Limit(query.PageSize + 1).Find(&rows).Error
	return rows, err
}

func (r *Repository) CityByCode(ctx context.Context, cityCode string, publishedOnly bool) (ServiceCity, error) {
	if r.db == nil {
		return ServiceCity{}, errors.New("database unavailable")
	}
	db := r.db.WithContext(ctx).Where("city_code = ?", cityCode)
	if publishedOnly {
		db = db.Where("status = ?", "published")
	}
	var row ServiceCity
	err := db.Take(&row).Error
	return row, err
}

func (r *Repository) CityByADCode(ctx context.Context, adcode string) (mappedCity, error) {
	if r.db == nil {
		return mappedCity{}, errors.New("database unavailable")
	}
	var row mappedCity
	err := r.db.WithContext(ctx).Table("service_cities c").
		Select("c.*, a.standard_name").
		Joins("JOIN service_city_adcodes a ON a.service_city_id = c.id").
		Where("a.adcode = ?", adcode).Take(&row).Error
	return row, err
}

func (r *Repository) HasPublishedCityName(ctx context.Context, cityName string) (bool, error) {
	if r.db == nil {
		return false, errors.New("database unavailable")
	}
	var count int64
	err := r.db.WithContext(ctx).Model(&ServiceCity{}).Where("status = 'published' AND name = ?", cityName).Count(&count).Error
	return count > 0, err
}

func (r *Repository) SavedAddress(ctx context.Context, customerID, addressID uint64) (SavedAddress, error) {
	if r.db == nil {
		return SavedAddress{}, errors.New("database unavailable")
	}
	var row SavedAddress
	err := r.db.WithContext(ctx).Table("customer_addresses").
		Where("id = ? AND customer_id = ? AND deleted_at IS NULL", addressID, customerID).Take(&row).Error
	return row, err
}

func (r *Repository) AdminCities(ctx context.Context, query pagination.Query) ([]ServiceCity, error) {
	if r.db == nil {
		return nil, errors.New("database unavailable")
	}
	var rows []ServiceCity
	err := r.db.WithContext(ctx).Order("updated_at DESC, id DESC").Offset(query.Offset).Limit(query.PageSize + 1).Find(&rows).Error
	return rows, err
}

func (r *Repository) CityADCodes(ctx context.Context, db *gorm.DB, cityID uint64) ([]ServiceCityAdcode, error) {
	var rows []ServiceCityAdcode
	err := db.WithContext(ctx).Where("service_city_id = ?", cityID).Order("adcode ASC").Find(&rows).Error
	return rows, err
}

func (r *Repository) LockCity(ctx context.Context, tx *gorm.DB, id uint64) (ServiceCity, error) {
	var row ServiceCity
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).Take(&row).Error
	return row, err
}

func (r *Repository) CreateCity(ctx context.Context, tx *gorm.DB, row *ServiceCity) error {
	return tx.WithContext(ctx).Create(row).Error
}

func (r *Repository) UpdateCity(ctx context.Context, tx *gorm.DB, id uint64, version uint32, values map[string]any) (bool, error) {
	values["version"] = gorm.Expr("version + 1")
	result := tx.WithContext(ctx).Model(&ServiceCity{}).Where("id = ? AND version = ?", id, version).Updates(values)
	return result.RowsAffected == 1, result.Error
}

func (r *Repository) ReplaceADCodes(ctx context.Context, tx *gorm.DB, cityID, actorID uint64, ids func() uint64, values []CoveredADCodeRequest) error {
	if err := tx.WithContext(ctx).Where("service_city_id = ?", cityID).Delete(&ServiceCityAdcode{}).Error; err != nil {
		return err
	}
	rows := make([]ServiceCityAdcode, 0, len(values))
	for _, value := range values {
		rows = append(rows, ServiceCityAdcode{ID: ids(), ServiceCityID: cityID, ADCode: value.ADCode, StandardName: value.StandardName, Level: value.Level, CreatedBy: actorID, UpdatedBy: actorID})
	}
	return tx.WithContext(ctx).Create(&rows).Error
}

func (r *Repository) CityPublishable(ctx context.Context, tx *gorm.DB, city ServiceCity) (bool, error) {
	var adcodes int64
	if err := tx.WithContext(ctx).Model(&ServiceCityAdcode{}).Where("service_city_id = ?", city.ID).Count(&adcodes).Error; err != nil {
		return false, err
	}
	var shops int64
	err := tx.WithContext(ctx).Table("shops").Where("city_code = ? AND status = 'active' AND service_mode = 'radius' AND deleted_at IS NULL", city.CityCode).Count(&shops).Error
	return adcodes > 0 && shops > 0, err
}

func (r *Repository) AdminPolicies(ctx context.Context, query pagination.Query) ([]DeliveryPromisePolicy, error) {
	if r.db == nil {
		return nil, errors.New("database unavailable")
	}
	var rows []DeliveryPromisePolicy
	err := r.db.WithContext(ctx).Order("policy_code ASC, version DESC").Offset(query.Offset).Limit(query.PageSize + 1).Find(&rows).Error
	return rows, err
}

func (r *Repository) LockPolicy(ctx context.Context, tx *gorm.DB, id uint64) (DeliveryPromisePolicy, error) {
	var row DeliveryPromisePolicy
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).Take(&row).Error
	return row, err
}

func (r *Repository) CreatePolicy(ctx context.Context, tx *gorm.DB, row *DeliveryPromisePolicy) error {
	return tx.WithContext(ctx).Create(row).Error
}

func (r *Repository) UpdatePolicy(ctx context.Context, tx *gorm.DB, id uint64, values map[string]any) error {
	return tx.WithContext(ctx).Model(&DeliveryPromisePolicy{}).Where("id = ?", id).Updates(values).Error
}

func (r *Repository) RetirePublishedPolicy(ctx context.Context, tx *gorm.DB, code string, exceptID, actorID uint64) error {
	return tx.WithContext(ctx).Model(&DeliveryPromisePolicy{}).
		Where("policy_code = ? AND status = 'published' AND id <> ?", code, exceptID).
		Updates(map[string]any{"status": "retired", "updated_by": actorID}).Error
}

func (r *Repository) RebindShopPolicy(ctx context.Context, tx *gorm.DB, code string, version uint32) error {
	return tx.WithContext(ctx).Table("shops").
		Where("overtime_policy_code = ? AND deleted_at IS NULL", code).
		Updates(map[string]any{"overtime_policy_version": version, "service_area_version": gorm.Expr("service_area_version + 1")}).Error
}

func (r *Repository) PolicyShopReferences(ctx context.Context, tx *gorm.DB, code string, version uint32) (int64, error) {
	var count int64
	err := tx.WithContext(ctx).Table("shops").
		Where("overtime_policy_code = ? AND overtime_policy_version = ? AND deleted_at IS NULL", code, version).
		Count(&count).Error
	return count, err
}

func (r *Repository) Audit(ctx context.Context, tx *gorm.DB, id, actorID uint64, action, resourceType string, resourceID uint64, before, after any, requestID, ip, userAgent *string) error {
	beforePayload, _ := json.Marshal(before)
	afterPayload, _ := json.Marshal(after)
	return tx.WithContext(ctx).Table("audit_logs").Create(map[string]any{
		"id": id, "actor_type": "admin", "actor_id": actorID, "action": action, "resource_type": resourceType,
		"resource_id": resourceID, "before_data": datatypes.JSON(beforePayload), "after_data": datatypes.JSON(afterPayload), "result": "success",
		"request_id": requestID, "ip": ip, "user_agent": userAgent, "created_at": time.Now().UTC(),
	}).Error
}

func isNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }
