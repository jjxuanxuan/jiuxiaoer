package cart

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

// EnsureCart 确保购物车存在且处于可用状态。
func (r *Repository) EnsureCart(ctx context.Context, tx *gorm.DB, customerID uint64, nextID func() uint64) (Cart, error) {
	var cart Cart
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("customer_id = ? AND deleted_at IS NULL", customerID).First(&cart).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		cart = Cart{ID: nextID(), CustomerID: customerID}
		if err := tx.WithContext(ctx).Create(&cart).Error; err != nil {
			return Cart{}, err
		}
		return cart, nil
	}
	return cart, err
}

// HasSelectedOtherShop 判断是否存在选中状态 Other 门店。
func (r *Repository) HasSelectedOtherShop(ctx context.Context, tx *gorm.DB, cartID, shopID uint64) (bool, error) {
	var count int64
	err := tx.WithContext(ctx).Model(&CartItem{}).
		Where("cart_id = ? AND shop_id <> ? AND selected = 1 AND deleted_at IS NULL", cartID, shopID).Count(&count).Error
	return count > 0, err
}

// SaleableShopProduct 返回可售门店商品。
func (r *Repository) SaleableShopProduct(ctx context.Context, tx *gorm.DB, shopProductID uint64) (ShopProductRow, error) {
	var row ShopProductRow
	err := tx.WithContext(ctx).
		Table("shop_products sp").
		Select(`
			sp.id AS shop_product_id,
			sp.shop_id,
			sp.product_id,
			p.name,
			p.brand_name,
			p.spec,
			p.image_url,
			sp.sale_price_amount,
			p.status AS product_status,
			c.status AS category_status,
			sp.status AS shop_product_status,
			s.status AS shop_status,
			s.business_status,
			COALESCE(ps.available_qty, 0) AS available_qty
		`).
		Joins("JOIN products p ON p.id = sp.product_id AND p.deleted_at IS NULL").
		Joins("JOIN categories c ON c.id = p.category_id AND c.deleted_at IS NULL").
		Joins("JOIN shops s ON s.id = sp.shop_id AND s.deleted_at IS NULL").
		Joins("LEFT JOIN product_stocks ps ON ps.shop_product_id = sp.id AND ps.deleted_at IS NULL").
		Where("sp.id = ? AND sp.deleted_at IS NULL", shopProductID).
		Scan(&row).Error
	if err != nil {
		return ShopProductRow{}, err
	}
	if row.ShopProductID == 0 {
		return ShopProductRow{}, gorm.ErrRecordNotFound
	}
	return row, nil
}

// CartItemQuantity 锁定当前购物车明细，使增量请求可以校验结果数量，
// 而不是悄然将其截断为 99。
func (r *Repository) CartItemQuantity(ctx context.Context, tx *gorm.DB, cartID, shopProductID uint64) (int, error) {
	item, found, err := r.LockCartItem(ctx, tx, cartID, shopProductID)
	if err != nil || !found {
		return 0, err
	}
	return item.Quantity, nil
}

// LockCartItem 返回购物车商品的数量和选中状态，供批量复购计算最终状态。
func (r *Repository) LockCartItem(ctx context.Context, tx *gorm.DB, cartID, shopProductID uint64) (CartItem, bool, error) {
	var item CartItem
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("cart_id=? AND shop_product_id=? AND deleted_at IS NULL", cartID, shopProductID).
		First(&item).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return CartItem{}, false, nil
	}
	return item, err == nil, err
}

// LockCustomerCartItem 在更新数量前于 SQL 内解析所有权。
func (r *Repository) LockCustomerCartItem(ctx context.Context, tx *gorm.DB, customerID, itemID uint64) (CartItem, error) {
	var item CartItem
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Table("cart_items ci").
		Select("ci.*").
		Joins("JOIN carts c ON c.id=ci.cart_id AND c.deleted_at IS NULL").
		Where("ci.id=? AND c.customer_id=? AND ci.deleted_at IS NULL", itemID, customerID).
		Take(&item).Error
	return item, err
}

// AddItem 添加明细。
func (r *Repository) AddItem(ctx context.Context, tx *gorm.DB, cartID uint64, product ShopProductRow, quantity int, nextID func() uint64) error {
	return tx.WithContext(ctx).Exec(`
		INSERT INTO cart_items (id, cart_id, shop_product_id, shop_id, product_id, quantity, selected)
		VALUES (?, ?, ?, ?, ?, ?, 1)
		ON DUPLICATE KEY UPDATE quantity = LEAST(quantity + VALUES(quantity), 99), selected = 1, updated_at = CURRENT_TIMESTAMP(3)
	`, nextID(), cartID, product.ShopProductID, product.ShopID, product.ProductID, quantity).Error
}

// UpdateItem 更新明细。
func (r *Repository) UpdateItem(ctx context.Context, tx *gorm.DB, customerID uint64, itemID uint64, quantity int) error {
	result := tx.WithContext(ctx).Exec(`
		UPDATE cart_items ci
		JOIN carts c ON c.id = ci.cart_id AND c.deleted_at IS NULL
		SET ci.quantity = ?, ci.updated_at = CURRENT_TIMESTAMP(3)
		WHERE ci.id = ? AND c.customer_id = ? AND ci.deleted_at IS NULL
	`, quantity, itemID, customerID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// DeleteItem 删除明细。
func (r *Repository) DeleteItem(ctx context.Context, tx *gorm.DB, customerID uint64, itemID uint64) error {
	result := tx.WithContext(ctx).Exec(`
		DELETE ci FROM cart_items ci
		JOIN carts c ON c.id = ci.cart_id AND c.deleted_at IS NULL
		WHERE ci.id = ? AND c.customer_id = ?
	`, itemID, customerID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// SetItemSelection 设置明细选中状态。
func (r *Repository) SetItemSelection(ctx context.Context, tx *gorm.DB, customerID, itemID uint64, selected bool) error {
	result := tx.WithContext(ctx).Exec(`
		UPDATE cart_items ci JOIN carts c ON c.id = ci.cart_id AND c.deleted_at IS NULL
		SET ci.selected = ?, ci.updated_at = CURRENT_TIMESTAMP(3)
		WHERE ci.id = ? AND c.customer_id = ? AND ci.deleted_at IS NULL
	`, selected, itemID, customerID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// SetShopSelection 设置门店选中状态。
func (r *Repository) SetShopSelection(ctx context.Context, tx *gorm.DB, customerID, shopID uint64, selected bool) error {
	return tx.WithContext(ctx).Exec(`
		UPDATE cart_items ci JOIN carts c ON c.id = ci.cart_id AND c.deleted_at IS NULL
		SET ci.selected = ?, ci.updated_at = CURRENT_TIMESTAMP(3)
		WHERE c.customer_id = ? AND ci.shop_id = ? AND ci.deleted_at IS NULL
	`, selected, customerID, shopID).Error
}

// ClearItems 清空明细。
func (r *Repository) ClearItems(ctx context.Context, tx *gorm.DB, customerID uint64, shopID *uint64) error {
	query := `DELETE ci FROM cart_items ci JOIN carts c ON c.id = ci.cart_id AND c.deleted_at IS NULL WHERE c.customer_id = ?`
	args := []any{customerID}
	if shopID != nil {
		query += " AND ci.shop_id = ?"
		args = append(args, *shopID)
	}
	return tx.WithContext(ctx).Exec(query, args...).Error
}

// ListItems 查询明细列表。
func (r *Repository) ListItems(ctx context.Context, db *gorm.DB, customerID uint64) ([]CartItemRow, error) {
	var rows []CartItemRow
	err := db.WithContext(ctx).
		Table("cart_items ci").
		Select(`
			ci.id,
			ci.shop_product_id,
			ci.shop_id,
			ci.product_id,
			COALESCE(p.name, '') AS name,
			p.brand_name,
			p.spec,
			p.image_url,
			ci.quantity,
			CASE
				WHEN sp.id IS NOT NULL AND p.id IS NOT NULL AND cat.id IS NOT NULL AND s.id IS NOT NULL
				THEN sp.sale_price_amount
				ELSE 0
			END AS sale_price_amount,
			ci.selected,
			COALESCE(p.status, '') AS product_status,
			COALESCE(cat.status, '') AS category_status,
			COALESCE(sp.status, '') AS shop_product_status,
			COALESCE(s.status, '') AS shop_status,
			COALESCE(s.business_status, '') AS business_status,
			COALESCE(ps.available_qty, 0) AS available_qty
		`).
		Joins("JOIN carts c ON c.id = ci.cart_id AND c.deleted_at IS NULL").
		Joins("LEFT JOIN shop_products sp ON sp.id = ci.shop_product_id AND sp.shop_id = ci.shop_id AND sp.product_id = ci.product_id AND sp.deleted_at IS NULL").
		Joins("LEFT JOIN products p ON p.id = ci.product_id AND p.deleted_at IS NULL").
		Joins("LEFT JOIN categories cat ON cat.id = p.category_id AND cat.deleted_at IS NULL").
		Joins("LEFT JOIN shops s ON s.id = ci.shop_id AND s.deleted_at IS NULL").
		Joins("LEFT JOIN product_stocks ps ON ps.shop_product_id = ci.shop_product_id AND ps.deleted_at IS NULL").
		Where("c.customer_id = ? AND ci.deleted_at IS NULL", customerID).
		Order("ci.updated_at DESC, ci.id DESC").
		Scan(&rows).Error
	return rows, err
}

// ListFrequentPurchases 聚合指定客户最近已完成订单中的商品，并将其映射到
// 当前服务门店。已经从当前商品主数据中删除的商品不会重新暴露。
func (r *Repository) ListFrequentPurchases(ctx context.Context, customerID, shopID uint64, since time.Time, limit int) ([]FrequentPurchaseRow, error) {
	var rows []FrequentPurchaseRow
	err := r.db.WithContext(ctx).Raw(`
		WITH ranked_history AS (
			SELECT
				oi.product_id,
				oi.order_id,
				oi.quantity,
				oi.sale_price_amount,
				o.completed_at,
				ROW_NUMBER() OVER (
					PARTITION BY oi.product_id
					ORDER BY o.completed_at DESC, o.id DESC, oi.id DESC
				) AS history_rank
			FROM orders o
			JOIN order_items oi ON oi.order_id = o.id AND oi.deleted_at IS NULL
			WHERE o.customer_id = ?
			  AND o.status = 'completed'
			  AND o.completed_at >= ?
			  AND o.deleted_at IS NULL
		),
		history AS (
			SELECT
				product_id,
				COUNT(DISTINCT order_id) AS purchase_count,
				SUM(quantity) AS purchased_quantity,
				MAX(completed_at) AS last_purchased_at,
				MAX(CASE WHEN history_rank = 1 THEN quantity END) AS last_quantity,
				MAX(CASE WHEN history_rank = 1 THEN sale_price_amount END) AS last_sale_price_amount
			FROM ranked_history
			GROUP BY product_id
		)
		SELECT
			h.product_id,
			COALESCE(sp.id, 0) AS shop_product_id,
			? AS shop_id,
			p.name,
			p.brand_name,
			p.spec,
			p.image_url,
			h.purchase_count,
			h.purchased_quantity,
			h.last_quantity,
			h.last_sale_price_amount,
			h.last_purchased_at,
			COALESCE(sp.sale_price_amount, 0) AS sale_price_amount,
			p.status AS product_status,
			COALESCE(c.status, '') AS category_status,
			COALESCE(sp.status, '') AS shop_product_status,
			COALESCE(s.status, '') AS shop_status,
			COALESCE(s.business_status, '') AS business_status,
			COALESCE(ps.available_qty, 0) AS available_qty
		FROM history h
		JOIN products p ON p.id = h.product_id AND p.deleted_at IS NULL
		LEFT JOIN categories c ON c.id = p.category_id AND c.deleted_at IS NULL
		LEFT JOIN shop_products sp
			ON sp.shop_id = ? AND sp.product_id = h.product_id AND sp.deleted_at IS NULL
		LEFT JOIN shops s ON s.id = ? AND s.deleted_at IS NULL
		LEFT JOIN product_stocks ps ON ps.shop_product_id = sp.id AND ps.deleted_at IS NULL
		ORDER BY h.purchase_count DESC, h.last_purchased_at DESC, h.product_id DESC
		LIMIT ?
	`, customerID, since, shopID, shopID, shopID, limit).Scan(&rows).Error
	return rows, err
}

// ShopProductsByProductIDs 返回当前门店内与商品主键对应的门店商品和实时库存。
func (r *Repository) ShopProductsByProductIDs(ctx context.Context, tx *gorm.DB, shopID uint64, productIDs []uint64) ([]ShopProductRow, error) {
	if len(productIDs) == 0 {
		return []ShopProductRow{}, nil
	}
	var rows []ShopProductRow
	err := tx.WithContext(ctx).
		Table("shop_products sp").
		Select(`
			sp.id AS shop_product_id,
			sp.shop_id,
			sp.product_id,
			p.name,
			p.brand_name,
			p.spec,
			p.image_url,
			sp.sale_price_amount,
			p.status AS product_status,
			c.status AS category_status,
			sp.status AS shop_product_status,
			s.status AS shop_status,
			s.business_status,
			COALESCE(ps.available_qty, 0) AS available_qty
		`).
		Joins("JOIN products p ON p.id = sp.product_id AND p.deleted_at IS NULL").
		Joins("JOIN categories c ON c.id = p.category_id AND c.deleted_at IS NULL").
		Joins("JOIN shops s ON s.id = sp.shop_id AND s.deleted_at IS NULL").
		Joins("LEFT JOIN product_stocks ps ON ps.shop_product_id = sp.id AND ps.deleted_at IS NULL").
		Where("sp.shop_id = ? AND sp.product_id IN ? AND sp.deleted_at IS NULL", shopID, productIDs).
		Scan(&rows).Error
	return rows, err
}

// DeselectOtherShops 在用户明确允许替换选中门店时取消其他门店商品的选中状态。
func (r *Repository) DeselectOtherShops(ctx context.Context, tx *gorm.DB, cartID, shopID uint64) error {
	return tx.WithContext(ctx).Model(&CartItem{}).
		Where("cart_id = ? AND shop_id <> ? AND selected = 1 AND deleted_at IS NULL", cartID, shopID).
		Updates(map[string]any{"selected": false}).Error
}

// SetTargetItemQuantity 将一键复购商品设置为目标数量并选中。
// EnsureCart 对购物车行的锁保证同一客户的批量变更串行执行。
func (r *Repository) SetTargetItemQuantity(ctx context.Context, tx *gorm.DB, cartID uint64, product ShopProductRow, quantity int, nextID func() uint64) error {
	var item CartItem
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("cart_id = ? AND shop_product_id = ? AND deleted_at IS NULL", cartID, product.ShopProductID).
		Take(&item).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return tx.WithContext(ctx).Create(&CartItem{
			ID:            nextID(),
			CartID:        cartID,
			ShopProductID: product.ShopProductID,
			ShopID:        product.ShopID,
			ProductID:     product.ProductID,
			Quantity:      quantity,
			Selected:      true,
		}).Error
	}
	if err != nil {
		return err
	}
	return tx.WithContext(ctx).Model(&CartItem{}).Where("id = ?", item.ID).
		Updates(map[string]any{"quantity": quantity, "selected": true}).Error
}
