package gift

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

const (
	LotSourcePurchase = core.LotSourcePurchase
	LotSourceGift     = core.LotSourceGift
	LotStatusActive   = core.LotStatusActive
	LotStatusDepleted = core.LotStatusDepleted
	LotStatusExpired  = core.LotStatusExpired
)

type GiftService struct {
	repo    *giftRepository
	idStore *idempotency.Store
	ids     *snowflake.Generator
	assets  *core.AssetService
	pepper  []byte
	now     func() time.Time
}

func NewGiftService(db *gorm.DB, ids *snowflake.Generator, pepper string) *GiftService {
	return &GiftService{
		repo:    newGiftRepository(db),
		idStore: idempotency.NewStore(db),
		ids:     ids,
		assets:  core.NewAssetService(ids),
		pepper:  []byte(pepper),
		now:     time.Now,
	}
}

func (s *GiftService) List(
	ctx context.Context,
	claims *auth.Claims,
	query pagination.Query,
	direction string,
	status string,
) ([]GiftDTO, string, error) {
	customerID, err := giftCustomerIDWithPermission(claims, "wine_ticket_gift:list")
	if err != nil {
		return nil, "", err
	}
	direction = strings.TrimSpace(direction)
	if direction == "" {
		direction = giftListDirectionOut
	}
	if !validGiftDirection(direction) {
		return nil, "", problem.InvalidArgument("VALIDATION_FAILED", "direction must be sent or received")
	}
	status = strings.TrimSpace(status)
	if !validGiftStatus(status) {
		return nil, "", problem.InvalidArgument("VALIDATION_FAILED", "invalid gift status")
	}
	rows, err := s.repo.listCustomerGifts(ctx, customerID, direction, status, query)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		rows = rows[:query.PageSize]
		next = pagination.NextPageTokenWithCursor(query, idString(rows[len(rows)-1].ID))
	}
	items := make([]GiftDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, giftDTO(row))
	}
	return items, next, nil
}

func (s *GiftService) Detail(ctx context.Context, claims *auth.Claims, giftNo string) (GiftDTO, error) {
	customerID, err := giftCustomerIDWithPermission(claims, "wine_ticket_gift:view")
	if err != nil {
		return GiftDTO{}, err
	}
	if err := validateBusinessNo(giftNo, "gift_no"); err != nil {
		return GiftDTO{}, err
	}
	row, err := s.repo.customerGiftByNo(ctx, customerID, giftNo)
	if isGiftNotFound(err) {
		return GiftDTO{}, problem.NotFound("WT_GIFT_NOT_FOUND", "wine ticket gift not found")
	}
	if err != nil {
		return GiftDTO{}, err
	}
	return giftDTO(row), nil
}

func (s *GiftService) Create(
	ctx context.Context,
	claims *auth.Claims,
	method string,
	path string,
	key string,
	request GiftCreateRequest,
) (response GiftDTO, resultErr error) {
	customerID, err := giftCustomerIDWithPermission(claims, "wine_ticket_gift:create")
	if err != nil {
		return GiftDTO{}, err
	}
	requestForHash := request
	request, err = normalizeGiftCreateRequest(request)
	if err != nil {
		return GiftDTO{}, err
	}
	requestHash := idempotency.RequestHash(requestForHash)

	resultErr = s.repo.dbConn().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.startIdempotency(ctx, tx, "customer", customerID, method, path, key, requestHash)
		if err != nil {
			return err
		}
		if !started {
			return s.cachedIdempotency(ctx, tx, "customer", customerID, path, key, &response)
		}

		now := s.nowShanghai()
		if err := s.requireAdultCustomer(ctx, tx, customerID, now); err != nil {
			return err
		}

		// 无锁解析锚点。下面的首个批次锁会按 FEFO 顺序覆盖完整不可变分组。
		anchor, err := s.repo.giftAnchorLot(ctx, tx, customerID, request.SourceLotNo, now)
		if isGiftNotFound(err) {
			return problem.NotFound("WT_LOT_NOT_FOUND", "wine ticket lot not found")
		}
		if err != nil {
			return err
		}
		group, err := s.repo.lockGiftGroupLots(ctx, tx, anchor, now)
		if err != nil {
			return err
		}
		if !giftGroupContainsAnchor(group, anchor.ID) {
			return problem.Conflict("WT_CONCURRENT_MODIFICATION", "wine ticket lot group changed concurrently")
		}

		selected, err := selectGiftFEFO(group, request.Quantity)
		if err != nil {
			return err
		}
		selectedIDs := make([]uint64, 0, len(selected))
		for _, item := range selected {
			selectedIDs = append(selectedIDs, item.lot.ID)
		}
		hasRenewal, err := s.repo.activeRenewalForLots(ctx, tx, selectedIDs)
		if err != nil {
			return err
		}
		if hasRenewal {
			return problem.Conflict("WT_RENEWAL_IN_PROGRESS", "a selected wine ticket lot has an active renewal")
		}

		giftID := s.ids.Next()
		earliest := selected[0].lot.ExpiresAt
		for _, item := range selected[1:] {
			if item.lot.ExpiresAt.Before(earliest) {
				earliest = item.lot.ExpiresAt
			}
		}
		deadline := now.Add(time.Duration(giftClaimTTL) * time.Second)
		if earliest.Before(deadline) {
			deadline = earliest
		}
		gift := Gift{
			ID:                giftID,
			GiftNo:            "WTG" + idString(giftID),
			GiverCustomerID:   customerID,
			IssuerMerchantID:  anchor.IssuerMerchantID,
			ProductID:         anchor.ProductID,
			RedeemCityCode:    anchor.RedeemCityCode,
			Quantity:          request.Quantity,
			Message:           request.Message,
			Status:            GiftStatusPending,
			ClaimDeadline:     deadline,
			EarliestExpiresAt: earliest,
			Version:           1,
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		if err := s.repo.createGift(ctx, tx, &gift); err != nil {
			return giftWriteError(err)
		}

		assetRepo := core.NewTransactionAssetRepository(tx)
		for _, item := range selected {
			if _, err := s.assets.Freeze(ctx, assetRepo, core.AssetCommand{
				LotID:           item.lot.ID,
				OwnerCustomerID: customerID,
				Quantity:        item.quantity,
				MarkUsed:        true,
				TransactionType: TransactionTypeGiftHold,
				BizType:         "gift",
				BizID:           giftID,
				ActionKey:       fmt.Sprintf("gift_hold:%d:%d", giftID, item.lot.ID),
				OccurredAt:      now,
			}); err != nil {
				return giftWriteError(err)
			}
			allocation := GiftAllocation{
				ID:              s.ids.Next(),
				GiftID:          giftID,
				SourceLotID:     item.lot.ID,
				Quantity:        item.quantity,
				SourceExpiresAt: item.lot.ExpiresAt,
				Status:          GiftAllocationStatusHeld,
				CreatedAt:       now,
				UpdatedAt:       now,
			}
			if err := s.repo.createGiftAllocation(ctx, tx, &allocation); err != nil {
				return giftWriteError(err)
			}
		}

		projected, err := s.repo.giftProjectionByID(ctx, tx, giftID)
		if err != nil {
			return err
		}
		response = giftDTO(projected)
		if err := s.writeGiftAudit(ctx, tx, "customer", customerID, "wine_ticket.gift.create", gift, "", GiftStatusPending, map[string]any{
			"gift_no": gift.GiftNo, "quantity": gift.Quantity,
		}); err != nil {
			return err
		}
		return s.idStore.Succeed(ctx, tx, "customer", customerID, path, key, response)
	})
	return response, resultErr
}

type giftFEFOSelection struct {
	lot      core.Lot
	quantity uint
}

func selectGiftFEFO(group []core.Lot, quantity uint) ([]giftFEFOSelection, error) {
	remaining := quantity
	selected := make([]giftFEFOSelection, 0, len(group))
	for _, lot := range group {
		if remaining == 0 {
			break
		}
		take := lot.AvailableQuantity
		if take > remaining {
			take = remaining
		}
		if take == 0 {
			continue
		}
		selected = append(selected, giftFEFOSelection{lot: lot, quantity: take})
		remaining -= take
	}
	if remaining != 0 {
		return nil, problem.Conflict("WT_INSUFFICIENT_QUANTITY", "insufficient wine ticket quantity in the selected group")
	}
	return selected, nil
}

func giftGroupContainsAnchor(group []core.Lot, anchorID uint64) bool {
	for _, lot := range group {
		if lot.ID == anchorID {
			return true
		}
	}
	return false
}

func (s *GiftService) CreateShareToken(
	ctx context.Context,
	claims *auth.Claims,
	giftNo string,
	request GiftShareTokenRequest,
) (response GiftShareTokenDTO, resultErr error) {
	customerID, err := giftCustomerIDWithPermission(claims, "wine_ticket_gift:share")
	if err != nil {
		return GiftShareTokenDTO{}, err
	}
	if err := validateBusinessNo(giftNo, "gift_no"); err != nil {
		return GiftShareTokenDTO{}, err
	}
	if request.ExpectedGiftVersion == 0 {
		return GiftShareTokenDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "expected_gift_version must be positive")
	}
	if len(s.pepper) < 32 {
		return GiftShareTokenDTO{}, problem.Internal("wine gift token pepper is not configured")
	}

	var terminalErr error
	resultErr = s.repo.dbConn().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := s.nowShanghai()
		gift, err := s.repo.lockGiftByNo(ctx, tx, giftNo)
		if isGiftNotFound(err) || (err == nil && gift.GiverCustomerID != customerID) {
			return problem.NotFound("WT_GIFT_NOT_FOUND", "wine ticket gift not found")
		}
		if err != nil {
			return err
		}
		tokens, err := s.repo.lockGiftTokens(ctx, tx, gift.ID)
		if err != nil {
			return err
		}

		if gift.Status == GiftStatusPending && !now.Before(gift.ClaimDeadline) {
			allocations, lots, err := s.lockGiftAllocationsAndLots(ctx, tx, gift.ID)
			if err != nil {
				return err
			}
			if err := s.restoreGiftLocked(ctx, tx, gift, tokens, allocations, lots, GiftStatusExpiredReturned, now); err != nil {
				return err
			}
			if err := s.writeGiftAudit(
				ctx, tx, "customer", customerID, "wine_ticket.gift.timeout",
				gift, GiftStatusPending, GiftStatusExpiredReturned,
				map[string]any{"gift_no": gift.GiftNo, "trigger": "share"},
			); err != nil {
				return err
			}
			terminalErr = problem.Conflict("WT_GIFT_EXPIRED", "wine ticket gift claim deadline has passed")
			return nil
		}
		if err := pendingGiftProblem(gift); err != nil {
			return err
		}
		if gift.Version != request.ExpectedGiftVersion {
			return problem.Conflict("WT_CONCURRENT_MODIFICATION", "wine ticket gift version changed")
		}

		activeCount, earliestActiveExpiry := activeGiftTokenCount(tokens, now)
		if activeCount >= giftActiveTokenMax {
			retryAfter := 1
			if earliestActiveExpiry != nil {
				retryAfter = int(math.Ceil(earliestActiveExpiry.Sub(now).Seconds()))
				if retryAfter < 1 {
					retryAfter = 1
				}
			}
			detail := problem.TooManyRequests("RATE_LIMITED", "active wine gift share token limit reached")
			detail.Data = map[string]any{"retry_after_seconds": retryAfter}
			return detail
		}

		rawToken, err := newGiftShareToken()
		if err != nil {
			return err
		}
		expiresAt := now.Add(time.Duration(giftShareTokenTTL) * time.Second)
		if gift.ClaimDeadline.Before(expiresAt) {
			expiresAt = gift.ClaimDeadline
		}
		if gift.EarliestExpiresAt.Before(expiresAt) {
			expiresAt = gift.EarliestExpiresAt
		}
		if !now.Before(expiresAt) {
			return problem.Conflict("WT_GIFT_EXPIRED", "wine ticket gift claim deadline has passed")
		}

		token := GiftClaimToken{
			ID:                 s.ids.Next(),
			GiftID:             gift.ID,
			TokenDigest:        s.tokenDigest(rawToken),
			IssuedByCustomerID: customerID,
			ExpiresAt:          expiresAt,
			RequestID:          requestctx.RequestIDPtr(ctx),
			CreatedAt:          now,
		}
		if err := s.repo.createGiftToken(ctx, tx, &token); err != nil {
			return giftWriteError(err)
		}
		if err := s.repo.updateGift(ctx, tx, gift.ID, gift.Version, map[string]any{
			"version":    gorm.Expr("version + 1"),
			"updated_at": now,
		}); err != nil {
			return problem.Conflict("WT_CONCURRENT_MODIFICATION", "wine ticket gift changed concurrently")
		}
		if err := s.writeGiftAudit(
			ctx, tx, "customer", customerID, "wine_ticket.gift.share_token.issue",
			gift, GiftStatusPending, GiftStatusPending,
			map[string]any{
				"gift_no":            gift.GiftNo,
				"token_expires_at":   formatShanghai(expiresAt),
				"active_token_count": activeCount + 1,
			},
		); err != nil {
			return err
		}

		// rawToken 只存在于当前栈上的响应中，不会传给共享幂等存储、
		// 审计日志、发件箱或仓储。
		response = GiftShareTokenDTO{
			ShareToken:      rawToken,
			ExpiresAt:       formatShanghai(expiresAt),
			MiniProgramPath: "/pages/wine-ticket/gifts/claim?gift_token=" + rawToken,
		}
		return nil
	})
	if resultErr != nil {
		return GiftShareTokenDTO{}, resultErr
	}
	if terminalErr != nil {
		return GiftShareTokenDTO{}, terminalErr
	}
	return response, nil
}

