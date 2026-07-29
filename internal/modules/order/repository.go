package order

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

// NewRepository 创建并初始化数据仓储。
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

// DB 返回当前数据仓储使用的数据库连接。
func (r *Repository) DB() *gorm.DB {
	return r.db
}

// GetAddress 获取地址。
func (r *Repository) GetAddress(ctx context.Context, tx *gorm.DB, customerID uint64, addressID uint64) (CustomerAddress, error) {
	var row CustomerAddress
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND customer_id = ? AND deleted_at IS NULL", addressID, customerID).
		First(&row).Error
	return row, err
}

// GetShopProductForOrder 获取门店商品 For 订单。
func (r *Repository) GetShopProductForOrder(ctx context.Context, tx *gorm.DB, shopID uint64, shopProductID uint64) (ShopProductRow, error) {
	var row ShopProductRow
	// 下单从 shop_products 读取，因为可售状态是门店级别的。
	err := tx.WithContext(ctx).
		Table("shop_products sp").
		Select(`
			sp.id AS shop_product_id,
			sp.shop_id,
			sp.merchant_id,
			sp.product_id,
			p.category_id,
			p.name,
			p.brand_name,
			p.spec,
			p.image_url,
			p.return_eligible,
			p.return_policy_code,
			p.return_policy_version,
			p.sealed_package_required,
			CASE WHEN p.age_restricted = 1 OR c.age_restricted = 1 THEN 1 ELSE 0 END AS age_restricted,
			sp.sale_price_amount,
			p.status AS product_status,
			c.status AS category_status,
			sp.status AS shop_product_status,
			s.status AS shop_status,
			s.business_status
		`).
		Joins("JOIN products p ON p.id = sp.product_id AND p.deleted_at IS NULL").
		Joins("JOIN categories c ON c.id = p.category_id AND c.deleted_at IS NULL").
		Joins("JOIN shops s ON s.id = sp.shop_id AND s.deleted_at IS NULL").
		Where("sp.id = ? AND sp.shop_id = ? AND sp.deleted_at IS NULL", shopProductID, shopID).
		Scan(&row).Error
	if err != nil {
		return ShopProductRow{}, err
	}
	if row.ShopProductID == 0 {
		return ShopProductRow{}, gorm.ErrRecordNotFound
	}
	return row, nil
}

// LockStock 使用 SELECT ... FOR UPDATE 串行化预占、释放和扣减库存操作。
func (r *Repository) LockStock(ctx context.Context, tx *gorm.DB, shopProductID uint64) (ProductStock, error) {
	var stock ProductStock
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("shop_product_id = ? AND deleted_at IS NULL", shopProductID).
		First(&stock).Error
	return stock, err
}

// ReserveStock 预留库存。
func (r *Repository) ReserveStock(ctx context.Context, tx *gorm.DB, stock ProductStock, quantity int) error {
	return tx.WithContext(ctx).Model(&ProductStock{}).
		Where("id = ?", stock.ID).
		Updates(map[string]any{
			"available_qty": gorm.Expr("available_qty - ?", quantity),
			"reserved_qty":  gorm.Expr("reserved_qty + ?", quantity),
			"version":       gorm.Expr("version + 1"),
		}).Error
}

// ReleaseStock 释放库存。
func (r *Repository) ReleaseStock(ctx context.Context, tx *gorm.DB, stock ProductStock, quantity int) error {
	return tx.WithContext(ctx).Model(&ProductStock{}).
		Where("id = ?", stock.ID).
		Updates(map[string]any{
			"available_qty": gorm.Expr("available_qty + ?", quantity),
			"reserved_qty":  gorm.Expr("reserved_qty - ?", quantity),
			"version":       gorm.Expr("version + 1"),
		}).Error
}

// DeductReservedStock 扣减Reserved 库存。
func (r *Repository) DeductReservedStock(ctx context.Context, tx *gorm.DB, stock ProductStock, quantity int) error {
	return tx.WithContext(ctx).Model(&ProductStock{}).
		Where("id = ?", stock.ID).
		Updates(map[string]any{
			"reserved_qty": gorm.Expr("reserved_qty - ?", quantity),
			"version":      gorm.Expr("version + 1"),
		}).Error
}

