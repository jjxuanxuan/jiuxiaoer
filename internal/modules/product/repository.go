package product

import (
	"context"

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
			p.name,
			p.brand_name,
			p.spec,
			p.image_url,
			p.description,
			sp.sale_price_amount,
			p.original_price_amount,
			(p.age_restricted OR c.age_restricted) AS age_restricted,
			sp.status,
			ps.available_qty
		`).
		Joins("JOIN products p ON p.id = sp.product_id AND p.deleted_at IS NULL").
		Joins("JOIN categories c ON c.id = p.category_id AND c.deleted_at IS NULL").
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
	db, err := pagination.ApplyFilter(db, query.Filter, productFilterColumns)
	if err != nil {
		return nil, err
	}
	db, err = pagination.ApplyOrder(db, query.OrderBy, productOrderColumns, "sp.sort_order ASC, sp.id ASC")
	if err != nil {
		return nil, err
	}

	var rows []ProductRow
	err = db.Offset(query.Offset).Limit(query.PageSize + 1).Scan(&rows).Error
	return rows, err
}

// listGenericProducts 查询Generic 商品列表。
func (r *Repository) listGenericProducts(ctx context.Context, query ListQuery) ([]ProductRow, error) {
	db := r.db.WithContext(ctx).Table("products p").Select(`
		p.id, p.category_id, p.name, p.brand_name, p.spec, p.image_url,
		p.description, p.original_price_amount, p.status,
		(p.age_restricted OR c.age_restricted) AS age_restricted
	`).Joins("JOIN categories c ON c.id = p.category_id AND c.deleted_at IS NULL").Where("p.status = 'on_sale' AND p.deleted_at IS NULL")
	if query.CategoryID != "" {
		db = db.Where("p.category_id = ?", query.CategoryID)
	}
	if query.Keyword != "" {
		db = db.Where("p.name LIKE ? OR p.brand_name LIKE ?", "%"+query.Keyword+"%", "%"+query.Keyword+"%")
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
	err = db.Offset(query.Offset).Limit(query.PageSize + 1).Scan(&rows).Error
	return rows, err
}

// GetPublicProduct 获取公开数据商品。
func (r *Repository) GetPublicProduct(ctx context.Context, productID, shopID uint64) (ProductRow, error) {
	if shopID == 0 {
		var product Product
		err := r.db.WithContext(ctx).Where("id = ? AND status = 'on_sale' AND deleted_at IS NULL", productID).Take(&product).Error
		if err != nil {
			return ProductRow{}, err
		}
		return ProductRow{ID: product.ID, CategoryID: product.CategoryID, Name: product.Name, BrandName: product.BrandName, Spec: product.Spec, ImageURL: product.ImageURL, Description: product.Description, OriginalPriceAmount: product.OriginalPriceAmount, Status: product.Status}, nil
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
			sp.status,
			ps.available_qty
		`).
		Joins("JOIN products p ON p.id = sp.product_id AND p.deleted_at IS NULL").
		Joins("JOIN shops s ON s.id = sp.shop_id AND s.deleted_at IS NULL").
		Joins("LEFT JOIN product_stocks ps ON ps.shop_product_id = sp.id AND ps.deleted_at IS NULL").
		Where("p.id = ? AND sp.shop_id = ? AND sp.deleted_at IS NULL AND sp.status = 'on_sale' AND p.status = 'on_sale' AND s.status = 'active' AND s.business_status = 'open'", productID, shopID).
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
