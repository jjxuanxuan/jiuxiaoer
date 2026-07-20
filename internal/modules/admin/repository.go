package admin

import (
	"context"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
)

type Repository struct {
	db *gorm.DB
}

// NewRepository 创建并初始化数据仓储。
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

// DB 返回当前数据仓储使用的数据库连接。
func (r *Repository) DB() *gorm.DB {
	return r.db
}

// 判断分类是否存在
func (r *Repository) RequireCategory(ctx context.Context, tx *gorm.DB, categoryID uint64) error {
	var count int64
	if err := tx.WithContext(ctx).Table("categories").Where("id = ? AND deleted_at IS NULL", categoryID).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// CreateProduct 创建商品。
func (r *Repository) CreateProduct(ctx context.Context, tx *gorm.DB, row Product) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// LockProduct 加锁并获取商品。
func (r *Repository) LockProduct(ctx context.Context, tx *gorm.DB, productID uint64) (Product, error) {
	var row Product
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND deleted_at IS NULL", productID).
		First(&row).Error
	return row, err
}

// UpdateProduct 更新商品。
func (r *Repository) UpdateProduct(ctx context.Context, tx *gorm.DB, productID uint64, values map[string]any) error {
	if len(values) == 0 {
		return nil
	}
	return tx.WithContext(ctx).Model(&Product{}).Where("id = ?", productID).Updates(values).Error
}

// ListOrders 查询订单列表。
func (r *Repository) ListOrders(ctx context.Context, status string, query pagination.Query) ([]Order, error) {
	db := r.db.WithContext(ctx).Where("deleted_at IS NULL")
	if status != "" {
		db = db.Where("status = ?", status)
	}
	db, err := pagination.ApplyFilter(db, query.Filter, adminOrderFilterColumns)
	if err != nil {
		return nil, err
	}
	db, err = pagination.ApplyOrder(db, query.OrderBy, adminOrderOrderColumns, "id DESC")
	if err != nil {
		return nil, err
	}
	var rows []Order
	err = db.Offset(query.Offset).Limit(query.PageSize + 1).Find(&rows).Error
	return rows, err
}

// GetOrder 获取订单。
func (r *Repository) GetOrder(ctx context.Context, orderID uint64) (Order, []OrderItem, error) {
	var row Order
	if err := r.db.WithContext(ctx).Where("id = ? AND deleted_at IS NULL", orderID).First(&row).Error; err != nil {
		return Order{}, nil, err
	}
	var items []OrderItem
	err := r.db.WithContext(ctx).Where("order_id = ? AND deleted_at IS NULL", orderID).Find(&items).Error
	return row, items, err
}

// ListStocks 查询Stocks列表。
func (r *Repository) ListStocks(ctx context.Context, shopID uint64, query pagination.Query) ([]StockRow, error) {
	db := r.db.WithContext(ctx).
		Table("product_stocks ps").
		Select(`
			ps.id,
			ps.shop_product_id,
			ps.shop_id,
			sp.merchant_id,
			ps.product_id,
			p.name AS product_name,
			ps.available_qty,
			ps.reserved_qty,
			ps.locked_qty,
			ps.low_stock_threshold,
			ps.version,
			ps.updated_at
		`).
		Joins("JOIN shop_products sp ON sp.id = ps.shop_product_id AND sp.deleted_at IS NULL").
		Joins("JOIN products p ON p.id = ps.product_id AND p.deleted_at IS NULL").
		Where("ps.deleted_at IS NULL")
	if shopID != 0 {
		db = db.Where("ps.shop_id = ?", shopID)
	}
	db, err := pagination.ApplyFilter(db, query.Filter, adminStockFilterColumns)
	if err != nil {
		return nil, err
	}
	db, err = pagination.ApplyOrder(db, query.OrderBy, adminStockOrderColumns, "ps.id ASC")
	if err != nil {
		return nil, err
	}
	var rows []StockRow
	err = db.Offset(query.Offset).Limit(query.PageSize + 1).Scan(&rows).Error
	return rows, err
}

// LockStockByShopProduct 加锁并获取库存 By 门店商品。
func (r *Repository) LockStockByShopProduct(ctx context.Context, tx *gorm.DB, shopProductID uint64) (ProductStock, error) {
	var row ProductStock
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("shop_product_id = ? AND deleted_at IS NULL", shopProductID).
		First(&row).Error
	return row, err
}

// AdjustStock 调整库存。
func (r *Repository) AdjustStock(ctx context.Context, tx *gorm.DB, stockID uint64, quantityDelta int) error {
	return tx.WithContext(ctx).Model(&ProductStock{}).
		Where("id = ?", stockID).
		Updates(map[string]any{
			"available_qty": gorm.Expr("available_qty + ?", quantityDelta),
			"version":       gorm.Expr("version + 1"),
		}).Error
}

// CreateStockRecord 创建库存记录。
func (r *Repository) CreateStockRecord(ctx context.Context, tx *gorm.DB, row StockRecord) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// ListMerchants 查询Merchants列表。
func (r *Repository) ListMerchants(ctx context.Context, reviewStatus string, query pagination.Query) ([]Merchant, error) {
	db := r.db.WithContext(ctx).Where("deleted_at IS NULL")
	if reviewStatus != "" {
		db = db.Where("review_status = ?", reviewStatus)
	}
	db, err := pagination.ApplyFilter(db, query.Filter, adminMerchantFilterColumns)
	if err != nil {
		return nil, err
	}
	db, err = pagination.ApplyOrder(db, query.OrderBy, adminMerchantOrderColumns, "id ASC")
	if err != nil {
		return nil, err
	}
	var rows []Merchant
	err = db.Offset(query.Offset).Limit(query.PageSize + 1).Find(&rows).Error
	return rows, err
}

// LockMerchant 加锁并获取商户。
func (r *Repository) LockMerchant(ctx context.Context, tx *gorm.DB, merchantID uint64) (Merchant, error) {
	var row Merchant
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND deleted_at IS NULL", merchantID).
		First(&row).Error
	return row, err
}

// UpdateMerchant 更新商户。
func (r *Repository) UpdateMerchant(ctx context.Context, tx *gorm.DB, merchantID uint64, values map[string]any) error {
	return tx.WithContext(ctx).Model(&Merchant{}).Where("id = ?", merchantID).Updates(values).Error
}

// ListAuditLogs 查询审计 Logs列表。
func (r *Repository) ListAuditLogs(ctx context.Context, query pagination.Query) ([]AuditLog, error) {
	db := r.db.WithContext(ctx).Where("deleted_at IS NULL")
	db, err := pagination.ApplyFilter(db, query.Filter, adminAuditFilterColumns)
	if err != nil {
		return nil, err
	}
	db, err = pagination.ApplyOrder(db, query.OrderBy, adminAuditOrderColumns, "id DESC")
	if err != nil {
		return nil, err
	}
	var rows []AuditLog
	err = db.Offset(query.Offset).Limit(query.PageSize + 1).Find(&rows).Error
	return rows, err
}

// CreateAuditLog 创建审计日志。
func (r *Repository) CreateAuditLog(ctx context.Context, tx *gorm.DB, row AuditLog) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// CreateOutbox 创建发件箱事件。
func (r *Repository) CreateOutbox(ctx context.Context, tx *gorm.DB, row OutboxEvent) error {
	return tx.WithContext(ctx).Create(&row).Error
}

var adminOrderOrderColumns = map[string]string{
	"id":              "id",
	"created_at":      "created_at",
	"updated_at":      "updated_at",
	"status":          "status",
	"pay_status":      "pay_status",
	"delivery_status": "delivery_status",
}

var adminOrderFilterColumns = map[string]string{
	"id":              "id",
	"customer_id":     "customer_id",
	"merchant_id":     "merchant_id",
	"shop_id":         "shop_id",
	"status":          "status",
	"pay_status":      "pay_status",
	"delivery_status": "delivery_status",
	"created_at":      "created_at",
}

var adminStockOrderColumns = map[string]string{
	"id":              "ps.id",
	"shop_product_id": "ps.shop_product_id",
	"updated_at":      "ps.updated_at",
	"available_qty":   "ps.available_qty",
	"reserved_qty":    "ps.reserved_qty",
	"locked_qty":      "ps.locked_qty",
	"product_name":    "p.name",
}

var adminStockFilterColumns = map[string]string{
	"id":              "ps.id",
	"shop_product_id": "ps.shop_product_id",
	"shop_id":         "ps.shop_id",
	"merchant_id":     "sp.merchant_id",
	"product_id":      "ps.product_id",
	"product_name":    "p.name",
	"name":            "p.name",
	"available_qty":   "ps.available_qty",
	"reserved_qty":    "ps.reserved_qty",
	"locked_qty":      "ps.locked_qty",
}

var adminMerchantOrderColumns = map[string]string{
	"id":            "id",
	"created_at":    "created_at",
	"updated_at":    "updated_at",
	"name":          "name",
	"status":        "status",
	"review_status": "review_status",
}

var adminMerchantFilterColumns = map[string]string{
	"id":            "id",
	"name":          "name",
	"status":        "status",
	"review_status": "review_status",
}

var adminAuditOrderColumns = map[string]string{
	"id":         "id",
	"created_at": "created_at",
	"updated_at": "updated_at",
}

var adminAuditFilterColumns = map[string]string{
	"id":            "id",
	"actor_type":    "actor_type",
	"actor_id":      "actor_id",
	"action":        "action",
	"resource_type": "resource_type",
	"resource_id":   "resource_id",
	"result":        "result",
	"created_at":    "created_at",
}