// CreateOrder 创建订单。
func (r *Repository) CreateOrder(ctx context.Context, tx *gorm.DB, row Order) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// CreateOrderItems 创建订单明细。
func (r *Repository) CreateOrderItems(ctx context.Context, tx *gorm.DB, rows []OrderItem) error {
	if len(rows) == 0 {
		return nil
	}
	return tx.WithContext(ctx).Create(&rows).Error
}

// CreateOrderLog 创建订单日志。
func (r *Repository) CreateOrderLog(ctx context.Context, tx *gorm.DB, row OrderLog) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// DeletePurchasedCartItems 删除Purchased 购物车明细。
// DeletePurchasedCartItems 只删除已提交订单所对应的购物车记录。
// 客户关联可防止一个客户结算时触碰另一客户的购物车，
// 即使两者包含相同的门店商品。
func (r *Repository) DeletePurchasedCartItems(ctx context.Context, tx *gorm.DB, customerID uint64, shopProductIDs []uint64) error {
	if len(shopProductIDs) == 0 {
		return nil
	}
	return tx.WithContext(ctx).Exec(`
		DELETE ci FROM cart_items ci
		JOIN carts c ON c.id = ci.cart_id AND c.deleted_at IS NULL
		WHERE c.customer_id = ? AND ci.shop_product_id IN ?
	`, customerID, shopProductIDs).Error
}

// CreateStockRecord 创建库存记录。
func (r *Repository) CreateStockRecord(ctx context.Context, tx *gorm.DB, row StockRecord) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// CreateOutbox 创建发件箱事件。
func (r *Repository) CreateOutbox(ctx context.Context, tx *gorm.DB, row OutboxEvent) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// ListCustomerOrders 查询用户订单列表。
func (r *Repository) ListCustomerOrders(ctx context.Context, customerID uint64, filters CustomerOrderListFilters, query pagination.Query) ([]Order, error) {
	db := r.db.WithContext(ctx).Where(
		"customer_id = ? AND order_type = ? AND settlement_mode = ? AND deleted_at IS NULL",
		customerID,
		retailOrderType,
		cashSettlementMode,
	)
	if filters.Status != "" {
		db = db.Where("status = ?", filters.Status)
	}
	db, err := pagination.ApplyTimeIDCursor(db, query, "created_at", "id", "desc")
	if err != nil {
		return nil, err
	}
	db, err = pagination.ApplyOrder(db, query.OrderBy, customerOrderOrderColumns, "created_at DESC, id DESC")
	if err != nil {
		return nil, err
	}
	var rows []Order
	err = pagination.OffsetDB(db, query).Limit(query.PageSize + 1).Find(&rows).Error
	return rows, err
}

// CustomerOrderItems 批量读取订单历史商品快照，避免列表摘要覆盖为当前商品资料。
func (r *Repository) CustomerOrderItems(ctx context.Context, orderIDs []uint64) ([]OrderItem, error) {
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

// OrderShops 批量读取订单引用的门店名称；历史软删除门店仍保留摘要。
func (r *Repository) OrderShops(ctx context.Context, shopIDs []uint64) ([]OrderShop, error) {
	if len(shopIDs) == 0 {
		return []OrderShop{}, nil
	}
	var rows []OrderShop
	err := r.db.WithContext(ctx).Where("id IN ?", shopIDs).Find(&rows).Error
	return rows, err
}

// OrderShopByID 返回单个订单的门店历史摘要。
func (r *Repository) OrderShopByID(ctx context.Context, shopID uint64) (OrderShop, error) {
	var row OrderShop
	err := r.db.WithContext(ctx).Where("id = ?", shopID).First(&row).Error
	return row, err
}

// LatestPaymentByOrder 返回订单最新支付事实，不返回 Provider 原始载荷。
func (r *Repository) LatestPaymentByOrder(ctx context.Context, orderID uint64) (Payment, error) {
	var row Payment
	err := r.db.WithContext(ctx).
		Where("order_id = ? AND deleted_at IS NULL", orderID).
		Order("created_at DESC, id DESC").
		First(&row).Error
	return row, err
}

// GetCustomerOrder 获取用户订单。
func (r *Repository) GetCustomerOrder(ctx context.Context, customerID uint64, orderID uint64) (Order, []OrderItem, error) {
	var row Order
	if err := r.db.WithContext(ctx).Where(
		"id = ? AND customer_id = ? AND order_type = ? AND settlement_mode = ? AND deleted_at IS NULL",
		orderID,
		customerID,
		retailOrderType,
		cashSettlementMode,
	).First(&row).Error; err != nil {
		return Order{}, nil, err
	}
	items, err := r.OrderItems(ctx, r.db, orderID)
	return row, items, err
}

// LockCustomerOrder 只锁定调用者自己的订单，用于 C 端操作。
func (r *Repository) LockCustomerOrder(ctx context.Context, tx *gorm.DB, customerID uint64, orderID uint64) (Order, error) {
	var row Order
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"id = ? AND customer_id = ? AND order_type = ? AND settlement_mode = ? AND deleted_at IS NULL",
			orderID,
			customerID,
			retailOrderType,
			cashSettlementMode,
		).
		First(&row).Error
	return row, err
}

// LockOrder 只用于已经通过权限校验的 admin 操作。
func (r *Repository) LockOrder(ctx context.Context, tx *gorm.DB, orderID uint64) (Order, error) {
	var row Order
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND deleted_at IS NULL", orderID).
		First(&row).Error
	return row, err
}

// LockNextExpiredOrder 加锁并获取Next 已过期订单。
func (r *Repository) LockNextExpiredOrder(ctx context.Context, tx *gorm.DB, now time.Time) (Order, error) {
	var row Order
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
		Where("status = 'pending_payment' AND expires_at IS NOT NULL AND expires_at <= ? AND deleted_at IS NULL", now).
		Order("expires_at ASC, id ASC").
		First(&row).Error
	return row, err
}

// NextExpiredOrder 返回Next 已过期订单。
func (r *Repository) NextExpiredOrder(ctx context.Context, now time.Time) (Order, error) {
	var row Order
	err := r.db.WithContext(ctx).
		Where("status = 'pending_payment' AND expires_at IS NOT NULL AND expires_at <= ? AND deleted_at IS NULL", now).
		Order("expires_at ASC, id ASC").First(&row).Error
	return row, err
}

// ClaimNextReconcilablePayment 认领一笔创建中或待处理支付，
// 且不会在请求服务商期间持续持有数据库锁。next_reconcile_at
// 既是短期租约，也是明确的重试计划。
func (r *Repository) ClaimNextReconcilablePayment(ctx context.Context, provider string, now, staleBefore time.Time) (Payment, error) {
	var payment Payment
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Table("payments AS p").Select("p.*").
			Joins("LEFT JOIN orders o ON o.id = p.order_id AND o.deleted_at IS NULL").
			Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where(`
				p.provider = ?
				AND p.status IN ?
				AND (
					(p.next_reconcile_at IS NOT NULL AND p.next_reconcile_at <= ?)
					OR (
						p.next_reconcile_at IS NULL
						AND p.status IN ('creating', 'pending')
						AND (p.status = 'pending' OR p.updated_at <= ?)
					)
				)
				AND p.deleted_at IS NULL
				AND (
					(
						COALESCE(p.biz_type, ?) = ?
						AND o.status = 'pending_payment'
						AND p.expires_at > ?
					)
					OR (
						p.biz_type IS NOT NULL
						AND p.biz_type <> ?
					)
				)
			`, provider, []string{"creating", "pending", "exception"}, now, staleBefore,
				RetailOrderPaymentBusiness, RetailOrderPaymentBusiness, now, RetailOrderPaymentBusiness).
			Order("COALESCE(p.next_reconcile_at,p.updated_at) ASC, p.id ASC").Take(&payment).Error; err != nil {
			return err
		}
		leaseUntil := now.Add(30 * time.Second)
		result := tx.Model(&Payment{}).Where(
			"id = ? AND status IN ?",
			payment.ID,
			[]string{"creating", "pending", "exception"},
		).
			Updates(map[string]any{"next_reconcile_at": leaseUntil, "reconcile_attempts": gorm.Expr("reconcile_attempts + 1"), "version": gorm.Expr("version + 1")})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		payment.ReconcileAttempts++
		payment.NextReconcileAt = &leaseUntil
		return nil
	})
	return payment, err
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

// ClosePendingPayments 关闭待处理 Payments并释放相关资源。
func (r *Repository) ClosePendingPayments(ctx context.Context, tx *gorm.DB, orderID uint64, now time.Time) error {
	return tx.WithContext(ctx).Model(&Payment{}).
		Where("order_id = ? AND status IN ? AND deleted_at IS NULL", orderID, []string{"creating", "pending"}).
		Updates(map[string]any{
			"status":          "closed",
			"provider_status": "CLOSED",
			"failed_at":       &now,
			"failure_code":    "ORDER_EXPIRED",
			"version":         gorm.Expr("version + 1"),
		}).Error
}

// CreatePayment 创建支付。
func (r *Repository) CreatePayment(ctx context.Context, tx *gorm.DB, row Payment) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// LockPaymentByOrderProvider 按订单和服务商加锁并获取支付记录。
func (r *Repository) LockPaymentByOrderProvider(ctx context.Context, tx *gorm.DB, orderID uint64, provider string) (Payment, error) {
	var payment Payment
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("order_id = ? AND provider = ? AND deleted_at IS NULL", orderID, provider).
		First(&payment).Error
	return payment, err
}

// LockPaymentByNo 按支付单号加锁并获取支付记录。
func (r *Repository) LockPaymentByNo(ctx context.Context, tx *gorm.DB, paymentNo string, provider string) (Payment, error) {
	var payment Payment
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("payment_no = ? AND provider = ? AND deleted_at IS NULL", paymentNo, provider).
		First(&payment).Error
	return payment, err
}

// LockPaymentByID 锁定一条公共支付记录，不假设其关联零售订单。
func (r *Repository) LockPaymentByID(ctx context.Context, tx *gorm.DB, paymentID uint64) (Payment, error) {
	var payment Payment
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND deleted_at IS NULL", paymentID).
		First(&payment).Error
	return payment, err
}

// GetPaymentByNo 按支付单号获取支付记录。
func (r *Repository) GetPaymentByNo(ctx context.Context, tx *gorm.DB, paymentNo string, provider string) (Payment, error) {
	var payment Payment
	err := tx.WithContext(ctx).
		Where("payment_no = ? AND provider = ? AND deleted_at IS NULL", paymentNo, provider).
		First(&payment).Error
	return payment, err
}

// GetPaymentByID 无锁加载一条公共支付记录。
func (r *Repository) GetPaymentByID(ctx context.Context, db *gorm.DB, paymentID uint64) (Payment, error) {
	if db == nil {
		db = r.db
	}
	var payment Payment
	err := db.WithContext(ctx).
		Where("id = ? AND deleted_at IS NULL", paymentID).
		First(&payment).Error
	return payment, err
}

// GetBusinessPayment 加载不可变的业务与支付关系。
func (r *Repository) GetBusinessPayment(ctx context.Context, bizType string, bizID uint64, provider string) (Payment, error) {
	var payment Payment
	err := r.db.WithContext(ctx).
		Where("biz_type = ? AND biz_id = ? AND provider = ? AND deleted_at IS NULL", bizType, bizID, provider).
		Order("id DESC").
		First(&payment).Error
	return payment, err
}

// GetCustomerPayment 获取用户支付。
func (r *Repository) GetCustomerPayment(ctx context.Context, customerID uint64, orderID uint64) (Payment, error) {
	var payment Payment
	err := r.db.WithContext(ctx).
		Where("customer_id = ? AND order_id = ? AND deleted_at IS NULL", customerID, orderID).
		Order("created_at DESC, id DESC").First(&payment).Error
	return payment, err
}

// GetPaymentByOrderProvider 按订单和服务商获取支付记录。
func (r *Repository) GetPaymentByOrderProvider(ctx context.Context, orderID uint64, provider string) (Payment, error) {
	var payment Payment
	err := r.db.WithContext(ctx).
		Where("order_id = ? AND provider = ? AND deleted_at IS NULL", orderID, provider).
		First(&payment).Error
	return payment, err
}

// UpdatePayment 更新支付。
func (r *Repository) UpdatePayment(ctx context.Context, tx *gorm.DB, paymentID uint64, values map[string]any) error {
	return tx.WithContext(ctx).Model(&Payment{}).Where("id = ?", paymentID).Updates(values).Error
}

// CustomerWeChatOpenID 返回用户 We Chat 打开ID。
func (r *Repository) CustomerWeChatOpenID(ctx context.Context, tx *gorm.DB, customerID uint64, appID string) (string, error) {
	var openID string
	err := tx.WithContext(ctx).Table("customer_identities").Select("provider_subject").
		Where("customer_id = ? AND provider = 'wechat_miniapp' AND app_id = ? AND status = 'active' AND deleted_at IS NULL", customerID, appID).
		Scan(&openID).Error
	if err != nil {
		return "", err
	}
	if openID == "" {
		return "", gorm.ErrRecordNotFound
	}
	return openID, nil
}

// CustomerPhoneBound 判断顾客是否已经完成手机号绑定。
func (r *Repository) CustomerPhoneBound(ctx context.Context, tx *gorm.DB, customerID uint64) (bool, error) {
	var count int64
	err := tx.WithContext(ctx).
		Table("customers").
		Where("id = ? AND TRIM(phone) <> '' AND deleted_at IS NULL", customerID).
		Count(&count).Error
	return count > 0, err
}

// CreatePaymentCallbackIfAbsent 创建支付回调 If Absent。
func (r *Repository) CreatePaymentCallbackIfAbsent(ctx context.Context, tx *gorm.DB, row PaymentCallback) (bool, error) {
	result := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	return result.RowsAffected == 1, result.Error
}

// PaymentCallbackByEvent 加载支付机构重复通知的持久化结果，
// 避免此前失败的回调被错误确认成功。
func (r *Repository) PaymentCallbackByEvent(ctx context.Context, tx *gorm.DB, provider, eventID string) (PaymentCallback, error) {
	var row PaymentCallback
	err := tx.WithContext(ctx).Where("provider=? AND provider_event_id=?", provider, eventID).Take(&row).Error
	return row, err
}

// UpdatePaymentCallback 更新支付回调。
func (r *Repository) UpdatePaymentCallback(ctx context.Context, tx *gorm.DB, callbackID uint64, values map[string]any) error {
	return tx.WithContext(ctx).Model(&PaymentCallback{}).Where("id = ?", callbackID).Updates(values).Error
}

// CreateAuditLog 创建审计日志。
func (r *Repository) CreateAuditLog(ctx context.Context, tx *gorm.DB, row AuditLog) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// GetPaymentByOrder 按订单获取支付记录。
func (r *Repository) GetPaymentByOrder(ctx context.Context, tx *gorm.DB, orderID uint64, channel string) (Payment, error) {
	var payment Payment
	err := tx.WithContext(ctx).Where("order_id = ? AND channel = ? AND deleted_at IS NULL", orderID, channel).First(&payment).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Payment{}, err
	}
	return payment, err
}

var customerOrderOrderColumns = map[string]string{
	"id":              "id",
	"created_at":      "created_at",
	"updated_at":      "updated_at",
	"status":          "status",
	"pay_status":      "pay_status",
	"delivery_status": "delivery_status",
}

var customerOrderFilterColumns = map[string]string{
	"id":              "id",
	"merchant_id":     "merchant_id",
	"shop_id":         "shop_id",
	"status":          "status",
	"pay_status":      "pay_status",
	"delivery_status": "delivery_status",
	"created_at":      "created_at",
}