func (s *GiftService) Preview(ctx context.Context, rawToken string) (GiftPreviewDTO, error) {
	if len(s.pepper) < 32 || !validGiftShareToken(rawToken) {
		return GiftPreviewDTO{}, invalidGiftTokenProblem()
	}
	row, err := s.repo.previewByTokenDigest(ctx, s.tokenDigest(rawToken), s.nowShanghai())
	if err != nil {
		// 匿名调用方不得区分未知、过期、撤销、已消费、已取消、已领取
		// 或内部悬空的令牌记录。
		return GiftPreviewDTO{}, invalidGiftTokenProblem()
	}
	return giftPreviewDTO(row), nil
}

func (s *GiftService) Claim(
	ctx context.Context,
	claims *auth.Claims,
	method string,
	path string,
	key string,
	rawToken string,
	_ GiftClaimRequest,
) (response GiftDTO, resultErr error) {
	customerID, err := giftCustomerIDWithPermission(claims, "wine_ticket_gift:claim")
	if err != nil {
		return GiftDTO{}, err
	}
	if len(s.pepper) < 32 || !validGiftShareToken(rawToken) {
		return GiftDTO{}, invalidGiftTokenProblem()
	}
	digest := s.tokenDigest(rawToken)

	// 事务获取首个业务锁前，必须通过该无锁摘要查询解析不可变礼赠 ID。
	giftID, err := s.repo.giftIDByTokenDigest(ctx, digest)
	if isGiftNotFound(err) {
		return GiftDTO{}, invalidGiftTokenProblem()
	}
	if err != nil {
		return GiftDTO{}, err
	}
	requestHash := idempotency.ResourceRequestHash("wine_ticket_gift_claim", digest, struct{}{})

	var terminalErr error
	resultErr = s.repo.dbConn().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.startIdempotency(ctx, tx, "customer", customerID, method, path, key, requestHash)
		if err != nil {
			return err
		}
		if !started {
			return s.cachedIdempotency(ctx, tx, "customer", customerID, path, key, &response)
		}

		now := s.nowShanghai()
		gift, err := s.repo.lockGiftByID(ctx, tx, giftID)
		if isGiftNotFound(err) {
			return invalidGiftTokenProblem()
		}
		if err != nil {
			return err
		}
		tokens, err := s.repo.lockGiftTokens(ctx, tx, gift.ID)
		if err != nil {
			return err
		}
		allocations, lots, err := s.lockGiftAllocationsAndLots(ctx, tx, gift.ID)
		if err != nil {
			return err
		}
		matchedToken, matched := findLockedToken(tokens, digest)

		if gift.Status == GiftStatusPending && !now.Before(gift.ClaimDeadline) {
			if err := s.restoreGiftLocked(ctx, tx, gift, tokens, allocations, lots, GiftStatusExpiredReturned, now); err != nil {
				return err
			}
			if err := s.writeGiftAudit(
				ctx, tx, "customer", customerID, "wine_ticket.gift.timeout",
				gift, GiftStatusPending, GiftStatusExpiredReturned,
				map[string]any{"gift_no": gift.GiftNo, "trigger": "claim"},
			); err != nil {
				return err
			}
			_ = s.idStore.Fail(ctx, tx, "customer", customerID, path, key)
			terminalErr = problem.Conflict("WT_GIFT_EXPIRED", "wine ticket gift claim deadline has passed")
			return nil
		}
		if gift.Status == GiftStatusClaimed {
			return problem.Conflict("WT_GIFT_ALREADY_CLAIMED", "wine ticket gift was already claimed")
		}
		if gift.Status == GiftStatusCancelled {
			return problem.Conflict("WT_GIFT_CANCELLED", "wine ticket gift was cancelled")
		}
		if gift.Status == GiftStatusExpiredReturned {
			return problem.Conflict("WT_GIFT_EXPIRED", "wine ticket gift claim deadline has passed")
		}
		if gift.Status != GiftStatusPending {
			return problem.Conflict("WT_GIFT_NOT_PENDING", "wine ticket gift is not pending")
		}
		if !matched || matchedToken.ConsumedAt != nil || matchedToken.RevokedAt != nil || !matchedToken.ExpiresAt.After(now) {
			return invalidGiftTokenProblem()
		}
		if gift.GiverCustomerID == customerID {
			return problem.Forbidden("WT_GIFT_SELF_CLAIM_FORBIDDEN", "a gift cannot be claimed by its giver")
		}
		if err := s.requireAdultCustomer(ctx, tx, customerID, now); err != nil {
			return err
		}
		if len(allocations) == 0 || len(lots) == 0 {
			return problem.Internal("wine ticket gift allocation evidence is missing")
		}
		if err := s.validateGiftHoldEvidence(
			ctx,
			tx,
			gift,
			allocations,
			lots,
		); err != nil {
			return err
		}

		sourceLots := giftLotMap(lots)
		assetRepo := core.NewTransactionAssetRepository(tx)
		for _, allocation := range allocations {
			if allocation.Status != GiftAllocationStatusHeld || allocation.ReceiverLotID != nil {
				return problem.Conflict("WT_GIFT_NOT_PENDING", "wine ticket gift allocation is not claimable")
			}
			source, ok := sourceLots[allocation.SourceLotID]
			if !ok || source.OwnerCustomerID != gift.GiverCustomerID ||
				source.IssuerMerchantID != gift.IssuerMerchantID ||
				source.ProductID != gift.ProductID ||
				source.RedeemCityCode != gift.RedeemCityCode ||
				!source.ExpiresAt.Equal(allocation.SourceExpiresAt) {
				return problem.Internal("wine ticket gift source lineage is inconsistent")
			}
			receiverLotID := s.ids.Next()
			receiverLot := core.Lot{
				ID:                receiverLotID,
				LotNo:             "WTL" + idString(receiverLotID),
				OwnerCustomerID:   customerID,
				PurchaseID:        source.PurchaseID,
				SourceType:        LotSourceGift,
				SourceLotID:       giftUint64Ptr(source.ID),
				SourceGiftID:      giftUint64Ptr(gift.ID),
				IssuerMerchantID:  source.IssuerMerchantID,
				ProductID:         source.ProductID,
				RedeemCityCode:    source.RedeemCityCode,
				TotalQuantity:     allocation.Quantity,
				AvailableQuantity: allocation.Quantity,
				OriginalExpiresAt: source.OriginalExpiresAt,
				ExpiresAt:         source.ExpiresAt,
				ExpiryChangedAt:   now,
				RenewalCount:      source.RenewalCount,
				EverUsed:          true,
				Status:            LotStatusActive,
				Version:           1,
				CreatedAt:         now,
				UpdatedAt:         now,
			}
			if _, err := s.assets.Transfer(ctx, assetRepo, core.TransferCommand{
				ReceiverLot:     receiverLot,
				TransactionType: TransactionTypeGiftClaim,
				BizType:         "gift",
				BizID:           gift.ID,
				ActionKey:       fmt.Sprintf("gift_claim:%d:%d", gift.ID, receiverLotID),
				OccurredAt:      now,
			}); err != nil {
				return giftWriteError(err)
			}
			if err := s.repo.updateGiftAllocation(ctx, tx, allocation.ID, map[string]any{
				"receiver_lot_id": receiverLotID,
				"status":          GiftAllocationStatusClaimed,
				"updated_at":      now,
			}); err != nil {
				return err
			}
		}

		for _, token := range tokens {
			if token.ID == matchedToken.ID {
				if err := s.repo.updateGiftToken(ctx, tx, token.ID, map[string]any{
					"consumed_at": now,
					"revoked_at":  nil,
				}); err != nil {
					return err
				}
				continue
			}
			if token.ConsumedAt == nil && token.RevokedAt == nil {
				if err := s.repo.updateGiftToken(ctx, tx, token.ID, map[string]any{"revoked_at": now}); err != nil {
					return err
				}
			}
		}
		if err := s.repo.updateGift(ctx, tx, gift.ID, gift.Version, map[string]any{
			"receiver_customer_id": customerID,
			"status":               GiftStatusClaimed,
			"claimed_at":           now,
			"version":              gorm.Expr("version + 1"),
			"updated_at":           now,
		}); err != nil {
			return problem.Conflict("WT_CONCURRENT_MODIFICATION", "wine ticket gift changed concurrently")
		}
		if err := s.writeGiftAudit(
			ctx, tx, "customer", customerID, "wine_ticket.gift.claim",
			gift, GiftStatusPending, GiftStatusClaimed,
			map[string]any{"gift_no": gift.GiftNo, "quantity": gift.Quantity},
		); err != nil {
			return err
		}
		if err := s.writeGiftOutbox(ctx, tx, "wine_ticket.gift_claimed", gift, map[string]any{
			"gift_no":              gift.GiftNo,
			"giver_customer_id":    idString(gift.GiverCustomerID),
			"receiver_customer_id": idString(customerID),
			"product_id":           idString(gift.ProductID),
			"quantity":             gift.Quantity,
		}); err != nil {
			return err
		}
		projected, err := s.repo.giftProjectionByID(ctx, tx, gift.ID)
		if err != nil {
			return err
		}
		response = giftDTO(projected)
		return s.idStore.Succeed(ctx, tx, "customer", customerID, path, key, response)
	})
	if resultErr != nil {
		return GiftDTO{}, resultErr
	}
	if terminalErr != nil {
		return GiftDTO{}, terminalErr
	}
	return response, nil
}

func (s *GiftService) Cancel(
	ctx context.Context,
	claims *auth.Claims,
	method string,
	path string,
	key string,
	giftNo string,
	request GiftExpectedVersionRequest,
) (response GiftDTO, resultErr error) {
	customerID, err := giftCustomerIDWithPermission(claims, "wine_ticket_gift:cancel")
	if err != nil {
		return GiftDTO{}, err
	}
	if err := validateBusinessNo(giftNo, "gift_no"); err != nil {
		return GiftDTO{}, err
	}
	if request.ExpectedVersion == 0 {
		return GiftDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "expected_version must be positive")
	}
	requestHash := idempotency.ResourceRequestHash("wine_ticket_gift_cancel", giftNo, request)

	var terminalErr error
	resultErr = s.repo.dbConn().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.startIdempotency(ctx, tx, "customer", customerID, method, path, key, requestHash)
		if err != nil {
			return err
		}
		if !started {
			return s.cachedIdempotency(ctx, tx, "customer", customerID, path, key, &response)
		}

		now := s.nowShanghai()
		gift, err := s.repo.lockGiftByNo(ctx, tx, giftNo)
		if isGiftNotFound(err) || (err == nil && gift.GiverCustomerID != customerID) {
			return problem.NotFound("WT_GIFT_NOT_FOUND", "wine ticket gift not found")
		}
		if err != nil {
			return err
		}
		tokens, err := s.repo.lockGiftTokens(ctx, tx, gift.ID)
		if err != nil {
			return err
		}
		allocations, lots, err := s.lockGiftAllocationsAndLots(ctx, tx, gift.ID)
		if err != nil {
			return err
		}

		if gift.Status == GiftStatusPending && !now.Before(gift.ClaimDeadline) {
			if err := s.restoreGiftLocked(ctx, tx, gift, tokens, allocations, lots, GiftStatusExpiredReturned, now); err != nil {
				return err
			}
			if err := s.writeGiftAudit(
				ctx, tx, "customer", customerID, "wine_ticket.gift.timeout",
				gift, GiftStatusPending, GiftStatusExpiredReturned,
				map[string]any{"gift_no": gift.GiftNo, "trigger": "cancel"},
			); err != nil {
				return err
			}
			_ = s.idStore.Fail(ctx, tx, "customer", customerID, path, key)
			terminalErr = problem.Conflict("WT_GIFT_EXPIRED", "wine ticket gift claim deadline has passed")
			return nil
		}
		if err := pendingGiftProblem(gift); err != nil {
			return err
		}
		if gift.Version != request.ExpectedVersion {
			return problem.Conflict("WT_CONCURRENT_MODIFICATION", "wine ticket gift version changed")
		}
		if err := s.restoreGiftLocked(ctx, tx, gift, tokens, allocations, lots, GiftStatusCancelled, now); err != nil {
			return err
		}
		if err := s.writeGiftAudit(
			ctx, tx, "customer", customerID, "wine_ticket.gift.cancel",
			gift, GiftStatusPending, GiftStatusCancelled,
			map[string]any{"gift_no": gift.GiftNo, "quantity": gift.Quantity},
		); err != nil {
			return err
		}
		projected, err := s.repo.giftProjectionByID(ctx, tx, gift.ID)
		if err != nil {
			return err
		}
		response = giftDTO(projected)
		return s.idStore.Succeed(ctx, tx, "customer", customerID, path, key, response)
	})
	if resultErr != nil {
		return GiftDTO{}, resultErr
	}
	if terminalErr != nil {
		return GiftDTO{}, terminalErr
	}
	return response, nil
}

