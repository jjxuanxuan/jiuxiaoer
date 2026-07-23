package deliveryreturn

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Repository struct{ db *gorm.DB }

func NewRepository(db *gorm.DB) *Repository { return &Repository{db: db} }
func (r *Repository) DB() *gorm.DB          { return r.db }

func (r *Repository) LockDelivery(ctx context.Context, tx *gorm.DB, id uint64) (DeliveryOrder, error) {
	var row DeliveryOrder
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id=? AND deleted_at IS NULL", id).First(&row).Error
	return row, err
}

func (r *Repository) Incident(ctx context.Context, tx *gorm.DB, id uint64) (IncidentRef, error) {
	var row IncidentRef
	err := tx.WithContext(ctx).Where("id=?", id).First(&row).Error
	return row, err
}

func (r *Repository) LockIncident(ctx context.Context, tx *gorm.DB, id uint64) (IncidentRef, error) {
	var row IncidentRef
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=?", id).First(&row).Error
	return row, err
}

func (r *Repository) IncidentEvidenceCount(ctx context.Context, tx *gorm.DB, id uint64) (int64, error) {
	var count int64
	err := tx.WithContext(ctx).Table("delivery_incident_evidence").
		Where("incident_id=? AND scan_status='clean'", id).Count(&count).Error
	return count, err
}

func (r *Repository) ActiveByDelivery(ctx context.Context, tx *gorm.DB, deliveryID uint64, lock bool) (Return, error) {
	query := tx.WithContext(ctx).Where("active_delivery_order_id=?", deliveryID)
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row Return
	err := query.First(&row).Error
	return row, err
}

func (r *Repository) CreateReturn(ctx context.Context, tx *gorm.DB, row *Return) error {
	return tx.WithContext(ctx).Create(row).Error
}

func (r *Repository) CreateHistory(ctx context.Context, tx *gorm.DB, row History) error {
	return tx.WithContext(ctx).Create(&row).Error
}

func (r *Repository) CreateAudit(ctx context.Context, tx *gorm.DB, row AuditLog) error {
	return tx.WithContext(ctx).Create(&row).Error
}

func (r *Repository) CreateOutbox(ctx context.Context, tx *gorm.DB, row OutboxEvent) error {
	return tx.WithContext(ctx).Create(&row).Error
}

func (r *Repository) ReturnByID(ctx context.Context, db *gorm.DB, id uint64, lock bool) (Return, error) {
	query := db.WithContext(ctx).Where("id=?", id)
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row Return
	err := query.Take(&row).Error
	return row, err
}

func (r *Repository) ReturnByAfterSale(ctx context.Context, tx *gorm.DB, afterSaleID uint64, lock bool) (Return, error) {
	query := tx.WithContext(ctx).Where("after_sale_id=?", afterSaleID)
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row Return
	err := query.Take(&row).Error
	return row, err
}

func (r *Repository) ReturnByIncident(ctx context.Context, tx *gorm.DB, incidentID uint64, lock bool) (Return, error) {
	query := tx.WithContext(ctx).Where("incident_id=?", incidentID)
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row Return
	err := query.Take(&row).Error
	return row, err
}

func (r *Repository) UpdateReturnVersioned(ctx context.Context, tx *gorm.DB, row Return, values map[string]any) (bool, error) {
	values["version"] = gorm.Expr("version+1")
	values["updated_at"] = time.Now().UTC()
	result := tx.WithContext(ctx).Model(&Return{}).Where("id=? AND version=?", row.ID, row.Version).Updates(values)
	return result.RowsAffected == 1, result.Error
}

func (r *Repository) UpdateDelivery(ctx context.Context, tx *gorm.DB, id uint64, values map[string]any) error {
	return tx.WithContext(ctx).Table("delivery_orders").Where("id=? AND deleted_at IS NULL", id).Updates(values).Error
}

func (r *Repository) UpdateOrder(ctx context.Context, tx *gorm.DB, id uint64, values map[string]any) error {
	return tx.WithContext(ctx).Table("orders").Where("id=? AND deleted_at IS NULL", id).Updates(values).Error
}

func (r *Repository) LockOrder(ctx context.Context, tx *gorm.DB, id uint64) (OrderRef, error) {
	var row OrderRef
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=? AND deleted_at IS NULL", id).Take(&row).Error
	return row, err
}

func (r *Repository) RiderAggregate(ctx context.Context, id, riderID uint64) (Aggregate, error) {
	var out Aggregate
	err := r.db.WithContext(ctx).Where("id=? AND rider_id=?", id, riderID).First(&out.Return).Error
	if err != nil {
		return Aggregate{}, err
	}
	err = r.loadAggregate(ctx, r.db, &out)
	return out, err
}

func (r *Repository) AdminAggregate(ctx context.Context, id uint64) (Aggregate, error) {
	var out Aggregate
	if err := r.db.WithContext(ctx).Where("id=?", id).Take(&out.Return).Error; err != nil {
		return Aggregate{}, err
	}
	return out, r.loadAggregate(ctx, r.db, &out)
}

func (r *Repository) StoreAggregate(ctx context.Context, id uint64, shopIDs []uint64) (Aggregate, error) {
	var out Aggregate
	if err := r.db.WithContext(ctx).Where("id=? AND shop_id IN ?", id, shopIDs).Take(&out.Return).Error; err != nil {
		return Aggregate{}, err
	}
	return out, r.loadAggregate(ctx, r.db, &out)
}

func (r *Repository) AggregateTx(ctx context.Context, tx *gorm.DB, id uint64) (Aggregate, error) {
	var out Aggregate
	if err := tx.WithContext(ctx).Where("id=?", id).Take(&out.Return).Error; err != nil {
		return Aggregate{}, err
	}
	return out, r.loadAggregate(ctx, tx, &out)
}

func (r *Repository) loadAggregate(ctx context.Context, db *gorm.DB, out *Aggregate) error {
	if err := db.WithContext(ctx).Where("delivery_return_id=?", out.Return.ID).Order("created_at,id").Find(&out.History).Error; err != nil {
		return err
	}
	if out.Return.AfterSaleID == nil {
		out.RefundStatus = "not_authorized"
		return nil
	}
	var refundStatus string
	if err := db.WithContext(ctx).Table("refunds").Select("status").Where("after_sale_id=? AND deleted_at IS NULL", *out.Return.AfterSaleID).Order("id DESC").Limit(1).Scan(&refundStatus).Error; err != nil {
		return err
	}
	out.RefundStatus = refundStatus
	var rows []struct {
		AfterSaleItemID  uint64
		OrderItemID      uint64
		ShopProductID    uint64
		ProductID        uint64
		ExpectedQuantity int
		ProductSnapshot  datatypes.JSON
		ReceivedQuantity *int
		Disposition      *string
		PolicyCode       *string
		PolicyVersion    *string
		AvailableBefore  *int
		AvailableAfter   *int
		Note             *string
	}
	err := db.WithContext(ctx).Table("after_sale_items asi").
		Select(`asi.id after_sale_item_id,asi.order_item_id,asi.shop_product_id,asi.product_id,
			asi.approved_quantity expected_quantity,oi.product_snapshot,
			rri.received_quantity,rri.disposition,rri.policy_code,rri.policy_version,
			rri.available_before,rri.available_after,rri.note`).
		Joins("JOIN order_items oi ON oi.id=asi.order_item_id AND oi.deleted_at IS NULL").
		Joins("LEFT JOIN return_receipts rr ON rr.after_sale_id=asi.after_sale_id").
		Joins("LEFT JOIN return_receipt_items rri ON rri.return_receipt_id=rr.id AND rri.after_sale_item_id=asi.id").
		Where("asi.after_sale_id=? AND asi.deleted_at IS NULL", *out.Return.AfterSaleID).Order("asi.id").Scan(&rows).Error
	if err != nil {
		return err
	}
	out.Items = make([]AggregateItem, 0, len(rows))
	for _, row := range rows {
		policyCode, policyVersion, _ := returnPolicy(row.ProductSnapshot)
		if row.PolicyCode != nil {
			policyCode = *row.PolicyCode
		}
		if row.PolicyVersion != nil {
			policyVersion = *row.PolicyVersion
		}
		out.Items = append(out.Items, AggregateItem{
			AfterSaleItemID: row.AfterSaleItemID, OrderItemID: row.OrderItemID,
			ShopProductID: row.ShopProductID, ProductID: row.ProductID,
			ExpectedQuantity: row.ExpectedQuantity, ReceivedQuantity: row.ReceivedQuantity,
			Disposition: row.Disposition, PolicyCode: policyCode, PolicyVersion: policyVersion,
			AvailableBefore: row.AvailableBefore, AvailableAfter: row.AvailableAfter, Note: row.Note,
		})
	}
	return nil
}

