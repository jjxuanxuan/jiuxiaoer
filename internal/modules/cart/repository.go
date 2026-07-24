package cart

import (
	"context"
	"errors"

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
	var item CartItem
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("cart_id=? AND shop_product_id=? AND deleted_at IS NULL", cartID, shopProductID).
		First(&item).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	return item.Quantity, err
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
