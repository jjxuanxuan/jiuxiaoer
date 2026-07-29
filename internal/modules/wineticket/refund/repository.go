package refund

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	purchasedomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/purchase"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

type refundRepository struct {
	db    *gorm.DB
	clock transactionClock
}

func newRefundRepository(db *gorm.DB) *refundRepository {
	return &refundRepository{db: db, clock: mysqlTransactionNow}
}

func (r *refundRepository) dbConn() *gorm.DB { return r.db }

func (r *refundRepository) createAudit(
	ctx context.Context,
	tx *gorm.DB,
	values map[string]any,
) error {
	return tx.WithContext(ctx).Table("audit_logs").Create(values).Error
}

func (r *refundRepository) createOutbox(
	ctx context.Context,
	tx *gorm.DB,
	values map[string]any,
) error {
	return tx.WithContext(ctx).Table("outbox_events").Create(values).Error
}

type transactionClock func(context.Context, *gorm.DB) (time.Time, error)

func mysqlTransactionNow(ctx context.Context, tx *gorm.DB) (time.Time, error) {
	var value time.Time
	if err := tx.WithContext(ctx).Raw("SELECT NOW(3)").Row().Scan(&value); err != nil {
		return time.Time{}, err
	}
	return value.In(shanghaiLocation).Truncate(time.Millisecond), nil
}

// transactionNow 从当前事务的数据库连接获取唯一权威时间。
// 退款报价过期、策略边界及 Create 中全部持久化时间都必须基于该值计算。
func (r *refundRepository) transactionNow(
	ctx context.Context,
	tx *gorm.DB,
) (time.Time, error) {
	if tx == nil {
		return time.Time{}, errors.New("wine ticket refund transaction is unavailable")
	}
	if r.clock == nil {
		return time.Time{}, errors.New("wine ticket refund transaction clock is unavailable")
	}
	return r.clock(ctx, tx)
}

func (r *refundRepository) purchaseByNo(
	ctx context.Context,
	db *gorm.DB,
	customerID uint64,
	purchaseNo string,
	lock bool,
) (purchasedomain.Purchase, error) {
	if db == nil {
		db = r.db
	}
	query := db.WithContext(ctx)
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row purchasedomain.Purchase
	err := query.
		Where("customer_id = ? AND purchase_no = ?", customerID, purchaseNo).
		Take(&row).Error
	return row, err
}

func (r *refundRepository) lockPurchaseByID(
	ctx context.Context,
	tx *gorm.DB,
	purchaseID uint64,
) (purchasedomain.Purchase, error) {
	var row purchasedomain.Purchase
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", purchaseID).
		Take(&row).Error
	return row, err
}

func (r *refundRepository) originalLots(
	ctx context.Context,
	db *gorm.DB,
	purchaseID uint64,
	lock bool,
) ([]core.Lot, error) {
	if db == nil {
		db = r.db
	}
	query := db.WithContext(ctx)
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var rows []core.Lot
	err := query.
		Where("purchase_id = ? AND source_type = ?", purchaseID, LotSourcePurchase).
		Order("id ASC").
		Find(&rows).Error
	return rows, err
}

func (r *refundRepository) paymentByID(
	ctx context.Context,
	db *gorm.DB,
	paymentID uint64,
	lock bool,
) (refundPayment, error) {
	if db == nil {
		db = r.db
	}
	query := db.WithContext(ctx)
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row refundPayment
	err := query.Where("id = ?", paymentID).Take(&row).Error
	return row, err
}

func (r *refundRepository) purchaseIssueTransactionCount(
	ctx context.Context,
	tx *gorm.DB,
	purchaseID uint64,
) (int64, error) {
	var count int64
	err := tx.WithContext(ctx).Table("wine_ticket_transactions").
		Where(
			"biz_type = ? AND biz_id = ? AND transaction_type = ?",
			"purchase",
			purchaseID,
			TransactionTypePurchaseIssue,
		).
		Count(&count).Error
	return count, err
}

func (r *refundRepository) consumeHeldAllocation(
	ctx context.Context,
	tx *gorm.DB,
	allocationID uint64,
	updatedAt time.Time,
) (bool, error) {
	result := tx.WithContext(ctx).Model(&RefundAllocation{}).
		Where(
			"id = ? AND status = ?",
			allocationID,
			RefundAllocationHeld,
		).
		Updates(map[string]any{
			"status":     RefundAllocationConsumed,
			"updated_at": updatedAt,
		})
	return result.RowsAffected == 1, result.Error
}

