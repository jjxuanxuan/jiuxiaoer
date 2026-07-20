package store

import (
	"context"
	"time"

	"gorm.io/datatypes"
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

// ListOrders 查询订单列表。
func (r *Repository) ListOrders(ctx context.Context, merchantID uint64, shopIDs []uint64, status string, query pagination.Query) ([]Order, error) {
	// 必须同时校验 merchant_id 和授权 shop_id；只校验 merchant_id 对员工账号来说范围过大。
	db := r.db.WithContext(ctx).
		Where("merchant_id = ? AND shop_id IN ? AND deleted_at IS NULL", merchantID, shopIDs)
	if status != "" {
		db = db.Where("status = ?", status)
	}
	db, err := pagination.ApplyFilter(db, query.Filter, storeOrderFilterColumns)
	if err != nil {
		return nil, err
	}
	db, err = pagination.ApplyOrder(db, query.OrderBy, storeOrderOrderColumns, "id DESC")
	if err != nil {
		return nil, err
	}
	var rows []Order
	err = db.Offset(query.Offset).Limit(query.PageSize + 1).Find(&rows).Error
	return rows, err
}

// LockAuthorizedOrder 保护履约状态流转，并在同一个查询里校验商户/门店范围。
func (r *Repository) LockAuthorizedOrder(ctx context.Context, tx *gorm.DB, merchantID uint64, shopIDs []uint64, orderID uint64) (Order, error) {
	var row Order
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND merchant_id = ? AND shop_id IN ? AND deleted_at IS NULL", orderID, merchantID, shopIDs).
		First(&row).Error
	return row, err
}

// OrderItems 返回订单明细。
func (r *Repository) OrderItems(ctx context.Context, db *gorm.DB, orderID uint64) ([]OrderItem, error) {
	var rows []OrderItem
	err := db.WithContext(ctx).Where("order_id = ? AND deleted_at IS NULL", orderID).Find(&rows).Error
	return rows, err
}

// UpdateOrder 更新订单。
func (r *Repository) UpdateOrder(ctx context.Context, tx *gorm.DB, orderID uint64, values map[string]any) error {
	return tx.WithContext(ctx).Model(&Order{}).Where("id = ?", orderID).Updates(values).Error
}

// CreateOrderLog 创建订单日志。
func (r *Repository) CreateOrderLog(ctx context.Context, tx *gorm.DB, row OrderLog) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// CreateDeliveryOrder 保持幂等，因为商户备货完成请求可能在客户端超时后重试。
func (r *Repository) CreateDeliveryOrder(ctx context.Context, tx *gorm.DB, row DeliveryOrder) error {
	return tx.WithContext(ctx).Exec(`
		INSERT INTO delivery_orders
			(id, order_id, shop_id, status, pickup_snapshot, recipient_snapshot)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			status = VALUES(status),
			pickup_snapshot = VALUES(pickup_snapshot),
			recipient_snapshot = VALUES(recipient_snapshot),
			updated_at = CURRENT_TIMESTAMP(3)
	`, row.ID, row.OrderID, row.ShopID, row.Status, row.PickupSnapshot, row.RecipientSnapshot).Error
}

// LockDeliveryByOrder 加锁并获取配送 By 订单。
func (r *Repository) LockDeliveryByOrder(ctx context.Context, tx *gorm.DB, orderID uint64) (DeliveryOrder, error) {
	var row DeliveryOrder
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("order_id=? AND deleted_at IS NULL", orderID).First(&row).Error
	return row, err
}

// MarkDeliveryPickupReady 标记配送 Pickup 就绪状态的状态。
func (r *Repository) MarkDeliveryPickupReady(ctx context.Context, tx *gorm.DB, deliveryID uint64, pickupSnapshot, recipientSnapshot datatypes.JSON, readyAt time.Time) error {
	return tx.WithContext(ctx).Model(&DeliveryOrder{}).Where("id=?", deliveryID).Updates(map[string]any{
		"pickup_ready_status": "ready", "pickup_ready_at": &readyAt,
		"pickup_snapshot": pickupSnapshot, "recipient_snapshot": recipientSnapshot,
	}).Error
}

// LockAuthorizedShop 在修改门店前校验门店归属和员工授权。
func (r *Repository) LockAuthorizedShop(ctx context.Context, tx *gorm.DB, merchantID uint64, shopIDs []uint64, shopID uint64) (Shop, error) {
	var row Shop
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND merchant_id = ? AND id IN ? AND deleted_at IS NULL", shopID, merchantID, shopIDs).
		First(&row).Error
	return row, err
}

// UpdateShop 更新门店。
func (r *Repository) UpdateShop(ctx context.Context, tx *gorm.DB, shopID uint64, values map[string]any) error {
	return tx.WithContext(ctx).Model(&Shop{}).Where("id = ?", shopID).Updates(values).Error
}

// ProductIDsByShop 返回商品 I Ds By 门店。
func (r *Repository) ProductIDsByShop(ctx context.Context, db *gorm.DB, shopID uint64) ([]uint64, error) {
	var productIDs []uint64
	err := db.WithContext(ctx).
		Table("shop_products").
		Select("product_id").
		Where("shop_id = ? AND deleted_at IS NULL", shopID).
		Scan(&productIDs).Error
	return productIDs, err
}

// ListShopProducts 查询门店商品列表。
func (r *Repository) ListShopProducts(ctx context.Context, merchantID uint64, shopIDs []uint64, shopID uint64, query pagination.Query) ([]ShopProductRow, error) {
	db := r.db.WithContext(ctx).
		Table("shop_products sp").
		Select(`
			sp.id,
			sp.merchant_id,
			sp.shop_id,
			sp.product_id,
			p.category_id,
			p.name,
			p.brand_name,
			p.spec,
			p.image_url,
			sp.sale_price_amount,
			p.original_price_amount,
			p.age_restricted,
			sp.status,
			sp.sort_order,
			COALESCE(ps.available_qty, 0) AS available_qty,
			COALESCE(ps.reserved_qty, 0) AS reserved_qty,
			COALESCE(ps.locked_qty, 0) AS locked_qty
		`).
		Joins("JOIN products p ON p.id = sp.product_id AND p.deleted_at IS NULL").
		Joins("LEFT JOIN product_stocks ps ON ps.shop_product_id = sp.id AND ps.deleted_at IS NULL").
		Where("sp.merchant_id = ? AND sp.shop_id IN ? AND sp.deleted_at IS NULL", merchantID, shopIDs)
	if shopID != 0 {
		db = db.Where("sp.shop_id = ?", shopID)
	}
	db, err := pagination.ApplyFilter(db, query.Filter, storeShopProductFilterColumns)
	if err != nil {
		return nil, err
	}
	db, err = pagination.ApplyOrder(db, query.OrderBy, storeShopProductOrderColumns, "sp.sort_order ASC, sp.id ASC")
	if err != nil {
		return nil, err
	}
	var rows []ShopProductRow
	err = db.Offset(query.Offset).Limit(query.PageSize + 1).Scan(&rows).Error
	return rows, err
}

// Product 返回商品。
func (r *Repository) Product(ctx context.Context, tx *gorm.DB, productID uint64) (Product, error) {
	var row Product
	err := tx.WithContext(ctx).Where("id = ? AND deleted_at IS NULL", productID).First(&row).Error
	return row, err
}

// CreateShopProduct 创建门店商品。
func (r *Repository) CreateShopProduct(ctx context.Context, tx *gorm.DB, row ShopProduct) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// LockAuthorizedShopProduct 保护门店商品更新和库存调整。
func (r *Repository) LockAuthorizedShopProduct(ctx context.Context, tx *gorm.DB, merchantID uint64, shopIDs []uint64, shopProductID uint64) (ShopProduct, error) {
	var row ShopProduct
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND merchant_id = ? AND shop_id IN ? AND deleted_at IS NULL", shopProductID, merchantID, shopIDs).
		First(&row).Error
	return row, err
}

// UpdateShopProduct 更新门店商品。
func (r *Repository) UpdateShopProduct(ctx context.Context, tx *gorm.DB, shopProductID uint64, values map[string]any) error {
	if len(values) == 0 {
		return nil
	}
	return tx.WithContext(ctx).Model(&ShopProduct{}).Where("id = ?", shopProductID).Updates(values).Error
}

// CreateStock 创建库存。
func (r *Repository) CreateStock(ctx context.Context, tx *gorm.DB, row ProductStock) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// LockStock 将商户库存调整与订单/支付库存流程串行化。
func (r *Repository) LockStock(ctx context.Context, tx *gorm.DB, shopProductID uint64) (ProductStock, error) {
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

// CreateAuditLog 创建审计日志。
func (r *Repository) CreateAuditLog(ctx context.Context, tx *gorm.DB, row AuditLog) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// CreateOutbox 创建发件箱事件。
func (r *Repository) CreateOutbox(ctx context.Context, tx *gorm.DB, row OutboxEvent) error {
	return tx.WithContext(ctx).Create(&row).Error
}

var storeOrderOrderColumns = map[string]string{
	"id":              "id",
	"created_at":      "created_at",
	"updated_at":      "updated_at",
	"status":          "status",
	"pay_status":      "pay_status",
	"delivery_status": "delivery_status",
}

var storeOrderFilterColumns = map[string]string{
	"id":              "id",
	"customer_id":     "customer_id",
	"shop_id":         "shop_id",
	"status":          "status",
	"pay_status":      "pay_status",
	"delivery_status": "delivery_status",
	"created_at":      "created_at",
}

var storeShopProductOrderColumns = map[string]string{
	"id":                "sp.id",
	"shop_product_id":   "sp.id",
	"created_at":        "sp.created_at",
	"updated_at":        "sp.updated_at",
	"sort_order":        "sp.sort_order",
	"sale_price_amount": "sp.sale_price_amount",
	"available_qty":     "COALESCE(ps.available_qty, 0)",
	"name":              "p.name",
}

var storeShopProductFilterColumns = map[string]string{
	"id":                "sp.id",
	"shop_product_id":   "sp.id",
	"shop_id":           "sp.shop_id",
	"product_id":        "sp.product_id",
	"status":            "sp.status",
	"name":              "p.name",
	"sale_price_amount": "sp.sale_price_amount",
	"available_qty":     "COALESCE(ps.available_qty, 0)",
	"reserved_qty":      "COALESCE(ps.reserved_qty, 0)",
	"locked_qty":        "COALESCE(ps.locked_qty, 0)",
}
