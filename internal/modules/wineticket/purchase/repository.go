package purchase

import (
	"context"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/catalog"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
)

type Repository struct {
	db      *gorm.DB
	catalog *catalog.Repository
}

type packageProductRecord struct {
	ID        uint64
	Name      string
	BrandName *string
	Spec      *string
	ImageURL  *string
}

func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db, catalog: catalog.NewRepository(db)}
}

func (r *Repository) DB() *gorm.DB { return r.db }

func (r *Repository) WithTransaction(
	ctx context.Context,
	fn func(*gorm.DB) error,
) error {
	return r.db.WithContext(ctx).Transaction(fn)
}

type CustomerEligibility struct {
	CustomerID               uint64
	CustomerStatus           string
	Phone                    string
	OpenID                   string
	IdentityCount            int64
	RealnameStatus           *string
	AdultResult              *string
	RealnameExpires          *time.Time
	RevokedAt                *time.Time
	PendingVerificationCount int64
}

type PurchaseRecord struct {
	Purchase `gorm:"embedded"`

	PackageNo        string         `gorm:"column:package_no"`
	PackageCode      string         `gorm:"column:package_code"`
	PackageName      string         `gorm:"column:package_name"`
	PackageType      string         `gorm:"column:package_type"`
	ProductName      string         `gorm:"column:product_name"`
	ProductBrandName *string        `gorm:"column:product_brand_name"`
	ProductSpec      *string        `gorm:"column:product_spec"`
	ProductImageURL  *string        `gorm:"column:product_image_url"`
	PaymentNo        string         `gorm:"column:payment_no"`
	PaymentStatus    string         `gorm:"column:payment_status"`
	ProviderStatus   *string        `gorm:"column:provider_status"`
	ClientPayload    datatypes.JSON `gorm:"column:client_payload"`
	RefundStatus     *string        `gorm:"column:refund_status"`
	RefundKind       *string        `gorm:"column:refund_kind"`
	RefundNo         *string        `gorm:"column:refund_no"`
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

type purchasePaymentDraft struct {
	Purchase     `gorm:"embedded"`
	PaymentState string `gorm:"column:payment_state"`
}

func (row LotFactRecord) HeldQuantity() uint {
	return row.RedemptionHeld + row.GiftHeld + row.RefundHeld
}

func purchaseProjection(db *gorm.DB) *gorm.DB {
	return db.Table("wine_ticket_purchases purchase").
		Select(`
			purchase.*,
			package.package_no AS package_no,
			package.package_code AS package_code,
			package.name AS package_name,
			package.package_type AS package_type,
			product.name AS product_name,
			product.brand_name AS product_brand_name,
			product.spec AS product_spec,
			product.image_url AS product_image_url,
			payment.payment_no AS payment_no,
			payment.status AS payment_status,
			payment.provider_status AS provider_status,
			payment.client_payload AS client_payload,
			purchase_refund.status AS refund_status,
			purchase_refund.refund_kind AS refund_kind,
			purchase_refund.wine_ticket_refund_no AS refund_no
		`).
		Joins("JOIN wine_ticket_packages package ON package.id = purchase.package_id").
		Joins("JOIN products product ON product.id = purchase.product_id").
		Joins("JOIN payments payment ON payment.id = purchase.payment_id").
		Joins(`
			LEFT JOIN wine_ticket_refunds purchase_refund
			  ON purchase_refund.id = (
				SELECT candidate.id
				  FROM wine_ticket_refunds candidate
				 WHERE candidate.purchase_id = purchase.id
				   AND candidate.customer_id = purchase.customer_id
				 ORDER BY candidate.id DESC
				 LIMIT 1
			  )
		`)
}

func PurchaseProjection(db *gorm.DB) *gorm.DB { return purchaseProjection(db) }

func (r *Repository) ListCustomerPurchases(ctx context.Context, customerID uint64, query pagination.Query, status string) ([]PurchaseRecord, error) {
	db := purchaseProjection(r.db.WithContext(ctx)).Where("purchase.customer_id = ?", customerID)
	if status != "" {
		db = db.Where("purchase.status = ?", status)
	}
	var err error
	db, err = pagination.ApplyIDCursor(db, query, "purchase.id", "desc")
	if err != nil {
		return nil, err
	}
	var rows []PurchaseRecord
	err = db.Order("purchase.id DESC").Limit(query.PageSize + 1).Scan(&rows).Error
	return rows, err
}

func (r *Repository) CustomerPurchaseByNo(ctx context.Context, customerID uint64, purchaseNo string) (PurchaseRecord, error) {
	var row PurchaseRecord
	err := purchaseProjection(r.db.WithContext(ctx)).
		Where("purchase.customer_id = ? AND purchase.purchase_no = ?", customerID, purchaseNo).
		Take(&row).Error
	return row, err
}

func (r *Repository) CustomerPurchaseByID(ctx context.Context, customerID, purchaseID uint64) (PurchaseRecord, error) {
	var row PurchaseRecord
	err := purchaseProjection(r.db.WithContext(ctx)).
		Where("purchase.customer_id = ? AND purchase.id = ?", customerID, purchaseID).
		Take(&row).Error
	return row, err
}

func (r *Repository) CustomerPurchaseByPaymentKey(ctx context.Context, tx *gorm.DB, customerID uint64, key string) (purchasePaymentDraft, error) {
	var row purchasePaymentDraft
	err := tx.WithContext(ctx).Table("wine_ticket_purchases purchase").
		Select("purchase.*, payment.status AS payment_state").
		Joins(`
			JOIN payments payment
			  ON payment.id = purchase.payment_id
			 AND payment.customer_id = purchase.customer_id
			 AND payment.biz_type = ?
			 AND payment.biz_id = purchase.id
		`, PurchasePaymentBusiness).
		Where(
			"purchase.customer_id = ? AND payment.idempotency_key = ?",
			customerID,
			key,
		).
		Order("purchase.id DESC").
		Take(&row).Error
	return row, err
}

func (r *Repository) LockPackageForPurchase(ctx context.Context, tx *gorm.DB, packageNo string) (catalog.Package, error) {
	var row catalog.Package
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("package_no = ? AND deleted_at IS NULL", packageNo).
		Take(&row).Error
	return row, err
}

func (r *Repository) CustomerPurchaseEligibility(ctx context.Context, tx *gorm.DB, customerID uint64, appID string, now time.Time) (CustomerEligibility, error) {
	var row CustomerEligibility
	identityPredicate := `
		identity.customer_id = customer.id
		AND identity.provider = 'wechat_miniapp'
		AND identity.status = 'active'
		AND identity.deleted_at IS NULL
	`
	selectArgs := []any{}
	if appID != "" {
		identityPredicate += " AND identity.app_id = ?"
		selectArgs = append(selectArgs, appID, appID)
	}
	selectArgs = append(selectArgs, now)
	err := tx.WithContext(ctx).Table("customers customer").
		Select(`
			customer.id AS customer_id,
			customer.status AS customer_status,
			customer.phone AS phone,
			COALESCE((
				SELECT identity.provider_subject
				FROM customer_identities identity
				WHERE `+identityPredicate+`
				ORDER BY identity.id DESC
				LIMIT 1
			), '') AS open_id,
			(
				SELECT COUNT(*)
				FROM customer_identities identity
				WHERE `+identityPredicate+`
			) AS identity_count,
			realname.status AS realname_status,
			realname.adult_result AS adult_result,
			realname.expires_at AS realname_expires,
			realname.revoked_at AS revoked_at,
			(
				SELECT COUNT(*)
				FROM identity_verification_requests verification_request
				WHERE verification_request.customer_id = customer.id
				  AND verification_request.status IN ('creating_session','pending')
				  AND (
					verification_request.session_expires_at IS NULL
					OR verification_request.session_expires_at > ?
				  )
			) AS pending_verification_count
		`, selectArgs...).
		Joins("LEFT JOIN customer_realname_verifications realname ON realname.customer_id = customer.id").
		Where("customer.id = ? AND customer.deleted_at IS NULL", customerID).
		Take(&row).Error
	if err == nil && row.RealnameExpires != nil && !row.RealnameExpires.After(now) {
		row.RealnameStatus = nil
	}
	return row, err
}

func (r *Repository) EnsurePurchaseQuota(ctx context.Context, tx *gorm.DB, quota *PurchaseQuota) error {
	return tx.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "customer_id"}, {Name: "package_code"}},
		DoNothing: true,
	}).Create(quota).Error
}

func (r *Repository) LockPurchaseQuota(ctx context.Context, tx *gorm.DB, customerID uint64, packageCode string) (PurchaseQuota, error) {
	var row PurchaseQuota
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("customer_id = ? AND package_code = ?", customerID, packageCode).
		Take(&row).Error
	return row, err
}

func (r *Repository) UpdatePurchaseQuota(ctx context.Context, tx *gorm.DB, quotaID uint64, values map[string]any) error {
	return tx.WithContext(ctx).Model(&PurchaseQuota{}).Where("id = ?", quotaID).Updates(values).Error
}

func (r *Repository) CreatePurchase(ctx context.Context, tx *gorm.DB, row *Purchase) error {
	return tx.WithContext(ctx).Create(row).Error
}

func (r *Repository) LockPurchaseByID(ctx context.Context, tx *gorm.DB, purchaseID uint64) (Purchase, error) {
	var row Purchase
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", purchaseID).
		Take(&row).Error
	return row, err
}

func (r *Repository) UpdatePurchase(ctx context.Context, tx *gorm.DB, purchaseID uint64, values map[string]any) error {
	return tx.WithContext(ctx).Model(&Purchase{}).Where("id = ?", purchaseID).Updates(values).Error
}

func (r *Repository) PurchaseLot(ctx context.Context, tx *gorm.DB, purchaseID uint64) (core.Lot, error) {
	var row core.Lot
	err := tx.WithContext(ctx).Where("purchase_id = ? AND source_type = ?", purchaseID, LotSourcePurchase).
		Order("id ASC").
		Take(&row).Error
	return row, err
}

func (r *Repository) TransactionByActionKey(ctx context.Context, tx *gorm.DB, actionKey string) (core.Transaction, error) {
	var row core.Transaction
	err := tx.WithContext(ctx).Where("action_key = ?", actionKey).Take(&row).Error
	return row, err
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

func LotFactProjection(db *gorm.DB) *gorm.DB { return lotFactProjection(db) }

// PurchaseLotFacts 限定在购买人所有权范围内。
// 接收方批次可以保留相同的 purchase_id 血缘，但关联条件会阻止其进入购买人响应。
func (r *Repository) PurchaseLotFacts(ctx context.Context, purchaseIDs []uint64) ([]LotFactRecord, error) {
	if len(purchaseIDs) == 0 {
		return []LotFactRecord{}, nil
	}
	var rows []LotFactRecord
	err := lotFactProjection(r.db.WithContext(ctx)).
		Where("lot.purchase_id IN ? AND lot.owner_customer_id = purchase.customer_id", purchaseIDs).
		Order("lot.expires_at ASC, lot.id ASC").
		Scan(&rows).Error
	return rows, err
}

func (r *Repository) PackageProduct(
	ctx context.Context,
	tx *gorm.DB,
	productID uint64,
) (packageProductRecord, error) {
	var row packageProductRecord
	err := tx.WithContext(ctx).
		Table("products").
		Select("id, name, brand_name, spec, image_url").
		Where("id = ? AND deleted_at IS NULL", productID).
		Take(&row).Error
	return row, err
}

func (r *Repository) SettlementRelation(
	ctx context.Context,
	tx *gorm.DB,
	row catalog.Package,
) (catalog.SettlementRelation, error) {
	return r.catalog.SettlementRelation(ctx, tx, row)
}

func (r *Repository) CreateAudit(
	ctx context.Context,
	tx *gorm.DB,
	values map[string]any,
) error {
	return tx.WithContext(ctx).Table("audit_logs").Create(values).Error
}

func (r *Repository) CreateOutbox(
	ctx context.Context,
	tx *gorm.DB,
	values map[string]any,
) error {
	return tx.WithContext(ctx).Table("outbox_events").Create(values).Error
}

func (r *Repository) issuanceCompensationFactCounts(
	ctx context.Context,
	tx *gorm.DB,
	purchaseID uint64,
) (
	lotCount int64,
	allocationCount int64,
	issueCount int64,
	activeRefundCount int64,
	err error,
) {
	err = tx.WithContext(ctx).
		Model(&core.Lot{}).
		Where("purchase_id = ?", purchaseID).
		Count(&lotCount).Error
	if err != nil {
		return
	}
	err = tx.WithContext(ctx).
		Table("wine_ticket_refund_allocations allocation").
		Joins(`
			JOIN wine_ticket_refunds business_refund
			  ON business_refund.id = allocation.wine_ticket_refund_id
		`).
		Where("business_refund.purchase_id = ?", purchaseID).
		Count(&allocationCount).Error
	if err != nil {
		return
	}
	err = tx.WithContext(ctx).
		Model(&core.Transaction{}).
		Where(
			"biz_type = ? AND biz_id = ? AND transaction_type = ?",
			"purchase",
			purchaseID,
			TransactionTypePurchaseIssue,
		).
		Count(&issueCount).Error
	if err != nil {
		return
	}
	err = tx.WithContext(ctx).
		Model(&issuanceCompensationRefund{}).
		Where(
			"purchase_id = ? AND status IN ?",
			purchaseID,
			wineTicketRefundActiveStatuses,
		).
		Count(&activeRefundCount).Error
	return
}

func (r *Repository) createIssuanceCompensationRefund(
	ctx context.Context,
	tx *gorm.DB,
	business *issuanceCompensationRefund,
	common *commonRefundRow,
) error {
	if err := tx.WithContext(ctx).Create(business).Error; err != nil {
		return err
	}
	return tx.WithContext(ctx).Create(common).Error
}
