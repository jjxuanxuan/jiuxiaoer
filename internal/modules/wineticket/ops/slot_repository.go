package ops

import (
	"context"
	"errors"
	"strings"
	"time"

	mysqlerr "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/redemption"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

type slotAdminRepository struct {
	db *gorm.DB
}

func newSlotAdminRepository(db *gorm.DB) *slotAdminRepository {
	return &slotAdminRepository{db: db}
}

func slotAdminProjection(db *gorm.DB) *gorm.DB {
	return db.Table("delivery_time_slots slot").
		Select(`
			slot.*,
			shop.name AS shop_name,
			merchant.id AS merchant_id,
			merchant.name AS merchant_name
		`).
		Joins(`
			JOIN shops shop
			  ON shop.id = slot.shop_id
			 AND shop.deleted_at IS NULL
		`).
		Joins(`
			JOIN merchants merchant
			  ON merchant.id = shop.merchant_id
			 AND merchant.deleted_at IS NULL
		`)
}

func (r *slotAdminRepository) list(
	ctx context.Context,
	query pagination.Query,
	filter slotAdminListFilter,
) ([]slotAdminRecord, error) {
	db := slotAdminProjection(r.db.WithContext(ctx))
	if filter.ShopID != nil {
		db = db.Where("slot.shop_id = ?", *filter.ShopID)
	} else if len(filter.AuthorizedShops) != 0 {
		db = db.Where("slot.shop_id IN ?", filter.AuthorizedShops)
	}
	if filter.ServiceDate != nil {
		db = db.Where("slot.service_date = ?", *filter.ServiceDate)
	}
	var err error
	db, err = pagination.ApplyIDCursor(db, query, "slot.id", "desc")
	if err != nil {
		return nil, err
	}
	var rows []slotAdminRecord
	err = db.Order("slot.id DESC").Limit(query.PageSize + 1).Scan(&rows).Error
	return rows, err
}

func (r *slotAdminRepository) slotByID(
	ctx context.Context,
	db *gorm.DB,
	slotID uint64,
	authorizedShops []uint64,
) (redemption.DeliveryTimeSlot, error) {
	if db == nil {
		db = r.db
	}
	var row redemption.DeliveryTimeSlot
	query := db.WithContext(ctx).Where("id = ?", slotID)
	if len(authorizedShops) != 0 {
		query = query.Where("shop_id IN ?", authorizedShops)
	}
	err := query.Take(&row).Error
	return row, err
}

func (r *slotAdminRepository) lockShop(
	ctx context.Context,
	tx *gorm.DB,
	shopID uint64,
) (slotAdminShop, error) {
	var row slotAdminShop
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND deleted_at IS NULL", shopID).
		Take(&row).Error
	return row, err
}

func (r *slotAdminRepository) merchant(
	ctx context.Context,
	tx *gorm.DB,
	merchantID uint64,
) (slotAdminMerchant, error) {
	var row slotAdminMerchant
	err := tx.WithContext(ctx).
		Where("id = ? AND deleted_at IS NULL", merchantID).
		Take(&row).Error
	return row, err
}

func (r *slotAdminRepository) lockSlot(
	ctx context.Context,
	tx *gorm.DB,
	slotID uint64,
	authorizedShops []uint64,
) (redemption.DeliveryTimeSlot, error) {
	var row redemption.DeliveryTimeSlot
	query := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", slotID)
	if len(authorizedShops) != 0 {
		query = query.Where("shop_id IN ?", authorizedShops)
	}
	err := query.Take(&row).Error
	return row, err
}

func (r *slotAdminRepository) lockOpenSlots(
	ctx context.Context,
	tx *gorm.DB,
	shopID uint64,
	serviceDate time.Time,
	excludeID uint64,
) ([]redemption.DeliveryTimeSlot, error) {
	db := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"shop_id = ? AND service_date = ? AND status = ?",
			shopID,
			serviceDate,
			DeliveryTimeSlotStatusOpen,
		)
	if excludeID != 0 {
		db = db.Where("id <> ?", excludeID)
	}
	var rows []redemption.DeliveryTimeSlot
	err := db.Order("start_time ASC, id ASC").Find(&rows).Error
	return rows, err
}

func (r *slotAdminRepository) create(
	ctx context.Context,
	tx *gorm.DB,
	row *redemption.DeliveryTimeSlot,
) error {
	return tx.WithContext(ctx).Create(row).Error
}

func (r *slotAdminRepository) updateVersioned(
	ctx context.Context,
	tx *gorm.DB,
	row redemption.DeliveryTimeSlot,
	values map[string]any,
) error {
	result := tx.WithContext(ctx).Model(&redemption.DeliveryTimeSlot{}).
		Where(
			"id = ? AND shop_id = ? AND version = ?",
			row.ID,
			row.ShopID,
			row.Version,
		).
		Updates(values)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return problem.Conflict(
			"WT_CONCURRENT_MODIFICATION",
			"delivery time slot changed concurrently",
		)
	}
	return nil
}

func slotAdminWriteError(err error) error {
	var mysqlError *mysqlerr.MySQLError
	if errors.As(err, &mysqlError) && mysqlError.Number == 1062 {
		return problem.Conflict(
			"WT_CONCURRENT_MODIFICATION",
			"an overlapping delivery time slot was created concurrently",
		)
	}
	if strings.Contains(strings.ToLower(err.Error()), "unique constraint") ||
		strings.Contains(strings.ToLower(err.Error()), "duplicate entry") {
		return problem.Conflict(
			"WT_CONCURRENT_MODIFICATION",
			"an overlapping delivery time slot was created concurrently",
		)
	}
	return err
}
