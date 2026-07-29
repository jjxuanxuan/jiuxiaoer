package redemption

import (
	"context"
	"errors"
	"sort"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
)

type redemptionRepository struct {
	db *gorm.DB
}

type serviceCoreRepository struct{}

func (r *serviceCoreRepository) createAudit(
	ctx context.Context,
	tx *gorm.DB,
	values map[string]any,
) error {
	return tx.WithContext(ctx).Table("audit_logs").Create(values).Error
}

func (r *serviceCoreRepository) createOutbox(
	ctx context.Context,
	tx *gorm.DB,
	values map[string]any,
) error {
	return tx.WithContext(ctx).Table("outbox_events").Create(values).Error
}

type redemptionCustomerAccount struct {
	ID     uint64
	Status string
}

type redemptionRealnameVerification struct {
	Status      string
	AdultResult string
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
}

func newRedemptionRepository(db *gorm.DB) *redemptionRepository {
	return &redemptionRepository{db: db}
}

func (r *redemptionRepository) dbConn() *gorm.DB {
	return r.db
}

func redemptionSlotProjection(db *gorm.DB, productID uint64) *gorm.DB {
	return db.Table("delivery_time_slots slot").
		Select(`
			slot.id AS slot_id,
			slot.shop_id AS shop_id,
			slot.service_date AS service_date,
			slot.start_time AS start_time,
			slot.end_time AS end_time,
			slot.cutoff_at AS cutoff_at,
			slot.capacity_orders AS capacity_orders,
			slot.reserved_orders AS reserved_orders,
			slot.status AS slot_status,
			slot.version AS slot_version,
			shop.merchant_id AS shop_merchant_id,
			shop.name AS shop_name,
			shop.status AS shop_status,
			shop.business_status AS shop_business_status,
			merchant.id AS merchant_id,
			merchant.name AS merchant_name,
			merchant.status AS merchant_status,
			merchant.review_status AS merchant_review_status,
			shop_product.id AS shop_product_id,
			shop_product.merchant_id AS shop_product_merchant_id,
			shop_product.shop_id AS shop_product_shop_id,
			shop_product.product_id AS shop_product_product_id,
			shop_product.status AS shop_product_status,
			product.id AS product_id,
			product.name AS product_name,
			product.brand_name AS product_brand_name,
			product.spec AS product_spec,
			product.image_url AS product_image_url,
			product.status AS product_status,
			product.age_restricted AS product_age_restricted,
			category.status AS category_status,
			stock.id AS stock_id,
			stock.available_qty AS stock_available_qty,
			stock.reserved_qty AS stock_reserved_qty,
			stock.locked_qty AS stock_locked_qty,
			stock.version AS stock_version
		`).
		Joins("JOIN shops shop ON shop.id = slot.shop_id AND shop.deleted_at IS NULL").
		Joins("JOIN merchants merchant ON merchant.id = shop.merchant_id AND merchant.deleted_at IS NULL").
		Joins(`
			JOIN shop_products shop_product
			  ON shop_product.shop_id = slot.shop_id
			 AND shop_product.product_id = ?
			 AND shop_product.deleted_at IS NULL
		`, productID).
		Joins("JOIN products product ON product.id = shop_product.product_id AND product.deleted_at IS NULL").
		Joins("JOIN categories category ON category.id = product.category_id AND category.deleted_at IS NULL").
		Joins(`
			JOIN product_stocks stock
			  ON stock.shop_product_id = shop_product.id
			 AND stock.shop_id = shop.id
			 AND stock.product_id = product.id
			 AND stock.deleted_at IS NULL
		`)
}

func (r *redemptionRepository) resolveSlotRelation(
	ctx context.Context,
	db *gorm.DB,
	slotID uint64,
	productID uint64,
) (redemptionSlotRelation, error) {
	var row redemptionSlotRelation
	err := redemptionSlotProjection(db.WithContext(ctx), productID).
		Where("slot.id = ?", slotID).
		Take(&row).Error
	return row, err
}

func (r *redemptionRepository) listSlotRelations(
	ctx context.Context,
	productID uint64,
	dateFrom time.Time,
	dateTo time.Time,
) ([]redemptionSlotRelation, error) {
	var rows []redemptionSlotRelation
	err := redemptionSlotProjection(r.db.WithContext(ctx), productID).
		Where("slot.service_date >= ? AND slot.service_date <= ?", dateFrom, dateTo).
		Order("slot.service_date ASC, slot.start_time ASC, slot.id ASC").
		Find(&rows).Error
	return rows, err
}

func (r *redemptionRepository) address(
	ctx context.Context,
	customerID uint64,
	addressID uint64,
	version uint,
) (redemptionAddressRecord, error) {
	var row redemptionAddressRecord
	err := r.db.WithContext(ctx).
		Where("id = ? AND customer_id = ? AND version = ? AND deleted_at IS NULL", addressID, customerID, version).
		Take(&row).Error
	return row, err
}

func (r *redemptionRepository) lockAddress(
	ctx context.Context,
	tx *gorm.DB,
	customerID uint64,
	addressID uint64,
	version uint,
) (redemptionAddressRecord, error) {
	var row redemptionAddressRecord
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND customer_id = ? AND version = ? AND deleted_at IS NULL", addressID, customerID, version).
		Take(&row).Error
	return row, err
}

func (r *redemptionRepository) customerOwnsAddress(
	ctx context.Context,
	tx *gorm.DB,
	customerID uint64,
	addressID uint64,
) (bool, error) {
	var count int64
	err := tx.WithContext(ctx).Model(&redemptionAddressRecord{}).
		Where("id = ? AND customer_id = ? AND deleted_at IS NULL", addressID, customerID).
		Count(&count).Error
	return count == 1, err
}

func activeRenewalGuardSQL(alias string) string {
	return `NOT EXISTS (
		SELECT 1
		FROM wine_ticket_renewals renewal
		WHERE renewal.lot_id = ` + alias + `.id
		  AND renewal.status IN (
			'pending_payment','payment_unknown','applying',
			'compensating_refund','refund_exception'
		  )
	)`
}

func (r *redemptionRepository) availableLotQuantity(
	ctx context.Context,
	customerID uint64,
	issuerMerchantID uint64,
	cityCode string,
	productID uint64,
	expiresAfter time.Time,
	excludeRenewalGuards bool,
) (uint, error) {
	var quantity uint
	query := r.db.WithContext(ctx).Table("wine_ticket_lots lot").
		Select("COALESCE(SUM(lot.available_quantity), 0)").
		Where(`
			lot.owner_customer_id = ?
			AND lot.issuer_merchant_id = ?
			AND lot.redeem_city_code = ?
			AND lot.product_id = ?
			AND lot.status = ?
			AND lot.available_quantity > 0
			AND lot.expires_at > ?
		`, customerID, issuerMerchantID, cityCode, productID, LotStatusActive, expiresAfter)
	if excludeRenewalGuards {
		query = query.Where(activeRenewalGuardSQL("lot"))
	}
	err := query.Scan(&quantity).Error
	return quantity, err
}

func (r *redemptionRepository) totalAvailableByCityProduct(
	ctx context.Context,
	customerID uint64,
	cityCode string,
	productID uint64,
	now time.Time,
	excludeRenewalGuards bool,
) (uint, error) {
	var quantity uint
	query := r.db.WithContext(ctx).Table("wine_ticket_lots lot").
		Select("COALESCE(SUM(lot.available_quantity), 0)").
		Where(`
			lot.owner_customer_id = ?
			AND lot.redeem_city_code = ?
			AND lot.product_id = ?
			AND lot.status = ?
			AND lot.available_quantity > 0
			AND lot.expires_at > ?
		`, customerID, cityCode, productID, LotStatusActive, now)
	if excludeRenewalGuards {
		query = query.Where(activeRenewalGuardSQL("lot"))
	}
	err := query.Scan(&quantity).Error
	return quantity, err
}

func (r *redemptionRepository) lockEligibleLots(
	ctx context.Context,
	tx *gorm.DB,
	customerID uint64,
	issuerMerchantID uint64,
	cityCode string,
	productID uint64,
	expiresAfter time.Time,
) ([]core.Lot, error) {
	var rows []core.Lot
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(`
			owner_customer_id = ?
			AND issuer_merchant_id = ?
			AND redeem_city_code = ?
			AND product_id = ?
			AND status = ?
			AND available_quantity > 0
			AND expires_at > ?
		`, customerID, issuerMerchantID, cityCode, productID, LotStatusActive, expiresAfter).
		Order("expires_at ASC, id ASC").
		Find(&rows).Error
	return rows, err
}

// lockActiveRenewalsForLots 遵循全局批次 -> 续期锁顺序。
// 查询前完整批次组已被锁定，因此 MySQL 在判断受保护批次时，
// 不会返回陈旧的 REPEATABLE READ 快照，也不会形成续期 -> 批次的反向锁循环。
func (r *redemptionRepository) lockActiveRenewalsForLots(
	ctx context.Context,
	tx *gorm.DB,
	lots []core.Lot,
) (map[uint64]struct{}, error) {
	lotIDs := make([]uint64, 0, len(lots))
	for _, lot := range lots {
		lotIDs = append(lotIDs, lot.ID)
	}
	if len(lotIDs) == 0 {
		return map[uint64]struct{}{}, nil
	}
	var rows []activeRenewalGuard
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("lot_id IN ? AND status IN ?", lotIDs, []string{
			"pending_payment", "payment_unknown", "applying",
			"compensating_refund", "refund_exception",
		}).
		Order("lot_id ASC, id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	guarded := make(map[uint64]struct{}, len(rows))
	for _, row := range rows {
		guarded[row.LotID] = struct{}{}
	}
	return guarded, nil
}

func (r *redemptionRepository) lockSlot(
	ctx context.Context,
	tx *gorm.DB,
	slotID uint64,
) (DeliveryTimeSlot, error) {
	var row DeliveryTimeSlot
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", slotID).
		Take(&row).Error
	return row, err
}

func (r *redemptionRepository) lockStock(
	ctx context.Context,
	tx *gorm.DB,
	shopProductID uint64,
) (PhysicalStock, error) {
	var row PhysicalStock
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("shop_product_id = ? AND deleted_at IS NULL", shopProductID).
		Take(&row).Error
	return row, err
}

func redemptionViewProjection(db *gorm.DB) *gorm.DB {
	return db.Table("wine_ticket_redemptions redemption").
		Select(`
			redemption.*,
			customer_order.order_no AS order_no,
			customer_order.status AS order_status,
			customer_order.pay_status AS order_pay_status,
			customer_order.delivery_status AS order_delivery_status,
			customer_order.order_type AS order_type,
			customer_order.settlement_mode AS settlement_mode,
			shop.name AS shop_name,
			product.name AS product_name,
			product.brand_name AS product_brand_name,
			product.spec AS product_spec,
			product.image_url AS product_image_url,
			delivery.id AS delivery_order_id,
			delivery.status AS delivery_status,
			delivery.rider_id AS rider_id,
			delivery.accepted_at AS accepted_at,
			delivery.picked_up_at AS delivery_picked_up_at,
			delivery.completed_at AS delivery_completed_at,
			delivery.cancelled_at AS delivery_cancelled_at
		`).
		Joins("JOIN orders customer_order ON customer_order.id = redemption.order_id AND customer_order.deleted_at IS NULL").
		Joins("LEFT JOIN shops shop ON shop.id = redemption.shop_id").
		Joins("LEFT JOIN products product ON product.id = redemption.product_id").
		Joins("LEFT JOIN delivery_orders delivery ON delivery.order_id = redemption.order_id AND delivery.deleted_at IS NULL")
}

func (r *redemptionRepository) listCustomerRedemptions(
	ctx context.Context,
	customerID uint64,
	query pagination.Query,
	status string,
) ([]redemptionView, error) {
	db := redemptionViewProjection(r.db.WithContext(ctx)).
		Where("redemption.customer_id = ?", customerID)
	if status != "" {
		db = db.Where("redemption.status = ?", status)
	}
	var err error
	db, err = pagination.ApplyIDCursor(db, query, "redemption.id", "desc")
	if err != nil {
		return nil, err
	}
	var rows []redemptionView
	err = db.Order("redemption.id DESC").Limit(query.PageSize + 1).Scan(&rows).Error
	return rows, err
}

func (r *redemptionRepository) customerRedemptionByNo(
	ctx context.Context,
	customerID uint64,
	redemptionNo string,
) (redemptionView, error) {
	var row redemptionView
	err := redemptionViewProjection(r.db.WithContext(ctx)).
		Where("redemption.customer_id = ? AND redemption.redemption_no = ?", customerID, redemptionNo).
		Take(&row).Error
	return row, err
}

func (r *redemptionRepository) customerRedemptionByID(
	ctx context.Context,
	db *gorm.DB,
	customerID uint64,
	redemptionID uint64,
) (redemptionView, error) {
	var row redemptionView
	err := redemptionViewProjection(db.WithContext(ctx)).
		Where("redemption.customer_id = ? AND redemption.id = ?", customerID, redemptionID).
		Take(&row).Error
	return row, err
}

func (r *redemptionRepository) allocationViews(
	ctx context.Context,
	db *gorm.DB,
	redemptionIDs []uint64,
) ([]redemptionAllocationView, error) {
	if len(redemptionIDs) == 0 {
		return []redemptionAllocationView{}, nil
	}
	var rows []redemptionAllocationView
	err := db.WithContext(ctx).Table("wine_ticket_redemption_allocations allocation").
		Select(`
			allocation.*,
			lot.lot_no AS lot_no,
			lot.status AS lot_status,
			lot.expires_at AS lot_expires_at
		`).
		Joins("JOIN wine_ticket_lots lot ON lot.id = allocation.lot_id").
		Where("allocation.redemption_id IN ?", redemptionIDs).
		Order("allocation.redemption_id ASC, allocation.id ASC").
		Scan(&rows).Error
	return rows, err
}

func (r *redemptionRepository) cancellationAnchor(
	ctx context.Context,
	customerID uint64,
	redemptionNo string,
) (Redemption, error) {
	var row Redemption
	err := r.db.WithContext(ctx).
		Where("customer_id = ? AND redemption_no = ?", customerID, redemptionNo).
		Take(&row).Error
	return row, err
}

func (r *redemptionRepository) lockOrder(
	ctx context.Context,
	tx *gorm.DB,
	orderID uint64,
) (order.Order, error) {
	var row order.Order
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND deleted_at IS NULL", orderID).
		Take(&row).Error
	return row, err
}

func (r *redemptionRepository) lockAfterSaleIDs(
	ctx context.Context,
	tx *gorm.DB,
	orderID uint64,
) ([]uint64, error) {
	var rows []struct {
		ID uint64
	}
	err := tx.WithContext(ctx).Table("after_sales").
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id").
		Where("order_id = ? AND deleted_at IS NULL", orderID).
		Order("id ASC").
		Find(&rows).Error
	ids := make([]uint64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids, err
}

func (r *redemptionRepository) lockDeliveryReturnIDs(
	ctx context.Context,
	tx *gorm.DB,
	orderID uint64,
) ([]uint64, error) {
	var rows []struct {
		ID uint64
	}
	err := tx.WithContext(ctx).Table("delivery_returns").
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id").
		Where("order_id = ?", orderID).
		Order("id ASC").
		Find(&rows).Error
	ids := make([]uint64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids, err
}

func (r *redemptionRepository) lockRedemption(
	ctx context.Context,
	tx *gorm.DB,
	redemptionID uint64,
	customerID uint64,
) (Redemption, error) {
	var row Redemption
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND customer_id = ?", redemptionID, customerID).
		Take(&row).Error
	return row, err
}

func (r *redemptionRepository) lockAllocations(
	ctx context.Context,
	tx *gorm.DB,
	redemptionID uint64,
) ([]RedemptionAllocation, error) {
	var rows []RedemptionAllocation
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("redemption_id = ?", redemptionID).
		Order("id ASC").
		Find(&rows).Error
	return rows, err
}

func (r *redemptionRepository) lockAllocationLots(
	ctx context.Context,
	tx *gorm.DB,
	allocations []RedemptionAllocation,
) ([]core.Lot, error) {
	ids := make([]uint64, 0, len(allocations))
	seen := make(map[uint64]struct{}, len(allocations))
	for _, allocation := range allocations {
		if _, exists := seen[allocation.LotID]; exists {
			continue
		}
		seen[allocation.LotID] = struct{}{}
		ids = append(ids, allocation.LotID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) == 0 {
		return []core.Lot{}, nil
	}
	var rows []core.Lot
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id IN ?", ids).
		Order("id ASC").
		Find(&rows).Error
	return rows, err
}

func (r *redemptionRepository) createOrder(
	ctx context.Context,
	tx *gorm.DB,
	row *order.Order,
) error {
	return tx.WithContext(ctx).Create(row).Error
}

func (r *redemptionRepository) createOrderItem(
	ctx context.Context,
	tx *gorm.DB,
	row *order.OrderItem,
) error {
	return tx.WithContext(ctx).Create(row).Error
}

func (r *redemptionRepository) createOrderLog(
	ctx context.Context,
	tx *gorm.DB,
	row *order.OrderLog,
) error {
	return tx.WithContext(ctx).Create(row).Error
}

func (r *redemptionRepository) createRedemption(
	ctx context.Context,
	tx *gorm.DB,
	row *Redemption,
) error {
	return tx.WithContext(ctx).Create(row).Error
}

func (r *redemptionRepository) createAllocation(
	ctx context.Context,
	tx *gorm.DB,
	row *RedemptionAllocation,
) error {
	return tx.WithContext(ctx).Create(row).Error
}

func (r *redemptionRepository) reserveSlot(
	ctx context.Context,
	tx *gorm.DB,
	slot DeliveryTimeSlot,
	now time.Time,
) (bool, error) {
	result := tx.WithContext(ctx).Model(&DeliveryTimeSlot{}).
		Where(`
			id = ? AND version = ? AND status = 'open'
			AND reserved_orders = ? AND reserved_orders < capacity_orders
		`, slot.ID, slot.Version, slot.ReservedOrders).
		Updates(map[string]any{
			"reserved_orders": slot.ReservedOrders + 1,
			"version":         slot.Version + 1,
			"updated_at":      now,
		})
	return result.RowsAffected == 1, result.Error
}

func (r *redemptionRepository) releaseSlot(
	ctx context.Context,
	tx *gorm.DB,
	slot DeliveryTimeSlot,
	now time.Time,
) (bool, error) {
	result := tx.WithContext(ctx).Model(&DeliveryTimeSlot{}).
		Where("id = ? AND version = ? AND reserved_orders > 0", slot.ID, slot.Version).
		Updates(map[string]any{
			"reserved_orders": slot.ReservedOrders - 1,
			"version":         slot.Version + 1,
			"updated_at":      now,
		})
	return result.RowsAffected == 1, result.Error
}

func (r *redemptionRepository) updatePhysicalStock(
	ctx context.Context,
	tx *gorm.DB,
	stock PhysicalStock,
	available int,
	now time.Time,
) (bool, error) {
	result := tx.WithContext(ctx).Model(&PhysicalStock{}).
		Where(
			"id = ? AND version = ? AND available_qty = ? AND deleted_at IS NULL",
			stock.ID,
			stock.Version,
			stock.AvailableQty,
		).
		Updates(map[string]any{
			"available_qty": available,
			"version":       stock.Version + 1,
			"updated_at":    now,
		})
	return result.RowsAffected == 1, result.Error
}

func (r *redemptionRepository) createStockRecord(
	ctx context.Context,
	tx *gorm.DB,
	values map[string]any,
) error {
	return tx.WithContext(ctx).Table("stock_records").Create(values).Error
}

func (r *redemptionRepository) restoreAllocation(
	ctx context.Context,
	tx *gorm.DB,
	allocationID uint64,
	now time.Time,
) (bool, error) {
	result := tx.WithContext(ctx).Model(&RedemptionAllocation{}).
		Where(
			"id = ? AND status = ?",
			allocationID,
			RedemptionAllocationStatusHeld,
		).
		Updates(map[string]any{
			"status":     RedemptionAllocationStatusRestored,
			"updated_at": now,
		})
	return result.RowsAffected == 1, result.Error
}

func (r *redemptionRepository) cancelOrder(
	ctx context.Context,
	tx *gorm.DB,
	row order.Order,
	source string,
	reason string,
	now time.Time,
) (bool, error) {
	result := tx.WithContext(ctx).Model(&order.Order{}).
		Where("id = ? AND version = ?", row.ID, row.Version).
		Updates(map[string]any{
			"status":             "cancelled",
			"delivery_status":    "cancelled",
			"cancel_source":      source,
			"cancel_reason_code": reason,
			"cancelled_at":       now,
			"version":            row.Version + 1,
			"updated_at":         now,
		})
	return result.RowsAffected == 1, result.Error
}

func (r *redemptionRepository) cancelRedemption(
	ctx context.Context,
	tx *gorm.DB,
	row Redemption,
	now time.Time,
) (bool, error) {
	result := tx.WithContext(ctx).Model(&Redemption{}).
		Where("id = ? AND version = ?", row.ID, row.Version).
		Updates(map[string]any{
			"status":       RedemptionStatusCancelled,
			"cancelled_at": now,
			"restored_at":  now,
			"version":      row.Version + 1,
			"updated_at":   now,
		})
	return result.RowsAffected == 1, result.Error
}

func (r *redemptionRepository) customerAccount(
	ctx context.Context,
	tx *gorm.DB,
	customerID uint64,
) (redemptionCustomerAccount, error) {
	var row redemptionCustomerAccount
	err := tx.WithContext(ctx).Table("customers").
		Select("id, status").
		Where("id = ? AND deleted_at IS NULL", customerID).
		Take(&row).Error
	return row, err
}

func (r *redemptionRepository) customerRealname(
	ctx context.Context,
	tx *gorm.DB,
	customerID uint64,
) (redemptionRealnameVerification, error) {
	var row redemptionRealnameVerification
	err := tx.WithContext(ctx).Table("customer_realname_verifications").
		Select("status, adult_result, expires_at, revoked_at").
		Where("customer_id = ?", customerID).
		Take(&row).Error
	return row, err
}

func (r *redemptionRepository) pendingIdentityVerification(
	ctx context.Context,
	tx *gorm.DB,
	customerID uint64,
	now time.Time,
) (bool, error) {
	var count int64
	err := tx.WithContext(ctx).Table("identity_verification_requests").
		Where(`
			customer_id = ? AND status IN ?
			AND (session_expires_at IS NULL OR session_expires_at > ?)
		`, customerID, []string{"creating_session", "pending"}, now).
		Count(&count).Error
	return count > 0, err
}

func isRedemptionNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}
