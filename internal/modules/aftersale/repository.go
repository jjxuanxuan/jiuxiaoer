package aftersale

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Repository struct{ db *gorm.DB }

// NewRepository 创建并初始化数据仓储。
func NewRepository(db *gorm.DB) *Repository { return &Repository{db: db} }

// DB 返回当前数据仓储使用的数据库连接。
func (r *Repository) DB() *gorm.DB { return r.db }

// LockOrder 加锁并获取订单。
func (r *Repository) LockOrder(ctx context.Context, tx *gorm.DB, orderID uint64) (OrderRow, error) {
	var row OrderRow
	err := tx.WithContext(ctx).Table("orders").Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND deleted_at IS NULL", orderID).Take(&row).Error
	return row, err
}

// OrderItems 返回订单明细。
func (r *Repository) OrderItems(ctx context.Context, tx *gorm.DB, orderID uint64, ids []uint64) ([]OrderItemRow, error) {
	var rows []OrderItemRow
	err := tx.WithContext(ctx).Table("order_items").Where("order_id = ? AND id IN ? AND deleted_at IS NULL", orderID, ids).Order("id").Find(&rows).Error
	return rows, err
}

// AllOrderItems returns the immutable item snapshots used by a system-created
// delivery-return after-sale. Keeping this query in the after-sale repository
// avoids duplicating refund allocation rules in the orchestration module.
func (r *Repository) AllOrderItems(ctx context.Context, tx *gorm.DB, orderID uint64) ([]OrderItemRow, error) {
	var rows []OrderItemRow
	err := tx.WithContext(ctx).Table("order_items").
		Where("order_id = ? AND deleted_at IS NULL", orderID).Order("id").Find(&rows).Error
	return rows, err
}

// BySource returns the one system after-sale owned by a delivery return.
func (r *Repository) BySource(ctx context.Context, tx *gorm.DB, sourceType string, sourceID uint64) (AfterSale, error) {
	var row AfterSale
	err := tx.WithContext(ctx).Where("source_type=? AND source_id=? AND deleted_at IS NULL", sourceType, sourceID).Take(&row).Error
	return row, err
}

// ActiveConflicts locks active after-sales for the order. A system full refund
// must not race a customer application or silently refund only a remainder.
func (r *Repository) ActiveConflicts(ctx context.Context, tx *gorm.DB, orderID uint64) (bool, error) {
	var rows []struct{ ID uint64 }
	err := tx.WithContext(ctx).Table("after_sales").Select("id").
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("order_id=? AND status NOT IN ? AND deleted_at IS NULL", orderID,
			[]string{"rejected", "withdrawn", "closed_manual"}).Order("id").Find(&rows).Error
	return len(rows) > 0, err
}

// RefundByAfterSale is used only for an idempotent source replay.
func (r *Repository) RefundByAfterSale(ctx context.Context, tx *gorm.DB, afterSaleID uint64) (Refund, error) {
	var row Refund
	err := tx.WithContext(ctx).Where("after_sale_id=? AND deleted_at IS NULL", afterSaleID).Order("id").Take(&row).Error
	return row, err
}

func repositoryNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }

// ActiveRequested 返回启用状态 Requested。
func (r *Repository) ActiveRequested(ctx context.Context, tx *gorm.DB, orderItemIDs []uint64) (map[uint64]struct {
	Quantity int
	Amount   int64
}, error) {
	type row struct {
		OrderItemID uint64
		Quantity    int
		Amount      int64
	}
	var rows []row
	err := tx.WithContext(ctx).Table("after_sale_items asi").Select("asi.order_item_id, SUM(asi.requested_quantity) quantity, SUM(asi.requested_amount) amount").
		Joins("JOIN after_sales a ON a.id = asi.after_sale_id AND a.deleted_at IS NULL").
		Where("asi.order_item_id IN ? AND asi.deleted_at IS NULL AND a.status NOT IN ?", orderItemIDs, []string{"rejected", "withdrawn", "closed_manual"}).Group("asi.order_item_id").Scan(&rows).Error
	out := make(map[uint64]struct {
		Quantity int
		Amount   int64
	}, len(rows))
	for _, v := range rows {
		out[v.OrderItemID] = struct {
			Quantity int
			Amount   int64
		}{v.Quantity, v.Amount}
	}
	return out, err
}

// DeliveryFeeClaimed 返回配送 Fee Claimed。
func (r *Repository) DeliveryFeeClaimed(ctx context.Context, tx *gorm.DB, orderID uint64) (bool, error) {
	var count int64
	err := tx.WithContext(ctx).Table("after_sales").Where("order_id=? AND include_delivery_fee=1 AND status NOT IN ? AND deleted_at IS NULL", orderID, []string{"rejected", "withdrawn", "closed_manual"}).Count(&count).Error
	return count > 0, err
}

// CreateRateCounts 创建速率 Counts。
func (r *Repository) CreateRateCounts(ctx context.Context, tx *gorm.DB, customerID, orderID uint64, hourAgo, dayAgo time.Time) (int64, int64, error) {
	var orderCount, customerCount int64
	if err := tx.WithContext(ctx).Model(&AfterSale{}).Where("order_id=? AND initiator_type='customer' AND created_at>=? AND deleted_at IS NULL", orderID, hourAgo).Count(&orderCount).Error; err != nil {
		return 0, 0, err
	}
	err := tx.WithContext(ctx).Model(&AfterSale{}).Where("customer_id=? AND initiator_type='customer' AND created_at>=? AND deleted_at IS NULL", customerID, dayAgo).Count(&customerCount).Error
	return orderCount, customerCount, err
}

// HistoryActionCount 返回History 操作数量。
func (r *Repository) HistoryActionCount(ctx context.Context, tx *gorm.DB, actorType string, actorID uint64, actionPattern string, since time.Time) (int64, error) {
	var count int64
	err := tx.WithContext(ctx).Model(&History{}).Where("actor_type=? AND actor_id=? AND action LIKE ? AND created_at>=?", actorType, actorID, actionPattern, since).Count(&count).Error
	return count, err
}

// Create 创建售后。
func (r *Repository) Create(ctx context.Context, tx *gorm.DB, row *AfterSale, items []Item, evidence []Evidence, history History, audit AuditLog, outbox OutboxEvent) error {
	if err := tx.WithContext(ctx).Create(row).Error; err != nil {
		return err
	}
	if len(items) > 0 {
		if err := tx.WithContext(ctx).Create(&items).Error; err != nil {
			return err
		}
	}
	if len(evidence) > 0 {
		if err := tx.WithContext(ctx).Create(&evidence).Error; err != nil {
			return err
		}
	}
	if err := tx.WithContext(ctx).Create(&history).Error; err != nil {
		return err
	}
	if err := tx.WithContext(ctx).Create(&audit).Error; err != nil {
		return err
	}
	return tx.WithContext(ctx).Create(&outbox).Error
}

// Owned 返回Owned。
func (r *Repository) Owned(ctx context.Context, db *gorm.DB, customerID, id uint64, lock bool) (AfterSale, error) {
	q := db.WithContext(ctx).Where("id=? AND customer_id=? AND deleted_at IS NULL", id, customerID)
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row AfterSale
	err := q.Take(&row).Error
	return row, err
}

// Scoped 返回Scoped。
func (r *Repository) Scoped(ctx context.Context, db *gorm.DB, id, merchantID uint64, shopIDs []uint64, lock bool) (AfterSale, error) {
	q := db.WithContext(ctx).Where("id=? AND merchant_id=? AND shop_id IN ? AND deleted_at IS NULL", id, merchantID, shopIDs)
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row AfterSale
	err := q.Take(&row).Error
	return row, err
}

// Any 返回Any。
func (r *Repository) Any(ctx context.Context, db *gorm.DB, id uint64, lock bool) (AfterSale, error) {
	q := db.WithContext(ctx).Where("id=? AND deleted_at IS NULL", id)
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row AfterSale
	err := q.Take(&row).Error
	return row, err
}

// Items 返回明细。
func (r *Repository) Items(ctx context.Context, db *gorm.DB, id uint64) ([]Item, error) {
	var rows []Item
	err := db.WithContext(ctx).Where("after_sale_id=? AND deleted_at IS NULL", id).Order("id").Find(&rows).Error
	return rows, err
}

// Evidence 返回Evidence。
func (r *Repository) Evidence(ctx context.Context, db *gorm.DB, id uint64) ([]Evidence, error) {
	var rows []Evidence
	err := db.WithContext(ctx).Where("after_sale_id=? AND deleted_at IS NULL", id).Order("id").Find(&rows).Error
	return rows, err
}

