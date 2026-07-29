package refund

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Repository struct{ db *gorm.DB }

var activeRefundStatuses = []string{"creating", "submission_unknown", "pending"}

var reservedRefundStatuses = []string{"creating", "submission_unknown", "pending", "exception"}

// NewRepository 创建并初始化数据仓储。
func NewRepository(db *gorm.DB) *Repository { return &Repository{db} }

// DB 返回当前数据仓储使用的数据库连接。
func (r *Repository) DB() *gorm.DB { return r.db }

// Claim 认领记录。
func (r *Repository) Claim(ctx context.Context, instance string, now time.Time) (Row, error) {
	var row Row
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("status IN ? AND (next_retry_at IS NULL OR next_retry_at<=?) AND (locked_until IS NULL OR locked_until<=?) AND deleted_at IS NULL", activeRefundStatuses, now, now).Order("next_retry_at,id").Take(&row).Error; e != nil {
			return e
		}
		until := now.Add(30 * time.Second)
		return tx.Model(&Row{}).Where("id=? AND status IN ?", row.ID, activeRefundStatuses).Updates(map[string]any{"locked_by": instance, "locked_until": until, "attempts": gorm.Expr("attempts+1"), "version": gorm.Expr("version+1")}).Error
	})
	return row, err
}

// Lock 加锁并获取记录。
func (r *Repository) Lock(ctx context.Context, tx *gorm.DB, id uint64) (Row, error) {
	var v Row
	e := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=? AND deleted_at IS NULL", id).Take(&v).Error
	return v, e
}

// Lookup 返回无锁路由快照。
// 外部结算处理器必须按业务特有锁计划重新锁定并校验。
func (r *Repository) Lookup(ctx context.Context, tx *gorm.DB, id uint64) (Row, error) {
	var v Row
	e := tx.WithContext(ctx).Where("id=? AND deleted_at IS NULL", id).Take(&v).Error
	return v, e
}

// ByNo 返回By 无。
func (r *Repository) ByNo(ctx context.Context, no string) (Row, error) {
	var v Row
	e := r.db.WithContext(ctx).Where("refund_no=? AND deleted_at IS NULL", no).Take(&v).Error
	return v, e
}

// LookupByNo 在现有事务内返回无锁路由快照。
// 后续锁计划由选定的业务处理器负责。
func (r *Repository) LookupByNo(ctx context.Context, tx *gorm.DB, no string) (Row, error) {
	var v Row
	e := tx.WithContext(ctx).Where("refund_no=? AND deleted_at IS NULL", no).Take(&v).Error
	return v, e
}

// LockByNo 加锁并获取By 无。
func (r *Repository) LockByNo(ctx context.Context, tx *gorm.DB, no string) (Row, error) {
	var v Row
	e := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("refund_no=? AND deleted_at IS NULL", no).Take(&v).Error
	return v, e
}

// Update 更新退款。
func (r *Repository) Update(ctx context.Context, tx *gorm.DB, id uint64, values map[string]any) error {
	values["version"] = gorm.Expr("version+1")
	return tx.WithContext(ctx).Model(&Row{}).Where("id=?", id).Updates(values).Error
}

// LockPayment 加锁并获取支付。
func (r *Repository) LockPayment(ctx context.Context, tx *gorm.DB, id uint64) (Payment, error) {
	var v Payment
	e := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=? AND deleted_at IS NULL", id).Take(&v).Error
	return v, e
}

// ReservedExcept 返回Reserved Except。
func (r *Repository) ReservedExcept(ctx context.Context, tx *gorm.DB, paymentID, refundID uint64) (int64, error) {
	var rows []struct{ Amount int64 }
	err := tx.WithContext(ctx).Table("refunds").Select("amount").Clauses(clause.Locking{Strength: "UPDATE"}).Where("payment_id=? AND id<>? AND status IN ? AND deleted_at IS NULL", paymentID, refundID, reservedRefundStatuses).Order("id").Find(&rows).Error
	var total int64
	for _, row := range rows {
		total += row.Amount
	}
	return total, err
}

// LockOrder 加锁并获取订单。
func (r *Repository) LockOrder(ctx context.Context, tx *gorm.DB, id uint64) (Order, error) {
	var v Order
	e := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=? AND deleted_at IS NULL", id).Take(&v).Error
	return v, e
}