func (r *Repository) ListAdmin(ctx context.Context, query ListQuery) ([]Return, error) {
	db := r.db.WithContext(ctx)
	if query.Status != "" {
		db = db.Where("status=?", query.Status)
	}
	var rows []Return
	err := db.Order("updated_at DESC,id DESC").Offset(query.Offset).Limit(query.Limit + 1).Find(&rows).Error
	return rows, err
}

func (r *Repository) ListStore(ctx context.Context, shopIDs []uint64, query ListQuery) ([]Return, error) {
	db := r.db.WithContext(ctx).Where("shop_id IN ?", shopIDs)
	if query.Status != "" {
		db = db.Where("status=?", query.Status)
	}
	var rows []Return
	err := db.Order("updated_at DESC,id DESC").Offset(query.Offset).Limit(query.Limit + 1).Find(&rows).Error
	return rows, err
}

func (r *Repository) AfterSale(ctx context.Context, tx *gorm.DB, id uint64, lock bool) (AfterSale, error) {
	query := tx.WithContext(ctx).Where("id=? AND deleted_at IS NULL", id)
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row AfterSale
	err := query.Take(&row).Error
	return row, err
}

func (r *Repository) AfterSaleItems(ctx context.Context, tx *gorm.DB, id uint64) ([]AfterSaleItem, error) {
	var rows []AfterSaleItem
	err := tx.WithContext(ctx).Where("after_sale_id=? AND deleted_at IS NULL", id).Order("shop_product_id,id").Find(&rows).Error
	return rows, err
}

func (r *Repository) OrderItemSnapshots(ctx context.Context, tx *gorm.DB, ids []uint64) (map[uint64]datatypes.JSON, error) {
	var rows []OrderItem
	err := tx.WithContext(ctx).Select("id,product_snapshot").Where("id IN ? AND deleted_at IS NULL", ids).Find(&rows).Error
	out := make(map[uint64]datatypes.JSON, len(rows))
	for _, row := range rows {
		out[row.ID] = row.ProductSnapshot
	}
	return out, err
}

func (r *Repository) CreateReceipt(ctx context.Context, tx *gorm.DB, receipt ReturnReceipt, items []ReceiptItem) error {
	if err := tx.WithContext(ctx).Create(&receipt).Error; err != nil {
		return err
	}
	if len(items) > 0 {
		return tx.WithContext(ctx).Create(&items).Error
	}
	return nil
}

func (r *Repository) UpdateAfterSaleItemDisposition(ctx context.Context, tx *gorm.DB, itemID uint64, disposition string) error {
	return tx.WithContext(ctx).Model(&AfterSaleItem{}).Where("id=?", itemID).Update("return_disposition", disposition).Error
}

func (r *Repository) LockStocks(ctx context.Context, tx *gorm.DB, shopProductIDs []uint64) (map[uint64]ProductStock, error) {
	sort.Slice(shopProductIDs, func(i, j int) bool { return shopProductIDs[i] < shopProductIDs[j] })
	var rows []ProductStock
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("shop_product_id IN ? AND deleted_at IS NULL", shopProductIDs).Order("shop_product_id").Find(&rows).Error
	out := make(map[uint64]ProductStock, len(rows))
	for _, row := range rows {
		out[row.ShopProductID] = row
	}
	return out, err
}