// QuarantinedEvidenceCount 返回Quarantined Evidence 数量。
func (r *Repository) QuarantinedEvidenceCount(ctx context.Context, db *gorm.DB, id uint64) (int64, error) {
	var count int64
	err := db.WithContext(ctx).Model(&Evidence{}).Where("after_sale_id=? AND status='quarantined' AND deleted_at IS NULL", id).Count(&count).Error
	return count, err
}

// History 返回History。
func (r *Repository) History(ctx context.Context, db *gorm.DB, id uint64) ([]History, error) {
	var rows []History
	err := db.WithContext(ctx).Where("after_sale_id=?", id).Order("created_at,id").Find(&rows).Error
	return rows, err
}

// ListCustomer 查询用户列表。
func (r *Repository) ListCustomer(ctx context.Context, customerID uint64, q ListQuery) ([]AfterSale, error) {
	db := r.db.WithContext(ctx).Where("customer_id=? AND deleted_at IS NULL", customerID)
	return listAfterSales(db, q)
}

// ListStore 查询门店列表。
func (r *Repository) ListStore(ctx context.Context, merchantID uint64, shopIDs []uint64, q ListQuery) ([]AfterSale, error) {
	db := r.db.WithContext(ctx).Where("merchant_id=? AND shop_id IN ? AND deleted_at IS NULL", merchantID, shopIDs)
	return listAfterSales(db, q)
}

// ListAdmin 查询管理端列表。
func (r *Repository) ListAdmin(ctx context.Context, q ListQuery) ([]AfterSale, error) {
	return listAfterSales(r.db.WithContext(ctx).Where("deleted_at IS NULL"), q)
}

// listAfterSales 查询售后 Sales列表。
func listAfterSales(db *gorm.DB, q ListQuery) ([]AfterSale, error) {
	if q.Status != "" {
		db = db.Where("status=?", q.Status)
	}
	if q.Type != "" {
		db = db.Where("type=?", q.Type)
	}
	var rows []AfterSale
	err := db.Order("created_at DESC,id DESC").Offset(q.Query.Offset).Limit(q.Query.PageSize + 1).Find(&rows).Error
	return rows, err
}

// UpdateCAS 更新CAS。
func (r *Repository) UpdateCAS(ctx context.Context, tx *gorm.DB, id uint64, version uint32, values map[string]any) (bool, error) {
	values["version"] = gorm.Expr("version+1")
	res := tx.WithContext(ctx).Model(&AfterSale{}).Where("id=? AND version=? AND deleted_at IS NULL", id, version).Updates(values)
	return res.RowsAffected == 1, res.Error
}

// AddEvidence 添加Evidence。
func (r *Repository) AddEvidence(ctx context.Context, tx *gorm.DB, rows []Evidence) error {
	return tx.WithContext(ctx).Create(&rows).Error
}

// CreateHistory 创建History。
func (r *Repository) CreateHistory(ctx context.Context, tx *gorm.DB, row History) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// CreateAudit 创建审计。
func (r *Repository) CreateAudit(ctx context.Context, tx *gorm.DB, row AuditLog) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// CreateOutbox 创建发件箱事件。
func (r *Repository) CreateOutbox(ctx context.Context, tx *gorm.DB, row OutboxEvent) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// UpdateOrderSummary 更新订单摘要。
func (r *Repository) UpdateOrderSummary(ctx context.Context, tx *gorm.DB, orderID uint64, status string) error {
	return tx.WithContext(ctx).Table("orders").Where("id=?", orderID).Updates(map[string]any{"after_sale_status": status, "version": gorm.Expr("version+1")}).Error
}

// LockPayment 加锁并获取支付。
func (r *Repository) LockPayment(ctx context.Context, tx *gorm.DB, orderID uint64) (PaymentRow, error) {
	var row PaymentRow
	err := tx.WithContext(ctx).Table("payments").Clauses(clause.Locking{Strength: "UPDATE"}).Where("order_id=? AND status='succeeded' AND deleted_at IS NULL", orderID).Take(&row).Error
	return row, err
}

// ReservedRefund 返回Reserved 退款。
func (r *Repository) ReservedRefund(ctx context.Context, tx *gorm.DB, paymentID uint64) (int64, error) {
	// A plain aggregate is a snapshot read under MySQL REPEATABLE READ. After
	// waiting on the payment lock it could miss a refund committed by the prior
	// owner, so use a locking current read and sum the reserved rows locally.
	var rows []struct{ Amount int64 }
	err := tx.WithContext(ctx).Table("refunds").Select("amount").Clauses(clause.Locking{Strength: "UPDATE"}).Where("payment_id=? AND status IN ? AND deleted_at IS NULL", paymentID, []string{"creating", "pending", "exception"}).Order("id").Find(&rows).Error
	var amount int64
	for _, row := range rows {
		amount += row.Amount
	}
	return amount, err
}

