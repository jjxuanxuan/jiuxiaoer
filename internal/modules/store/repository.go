package store

import (
	"context"
	"strings"
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
func (r *Repository) ListOrders(ctx context.Context, merchantID uint64, shopIDs []uint64, filters StoreOrderListFilters, query pagination.Query) ([]Order, error) {
	// 必须同时校验 merchant_id 和授权 shop_id；只校验 merchant_id 对员工账号来说范围过大。
	db := r.db.WithContext(ctx).
		Table("orders").
		Select(`
			orders.*,
			delivery.scheduled_start_at AS delivery_scheduled_start_at,
			delivery.scheduled_end_at AS delivery_scheduled_end_at,
			delivery.not_before_at AS delivery_not_before_at
		`).
		Joins(`
			LEFT JOIN delivery_orders delivery
			  ON delivery.order_id = orders.id
			 AND delivery.deleted_at IS NULL
		`).
		Where(
			"orders.merchant_id = ? AND orders.shop_id IN ? AND orders.deleted_at IS NULL",
			merchantID,
			shopIDs,
		)
	if filters.ShopID != 0 {
		db = db.Where("orders.shop_id = ?", filters.ShopID)
	}
	if filters.Status != "" {
		db = db.Where("orders.status = ?", filters.Status)
	}
	if filters.OrderNo != "" {
		db = db.Where("orders.order_no = ?", filters.OrderNo)
	}
	if filters.Keyword != "" {
		db = db.Where("orders.order_no LIKE ?", escapeLike(filters.Keyword)+"%")
	}
	if filters.PaidFrom != nil {
		db = db.Where("orders.paid_at >= ?", *filters.PaidFrom)
	}
	if filters.PaidTo != nil {
		db = db.Where("orders.paid_at <= ?", *filters.PaidTo)
	}
	db, err := pagination.ApplyOrder(db, query.OrderBy, storeOrderOrderColumns, "created_at DESC, id DESC")
	if err != nil {
		return nil, err
	}
	if storeOrderUsesKeyset(query.OrderBy) {
		db, err = pagination.ApplyTimeIDCursor(
			db,
			query,
			"orders.created_at",
			"orders.id",
			"desc",
		)
		if err != nil {
			return nil, err
		}
	}
	var rows []Order
	err = pagination.OffsetDB(db, query).Limit(query.PageSize + 1).Find(&rows).Error
	return rows, err
}

// OrderItemsForOrders 批量读取列表摘要所需的历史商品快照，避免按订单 N+1 查询。
func (r *Repository) OrderItemsForOrders(ctx context.Context, orderIDs []uint64) ([]OrderItem, error) {
	if len(orderIDs) == 0 {
		return []OrderItem{}, nil
	}
	var rows []OrderItem
	err := r.db.WithContext(ctx).
		Where("order_id IN ? AND deleted_at IS NULL", orderIDs).
		Order("order_id ASC, id ASC").
		Find(&rows).Error
	return rows, err
}

// AuthorizedShops 批量读取订单列表中的门店摘要，并保持商户对象边界。
func (r *Repository) AuthorizedShops(ctx context.Context, merchantID uint64, shopIDs []uint64) ([]Shop, error) {
	if len(shopIDs) == 0 {
		return []Shop{}, nil
	}
	var rows []Shop
	err := r.db.WithContext(ctx).
		Where("merchant_id = ? AND id IN ? AND deleted_at IS NULL", merchantID, shopIDs).
		Find(&rows).Error
	return rows, err
}

// AuthorizedOrder 按商户和授权门店范围读取订单。范围外订单与不存在订单
// 使用同一查询结果，避免详情接口泄露对象是否存在。
func (r *Repository) AuthorizedOrder(ctx context.Context, db *gorm.DB, merchantID uint64, shopIDs []uint64, orderID uint64) (Order, error) {
	var row Order
	err := db.WithContext(ctx).
		Where("id = ? AND merchant_id = ? AND shop_id IN ? AND deleted_at IS NULL", orderID, merchantID, shopIDs).
		First(&row).Error
	return row, err
}

// OrderShop 返回订单关联的门店摘要。
func (r *Repository) OrderShop(ctx context.Context, db *gorm.DB, merchantID, shopID uint64) (Shop, error) {
	var row Shop
	err := db.WithContext(ctx).Where("id = ? AND merchant_id = ?", shopID, merchantID).First(&row).Error
	return row, err
}

// PaymentByOrder 返回订单最近一条有效支付事实。
func (r *Repository) PaymentByOrder(ctx context.Context, db *gorm.DB, orderID uint64) (Payment, error) {
	var row Payment
	err := db.WithContext(ctx).
		Where("order_id = ? AND deleted_at IS NULL", orderID).
		Order("created_at DESC, id DESC").
		First(&row).Error
	return row, err
}

// DeliveryByOrder 返回订单当前配送事实。
func (r *Repository) DeliveryByOrder(ctx context.Context, db *gorm.DB, orderID uint64) (DeliveryOrder, error) {
	var row DeliveryOrder
	err := db.WithContext(ctx).
		Where("order_id = ? AND deleted_at IS NULL", orderID).
		First(&row).Error
	return row, err
}

// RecentOrderLogs 返回商家可见详情所需的最近状态日志。Service 只投影安全字段。
func (r *Repository) RecentOrderLogs(ctx context.Context, db *gorm.DB, orderID uint64, limit int) ([]OrderLog, error) {
	var rows []OrderLog
	err := db.WithContext(ctx).
		Where("order_id = ? AND deleted_at IS NULL", orderID).
		Order("created_at DESC, id DESC").
		Limit(limit).
		Find(&rows).Error
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

// TransitionOrder 仅从调用方已锁定并观察到的状态和版本原子推进订单。
// 即使使用 SELECT FOR UPDATE 也保留条件更新，使乐观并发控制成为明确的
// 数据库不变量，而不仅是服务层检查。
func (r *Repository) TransitionOrder(ctx context.Context, tx *gorm.DB, orderID uint64, expectedStatus string, expectedVersion int, values map[string]any) (bool, error) {
	updates := make(map[string]any, len(values)+1)
	for key, value := range values {
		updates[key] = value
	}
	updates["version"] = gorm.Expr("version + 1")
	result := tx.WithContext(ctx).Model(&Order{}).
		Where("id = ? AND status = ? AND version = ? AND deleted_at IS NULL", orderID, expectedStatus, expectedVersion).
		Updates(updates)
	return result.RowsAffected == 1, result.Error
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

// LockDeliveryByOrder 按订单加锁并获取配送记录。
func (r *Repository) LockDeliveryByOrder(ctx context.Context, tx *gorm.DB, orderID uint64) (DeliveryOrder, error) {
	var row DeliveryOrder
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("order_id=? AND deleted_at IS NULL", orderID).First(&row).Error
	return row, err
}

// DeliveryIDByOrder 是无锁探测，只用于确定配送优先的锁目标。
// 调用方在 LockDeliveryByID 后必须重新读取并校验关系。
func (r *Repository) DeliveryIDByOrder(
	ctx context.Context,
	db *gorm.DB,
	orderID uint64,
) (uint64, error) {
	var row struct {
		ID uint64
	}
	err := db.WithContext(ctx).Table("delivery_orders").
		Select("id").
		Where("order_id = ? AND deleted_at IS NULL", orderID).
		Take(&row).Error
	return row.ID, err
}

func (r *Repository) LockDeliveryByID(
	ctx context.Context,
	tx *gorm.DB,
	deliveryID uint64,
) (DeliveryOrder, error) {
	var row DeliveryOrder
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND deleted_at IS NULL", deliveryID).
		Take(&row).Error
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

// ProductIDsByShop 返回门店商品 ID 列表。
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
func (r *Repository) ListShopProducts(ctx context.Context, merchantID uint64, shopIDs []uint64, filters StoreInventoryFilters, query pagination.Query) ([]ShopProductRow, error) {
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
			COALESCE(ps.locked_qty, 0) AS locked_qty,
			COALESCE(ps.low_stock_threshold, 0) AS low_stock_threshold,
			COALESCE(ps.version, 0) AS version,
			ps.updated_at AS stock_updated_at,
			sp.updated_at AS shop_product_updated_at
		`).
		Joins("JOIN products p ON p.id = sp.product_id AND p.deleted_at IS NULL").
		Joins("LEFT JOIN product_stocks ps ON ps.shop_product_id = sp.id AND ps.deleted_at IS NULL").
		Where("sp.merchant_id = ? AND sp.shop_id IN ? AND sp.deleted_at IS NULL", merchantID, shopIDs)
	if filters.ShopID != 0 {
		db = db.Where("sp.shop_id = ?", filters.ShopID)
	}
	if filters.Status != "" {
		db = db.Where("sp.status = ?", filters.Status)
	}
	if filters.Keyword != "" {
		keyword := "%" + escapeLike(filters.Keyword) + "%"
		db = db.Where("(p.name LIKE ? OR p.brand_name LIKE ? OR p.spec LIKE ?)", keyword, keyword, keyword)
	}
	if filters.LowStockOnly {
		db = db.Where("COALESCE(ps.available_qty, 0) <= COALESCE(ps.low_stock_threshold, 0)")
	}
	db, err := pagination.ApplyOrder(db, query.OrderBy, storeShopProductOrderColumns, "COALESCE(ps.updated_at, sp.updated_at) DESC, sp.id DESC")
	if err != nil {
		return nil, err
	}
	if storeInventoryUsesKeyset(query.OrderBy) {
		db, err = pagination.ApplyTimeIDCursor(db, query, "COALESCE(ps.updated_at, sp.updated_at)", "sp.id", "desc")
		if err != nil {
			return nil, err
		}
	}
	var rows []ShopProductRow
	err = pagination.OffsetDB(db, query).Limit(query.PageSize + 1).Scan(&rows).Error
	for i := range rows {
		rows[i].UpdatedAt = rows[i].ShopProductUpdatedAt
		if rows[i].StockUpdatedAt != nil {
			rows[i].UpdatedAt = *rows[i].StockUpdatedAt
		}
	}
	return rows, err
}

func escapeLike(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

func storeOrderUsesKeyset(orderBy string) bool {
	orderBy = strings.ToLower(strings.TrimSpace(orderBy))
	return orderBy == "" || orderBy == "created_at desc,id desc"
}

func storeInventoryUsesKeyset(orderBy string) bool {
	orderBy = strings.ToLower(strings.TrimSpace(orderBy))
	return orderBy == "" || orderBy == "updated_at desc,id desc"
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
	"id":              "orders.id",
	"created_at":      "orders.created_at",
	"updated_at":      "orders.updated_at",
	"status":          "orders.status",
	"pay_status":      "orders.pay_status",
	"delivery_status": "orders.delivery_status",
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
