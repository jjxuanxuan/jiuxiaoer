package cabinet

import (
	"context"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
)

type Repository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

type LotFactRecord struct {
	core.Lot `gorm:"embedded"`

	PurchaseNo         string         `gorm:"column:purchase_no"`
	SourceLotNo        *string        `gorm:"column:source_lot_no"`
	SourceGiftNo       *string        `gorm:"column:source_gift_no"`
	ProductName        string         `gorm:"column:product_name"`
	ProductBrandName   *string        `gorm:"column:product_brand_name"`
	ProductSpec        *string        `gorm:"column:product_spec"`
	ProductImageURL    *string        `gorm:"column:product_image_url"`
	IssuerMerchantName string         `gorm:"column:issuer_merchant_name"`
	RedemptionHeld     uint           `gorm:"column:redemption_held"`
	GiftHeld           uint           `gorm:"column:gift_held"`
	RefundHeld         uint           `gorm:"column:refund_held"`
	ExtractedQuantity  uint           `gorm:"column:extracted_quantity"`
	ActiveRenewalCount uint           `gorm:"column:active_renewal_count"`
	RenewalPolicy      datatypes.JSON `gorm:"column:renewal_policy_snapshot"`
}

func (row LotFactRecord) HeldQuantity() uint {
	return row.RedemptionHeld + row.GiftHeld + row.RefundHeld
}

type TransactionRecord struct {
	core.Transaction `gorm:"embedded"`

	LotNo            string  `gorm:"column:lot_no"`
	ProductID        uint64  `gorm:"column:product_id"`
	ProductName      string  `gorm:"column:product_name"`
	ProductBrandName *string `gorm:"column:product_brand_name"`
	ProductSpec      *string `gorm:"column:product_spec"`
	ProductImageURL  *string `gorm:"column:product_image_url"`
	BizNo            string  `gorm:"column:biz_no"`
	BizStatus        *string `gorm:"column:biz_status"`
}

func lotFactProjection(db *gorm.DB) *gorm.DB {
	return db.Table("wine_ticket_lots lot").
		Select(`
			lot.*,
			purchase.purchase_no AS purchase_no,
			source_lot.lot_no AS source_lot_no,
			source_gift.gift_no AS source_gift_no,
			purchase.renewal_policy_snapshot AS renewal_policy_snapshot,
			product.name AS product_name,
			product.brand_name AS product_brand_name,
			product.spec AS product_spec,
			product.image_url AS product_image_url,
			merchant.name AS issuer_merchant_name,
			COALESCE(redemption_held.quantity, 0) AS redemption_held,
			COALESCE(gift_held.quantity, 0) AS gift_held,
			COALESCE(refund_held.quantity, 0) AS refund_held,
			COALESCE(redemption_consumed.quantity, 0) AS extracted_quantity,
			COALESCE(active_renewal.quantity, 0) AS active_renewal_count
		`).
		Joins("JOIN wine_ticket_purchases purchase ON purchase.id = lot.purchase_id").
		Joins("LEFT JOIN wine_ticket_lots source_lot ON source_lot.id = lot.source_lot_id").
		Joins("LEFT JOIN wine_ticket_gifts source_gift ON source_gift.id = lot.source_gift_id").
		Joins("JOIN products product ON product.id = lot.product_id").
		Joins("JOIN merchants merchant ON merchant.id = lot.issuer_merchant_id").
		Joins(`
			LEFT JOIN (
				SELECT lot_id, SUM(quantity) AS quantity
				FROM wine_ticket_redemption_allocations
				WHERE status = 'held'
				GROUP BY lot_id
			) redemption_held ON redemption_held.lot_id = lot.id
		`).
		Joins(`
			LEFT JOIN (
				SELECT source_lot_id AS lot_id, SUM(quantity) AS quantity
				FROM wine_ticket_gift_allocations
				WHERE status = 'held'
				GROUP BY source_lot_id
			) gift_held ON gift_held.lot_id = lot.id
		`).
		Joins(`
			LEFT JOIN (
				SELECT lot_id, SUM(quantity) AS quantity
				FROM wine_ticket_refund_allocations
				WHERE status = 'held'
				GROUP BY lot_id
			) refund_held ON refund_held.lot_id = lot.id
		`).
		Joins(`
			LEFT JOIN (
				SELECT lot_id, SUM(quantity) AS quantity
				FROM wine_ticket_redemption_allocations
				WHERE status = 'consumed'
				GROUP BY lot_id
			) redemption_consumed ON redemption_consumed.lot_id = lot.id
		`).
		Joins(`
			LEFT JOIN (
				SELECT lot_id, COUNT(*) AS quantity
				FROM wine_ticket_renewals
				WHERE status IN (
					'pending_payment','payment_unknown','applying',
					'compensating_refund','refund_exception'
				)
				GROUP BY lot_id
			) active_renewal ON active_renewal.lot_id = lot.id
		`)
}

func (r *Repository) ListCustomerLots(
	ctx context.Context,
	customerID uint64,
	query pagination.Query,
	productID uint64,
	status string,
) ([]LotFactRecord, error) {
	db := lotFactProjection(r.db.WithContext(ctx)).
		Where("lot.owner_customer_id = ?", customerID)
	if productID != 0 {
		db = db.Where("lot.product_id = ?", productID)
	}
	if status != "" {
		db = db.Where("lot.status = ?", status)
	}
	var err error
	db, err = pagination.ApplyIDCursor(db, query, "lot.id", "desc")
	if err != nil {
		return nil, err
	}
	var rows []LotFactRecord
	err = db.Order("lot.id DESC").Limit(query.PageSize + 1).Scan(&rows).Error
	return rows, err
}

func (r *Repository) CustomerLotByNo(
	ctx context.Context,
	customerID uint64,
	lotNo string,
) (LotFactRecord, error) {
	var row LotFactRecord
	err := lotFactProjection(r.db.WithContext(ctx)).
		Where("lot.owner_customer_id = ? AND lot.lot_no = ?", customerID, lotNo).
		Take(&row).Error
	return row, err
}

func (r *Repository) ActiveCustomerLotFacts(
	ctx context.Context,
	customerID uint64,
	now time.Time,
) ([]LotFactRecord, error) {
	var rows []LotFactRecord
	err := lotFactProjection(r.db.WithContext(ctx)).
		Where(`
			lot.owner_customer_id = ?
			AND lot.expires_at > ?
			AND (
				(lot.status = ? AND lot.available_quantity > 0)
				OR (
					lot.status IN (?, ?)
					AND (
						COALESCE(redemption_held.quantity, 0) +
						COALESCE(gift_held.quantity, 0) +
						COALESCE(refund_held.quantity, 0)
					) > 0
				)
			)
		`, customerID, now, core.LotStatusActive, core.LotStatusActive, core.LotStatusDepleted).
		Order("lot.expires_at ASC, lot.id ASC").
		Scan(&rows).Error
	return rows, err
}

func transactionProjection(db *gorm.DB) *gorm.DB {
	return db.Table("wine_ticket_transactions ledger").
		Select(`
			ledger.*,
			lot.lot_no AS lot_no,
			lot.product_id AS product_id,
			product.name AS product_name,
			product.brand_name AS product_brand_name,
			product.spec AS product_spec,
			product.image_url AS product_image_url,
			CASE
				WHEN ledger.biz_type IN ('purchase','wine_ticket_purchase')
					THEN purchase_biz.purchase_no
				WHEN ledger.biz_type = 'redemption'
					THEN redemption_biz.redemption_no
				WHEN ledger.biz_type = 'gift'
					THEN gift_biz.gift_no
				WHEN ledger.biz_type = 'refund'
					THEN refund_biz.wine_ticket_refund_no
				ELSE ''
			END AS biz_no,
			CASE
				WHEN ledger.biz_type IN ('purchase','wine_ticket_purchase')
					THEN purchase_biz.status
				WHEN ledger.biz_type = 'redemption'
					THEN redemption_biz.status
				WHEN ledger.biz_type = 'gift'
					THEN gift_biz.status
				WHEN ledger.biz_type = 'refund'
					THEN refund_biz.status
				ELSE NULL
			END AS biz_status
		`).
		Joins("JOIN wine_ticket_lots lot ON lot.id = ledger.lot_id").
		Joins("JOIN products product ON product.id = lot.product_id").
		Joins("LEFT JOIN wine_ticket_purchases purchase_biz ON purchase_biz.id = ledger.biz_id AND ledger.biz_type IN ('purchase','wine_ticket_purchase')").
		Joins("LEFT JOIN wine_ticket_redemptions redemption_biz ON redemption_biz.id = ledger.biz_id AND ledger.biz_type = 'redemption'").
		Joins("LEFT JOIN wine_ticket_gifts gift_biz ON gift_biz.id = ledger.biz_id AND ledger.biz_type = 'gift'").
		Joins("LEFT JOIN wine_ticket_refunds refund_biz ON refund_biz.id = ledger.biz_id AND ledger.biz_type = 'refund'")
}

func (r *Repository) ListCustomerTransactions(
	ctx context.Context,
	customerID uint64,
	query pagination.Query,
	lotNo string,
	transactionType string,
) ([]TransactionRecord, error) {
	db := transactionProjection(r.db.WithContext(ctx)).
		Where(
			"ledger.owner_customer_id = ? AND lot.owner_customer_id = ?",
			customerID,
			customerID,
		)
	if lotNo != "" {
		db = db.Where("lot.lot_no = ?", lotNo)
	}
	if transactionType != "" {
		db = db.Where("ledger.transaction_type = ?", transactionType)
	}
	var err error
	db, err = pagination.ApplyIDCursor(db, query, "ledger.id", "desc")
	if err != nil {
		return nil, err
	}
	var rows []TransactionRecord
	err = db.Order("ledger.id DESC").Limit(query.PageSize + 1).Scan(&rows).Error
	return rows, err
}

func (r *Repository) LatestCustomerLotTransactions(
	ctx context.Context,
	customerID uint64,
	lotID uint64,
	limit int,
) ([]TransactionRecord, error) {
	var rows []TransactionRecord
	err := transactionProjection(r.db.WithContext(ctx)).
		Where(
			"ledger.owner_customer_id = ? AND lot.owner_customer_id = ? AND ledger.lot_id = ?",
			customerID,
			customerID,
			lotID,
		).
		Order("ledger.id DESC").
		Limit(limit).
		Scan(&rows).Error
	return rows, err
}