// LockAfterSale 加锁并获取售后销售。
func (r *Repository) LockAfterSale(ctx context.Context, tx *gorm.DB, id uint64) (AfterSale, error) {
	var v AfterSale
	e := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=? AND deleted_at IS NULL", id).Take(&v).Error
	return v, e
}

// Items 返回明细。
func (r *Repository) Items(ctx context.Context, tx *gorm.DB, refundID uint64) ([]RefundItem, error) {
	var v []RefundItem
	e := tx.WithContext(ctx).Where("refund_id=?", refundID).Find(&v).Error
	return v, e
}

// ReplacementByOriginal 返回已为 CLOSED 退款创建的替代退款。
// 数据库唯一索引仍是最终并发保护。
func (r *Repository) ReplacementByOriginal(ctx context.Context, tx *gorm.DB, refundID uint64) (Row, error) {
	var row Row
	err := tx.WithContext(ctx).Where("replaces_refund_id=? AND deleted_at IS NULL", refundID).Take(&row).Error
	return row, err
}

// CreateReplacement 原子创建替代退款并复制其不可变商品分配。
func (r *Repository) CreateReplacement(ctx context.Context, tx *gorm.DB, row *Row, items []RefundItem) error {
	if err := tx.WithContext(ctx).Create(row).Error; err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	return tx.WithContext(ctx).Create(&items).Error
}

// ApplyItem 应用明细。
func (r *Repository) ApplyItem(ctx context.Context, tx *gorm.DB, id uint64, amount int64) error {
	res := tx.WithContext(ctx).Model(&AfterSaleItem{}).Where("id=? AND refunded_amount+?<=approved_amount", id, amount).Update("refunded_amount", gorm.Expr("refunded_amount+?", amount))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return gorm.ErrInvalidData
	}
	return nil
}

// CreateCallback 创建回调。
func (r *Repository) CreateCallback(ctx context.Context, tx *gorm.DB, v *Callback) error {
	return tx.WithContext(ctx).Create(v).Error
}

// CreateCallbackIfAbsent 创建回调 If Absent。
func (r *Repository) CreateCallbackIfAbsent(ctx context.Context, tx *gorm.DB, v *Callback) (bool, error) {
	result := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(v)
	return result.RowsAffected == 1, result.Error
}

// CallbackByEvent 保留支付机构重复通知的处理结果。
func (r *Repository) CallbackByEvent(ctx context.Context, tx *gorm.DB, provider, eventID string) (Callback, error) {
	var row Callback
	err := tx.WithContext(ctx).Where("provider=? AND provider_event_id=?", provider, eventID).Take(&row).Error
	return row, err
}

// UpdateCallback 更新回调。
func (r *Repository) UpdateCallback(ctx context.Context, tx *gorm.DB, id uint64, values map[string]any) error {
	return tx.WithContext(ctx).Model(&Callback{}).Where("id=?", id).Updates(values).Error
}

// CreateOutbox 创建发件箱事件。
func (r *Repository) CreateOutbox(ctx context.Context, tx *gorm.DB, v Outbox) error {
	return tx.WithContext(ctx).Create(&v).Error
}

// List 查询Row列表列表。
func (r *Repository) List(ctx context.Context, status string, offset, size int) ([]Row, error) {
	q := r.db.WithContext(ctx).
		Where("deleted_at IS NULL").
		Where(
			"(biz_type = ? OR (biz_type IS NULL AND after_sale_id IS NOT NULL))",
			RetailAfterSaleRefundBusiness,
		)
	if status != "" {
		q = q.Where("status=?", status)
	}
	var rows []Row
	e := q.Order("created_at DESC,id DESC").Offset(offset).Limit(size + 1).Find(&rows).Error
	return rows, e
}

// RepairCandidates 返回受控存量退款修复手册覆盖的旧版非终态记录。
// 扫描为只读；每条记录仍须预览并通过修复操作显式应用。
func (r *Repository) RepairCandidates(ctx context.Context, afterID uint64, size int) ([]Row, error) {
	var rows []Row
	err := r.db.WithContext(ctx).
		Where("provider=? AND status IN ? AND id>? AND deleted_at IS NULL", "wechat", []string{"creating", "pending", "exception"}, afterID).
		Where(
			"(biz_type = ? OR (biz_type IS NULL AND after_sale_id IS NOT NULL))",
			RetailAfterSaleRefundBusiness,
		).
		Order("id").
		Limit(size + 1).
		Find(&rows).Error
	return rows, err
}

// isNotFound 判断不 Found是否成立。
func isNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }
