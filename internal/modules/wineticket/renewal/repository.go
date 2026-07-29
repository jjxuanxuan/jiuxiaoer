package renewal

import (
	"context"
	"errors"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

type renewalRepository struct {
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

func newRenewalRepository(db *gorm.DB) *renewalRepository {
	return &renewalRepository{db: db}
}

func (r *renewalRepository) dbConn() *gorm.DB { return r.db }

func (r *renewalRepository) customerPurchaseEligibility(
	ctx context.Context,
	tx *gorm.DB,
	customerID uint64,
	appID string,
	now time.Time,
) (CustomerEligibility, error) {
	if tx == nil {
		tx = r.db
	}
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
		Joins(`
			LEFT JOIN customer_realname_verifications realname
			  ON realname.customer_id = customer.id
		`).
		Where(
			"customer.id = ? AND customer.deleted_at IS NULL",
			customerID,
		).
		Take(&row).Error
	if err == nil &&
		row.RealnameExpires != nil &&
		!row.RealnameExpires.After(now) {
		row.RealnameStatus = nil
	}
	return row, err
}

func (r *renewalRepository) incrementRefundedPaymentAmount(
	ctx context.Context,
	tx *gorm.DB,
	paymentID uint64,
	amount int64,
) (bool, error) {
	result := tx.WithContext(ctx).Model(&refund.Payment{}).
		Where(
			"id = ? AND refunded_amount + ? <= amount",
			paymentID,
			amount,
		).
		Updates(map[string]any{
			"refunded_amount": gorm.Expr("refunded_amount + ?", amount),
			"version":         gorm.Expr("version + 1"),
		})
	return result.RowsAffected == 1, result.Error
}

func (r *renewalRepository) lockRefundPayment(
	ctx context.Context,
	tx *gorm.DB,
	paymentID uint64,
) (refund.Payment, error) {
	var row refund.Payment
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", paymentID).
		Take(&row).Error
	return row, err
}

func (r *renewalRepository) customerLotByNo(
	ctx context.Context,
	db *gorm.DB,
	customerID uint64,
	lotNo string,
	lock bool,
) (core.Lot, error) {
	if db == nil {
		db = r.db
	}
	query := db.WithContext(ctx)
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row core.Lot
	err := query.
		Where("lot_no = ? AND owner_customer_id = ?", lotNo, customerID).
		Take(&row).Error
	return row, err
}

func (r *renewalRepository) lockLotByID(
	ctx context.Context,
	tx *gorm.DB,
	lotID uint64,
) (core.Lot, error) {
	var row core.Lot
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", lotID).
		Take(&row).Error
	return row, err
}

func (r *renewalRepository) policySnapshot(
	ctx context.Context,
	db *gorm.DB,
	purchaseID uint64,
) (datatypes.JSON, error) {
	if db == nil {
		db = r.db
	}
	var row struct {
		RenewalPolicySnapshot datatypes.JSON `gorm:"column:renewal_policy_snapshot"`
	}
	err := db.WithContext(ctx).
		Table("wine_ticket_purchases").
		Select("renewal_policy_snapshot").
		Where("id = ?", purchaseID).
		Take(&row).Error
	return row.RenewalPolicySnapshot, err
}

func (r *renewalRepository) hasActiveHold(
	ctx context.Context,
	db *gorm.DB,
	lotID uint64,
) (bool, error) {
	if db == nil {
		db = r.db
	}
	var row struct {
		Blocked int `gorm:"column:blocked"`
	}
	err := db.WithContext(ctx).Raw(`
		SELECT CASE WHEN
			EXISTS (
				SELECT 1 FROM wine_ticket_redemption_allocations
				WHERE lot_id = ? AND status = 'held'
			)
			OR EXISTS (
				SELECT 1 FROM wine_ticket_gift_allocations
				WHERE source_lot_id = ? AND status = 'held'
			)
			OR EXISTS (
				SELECT 1 FROM wine_ticket_refund_allocations
				WHERE lot_id = ? AND status = 'held'
			)
		THEN 1 ELSE 0 END AS blocked
	`, lotID, lotID, lotID).Scan(&row).Error
	return row.Blocked != 0, err
}

func (r *renewalRepository) activeRenewal(
	ctx context.Context,
	db *gorm.DB,
	lotID uint64,
	lock bool,
) (Renewal, error) {
	if db == nil {
		db = r.db
	}
	query := db.WithContext(ctx)
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row Renewal
	err := query.
		Where("lot_id = ? AND status IN ?", lotID, renewalActiveStatuses).
		Order("id ASC").
		Take(&row).Error
	return row, err
}

func (r *renewalRepository) createRenewal(
	ctx context.Context,
	tx *gorm.DB,
	row *Renewal,
) error {
	return tx.WithContext(ctx).Create(row).Error
}

func (r *renewalRepository) updateLotVersioned(
	ctx context.Context,
	tx *gorm.DB,
	lotID uint64,
	expectedVersion uint,
	values map[string]any,
) error {
	result := tx.WithContext(ctx).Model(&core.Lot{}).
		Where("id = ? AND version = ?", lotID, expectedVersion).
		Updates(values)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return problem.Conflict(
			"WT_CONCURRENT_MODIFICATION",
			"wine ticket lot changed while renewal was being created",
		)
	}
	return nil
}

func (r *renewalRepository) updateRenewalVersioned(
	ctx context.Context,
	tx *gorm.DB,
	row Renewal,
	values map[string]any,
) error {
	result := tx.WithContext(ctx).Model(&Renewal{}).
		Where("id = ? AND version = ?", row.ID, row.Version).
		Updates(values)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return problem.Conflict(
			"WT_CONCURRENT_MODIFICATION",
			"wine ticket renewal changed concurrently",
		)
	}
	return nil
}

func renewalProjection(db *gorm.DB) *gorm.DB {
	return db.Table("wine_ticket_renewals renewal").
		Select(`
			renewal.*,
			lot.lot_no AS lot_no,
			payment.status AS payment_status,
			payment.client_payload AS payment_client_payload,
			compensation.status AS refund_status,
			compensation.provider_status AS refund_provider_status,
			compensation.failure_code AS refund_failure_code
		`).
		Joins("JOIN wine_ticket_lots lot ON lot.id = renewal.lot_id").
		Joins("LEFT JOIN payments payment ON payment.id = renewal.payment_id").
		Joins("LEFT JOIN refunds compensation ON compensation.id = renewal.compensating_refund_id")
}

func (r *renewalRepository) customerRenewalByNo(
	ctx context.Context,
	customerID uint64,
	renewalNo string,
) (renewalRecord, error) {
	var row renewalRecord
	err := renewalProjection(r.db.WithContext(ctx)).
		Where("renewal.customer_id = ? AND renewal.renewal_no = ?", customerID, renewalNo).
		Take(&row).Error
	return row, err
}

func (r *renewalRepository) customerRenewalByID(
	ctx context.Context,
	customerID, renewalID uint64,
) (renewalRecord, error) {
	var row renewalRecord
	err := renewalProjection(r.db.WithContext(ctx)).
		Where("renewal.customer_id = ? AND renewal.id = ?", customerID, renewalID).
		Take(&row).Error
	return row, err
}

func (r *renewalRepository) listCustomerRenewals(
	ctx context.Context,
	customerID uint64,
	query pagination.Query,
	status string,
) ([]renewalRecord, error) {
	db := renewalProjection(r.db.WithContext(ctx)).
		Where("renewal.customer_id = ?", customerID)
	if status != "" {
		db = db.Where("renewal.status = ?", status)
	}
	var err error
	db, err = pagination.ApplyIDCursor(db, query, "renewal.id", "desc")
	if err != nil {
		return nil, err
	}
	var rows []renewalRecord
	err = db.Order("renewal.id DESC").Limit(query.PageSize + 1).Scan(&rows).Error
	return rows, err
}

func (r *renewalRepository) renewalByPaymentKey(
	ctx context.Context,
	tx *gorm.DB,
	customerID uint64,
	key string,
) (Renewal, error) {
	var row Renewal
	err := tx.WithContext(ctx).
		Table("wine_ticket_renewals renewal").
		Select("renewal.*").
		Joins("JOIN payments payment ON payment.id = renewal.payment_id").
		Where(
			"renewal.customer_id = ? AND payment.biz_type = ? AND payment.idempotency_key = ?",
			customerID,
			RenewalPaymentBusiness,
			key,
		).
		Order("renewal.id DESC").
		Take(&row).Error
	return row, err
}

func (r *renewalRepository) renewalByID(
	ctx context.Context,
	db *gorm.DB,
	renewalID uint64,
	lock bool,
) (Renewal, error) {
	if db == nil {
		db = r.db
	}
	query := db.WithContext(ctx)
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row Renewal
	err := query.Where("id = ?", renewalID).Take(&row).Error
	return row, err
}

func (r *renewalRepository) lockRenewalAfterLot(
	ctx context.Context,
	tx *gorm.DB,
	renewalID, lotID uint64,
) (Renewal, error) {
	var row Renewal
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND lot_id = ?", renewalID, lotID).
		Take(&row).Error
	return row, err
}

func (r *renewalRepository) paymentByID(
	ctx context.Context,
	db *gorm.DB,
	paymentID uint64,
	lock bool,
) (order.Payment, error) {
	if db == nil {
		db = r.db
	}
	query := db.WithContext(ctx)
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row order.Payment
	err := query.Where("id = ?", paymentID).Take(&row).Error
	return row, err
}

func (r *renewalRepository) createCompensationRefund(
	ctx context.Context,
	tx *gorm.DB,
	row *refund.Row,
) error {
	return tx.WithContext(ctx).Create(row).Error
}

func (r *renewalRepository) lockCompensationRefunds(
	ctx context.Context,
	tx *gorm.DB,
	renewalID uint64,
) ([]refund.Row, error) {
	var rows []refund.Row
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("biz_type = ? AND biz_id = ?", RenewalCompensationRefundBusiness, renewalID).
		Order("id ASC").
		Find(&rows).Error
	return rows, err
}

func (r *renewalRepository) updateRefundVersioned(
	ctx context.Context,
	tx *gorm.DB,
	row refund.Row,
	values map[string]any,
) error {
	values["version"] = gorm.Expr("version + 1")
	result := tx.WithContext(ctx).Model(&refund.Row{}).
		Where("id = ? AND version = ?", row.ID, row.Version).
		Updates(values)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return problem.Conflict(
			"WT_CONCURRENT_MODIFICATION",
			"renewal compensation refund changed concurrently",
		)
	}
	return nil
}

func (r *renewalRepository) compensationRefundByID(
	rows []refund.Row,
	refundID uint64,
) (refund.Row, error) {
	for _, row := range rows {
		if row.ID == refundID {
			return row, nil
		}
	}
	return refund.Row{}, gorm.ErrRecordNotFound
}

func (r *renewalRepository) replacementFor(
	rows []refund.Row,
	refundID uint64,
) (refund.Row, error) {
	for _, row := range rows {
		if row.ReplacesRefundID != nil && *row.ReplacesRefundID == refundID {
			return row, nil
		}
	}
	return refund.Row{}, gorm.ErrRecordNotFound
}

func renewalNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}
