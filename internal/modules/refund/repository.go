package refund

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Repository struct{ db *gorm.DB }

// NewRepository 创建并初始化数据仓储。
func NewRepository(db *gorm.DB) *Repository { return &Repository{db} }

// DB 返回当前数据仓储使用的数据库连接。
func (r *Repository) DB() *gorm.DB { return r.db }

// Claim 认领记录。
func (r *Repository) Claim(ctx context.Context, instance string, now time.Time) (Row, error) {
	var row Row
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("status IN ? AND (next_retry_at IS NULL OR next_retry_at<=?) AND (locked_until IS NULL OR locked_until<=?) AND deleted_at IS NULL", []string{"creating", "pending"}, now, now).Order("next_retry_at,id").Take(&row).Error; e != nil {
			return e
		}
		until := now.Add(30 * time.Second)
		return tx.Model(&Row{}).Where("id=? AND status IN ?", row.ID, []string{"creating", "pending"}).Updates(map[string]any{"locked_by": instance, "locked_until": until, "attempts": gorm.Expr("attempts+1"), "version": gorm.Expr("version+1")}).Error
	})
	return row, err
}

// Lock 加锁并获取记录。
func (r *Repository) Lock(ctx context.Context, tx *gorm.DB, id uint64) (Row, error) {
	var v Row
	e := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=? AND deleted_at IS NULL", id).Take(&v).Error
	return v, e
}

// ByNo 返回By 无。
func (r *Repository) ByNo(ctx context.Context, no string) (Row, error) {
	var v Row
	e := r.db.WithContext(ctx).Where("refund_no=? AND deleted_at IS NULL", no).Take(&v).Error
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
	err := tx.WithContext(ctx).Table("refunds").Select("amount").Clauses(clause.Locking{Strength: "UPDATE"}).Where("payment_id=? AND id<>? AND status IN ? AND deleted_at IS NULL", paymentID, refundID, []string{"creating", "pending", "exception"}).Order("id").Find(&rows).Error
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
	q := r.db.WithContext(ctx).Where("deleted_at IS NULL")
	if status != "" {
		q = q.Where("status=?", status)
	}
	var rows []Row
	e := q.Order("created_at DESC,id DESC").Offset(offset).Limit(size + 1).Find(&rows).Error
	return rows, e
}

// Retry 重试退款。
func (r *Repository) Retry(ctx context.Context, id uint64, now time.Time) (bool, error) {
	result := r.db.WithContext(ctx).Model(&Row{}).
		Where("id=? AND status IN ? AND deleted_at IS NULL", id, []string{"pending", "failed", "exception"}).
		Updates(map[string]any{"status": "pending", "next_retry_at": now, "locked_by": nil, "locked_until": nil, "failure_code": nil, "failure_detail": nil, "version": gorm.Expr("version+1")})
	return result.RowsAffected == 1, result.Error
}

// isNotFound 判断不 Found是否成立。
func isNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }
