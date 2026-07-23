package product

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
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

// CategoryCatalogRevision derives a stable revision from every category fact
// that can affect the public response. It deliberately includes inactive and
// soft-deleted rows, so INSERT/UPDATE/status/soft-delete/hard-delete mutations
// switch the cache key even when the writer is an operational job that cannot
// call application-level cache invalidation.
func (r *Repository) CategoryCatalogRevision(ctx context.Context) (string, error) {
	type revisionRow struct {
		ID            uint64
		ParentID      *uint64
		Name          string
		SortOrder     int64
		Status        string
		AgeRestricted bool
		DeletedAt     *time.Time
	}
	var rows []revisionRow
	err := r.db.WithContext(ctx).
		Table("categories").
		Select("id, parent_id, name, sort_order, status, age_restricted, deleted_at").
		Order("id ASC").
		Scan(&rows).Error
	if err != nil {
		return "", err
	}

	digest := sha256.New()
	for _, row := range rows {
		writeRevisionUint64(digest, row.ID)
		writeRevisionOptionalUint64(digest, row.ParentID)
		writeRevisionString(digest, row.Name)
		writeRevisionUint64(digest, uint64(row.SortOrder))
		writeRevisionString(digest, row.Status)
		if row.AgeRestricted {
			writeRevisionUint64(digest, 1)
		} else {
			writeRevisionUint64(digest, 0)
		}
		if row.DeletedAt == nil {
			writeRevisionUint64(digest, 0)
		} else {
			writeRevisionUint64(digest, 1)
			writeRevisionUint64(digest, uint64(row.DeletedAt.UTC().UnixMicro()))
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func writeRevisionOptionalUint64(target hash.Hash, value *uint64) {
	if value == nil {
		writeRevisionUint64(target, 0)
		return
	}
	writeRevisionUint64(target, 1)
	writeRevisionUint64(target, *value)
}

func writeRevisionString(target hash.Hash, value string) {
	writeRevisionUint64(target, uint64(len(value)))
	_, _ = target.Write([]byte(value))
}

func writeRevisionUint64(target hash.Hash, value uint64) {
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], value)
	_, _ = target.Write(data[:])
}

// ListCategories 查询Categories列表。
func (r *Repository) ListCategories(ctx context.Context) ([]Category, error) {
	var categories []Category
	err := r.db.WithContext(ctx).
		Where("status = 'active' AND deleted_at IS NULL").
		Order("sort_order ASC, id ASC").
		Find(&categories).Error
	return categories, err
}

// ListProducts 查询商品列表。
func (r *Repository) ListProducts(ctx context.Context, query ListQuery) ([]ProductRow, error) {
	if query.ShopID == "" {
		return r.listGenericProducts(ctx, query)
	}
	db := r.db.WithContext(ctx).
		Table("shop_products sp").
		Select(`
			p.id,
			p.category_id,
			sp.shop_id,
			sp.id AS shop_product_id,
			sp.sort_order,
			p.name,
			p.brand_name,
			p.spec,
			p.image_url,
			p.description,
			sp.sale_price_amount,
			p.original_price_amount,
			(p.age_restricted OR c.age_restricted) AS age_restricted,
			p.status,
			sp.status AS shop_product_status,
			s.status AS shop_status,
			s.business_status,
			COALESCE(ps.available_qty, 0) AS available_qty
		`).
		Joins("JOIN products p ON p.id = sp.product_id AND p.deleted_at IS NULL").
		Joins("JOIN categories c ON c.id = p.category_id AND c.status = 'active' AND c.deleted_at IS NULL").
		Joins("JOIN shops s ON s.id = sp.shop_id AND s.deleted_at IS NULL").
		Joins("LEFT JOIN product_stocks ps ON ps.shop_product_id = sp.id AND ps.deleted_at IS NULL").
		Where("sp.deleted_at IS NULL AND sp.status = 'on_sale' AND p.status = 'on_sale' AND s.status = 'active' AND s.business_status = 'open'")

	db = db.Where("sp.shop_id = ?", query.ShopID)
	if query.CategoryID != "" {
		db = db.Where("p.category_id = ?", query.CategoryID)
	}
	if query.Keyword != "" {
		db = db.Where("p.name LIKE ? OR p.brand_name LIKE ?", "%"+query.Keyword+"%", "%"+query.Keyword+"%")
	}
	if query.OrderBy == "" {
		var err error
		db, err = pagination.ApplyIntIDCursor(db, query.Query, "sp.sort_order", "sp.id", "asc")
		if err != nil {
			return nil, err
		}
	}
	db, err := pagination.ApplyFilter(db, query.Filter, productFilterColumns)
	if err != nil {
		return nil, err
	}
	db, err = pagination.ApplyOrder(db, query.OrderBy, productOrderColumns, "sp.sort_order ASC, sp.id ASC")
	if err != nil {
		return nil, err
	}

	var rows []ProductRow
	err = pagination.OffsetDB(db, query.Query).Limit(query.PageSize + 1).Scan(&rows).Error
	return rows, err
}

// listGenericProducts 查询Generic 商品列表。
func (r *Repository) listGenericProducts(ctx context.Context, query ListQuery) ([]ProductRow, error) {
	db := r.db.WithContext(ctx).Table("products p").Select(`
		p.id, p.category_id, p.name, p.brand_name, p.spec, p.image_url,
		p.description, p.original_price_amount, p.status,
		(p.age_restricted OR c.age_restricted) AS age_restricted
	`).Joins("JOIN categories c ON c.id = p.category_id AND c.status = 'active' AND c.deleted_at IS NULL").Where("p.status = 'on_sale' AND p.deleted_at IS NULL")
	if query.CategoryID != "" {
		db = db.Where("p.category_id = ?", query.CategoryID)
	}
	if query.Keyword != "" {
		db = db.Where("p.name LIKE ? OR p.brand_name LIKE ?", "%"+query.Keyword+"%", "%"+query.Keyword+"%")
	}
	if query.OrderBy == "" {
		var err error
		db, err = pagination.ApplyIDCursor(db, query.Query, "p.id", "asc")
		if err != nil {
			return nil, err
		}
	}
	db, err := pagination.ApplyFilter(db, query.Filter, genericProductFilterColumns)
	if err != nil {
		return nil, err
	}
	db, err = pagination.ApplyOrder(db, query.OrderBy, genericProductOrderColumns, "p.id ASC")
	if err != nil {
		return nil, err
	}
	var rows []ProductRow
	err = pagination.OffsetDB(db, query.Query).Limit(query.PageSize + 1).Scan(&rows).Error
	return rows, err
}

// GetPublicProduct 获取公开数据商品。
func (r *Repository) GetPublicProduct(ctx context.Context, productID, shopID uint64) (ProductRow, error) {
	if shopID == 0 {
		var row ProductRow
		err := r.db.WithContext(ctx).
			Table("products p").
			Select(`
				p.id,
				p.category_id,
				p.name,
				p.brand_name,
				p.spec,
				p.image_url,
				p.description,
				p.original_price_amount,
				p.status,
				(p.age_restricted OR c.age_restricted) AS age_restricted
			`).
			Joins("JOIN categories c ON c.id = p.category_id AND c.status = 'active' AND c.deleted_at IS NULL").
			Where("p.id = ? AND p.status = 'on_sale' AND p.deleted_at IS NULL", productID).
			Limit(1).
			Scan(&row).Error
		if err != nil {
			return ProductRow{}, err
		}
		if row.ID == 0 {
			return ProductRow{}, gorm.ErrRecordNotFound
		}
		return row, nil
	}
	var row ProductRow
	err := r.db.WithContext(ctx).
		Table("shop_products sp").
		Select(`
			p.id,
			p.category_id,
			sp.shop_id,
			sp.id AS shop_product_id,
			p.name,
			p.brand_name,
			p.spec,
			p.image_url,
			p.description,
			sp.sale_price_amount,
			p.original_price_amount,
			p.status,
			sp.status AS shop_product_status,
			s.status AS shop_status,
			s.business_status,
			COALESCE(ps.available_qty, 0) AS available_qty,
			(p.age_restricted OR c.age_restricted) AS age_restricted
		`).
		Joins("JOIN products p ON p.id = sp.product_id AND p.deleted_at IS NULL").
		Joins("JOIN categories c ON c.id = p.category_id AND c.status = 'active' AND c.deleted_at IS NULL").
		Joins("JOIN shops s ON s.id = sp.shop_id AND s.deleted_at IS NULL").
		Joins("LEFT JOIN product_stocks ps ON ps.shop_product_id = sp.id AND ps.deleted_at IS NULL").
		Where("p.id = ? AND sp.shop_id = ? AND sp.deleted_at IS NULL", productID, shopID).
		Order("sp.sort_order ASC, sp.id ASC").
		Limit(1).
		Scan(&row).Error
	if err != nil {
		return ProductRow{}, err
	}
	if row.ID == 0 {
		return ProductRow{}, gorm.ErrRecordNotFound
	}
	return row, nil
}

var genericProductOrderColumns = map[string]string{
	"id": "p.id", "created_at": "p.created_at", "updated_at": "p.updated_at", "name": "p.name",
}

var genericProductFilterColumns = map[string]string{
	"id": "p.id", "status": "p.status", "category_id": "p.category_id", "name": "p.name",
}

var productOrderColumns = map[string]string{
	"id":                "sp.id",
	"shop_product_id":   "sp.id",
	"created_at":        "sp.created_at",
	"updated_at":        "sp.updated_at",
	"sort_order":        "sp.sort_order",
	"sale_price_amount": "sp.sale_price_amount",
	"available_qty":     "COALESCE(ps.available_qty, 0)",
	"name":              "p.name",
}

var productFilterColumns = map[string]string{
	"id":                "p.id",
	"shop_product_id":   "sp.id",
	"shop_id":           "sp.shop_id",
	"status":            "sp.status",
	"category_id":       "p.category_id",
	"name":              "p.name",
	"sale_price_amount": "sp.sale_price_amount",
	"available_qty":     "COALESCE(ps.available_qty, 0)",
}