func (r *refundRepository) incrementPaymentRefundedAmount(
	ctx context.Context,
	tx *gorm.DB,
	paymentID uint64,
	amount int64,
	updatedAt time.Time,
) (bool, error) {
	result := tx.WithContext(ctx).Model(&refundPayment{}).
		Where(
			"id = ? AND refunded_amount + ? <= amount",
			paymentID,
			amount,
		).
		Updates(map[string]any{
			"refunded_amount": gorm.Expr("refunded_amount + ?", amount),
			"version":         gorm.Expr("version + 1"),
			"updated_at":      updatedAt,
		})
	return result.RowsAffected == 1, result.Error
}

func (r *refundRepository) eligibilityFacts(
	ctx context.Context,
	db *gorm.DB,
	purchaseRow purchasedomain.Purchase,
	lots []core.Lot,
) (refundEligibilityFacts, error) {
	if db == nil {
		db = r.db
	}
	facts := refundEligibilityFacts{
		Purchase:     purchaseRow,
		OriginalLots: lots,
		HeldByLot:    make(map[uint64]uint, len(lots)),
	}
	var err error
	facts.Payment, err = r.paymentByID(ctx, db, purchaseRow.PaymentID, false)
	if err != nil {
		return refundEligibilityFacts{}, err
	}
	if err := decodePolicyJSON(purchaseRow.RefundPolicySnapshot, &facts.Policy,
		"schema_version", "enabled", "window_hours", "require_never_used", "fee_amount",
	); err != nil {
		return refundEligibilityFacts{}, problem.Internal("wine ticket purchase refund policy snapshot is invalid")
	}
	if facts.Payment.PaidAt != nil {
		facts.WindowEndsAt = facts.Payment.PaidAt.In(shanghaiLocation).
			Add(time.Duration(facts.Policy.WindowHours) * time.Hour).
			Truncate(time.Millisecond)
	}
	facts.RefundableAmount = facts.Payment.Amount - facts.Payment.RefundedAmount

	lotIDs := make([]uint64, 0, len(lots))
	for _, lot := range lots {
		lotIDs = append(lotIDs, lot.ID)
	}
	if len(lotIDs) > 0 {
		type heldRow struct {
			LotID    uint64 `gorm:"column:lot_id"`
			Quantity uint   `gorm:"column:quantity"`
		}
		var held []heldRow
		err = db.WithContext(ctx).Raw(`
			SELECT lot_id, SUM(quantity) AS quantity
			FROM (
				SELECT lot_id, quantity
				FROM wine_ticket_redemption_allocations
				WHERE lot_id IN ? AND status = 'held'
				UNION ALL
				SELECT source_lot_id AS lot_id, quantity
				FROM wine_ticket_gift_allocations
				WHERE source_lot_id IN ? AND status = 'held'
				UNION ALL
				SELECT lot_id, quantity
				FROM wine_ticket_refund_allocations
				WHERE lot_id IN ? AND status = 'held'
			) held
			GROUP BY lot_id
		`, lotIDs, lotIDs, lotIDs).Scan(&held).Error
		if err != nil {
			return refundEligibilityFacts{}, err
		}
		for _, row := range held {
			facts.HeldByLot[row.LotID] = row.Quantity
			facts.ActiveHoldCount += int64(row.Quantity)
		}

		var counts struct {
			RedemptionHistory int64 `gorm:"column:redemption_history"`
			GiftHistory       int64 `gorm:"column:gift_history"`
			RenewalHistory    int64 `gorm:"column:renewal_history"`
			ActiveRenewals    int64 `gorm:"column:active_renewals"`
		}
		err = db.WithContext(ctx).Raw(`
			SELECT
				(SELECT COUNT(*) FROM wine_ticket_redemption_allocations WHERE lot_id IN ?) AS redemption_history,
				(SELECT COUNT(*) FROM wine_ticket_gift_allocations WHERE source_lot_id IN ?) AS gift_history,
				(SELECT COUNT(*) FROM wine_ticket_renewals WHERE lot_id IN ?) AS renewal_history,
				(SELECT COUNT(*) FROM wine_ticket_renewals
				 WHERE lot_id IN ? AND status IN ?) AS active_renewals
		`, lotIDs, lotIDs, lotIDs, lotIDs, renewalActiveStatuses).Scan(&counts).Error
		if err != nil {
			return refundEligibilityFacts{}, err
		}
		facts.HistoryCount = counts.RedemptionHistory + counts.GiftHistory + counts.RenewalHistory
		facts.ActiveHoldCount += counts.ActiveRenewals
	}

	if err := db.WithContext(ctx).Model(&WineTicketRefund{}).
		Where("purchase_id = ? AND status IN ?", purchaseRow.ID, wineTicketRefundActiveStatuses).
		Count(&facts.ActiveRefundCount).Error; err != nil {
		return refundEligibilityFacts{}, err
	}
	if err := db.WithContext(ctx).Table("wine_ticket_transactions").
		Where("biz_type = ? AND biz_id = ? AND transaction_type = ?",
			"purchase", purchaseRow.ID, TransactionTypePurchaseIssue).
		Count(&facts.IssueCount).Error; err != nil {
		return refundEligibilityFacts{}, err
	}
	var issued struct {
		Quantity int64 `gorm:"column:quantity"`
	}
	if err := db.WithContext(ctx).Table("wine_ticket_transactions").
		Select("COALESCE(SUM(quantity_delta), 0) AS quantity").
		Where("biz_type = ? AND biz_id = ? AND transaction_type = ?",
			"purchase", purchaseRow.ID, TransactionTypePurchaseIssue).
		Scan(&issued).Error; err != nil {
		return refundEligibilityFacts{}, err
	}
	facts.IssueQuantity = issued.Quantity
	return facts, nil
}

func (r *refundRepository) customerStatus(
	ctx context.Context,
	db *gorm.DB,
	customerID uint64,
) (string, error) {
	if db == nil {
		db = r.db
	}
	var row struct {
		Status string `gorm:"column:status"`
	}
	err := db.WithContext(ctx).Table("customers").
		Select("status").
		Where("id = ? AND deleted_at IS NULL", customerID).
		Take(&row).Error
	return row.Status, err
}

func (r *refundRepository) activeRefund(
	ctx context.Context,
	db *gorm.DB,
	purchaseID uint64,
	lock bool,
) (WineTicketRefund, error) {
	if db == nil {
		db = r.db
	}
	query := db.WithContext(ctx)
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row WineTicketRefund
	err := query.Where("purchase_id = ? AND status IN ?", purchaseID, wineTicketRefundActiveStatuses).
		Order("id ASC").Take(&row).Error
	return row, err
}

func (r *refundRepository) createBusinessRefund(
	ctx context.Context,
	tx *gorm.DB,
	row *WineTicketRefund,
) error {
	return tx.WithContext(ctx).Create(row).Error
}

func (r *refundRepository) createAllocations(
	ctx context.Context,
	tx *gorm.DB,
	rows []RefundAllocation,
) error {
	if len(rows) == 0 {
		return nil
	}
	return tx.WithContext(ctx).Create(&rows).Error
}

func (r *refundRepository) createCommonRefund(
	ctx context.Context,
	tx *gorm.DB,
	row *commonRefundRow,
) error {
	return tx.WithContext(ctx).Create(row).Error
}

func (r *refundRepository) updatePurchaseVersioned(
	ctx context.Context,
	tx *gorm.DB,
	row purchasedomain.Purchase,
	values map[string]any,
) error {
	values["version"] = gorm.Expr("version + 1")
	result := tx.WithContext(ctx).Model(&purchasedomain.Purchase{}).
		Where("id = ? AND version = ?", row.ID, row.Version).
		Updates(values)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return problem.Conflict("WT_CONCURRENT_MODIFICATION", "wine ticket purchase changed concurrently")
	}
	return nil
}

func (r *refundRepository) updateLotVersioned(
	ctx context.Context,
	tx *gorm.DB,
	row core.Lot,
	values map[string]any,
) error {
	values["version"] = gorm.Expr("version + 1")
	result := tx.WithContext(ctx).Model(&core.Lot{}).
		Where("id = ? AND version = ?", row.ID, row.Version).
		Updates(values)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return problem.Conflict("WT_CONCURRENT_MODIFICATION", "wine ticket lot changed concurrently")
	}
	return nil
}

func refundProjection(db *gorm.DB) *gorm.DB {
	return db.Table("wine_ticket_refunds business").
		Select(`
			business.*,
			purchase.purchase_no AS purchase_no,
			common.status AS common_status,
			common.provider_status AS common_provider_status,
			common.failure_code AS common_failure_code,
			common.updated_at AS common_updated_at,
			COALESCE(SUM(CASE WHEN allocation.status = 'held' THEN 1 ELSE 0 END), 0) AS held_count,
			COALESCE(SUM(CASE WHEN allocation.status = 'consumed' THEN 1 ELSE 0 END), 0) AS consumed_count,
			COALESCE(SUM(CASE WHEN allocation.status = 'restored' THEN 1 ELSE 0 END), 0) AS restored_count,
			COALESCE(SUM(CASE WHEN allocation.status = 'exception' THEN 1 ELSE 0 END), 0) AS exception_count
		`).
		Joins("JOIN wine_ticket_purchases purchase ON purchase.id = business.purchase_id").
		Joins("JOIN refunds common ON common.id = business.current_refund_id").
		Joins("LEFT JOIN wine_ticket_refund_allocations allocation ON allocation.wine_ticket_refund_id = business.id").
		Group("business.id, purchase.purchase_no, common.id")
}

func (r *refundRepository) customerRefundByNo(
	ctx context.Context,
	customerID uint64,
	refundNo string,
) (refundRecord, error) {
	var row refundRecord
	err := refundProjection(r.db.WithContext(ctx)).
		Where("business.customer_id = ? AND business.wine_ticket_refund_no = ?", customerID, refundNo).
		Take(&row).Error
	return row, err
}

func (r *refundRepository) customerRefundByID(
	ctx context.Context,
	customerID, refundID uint64,
) (refundRecord, error) {
	var row refundRecord
	err := refundProjection(r.db.WithContext(ctx)).
		Where("business.customer_id = ? AND business.id = ?", customerID, refundID).
		Take(&row).Error
	return row, err
}

func (r *refundRepository) listCustomerRefunds(
	ctx context.Context,
	customerID uint64,
	query pagination.Query,
	status string,
) ([]refundRecord, error) {
	db := refundProjection(r.db.WithContext(ctx)).
		Where("business.customer_id = ?", customerID)
	if status != "" {
		db = db.Where("business.status = ?", status)
	}
	var err error
	db, err = pagination.ApplyIDCursor(db, query, "business.id", "desc")
	if err != nil {
		return nil, err
	}
	var rows []refundRecord
	err = db.Order("business.id DESC").Limit(query.PageSize + 1).Scan(&rows).Error
	return rows, err
}

func (r *refundRepository) lockCommonRefunds(
	ctx context.Context,
	tx *gorm.DB,
	businessID uint64,
) ([]commonRefundRow, error) {
	var rows []commonRefundRow
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("biz_type = ? AND biz_id = ?", WineTicketPurchaseRefundBusiness, businessID).
		Order("id ASC").
		Find(&rows).Error
	return rows, err
}

func (r *refundRepository) lockBusinessRefund(
	ctx context.Context,
	tx *gorm.DB,
	businessID uint64,
) (WineTicketRefund, error) {
	var row WineTicketRefund
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", businessID).
		Take(&row).Error
	return row, err
}

func (r *refundRepository) lockAllocations(
	ctx context.Context,
	tx *gorm.DB,
	businessID uint64,
) ([]RefundAllocation, error) {
	var rows []RefundAllocation
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("wine_ticket_refund_id = ?", businessID).
		Order("id ASC").
		Find(&rows).Error
	return rows, err
}

func (r *refundRepository) updateCommonRefund(
	ctx context.Context,
	tx *gorm.DB,
	row commonRefundRow,
	values map[string]any,
) error {
	values["version"] = gorm.Expr("version + 1")
	result := tx.WithContext(ctx).Model(&commonRefundRow{}).
		Where("id = ? AND version = ?", row.ID, row.Version).
		Updates(values)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return problem.Conflict("WT_CONCURRENT_MODIFICATION", "common refund changed concurrently")
	}
	return nil
}

func (r *refundRepository) updateBusinessRefund(
	ctx context.Context,
	tx *gorm.DB,
	row WineTicketRefund,
	values map[string]any,
) error {
	values["version"] = gorm.Expr("version + 1")
	result := tx.WithContext(ctx).Model(&WineTicketRefund{}).
		Where("id = ? AND version = ?", row.ID, row.Version).
		Updates(values)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return problem.Conflict("WT_CONCURRENT_MODIFICATION", "wine ticket refund changed concurrently")
	}
	return nil
}

func refundNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }
