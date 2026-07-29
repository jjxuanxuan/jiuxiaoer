package catalog

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
)

type Repository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) *Repository { return &Repository{db: db} }

func (r *Repository) DB() *gorm.DB { return r.db }

func packageProjection(db *gorm.DB, public bool) *gorm.DB {
	join := "LEFT JOIN products product_projection ON product_projection.id = wine_ticket_packages.product_id"
	if public {
		join = `
			JOIN products product_projection
			  ON product_projection.id = wine_ticket_packages.product_id
			 AND product_projection.deleted_at IS NULL
			 AND product_projection.status = 'on_sale'
			JOIN categories product_category
			  ON product_category.id = product_projection.category_id
			 AND product_category.deleted_at IS NULL
			 AND product_category.status = 'active'
			 AND (product_projection.age_restricted OR product_category.age_restricted)
			JOIN merchants issuer_merchant
			  ON issuer_merchant.id = wine_ticket_packages.issuer_merchant_id
			 AND issuer_merchant.deleted_at IS NULL
			 AND issuer_merchant.status = 'active'
			 AND issuer_merchant.review_status = 'approved'
			JOIN shops settlement_shop
			  ON settlement_shop.id = wine_ticket_packages.settlement_shop_id
			 AND settlement_shop.merchant_id = wine_ticket_packages.issuer_merchant_id
			 AND settlement_shop.deleted_at IS NULL
			 AND settlement_shop.status = 'active'
			 AND settlement_shop.business_status = 'open'
			JOIN shop_products settlement_shop_product
			  ON settlement_shop_product.id = wine_ticket_packages.settlement_shop_product_id
			 AND settlement_shop_product.merchant_id = wine_ticket_packages.issuer_merchant_id
			 AND settlement_shop_product.shop_id = wine_ticket_packages.settlement_shop_id
			 AND settlement_shop_product.product_id = wine_ticket_packages.product_id
			 AND settlement_shop_product.deleted_at IS NULL
			 AND settlement_shop_product.status = 'on_sale'
		`
	}
	return db.Table("wine_ticket_packages").
		Select(`
			wine_ticket_packages.*,
			product_projection.name AS product_name,
			product_projection.brand_name AS product_brand_name,
			product_projection.spec AS product_spec,
			product_projection.image_url AS product_image_url
		`).
		Joins(join)
}

func (r *Repository) ListPublicPackages(ctx context.Context, query pagination.Query, filter PackageListFilter, now time.Time) ([]PackageRecord, error) {
	db := packageProjection(r.db.WithContext(ctx), true).
		Where("wine_ticket_packages.deleted_at IS NULL").
		Where("wine_ticket_packages.status = ?", PackageStatusPublished).
		Where("(wine_ticket_packages.sale_start_at IS NULL OR wine_ticket_packages.sale_start_at <= ?)", now).
		Where("(wine_ticket_packages.sale_end_at IS NULL OR wine_ticket_packages.sale_end_at > ?)", now)
	if filter.PackageType != "" {
		db = db.Where("wine_ticket_packages.package_type = ?", filter.PackageType)
	}
	var err error
	db, err = pagination.ApplyIDCursor(db, query, "wine_ticket_packages.id", "desc")
	if err != nil {
		return nil, err
	}
	var rows []PackageRecord
	err = db.Order("wine_ticket_packages.id DESC").Limit(query.PageSize + 1).Scan(&rows).Error
	return rows, err
}

func (r *Repository) PublicPackageByNo(ctx context.Context, packageNo string, now time.Time) (PackageRecord, error) {
	var row PackageRecord
	err := packageProjection(r.db.WithContext(ctx), true).
		Where("wine_ticket_packages.package_no = ? AND wine_ticket_packages.deleted_at IS NULL", packageNo).
		Where("wine_ticket_packages.status = ?", PackageStatusPublished).
		Where("(wine_ticket_packages.sale_start_at IS NULL OR wine_ticket_packages.sale_start_at <= ?)", now).
		Where("(wine_ticket_packages.sale_end_at IS NULL OR wine_ticket_packages.sale_end_at > ?)", now).
		Take(&row).Error
	return row, err
}

func (r *Repository) ListAdminPackages(ctx context.Context, query pagination.Query) ([]PackageRecord, error) {
	db := packageProjection(r.db.WithContext(ctx), false).
		Where("wine_ticket_packages.deleted_at IS NULL")
	var err error
	db, err = pagination.ApplyIDCursor(db, query, "wine_ticket_packages.id", "desc")
	if err != nil {
		return nil, err
	}
	var rows []PackageRecord
	err = db.Order("wine_ticket_packages.id DESC").Limit(query.PageSize + 1).Scan(&rows).Error
	return rows, err
}

func (r *Repository) AdminPackageByNo(ctx context.Context, db *gorm.DB, packageNo string) (PackageRecord, error) {
	if db == nil {
		db = r.db
	}
	var row PackageRecord
	err := packageProjection(db.WithContext(ctx), false).
		Where("wine_ticket_packages.package_no = ? AND wine_ticket_packages.deleted_at IS NULL", packageNo).
		Take(&row).Error
	return row, err
}

func (r *Repository) LockPackageByNo(ctx context.Context, tx *gorm.DB, packageNo string) (Package, error) {
	var row Package
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("package_no = ? AND deleted_at IS NULL", packageNo).
		Take(&row).Error
	return row, err
}

// NextPackageVersionLocked 通过锁定同一套餐编码的最新记录串行分配版本。
// 在 MySQL/InnoDB 中，编码与版本的唯一索引还会保护空区间及并发插入场景。
func (r *Repository) NextPackageVersionLocked(ctx context.Context, tx *gorm.DB, packageCode string) (uint, error) {
	var newest Package
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("package_code = ?", packageCode).
		Order("package_version DESC").
		Take(&newest).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	return newest.PackageVersion + 1, nil
}

func (r *Repository) CreatePackage(ctx context.Context, tx *gorm.DB, row *Package) error {
	return tx.WithContext(ctx).Create(row).Error
}

func (r *Repository) UpdateDraftPackage(ctx context.Context, tx *gorm.DB, row Package, expectedVersion uint, values map[string]any) (bool, error) {
	result := tx.WithContext(ctx).Model(&Package{}).
		Where("id = ? AND version = ? AND status = ? AND deleted_at IS NULL", row.ID, expectedVersion, PackageStatusDraft).
		Updates(values)
	return result.RowsAffected == 1, result.Error
}

func (r *Repository) TransitionPackage(ctx context.Context, tx *gorm.DB, row Package, expectedStatus string, expectedVersion uint, values map[string]any) (bool, error) {
	result := tx.WithContext(ctx).Model(&Package{}).
		Where("id = ? AND status = ? AND version = ? AND deleted_at IS NULL", row.ID, expectedStatus, expectedVersion).
		Updates(values)
	return result.RowsAffected == 1, result.Error
}

func (r *Repository) OtherPublishedPackageForCode(ctx context.Context, tx *gorm.DB, packageCode string, excludedID uint64) (Package, error) {
	var row Package
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("package_code = ? AND status = ? AND id <> ? AND deleted_at IS NULL", packageCode, PackageStatusPublished, excludedID).
		Take(&row).Error
	return row, err
}

func (r *Repository) SettlementRelation(ctx context.Context, tx *gorm.DB, row Package) (SettlementRelation, error) {
	var relation SettlementRelation
	err := tx.WithContext(ctx).Table("merchants merchant").
		Select(`
			merchant.id AS merchant_id,
			merchant.status AS merchant_status,
			merchant.review_status AS merchant_review_status,
			settlement_shop.id AS shop_id,
			settlement_shop.merchant_id AS shop_merchant_id,
			settlement_shop.status AS shop_status,
			settlement_shop.business_status AS shop_business_status,
			settlement_shop_product.id AS shop_product_id,
			settlement_shop_product.merchant_id AS shop_product_merchant_id,
			settlement_shop_product.shop_id AS shop_product_shop_id,
			settlement_shop_product.product_id AS shop_product_product_id,
			settlement_shop_product.status AS shop_product_status,
			settlement_product.id AS product_id,
			settlement_product.status AS product_status,
			settlement_category.status AS product_category_status,
			(settlement_product.age_restricted OR settlement_category.age_restricted) AS product_age_restricted
		`).
		Joins("JOIN shops settlement_shop ON settlement_shop.id = ? AND settlement_shop.deleted_at IS NULL", row.SettlementShopID).
		Joins("JOIN shop_products settlement_shop_product ON settlement_shop_product.id = ? AND settlement_shop_product.deleted_at IS NULL", row.SettlementShopProductID).
		Joins("JOIN products settlement_product ON settlement_product.id = ? AND settlement_product.deleted_at IS NULL", row.ProductID).
		Joins("JOIN categories settlement_category ON settlement_category.id = settlement_product.category_id AND settlement_category.deleted_at IS NULL").
		Where("merchant.id = ? AND merchant.deleted_at IS NULL", row.IssuerMerchantID).
		Take(&relation).Error
	return relation, err
}

func (r *Repository) CreateAudit(ctx context.Context, tx *gorm.DB, values map[string]any) error {
	return tx.WithContext(ctx).Table("audit_logs").Create(values).Error
}