// ExpireDue 恢复已经越过严格领取边界的待领取礼赠。
// 后台任务可以安全重跑：终态礼赠会被跳过，每个权益副作用也都有全局唯一 action_key。
func (s *GiftService) ExpireDue(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	now := s.nowShanghai()
	ids, err := s.repo.dueGiftIDs(ctx, now, limit)
	if err != nil {
		return 0, err
	}
	expired := 0
	for _, giftID := range ids {
		didExpire := false
		err := s.repo.dbConn().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			gift, err := s.repo.lockGiftByID(ctx, tx, giftID)
			if isGiftNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			tokens, err := s.repo.lockGiftTokens(ctx, tx, gift.ID)
			if err != nil {
				return err
			}
			allocations, lots, err := s.lockGiftAllocationsAndLots(ctx, tx, gift.ID)
			if err != nil {
				return err
			}
			if gift.Status != GiftStatusPending || now.Before(gift.ClaimDeadline) {
				return nil
			}
			if err := s.restoreGiftLocked(ctx, tx, gift, tokens, allocations, lots, GiftStatusExpiredReturned, now); err != nil {
				return err
			}
			if err := s.writeGiftAudit(
				ctx, tx, "system", 0, "wine_ticket.gift.timeout",
				gift, GiftStatusPending, GiftStatusExpiredReturned,
				map[string]any{"gift_no": gift.GiftNo, "trigger": "worker"},
			); err != nil {
				return err
			}
			didExpire = true
			return nil
		})
		if err != nil {
			return expired, err
		}
		if didExpire {
			expired++
		}
	}
	return expired, nil
}

func (s *GiftService) lockGiftAllocationsAndLots(
	ctx context.Context,
	tx *gorm.DB,
	giftID uint64,
) ([]GiftAllocation, []core.Lot, error) {
	allocations, err := s.repo.lockGiftAllocations(ctx, tx, giftID)
	if err != nil {
		return nil, nil, err
	}
	lots, err := s.repo.lockGiftLots(ctx, tx, giftSourceLotIDs(allocations))
	if err != nil {
		return nil, nil, err
	}
	return allocations, lots, nil
}

func (s *GiftService) restoreGiftLocked(
	ctx context.Context,
	tx *gorm.DB,
	gift Gift,
	tokens []GiftClaimToken,
	allocations []GiftAllocation,
	lots []core.Lot,
	terminalStatus string,
	now time.Time,
) error {
	if terminalStatus != GiftStatusCancelled && terminalStatus != GiftStatusExpiredReturned {
		return problem.Internal("invalid gift restore terminal status")
	}
	if len(allocations) == 0 {
		return problem.Internal("wine ticket gift allocation evidence is missing")
	}
	if err := s.validateGiftHoldEvidence(
		ctx,
		tx,
		gift,
		allocations,
		lots,
	); err != nil {
		return err
	}
	sourceLots := giftLotMap(lots)
	assetRepo := core.NewTransactionAssetRepository(tx)
	for _, allocation := range allocations {
		if allocation.Status != GiftAllocationStatusHeld || allocation.ReceiverLotID != nil {
			return problem.Conflict("WT_GIFT_NOT_PENDING", "wine ticket gift allocation is not restorable")
		}
		source, ok := sourceLots[allocation.SourceLotID]
		if !ok ||
			source.OwnerCustomerID != gift.GiverCustomerID ||
			source.IssuerMerchantID != gift.IssuerMerchantID ||
			source.ProductID != gift.ProductID ||
			source.RedeemCityCode != gift.RedeemCityCode ||
			!source.ExpiresAt.Equal(allocation.SourceExpiresAt) {
			return problem.Internal("wine ticket gift source lineage is inconsistent")
		}
		before := source.AvailableQuantity
		if before > source.TotalQuantity || allocation.Quantity > source.TotalQuantity-before {
			return problem.Internal("wine ticket gift restore would exceed source lot quantity")
		}
		restoreActionKey := fmt.Sprintf("gift_restore:%d:%d", gift.ID, source.ID)
		expiryActionKey := fmt.Sprintf(
			"expiry:%d:%d:after:gift_restore:%d",
			source.ID,
			source.ExpiresAt.UnixMilli(),
			gift.ID,
		)
		if _, err := s.assets.Restore(ctx, assetRepo, core.AssetCommand{
			LotID:           source.ID,
			OwnerCustomerID: source.OwnerCustomerID,
			Quantity:        allocation.Quantity,
			TransactionType: TransactionTypeGiftRestore,
			BizType:         "gift",
			BizID:           gift.ID,
			ActionKey:       restoreActionKey,
			OccurredAt:      now,
			ExpiryEvidence: &core.AssetEvidence{
				TransactionType: TransactionTypeExpiry,
				BizType:         "gift",
				BizID:           gift.ID,
				ActionKey:       expiryActionKey,
			},
		}); err != nil {
			return giftWriteError(err)
		}
		if err := s.repo.updateGiftAllocation(ctx, tx, allocation.ID, map[string]any{
			"status":     GiftAllocationStatusRestored,
			"updated_at": now,
		}); err != nil {
			return err
		}
	}

	for _, token := range tokens {
		if token.ConsumedAt == nil && token.RevokedAt == nil {
			if err := s.repo.updateGiftToken(ctx, tx, token.ID, map[string]any{"revoked_at": now}); err != nil {
				return err
			}
		}
	}
	values := map[string]any{
		"status":      terminalStatus,
		"returned_at": now,
		"version":     gorm.Expr("version + 1"),
		"updated_at":  now,
	}
	if terminalStatus == GiftStatusCancelled {
		values["cancelled_at"] = now
	}
	if err := s.repo.updateGift(ctx, tx, gift.ID, gift.Version, values); err != nil {
		return problem.Conflict("WT_CONCURRENT_MODIFICATION", "wine ticket gift changed concurrently")
	}
	return s.writeGiftOutbox(ctx, tx, "wine_ticket.gift_returned", gift, map[string]any{
		"gift_no":           gift.GiftNo,
		"giver_customer_id": idString(gift.GiverCustomerID),
		"product_id":        idString(gift.ProductID),
		"quantity":          gift.Quantity,
		"status":            terminalStatus,
	})
}

// validateGiftHoldEvidence 在可变礼赠状态不再由每条分配记录对应的一条
// 精确不可变冻结流水支撑时，以失败关闭方式阻止领取和恢复。
// 调用方已按礼赠记录 -> 分配记录 -> 批次的标准顺序持锁；
// 台账记录只追加，因此无需额外行锁。
func (s *GiftService) validateGiftHoldEvidence(
	ctx context.Context,
	tx *gorm.DB,
	gift Gift,
	allocations []GiftAllocation,
	lots []core.Lot,
) error {
	if gift.Quantity == 0 || len(allocations) == 0 {
		return problem.Internal("wine ticket gift allocation evidence is missing")
	}
	holds, err := s.repo.giftHoldTransactions(ctx, tx, gift.ID)
	if err != nil {
		return err
	}
	if len(holds) != len(allocations) {
		return problem.Internal("wine ticket gift hold ledger is incomplete")
	}

	sourceLots := giftLotMap(lots)
	holdsByAction := make(map[string]core.Transaction, len(holds))
	for _, hold := range holds {
		if _, duplicate := holdsByAction[hold.ActionKey]; duplicate {
			return problem.Internal("wine ticket gift hold ledger is duplicated")
		}
		holdsByAction[hold.ActionKey] = hold
	}

	seenLots := make(map[uint64]struct{}, len(allocations))
	var allocationTotal uint
	for _, allocation := range allocations {
		if allocation.SourceLotID == 0 || allocation.Quantity == 0 {
			return problem.Internal("wine ticket gift allocation evidence is invalid")
		}
		if _, duplicate := seenLots[allocation.SourceLotID]; duplicate {
			return problem.Internal("wine ticket gift allocation evidence is duplicated")
		}
		seenLots[allocation.SourceLotID] = struct{}{}
		if allocationTotal > ^uint(0)-allocation.Quantity {
			return problem.Internal("wine ticket gift allocation quantity overflow")
		}
		allocationTotal += allocation.Quantity

		source, ok := sourceLots[allocation.SourceLotID]
		if !ok ||
			source.OwnerCustomerID != gift.GiverCustomerID ||
			source.IssuerMerchantID != gift.IssuerMerchantID ||
			source.ProductID != gift.ProductID ||
			source.RedeemCityCode != gift.RedeemCityCode ||
			!source.ExpiresAt.Equal(allocation.SourceExpiresAt) {
			return problem.Internal("wine ticket gift source lineage is inconsistent")
		}

		actionKey := fmt.Sprintf(
			"gift_hold:%d:%d",
			gift.ID,
			allocation.SourceLotID,
		)
		hold, ok := holdsByAction[actionKey]
		if !ok ||
			allocation.Quantity > uint(^uint(0)>>1) ||
			hold.LotID != allocation.SourceLotID ||
			hold.OwnerCustomerID != gift.GiverCustomerID ||
			hold.TransactionType != TransactionTypeGiftHold ||
			hold.BizType != "gift" ||
			hold.BizID != gift.ID ||
			hold.QuantityDelta != -int(allocation.Quantity) ||
			hold.BeforeAvailableQuantity < allocation.Quantity ||
			hold.AfterAvailableQuantity !=
				hold.BeforeAvailableQuantity-allocation.Quantity {
			return problem.Internal("wine ticket gift hold ledger is inconsistent")
		}
		delete(holdsByAction, actionKey)
	}
	if allocationTotal != gift.Quantity || len(holdsByAction) != 0 {
		return problem.Internal("wine ticket gift allocation quantity does not match its hold ledger")
	}
	return nil
}

func (s *GiftService) writeGiftAudit(
	ctx context.Context,
	tx *gorm.DB,
	actorType string,
	actorID uint64,
	action string,
	gift Gift,
	beforeStatus string,
	afterStatus string,
	safeMetadata map[string]any,
) error {
	before := map[string]any{}
	if beforeStatus != "" {
		before["status"] = beforeStatus
		before["version"] = gift.Version
	}
	afterVersion := gift.Version + 1
	if beforeStatus == "" {
		afterVersion = gift.Version
	}
	after := make(map[string]any, len(safeMetadata)+2)
	for key, value := range safeMetadata {
		after[key] = value
	}
	if afterStatus != "" {
		after["status"] = afterStatus
		after["version"] = afterVersion
	}
	values := map[string]any{
		"id":            s.ids.Next(),
		"event_id":      uuid.NewString(),
		"actor_type":    actorType,
		"actor_id":      actorID,
		"action":        action,
		"resource_type": "wine_ticket_gift",
		"resource_id":   gift.ID,
		"before_data":   jsonData(before),
		"after_data":    jsonData(after),
		"result":        "success",
		"before_status": giftOptionalString(beforeStatus),
		"after_status":  giftOptionalString(afterStatus),
		"version":       afterVersion,
		"request_id":    requestctx.RequestIDPtr(ctx),
		"ip_hash":       requestctx.IPHashPtr(ctx),
		"user_agent":    requestctx.UserAgentPtr(ctx),
		"created_at":    s.nowShanghai(),
	}
	if accountID := requestctx.AccountID(ctx); accountID != 0 {
		values["account_id"] = accountID
	}
	return s.repo.createAudit(ctx, tx, values)
}

func (s *GiftService) writeGiftOutbox(
	ctx context.Context,
	tx *gorm.DB,
	eventType string,
	gift Gift,
	safePayload map[string]any,
) error {
	return s.repo.createOutbox(ctx, tx, map[string]any{
		"id":             s.ids.Next(),
		"event_id":       uuid.NewString(),
		"event_type":     eventType,
		"event_version":  1,
		"spec_version":   "1.0",
		"aggregate_type": "wine_ticket_gift",
		"aggregate_id":   gift.ID,
		"producer":       "wine-ticket",
		"payload":        jsonData(safePayload),
		"status":         "pending",
		"retry_count":    0,
		"request_id":     requestctx.RequestIDPtr(ctx),
		"created_at":     s.nowShanghai(),
	})
}

func (s *GiftService) requireAdultCustomer(ctx context.Context, tx *gorm.DB, customerID uint64, now time.Time) error {
	customer, err := s.repo.customerAccount(ctx, tx, customerID)
	if isGiftNotFound(err) || (err == nil && customer.Status != "active") {
		return problem.Forbidden("PERM_FORBIDDEN", "customer account is not active")
	}
	if err != nil {
		return err
	}

	current, err := s.repo.customerRealname(ctx, tx, customerID)
	valid := err == nil &&
		current.Status == "verified" &&
		current.AdultResult == "adult" &&
		current.RevokedAt == nil &&
		(current.ExpiresAt == nil || current.ExpiresAt.After(now))
	if valid {
		return nil
	}
	if err != nil && !isGiftNotFound(err) {
		return err
	}

	pending, err := s.repo.pendingIdentityVerification(ctx, tx, customerID, now)
	if err != nil {
		return err
	}
	if pending {
		return problem.Conflict("IDENTITY_VERIFICATION_PENDING", "identity verification is still processing")
	}
	if current.Status == "verified" && current.AdultResult == "minor" && current.RevokedAt == nil {
		return problem.New(http.StatusUnprocessableEntity, "UNDERAGE_RESTRICTED", "Unprocessable Entity", "wine gift actions require an adult customer")
	}
	return problem.New(http.StatusUnprocessableEntity, "REALNAME_REQUIRED", "Unprocessable Entity", "valid real-name verification required")
}

func (s *GiftService) tokenDigest(rawToken string) string {
	sum := sha256.New()
	_, _ = sum.Write([]byte(rawToken))
	_, _ = sum.Write(s.pepper)
	return hex.EncodeToString(sum.Sum(nil))
}

func newGiftShareToken() (string, error) {
	payload := make([]byte, 32)
	if _, err := rand.Read(payload); err != nil {
		return "", problem.Internal("failed to generate wine gift share token")
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func validGiftShareToken(raw string) bool {
	if len(raw) < giftTokenMinLength || len(raw) > giftTokenMaxLength {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	return err == nil && len(decoded) >= 32
}

func invalidGiftTokenProblem() error {
	return problem.NotFound("WT_GIFT_TOKEN_INVALID", "wine ticket gift is unavailable")
}

func pendingGiftProblem(gift Gift) error {
	switch gift.Status {
	case GiftStatusPending:
		return nil
	case GiftStatusClaimed:
		return problem.Conflict("WT_GIFT_NOT_PENDING", "wine ticket gift is not pending")
	case GiftStatusCancelled:
		return problem.Conflict("WT_GIFT_CANCELLED", "wine ticket gift was cancelled")
	case GiftStatusExpiredReturned:
		return problem.Conflict("WT_GIFT_EXPIRED", "wine ticket gift claim deadline has passed")
	default:
		return problem.Conflict("WT_GIFT_NOT_PENDING", "wine ticket gift is not pending")
	}
}

func giftCustomerIDWithPermission(claims *auth.Claims, permission string) (uint64, error) {
	if claims == nil || claims.AccountType != "customer" {
		return 0, problem.Unauthorized("AUTH_UNAUTHORIZED", "customer authentication required")
	}
	customerID, err := strconv.ParseUint(claims.CustomerID, 10, 64)
	if err != nil || customerID == 0 {
		return 0, problem.Unauthorized("AUTH_UNAUTHORIZED", "invalid customer identity")
	}
	for _, granted := range claims.Permissions {
		if granted == permission {
			return customerID, nil
		}
	}
	return 0, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
}

func (s *GiftService) nowShanghai() time.Time {
	return s.now().In(shanghaiLocation).Truncate(time.Millisecond)
}

func giftUint64Ptr(value uint64) *uint64 { return &value }

func giftWriteError(err error) error {
	if isGiftDuplicateKey(err) {
		return problem.Conflict("WT_CONCURRENT_MODIFICATION", "wine ticket gift changed concurrently")
	}
	return err
}

func isGiftDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "duplicate") ||
		strings.Contains(message, "unique constraint") ||
		strings.Contains(message, "constraint failed")
}

func (s *GiftService) startIdempotency(
	ctx context.Context,
	tx *gorm.DB,
	actorType string,
	actorID uint64,
	method string,
	path string,
	key string,
	requestHash string,
) (bool, error) {
	return s.idStore.StartAt(
		ctx,
		tx,
		s.ids.Next(),
		actorType,
		actorID,
		method,
		path,
		key,
		requestHash,
		s.now(),
	)
}

func (s *GiftService) cachedIdempotency(
	ctx context.Context,
	tx *gorm.DB,
	actorType string,
	actorID uint64,
	path string,
	key string,
	out any,
) error {
	found, err := s.idStore.CachedResponse(ctx, tx, actorType, actorID, path, key, out)
	if err != nil {
		return err
	}
	if !found {
		return problem.Conflict("IDEMPOTENCY_IN_PROGRESS", "request with the same idempotency key is still processing")
	}
	return nil
}

func giftTimePtr(value time.Time) *time.Time { return &value }

func giftOptionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