// CreateRefund 创建退款。
func (r *Repository) CreateRefund(ctx context.Context, tx *gorm.DB, row Refund, items []RefundItem) error {
	if err := tx.WithContext(ctx).Create(&row).Error; err != nil {
		return err
	}
	if len(items) > 0 {
		return tx.WithContext(ctx).Create(&items).Error
	}
	return nil
}

// ApproveItems 审批通过明细。
func (r *Repository) ApproveItems(ctx context.Context, tx *gorm.DB, items []Item) error {
	for _, it := range items {
		if err := tx.WithContext(ctx).Model(&Item{}).Where("id=?", it.ID).Updates(map[string]any{"approved_quantity": it.ApprovedQuantity, "approved_amount": it.ApprovedAmount}).Error; err != nil {
			return err
		}
	}
	return nil
}

// CreateReplacement 创建Replacement。
func (r *Repository) CreateReplacement(ctx context.Context, tx *gorm.DB, row Replacement) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// Replacement 返回Replacement。
func (r *Repository) Replacement(ctx context.Context, tx *gorm.DB, afterSaleID uint64, lock bool) (Replacement, error) {
	q := tx.WithContext(ctx).Where("after_sale_id=?", afterSaleID)
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row Replacement
	err := q.Take(&row).Error
	return row, err
}

// CreateReturnReceipt 创建Return Receipt。
func (r *Repository) CreateReturnReceipt(ctx context.Context, tx *gorm.DB, row ReturnReceipt) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// LockStock 加锁并获取库存。
func (r *Repository) LockStock(ctx context.Context, tx *gorm.DB, shopProductID uint64) (ProductStock, error) {
	var row ProductStock
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("shop_product_id=? AND deleted_at IS NULL", shopProductID).Take(&row).Error
	return row, err
}

// AddAvailableStock 添加可用库存。
func (r *Repository) AddAvailableStock(ctx context.Context, tx *gorm.DB, stock ProductStock, quantity int) error {
	return tx.WithContext(ctx).Model(&ProductStock{}).Where("id=?", stock.ID).Updates(map[string]any{"available_qty": gorm.Expr("available_qty+?", quantity), "version": gorm.Expr("version+1")}).Error
}

// ReserveReplacementStock 预留Replacement 库存。
func (r *Repository) ReserveReplacementStock(ctx context.Context, tx *gorm.DB, stock ProductStock, quantity int) (bool, error) {
	result := tx.WithContext(ctx).Model(&ProductStock{}).Where("id=? AND available_qty>=?", stock.ID, quantity).Updates(map[string]any{"available_qty": gorm.Expr("available_qty-?", quantity), "reserved_qty": gorm.Expr("reserved_qty+?", quantity), "version": gorm.Expr("version+1")})
	return result.RowsAffected == 1, result.Error
}

// CreateStockRecord 创建库存记录。
func (r *Repository) CreateStockRecord(ctx context.Context, tx *gorm.DB, row StockRecord) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// UpdateItemsDisposition 更新明细 Disposition。
func (r *Repository) UpdateItemsDisposition(ctx context.Context, tx *gorm.DB, afterSaleID uint64, disposition string) error {
	return tx.WithContext(ctx).Model(&Item{}).Where("after_sale_id=? AND deleted_at IS NULL", afterSaleID).Update("return_disposition", disposition).Error
}

// UpdateReplacement 更新Replacement。
func (r *Repository) UpdateReplacement(ctx context.Context, tx *gorm.DB, id uint64, version uint32, values map[string]any) (bool, error) {
	values["version"] = gorm.Expr("version+1")
	result := tx.WithContext(ctx).Model(&Replacement{}).Where("id=? AND version=?", id, version).Updates(values)
	return result.RowsAffected == 1, result.Error
}

// CreateCompensation 创建Compensation。
func (r *Repository) CreateCompensation(ctx context.Context, tx *gorm.DB, row Compensation) error {
	return tx.WithContext(ctx).Create(&row).Error
}

// nowPtr 返回now Ptr。
func nowPtr(t time.Time) *time.Time { return &t }