func (r *Repository) AddAvailable(ctx context.Context, tx *gorm.DB, stock ProductStock, quantity int) error {
	return tx.WithContext(ctx).Model(&ProductStock{}).Where("id=?", stock.ID).
		Updates(map[string]any{"available_qty": gorm.Expr("available_qty+?", quantity), "version": gorm.Expr("version+1")}).Error
}

func (r *Repository) CreateStockRecord(ctx context.Context, tx *gorm.DB, row StockRecord) error {
	return tx.WithContext(ctx).Create(&row).Error
}

func (r *Repository) RefundStatus(ctx context.Context, tx *gorm.DB, afterSaleID uint64) (string, error) {
	var status string
	err := tx.WithContext(ctx).Table("refunds").Select("status").Where("after_sale_id=? AND deleted_at IS NULL", afterSaleID).Order("id DESC").Take(&status).Error
	return status, err
}

func (r *Repository) ClosureComplete(ctx context.Context, tx *gorm.DB, afterSaleID, returnID uint64) (bool, error) {
	var expected int64
	if err := tx.WithContext(ctx).Table("after_sale_items").Where("after_sale_id=? AND deleted_at IS NULL", afterSaleID).Count(&expected).Error; err != nil {
		return false, err
	}
	var receipt struct{ ID uint64 }
	if err := tx.WithContext(ctx).Table("return_receipts").Select("id").Where("after_sale_id=?", afterSaleID).Take(&receipt).Error; err != nil {
		if IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	var items []ReceiptItem
	if err := tx.WithContext(ctx).Where("return_receipt_id=?", receipt.ID).Find(&items).Error; err != nil {
		return false, err
	}
	if expected == 0 || int64(len(items)) != expected {
		return false, nil
	}
	for _, item := range items {
		if item.ReceivedQuantity != item.ExpectedQuantity {
			return false, nil
		}
		if item.Disposition == "restock" {
			key := fmt.Sprintf("delivery_return:%d:%d:restock", returnID, item.AfterSaleItemID)
			var count int64
			if err := tx.WithContext(ctx).Table("stock_records").Where("business_action_key=?", key).Count(&count).Error; err != nil {
				return false, err
			}
			if count == 0 {
				return false, nil
			}
		}
	}
	return true, nil
}

func (r *Repository) HistoryActionExists(ctx context.Context, tx *gorm.DB, returnID uint64, action string) (bool, error) {
	var count int64
	err := tx.WithContext(ctx).Model(&History{}).Where("delivery_return_id=? AND action=?", returnID, action).Count(&count).Error
	return count > 0, err
}

func (r *Repository) SLACandidates(ctx context.Context, now time.Time, reminderAfter time.Duration, limit int) ([]uint64, error) {
	var ids []uint64
	err := r.db.WithContext(ctx).Model(&Return{}).Select("id").
		Where("status IN ? AND approved_at IS NOT NULL AND approved_at<=?", []string{StatusReturning, StatusArrived, StatusException}, now.Add(-reminderAfter)).
		Order("receipt_deadline_at,id").Limit(limit).Scan(&ids).Error
	return ids, err
}

func (r *Repository) ClosureCandidates(ctx context.Context, limit int) ([]uint64, error) {
	var ids []uint64
	err := r.db.WithContext(ctx).Table("delivery_returns dr").Distinct("dr.after_sale_id").
		Joins("JOIN refunds r ON r.after_sale_id=dr.after_sale_id AND r.status='succeeded' AND r.deleted_at IS NULL").
		Joins("JOIN return_receipts rr ON rr.after_sale_id=dr.after_sale_id").
		Where("dr.status IN ? AND dr.after_sale_id IS NOT NULL", []string{StatusReceived, StatusException}).
		Order("dr.after_sale_id").Limit(limit).Scan(&ids).Error
	return ids, err
}

func (r *Repository) HasActiveLocked(ctx context.Context, tx *gorm.DB, deliveryID uint64) (bool, error) {
	var rows []Return
	err := tx.WithContext(ctx).Select("id").Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("active_delivery_order_id=?", deliveryID).Limit(1).Find(&rows).Error
	return len(rows) > 0, err
}

func IsNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }
