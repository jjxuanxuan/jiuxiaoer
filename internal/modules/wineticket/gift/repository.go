package gift

import (
	"context"
	"errors"
	"sort"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
)

type giftRepository struct {
	db *gorm.DB
}

type giftCustomerAccount struct {
	ID     uint64
	Status string
}

type giftRealnameVerification struct {
	Status      string
	AdultResult string
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
}

func newGiftRepository(db *gorm.DB) *giftRepository {
	return &giftRepository{db: db}
}

func (r *giftRepository) dbConn() *gorm.DB { return r.db }

func (r *giftRepository) createAudit(
	ctx context.Context,
	tx *gorm.DB,
	values map[string]any,
) error {
	return tx.WithContext(ctx).Table("audit_logs").Create(values).Error
}

func (r *giftRepository) createOutbox(
	ctx context.Context,
	tx *gorm.DB,
	values map[string]any,
) error {
	return tx.WithContext(ctx).Table("outbox_events").Create(values).Error
}

func (r *giftRepository) customerAccount(
	ctx context.Context,
	tx *gorm.DB,
	customerID uint64,
) (giftCustomerAccount, error) {
	var row giftCustomerAccount
	err := tx.WithContext(ctx).Table("customers").
		Select("id, status").
		Where("id = ? AND deleted_at IS NULL", customerID).
		Take(&row).Error
	return row, err
}

func (r *giftRepository) customerRealname(
	ctx context.Context,
	tx *gorm.DB,
	customerID uint64,
) (giftRealnameVerification, error) {
	var row giftRealnameVerification
	err := tx.WithContext(ctx).Table("customer_realname_verifications").
		Select("status, adult_result, expires_at, revoked_at").
		Where("customer_id = ?", customerID).
		Take(&row).Error
	return row, err
}

func (r *giftRepository) pendingIdentityVerification(
	ctx context.Context,
	tx *gorm.DB,
	customerID uint64,
	now time.Time,
) (bool, error) {
	var count int64
	err := tx.WithContext(ctx).Table("identity_verification_requests").
		Where(`
			customer_id = ?
			AND status IN ?
			AND (session_expires_at IS NULL OR session_expires_at > ?)
		`, customerID, []string{"creating_session", "pending"}, now).
		Count(&count).Error
	return count > 0, err
}

func giftProjectionQuery(db *gorm.DB) *gorm.DB {
	return db.Table("wine_ticket_gifts gift").
		Select(`
			gift.*,
			product.name AS product_name,
			product.brand_name AS product_brand_name,
			product.spec AS product_spec,
			product.image_url AS product_image_url,
			giver.nickname AS giver_nickname,
			receiver.nickname AS receiver_nickname
		`).
		Joins("JOIN products product ON product.id = gift.product_id").
		Joins("LEFT JOIN customers giver ON giver.id = gift.giver_customer_id AND giver.deleted_at IS NULL").
		Joins("LEFT JOIN customers receiver ON receiver.id = gift.receiver_customer_id AND receiver.deleted_at IS NULL")
}

func (r *giftRepository) listCustomerGifts(
	ctx context.Context,
	customerID uint64,
	direction string,
	status string,
	query pagination.Query,
) ([]giftProjection, error) {
	db := giftProjectionQuery(r.db.WithContext(ctx))
	if direction == giftListDirectionIn {
		db = db.Where("gift.receiver_customer_id = ?", customerID)
	} else {
		db = db.Where("gift.giver_customer_id = ?", customerID)
	}
	if status != "" {
		db = db.Where("gift.status = ?", status)
	}
	var err error
	db, err = pagination.ApplyIDCursor(db, query, "gift.id", "desc")
	if err != nil {
		return nil, err
	}
	var rows []giftProjection
	err = db.Order("gift.id DESC").Limit(query.PageSize + 1).Scan(&rows).Error
	return rows, err
}

func (r *giftRepository) customerGiftByNo(ctx context.Context, customerID uint64, giftNo string) (giftProjection, error) {
	var row giftProjection
	err := giftProjectionQuery(r.db.WithContext(ctx)).
		Where("gift.gift_no = ? AND (gift.giver_customer_id = ? OR gift.receiver_customer_id = ?)", giftNo, customerID, customerID).
		Take(&row).Error
	return row, err
}

func (r *giftRepository) giftProjectionByID(ctx context.Context, tx *gorm.DB, giftID uint64) (giftProjection, error) {
	if tx == nil {
		tx = r.db
	}
	var row giftProjection
	err := giftProjectionQuery(tx.WithContext(ctx)).Where("gift.id = ?", giftID).Take(&row).Error
	return row, err
}

// giftAnchorLot 只解析不可变的 FEFO 分组。
// 在通过一次有序查询锁定完整分组前，不会单独锁定锚点批次。
func (r *giftRepository) giftAnchorLot(ctx context.Context, tx *gorm.DB, ownerID uint64, lotNo string, now time.Time) (core.Lot, error) {
	var row core.Lot
	err := tx.WithContext(ctx).
		Where(`
			lot_no = ?
			AND owner_customer_id = ?
			AND status = ?
			AND available_quantity > 0
			AND expires_at > ?
		`, lotNo, ownerID, LotStatusActive, now).
		Take(&row).Error
	return row, err
}

// lockGiftGroupLots 是礼赠创建过程中唯一的首个批次锁。
// 查询匹配 idx_wt_lot_owner_group_fefo，并按确定的 FEFO 顺序
// 锁定全部未过期候选记录。
func (r *giftRepository) lockGiftGroupLots(ctx context.Context, tx *gorm.DB, anchor core.Lot, now time.Time) ([]core.Lot, error) {
	var rows []core.Lot
	err := tx.WithContext(ctx).Model(&core.Lot{}).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(`
			owner_customer_id = ?
			AND issuer_merchant_id = ?
			AND redeem_city_code = ?
			AND product_id = ?
			AND status = ?
			AND available_quantity > 0
			AND expires_at > ?
		`, anchor.OwnerCustomerID, anchor.IssuerMerchantID, anchor.RedeemCityCode, anchor.ProductID, LotStatusActive, now).
		Order("expires_at ASC, id ASC").
		Find(&rows).Error
	return rows, err
}

func (r *giftRepository) activeRenewalForLots(ctx context.Context, tx *gorm.DB, lotIDs []uint64) (bool, error) {
	if len(lotIDs) == 0 {
		return false, nil
	}
	var rows []activeRenewalGuard
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("lot_id IN ? AND status IN ?", lotIDs, []string{
			"pending_payment", "payment_unknown", "applying", "compensating_refund", "refund_exception",
		}).
		Order("id ASC").
		Find(&rows).Error
	return len(rows) > 0, err
}

func (r *giftRepository) createGift(ctx context.Context, tx *gorm.DB, row *Gift) error {
	return tx.WithContext(ctx).Create(row).Error
}

func (r *giftRepository) createGiftAllocation(ctx context.Context, tx *gorm.DB, row *GiftAllocation) error {
	return tx.WithContext(ctx).Create(row).Error
}

func (r *giftRepository) createGiftToken(ctx context.Context, tx *gorm.DB, row *GiftClaimToken) error {
	return tx.WithContext(ctx).Create(row).Error
}

func (r *giftRepository) lockGiftByNo(ctx context.Context, tx *gorm.DB, giftNo string) (Gift, error) {
	var row Gift
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("gift_no = ?", giftNo).
		Take(&row).Error
	return row, err
}

func (r *giftRepository) lockGiftByID(ctx context.Context, tx *gorm.DB, giftID uint64) (Gift, error) {
	var row Gift
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", giftID).
		Take(&row).Error
	return row, err
}

func (r *giftRepository) updateGift(ctx context.Context, tx *gorm.DB, giftID uint64, expectedVersion uint, values map[string]any) error {
	result := tx.WithContext(ctx).Model(&Gift{}).
		Where("id = ? AND version = ?", giftID, expectedVersion).
		Updates(values)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// lockGiftTokens 始终按 ID 升序锁定礼赠的全部令牌。
// 若锁定前过滤有效令牌，新令牌可能与领取或取消流程形成令牌与礼赠的循环锁。
func (r *giftRepository) lockGiftTokens(ctx context.Context, tx *gorm.DB, giftID uint64) ([]GiftClaimToken, error) {
	var rows []GiftClaimToken
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("gift_id = ?", giftID).
		Order("id ASC").
		Find(&rows).Error
	return rows, err
}

func (r *giftRepository) lockGiftAllocations(ctx context.Context, tx *gorm.DB, giftID uint64) ([]GiftAllocation, error) {
	var rows []GiftAllocation
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("gift_id = ?", giftID).
		Order("id ASC").
		Find(&rows).Error
	return rows, err
}

func (r *giftRepository) lockGiftLots(ctx context.Context, tx *gorm.DB, lotIDs []uint64) ([]core.Lot, error) {
	if len(lotIDs) == 0 {
		return nil, nil
	}
	unique := make(map[uint64]struct{}, len(lotIDs))
	canonicalIDs := make([]uint64, 0, len(lotIDs))
	for _, lotID := range lotIDs {
		if lotID == 0 {
			continue
		}
		if _, exists := unique[lotID]; exists {
			continue
		}
		unique[lotID] = struct{}{}
		canonicalIDs = append(canonicalIDs, lotID)
	}
	if len(canonicalIDs) == 0 {
		return nil, nil
	}
	sort.Slice(canonicalIDs, func(i, j int) bool {
		return canonicalIDs[i] < canonicalIDs[j]
	})
	var rows []core.Lot
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id IN ?", canonicalIDs).
		Order("id ASC").
		Find(&rows).Error
	return rows, err
}

// giftHoldTransactions 返回一笔礼赠完整且不可变的冻结台账。
// 调用方在变更任何权益状态前，必须将其与已锁定的分配记录比对。
func (r *giftRepository) giftHoldTransactions(
	ctx context.Context,
	tx *gorm.DB,
	giftID uint64,
) ([]core.Transaction, error) {
	var rows []core.Transaction
	err := tx.WithContext(ctx).
		Where(
			"biz_type = ? AND biz_id = ? AND transaction_type = ?",
			"gift",
			giftID,
			TransactionTypeGiftHold,
		).
		Order("id ASC").
		Find(&rows).Error
	return rows, err
}

func (r *giftRepository) updateGiftAllocation(ctx context.Context, tx *gorm.DB, allocationID uint64, values map[string]any) error {
	result := tx.WithContext(ctx).Model(&GiftAllocation{}).Where("id = ?", allocationID).Updates(values)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func (r *giftRepository) updateGiftToken(ctx context.Context, tx *gorm.DB, tokenID uint64, values map[string]any) error {
	result := tx.WithContext(ctx).Model(&GiftClaimToken{}).Where("id = ?", tokenID).Updates(values)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// giftIDByTokenDigest 被有意设计为无锁查询。
// 领取流程先解析不可变的礼赠 ID，再进入以礼赠记录为首的标准锁顺序。
func (r *giftRepository) giftIDByTokenDigest(ctx context.Context, digest string) (uint64, error) {
	var row struct {
		GiftID uint64
	}
	err := r.db.WithContext(ctx).Table("wine_ticket_gift_claim_tokens").
		Select("gift_id").
		Where("token_digest = ?", digest).
		Take(&row).Error
	return row.GiftID, err
}

func (r *giftRepository) previewByTokenDigest(ctx context.Context, digest string, now time.Time) (giftProjection, error) {
	var row giftProjection
	err := giftProjectionQuery(r.db.WithContext(ctx)).
		Joins("JOIN wine_ticket_gift_claim_tokens token ON token.gift_id = gift.id").
		Where(`
			token.token_digest = ?
			AND token.consumed_at IS NULL
			AND token.revoked_at IS NULL
			AND token.expires_at > ?
			AND gift.status = ?
			AND gift.claim_deadline > ?
			AND gift.earliest_expires_at > ?
		`, digest, now, GiftStatusPending, now, now).
		Take(&row).Error
	return row, err
}

func (r *giftRepository) dueGiftIDs(ctx context.Context, now time.Time, limit int) ([]uint64, error) {
	var ids []uint64
	err := r.db.WithContext(ctx).Model(&Gift{}).
		Where("status = ? AND claim_deadline <= ?", GiftStatusPending, now).
		Order("claim_deadline ASC, id ASC").
		Limit(limit).
		Pluck("id", &ids).Error
	return ids, err
}

func findLockedToken(tokens []GiftClaimToken, digest string) (GiftClaimToken, bool) {
	for _, token := range tokens {
		if token.TokenDigest == digest {
			return token, true
		}
	}
	return GiftClaimToken{}, false
}

func activeGiftTokenCount(tokens []GiftClaimToken, now time.Time) (int, *time.Time) {
	count := 0
	var earliest *time.Time
	for index := range tokens {
		token := tokens[index]
		if token.ConsumedAt != nil || token.RevokedAt != nil || !token.ExpiresAt.After(now) {
			continue
		}
		count++
		if earliest == nil || token.ExpiresAt.Before(*earliest) {
			value := token.ExpiresAt
			earliest = &value
		}
	}
	return count, earliest
}

func giftLotMap(rows []core.Lot) map[uint64]core.Lot {
	result := make(map[uint64]core.Lot, len(rows))
	for _, row := range rows {
		result[row.ID] = row
	}
	return result
}

func giftSourceLotIDs(allocations []GiftAllocation) []uint64 {
	result := make([]uint64, 0, len(allocations))
	seen := make(map[uint64]struct{}, len(allocations))
	for _, allocation := range allocations {
		if _, ok := seen[allocation.SourceLotID]; ok {
			continue
		}
		seen[allocation.SourceLotID] = struct{}{}
		result = append(result, allocation.SourceLotID)
	}
	return result
}

func isGiftNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}
