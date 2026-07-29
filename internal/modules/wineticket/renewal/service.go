package renewal

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

const renewalQuoteTTL = 10 * time.Minute

// RenewalService 与购买 Service 有意分离，
// 因为每种资金业务都需要独立的 order.PaymentSettlementHandler。
type RenewalService struct {
	core           *serviceCore
	repo           *renewalRepository
	ids            *snowflake.Generator
	assets         *core.AssetService
	paymentService *order.Service
	quoteSecret    []byte
	wechatAppID    string
	now            func() time.Time
}

func NewRenewalService(
	db *gorm.DB,
	ids *snowflake.Generator,
	quoteSecret string,
) *RenewalService {
	return &RenewalService{
		core:        newServiceCore(db, ids),
		repo:        newRenewalRepository(db),
		ids:         ids,
		assets:      core.NewAssetService(ids),
		quoteSecret: []byte(quoteSecret),
		now:         time.Now,
	}
}

func (s *RenewalService) WithPaymentService(service *order.Service) *RenewalService {
	s.paymentService = service
	return s
}

func (s *RenewalService) WithWeChatAppID(appID string) *RenewalService {
	s.wechatAppID = strings.TrimSpace(appID)
	s.core.wechatAppID = s.wechatAppID
	return s
}

// WithRenewalClock 用于后台任务及确定性的边界测试。
func (s *RenewalService) WithRenewalClock(now func() time.Time) *RenewalService {
	if now != nil {
		s.now = now
		s.core.setClock(now)
		s.assets.WithClock(now)
	}
	return s
}

func (s *RenewalService) nowShanghai() time.Time {
	return s.now().In(shanghaiLocation).Truncate(time.Millisecond)
}

func (s *RenewalService) RenewalQuote(
	ctx context.Context,
	claims *auth.Claims,
	lotNo string,
) (RenewalQuoteDTO, error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_renewal:quote")
	if err != nil {
		return RenewalQuoteDTO{}, err
	}
	lotNo = strings.TrimSpace(lotNo)
	if err := validateBusinessNo(lotNo, "lot_no"); err != nil {
		return RenewalQuoteDTO{}, err
	}
	if err := s.validateQuoteConfiguration(); err != nil {
		return RenewalQuoteDTO{}, err
	}

	lot, err := s.repo.customerLotByNo(ctx, nil, customerID, lotNo, false)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return RenewalQuoteDTO{}, problem.NotFound("WT_LOT_NOT_FOUND", "wine ticket lot not found")
	}
	if err != nil {
		return RenewalQuoteDTO{}, err
	}
	policy, err := s.loadRenewalPolicy(ctx, nil, lot)
	if err != nil {
		return RenewalQuoteDTO{}, err
	}
	blocked, err := s.repo.hasActiveHold(ctx, nil, lot.ID)
	if err != nil {
		return RenewalQuoteDTO{}, err
	}
	active, err := s.repo.activeRenewal(ctx, nil, lot.ID, false)
	hasActiveRenewal := err == nil && active.ID != 0
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return RenewalQuoteDTO{}, err
	}
	now := s.nowShanghai()
	if err := validateRenewalEligibility(lot, policy.Policy, blocked, hasActiveRenewal, now); err != nil {
		return RenewalQuoteDTO{}, err
	}

	newExpiry := renewalNewExpiry(lot.ExpiresAt, policy.Policy.ExtensionDays)
	quoteExpiry := now.Add(renewalQuoteTTL)
	if quoteExpiry.After(lot.ExpiresAt) {
		quoteExpiry = lot.ExpiresAt.In(shanghaiLocation).Truncate(time.Millisecond)
	}
	claimsPayload := renewalQuoteClaims{
		SchemaVersion:      1,
		CustomerID:         idString(customerID),
		LotID:              idString(lot.ID),
		LotNo:              lot.LotNo,
		ExpectedLotVersion: lot.Version,
		ExtensionDays:      uint(policy.Policy.ExtensionDays),
		FeeAmount:          policy.Policy.FeeAmount,
		OldExpiresAtMS:     lot.ExpiresAt.UnixMilli(),
		NewExpiresAtMS:     newExpiry.UnixMilli(),
		QuoteExpiresAtMS:   quoteExpiry.UnixMilli(),
		PolicyDigest:       renewalPolicyDigest(policy.PolicySnapshot),
	}
	token, err := s.signRenewalQuote(claimsPayload)
	if err != nil {
		return RenewalQuoteDTO{}, err
	}
	return RenewalQuoteDTO{
		Eligible:        true,
		ReasonCode:      "eligible",
		ExtensionDays:   uint(policy.Policy.ExtensionDays),
		FeeAmount:       policy.Policy.FeeAmount,
		OldExpiresAt:    formatShanghai(lot.ExpiresAt),
		NewExpiresAt:    formatShanghai(newExpiry),
		ExpectedVersion: lot.Version,
		QuoteExpiresAt:  formatShanghai(quoteExpiry),
		QuoteToken:      token,
		RenewalCount:    lot.RenewalCount,
		MaxRenewalCount: uint(policy.Policy.MaxCount),
		PolicySummary:   renewalPolicySummary(policy.Policy),
	}, nil
}

func (s *RenewalService) CreateRenewal(
	ctx context.Context,
	claims *auth.Claims,
	method, path, key, lotNo string,
	req RenewalCreateRequest,
) (response RenewalDTO, resultErr error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_renewal:create")
	if err != nil {
		return RenewalDTO{}, err
	}
	lotNo = strings.TrimSpace(lotNo)
	if err := validateBusinessNo(lotNo, "lot_no"); err != nil {
		return RenewalDTO{}, err
	}
	req.QuoteToken = strings.TrimSpace(req.QuoteToken)
	if req.ExpectedLotVersion == 0 || len(req.QuoteToken) < 32 || len(req.QuoteToken) > 2048 {
		return RenewalDTO{}, problem.InvalidArgument(
			"VALIDATION_FAILED",
			"expected_lot_version and a valid quote_token are required",
		)
	}
	if err := s.validateQuoteConfiguration(); err != nil {
		return RenewalDTO{}, err
	}
	requestHash := idempotency.ResourceRequestHash(
		"wine_ticket.renewal.create",
		lotNo,
		req,
	)

	var (
		renewalID     uint64
		openID        string
		replayed      bool
		completedInTx bool
		quote         renewalQuoteClaims
	)
	claimID := s.ids.Next()
	resultErr = s.repo.dbConn().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.core.claimIdempotencyWithID(
			ctx,
			tx,
			claimID,
			claims.AccountType,
			customerID,
			method,
			path,
			key,
			requestHash,
		)
		if err != nil {
			return err
		}
		if !started {
			replayed = true
			return s.core.cachedResponse(
				ctx,
				tx,
				claims.AccountType,
				customerID,
				path,
				key,
				&response,
			)
		}

		// 付费续期会在调用支付机构前提交业务和支付草稿。
		// 租约过期后应重新认领同一草稿。
		existing, existingErr := s.repo.renewalByPaymentKey(ctx, tx, customerID, key)
		if existingErr == nil {
			if existing.PaymentID == nil || *existing.PaymentID == 0 {
				return problem.Internal("renewal payment draft is incomplete")
			}
			renewalID = existing.ID
			payment, paymentErr := s.repo.paymentByID(
				ctx,
				tx,
				*existing.PaymentID,
				false,
			)
			if paymentErr != nil {
				return paymentErr
			}
			if payment.Status == "creating" {
				identity, identityErr := s.repo.customerPurchaseEligibility(
					ctx,
					tx,
					customerID,
					s.wechatAppID,
					s.nowShanghai(),
				)
				if identityErr != nil {
					return identityErr
				}
				openID, identityErr = renewalPaymentOpenID(identity)
				return identityErr
			}
			return nil
		}
		if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
			return existingErr
		}

		now := s.nowShanghai()
		quote, err = s.verifyRenewalQuote(
			req.QuoteToken,
			customerID,
			lotNo,
			now,
		)
		if err != nil {
			return err
		}
		if quote.ExpectedLotVersion != req.ExpectedLotVersion {
			return renewalQuoteExpired()
		}
		lot, err := s.repo.customerLotByNo(ctx, tx, customerID, lotNo, true)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("WT_LOT_NOT_FOUND", "wine ticket lot not found")
		}
		if err != nil {
			return err
		}
		policy, err := s.loadRenewalPolicy(ctx, tx, lot)
		if err != nil {
			return err
		}
		blocked, err := s.repo.hasActiveHold(ctx, tx, lot.ID)
		if err != nil {
			return err
		}
		active, err := s.repo.activeRenewal(ctx, tx, lot.ID, true)
		hasActiveRenewal := err == nil && active.ID != 0
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := validateRenewalEligibility(
			lot,
			policy.Policy,
			blocked,
			hasActiveRenewal,
			now,
		); err != nil {
			return err
		}
		if lot.Version != req.ExpectedLotVersion ||
			lot.Version != quote.ExpectedLotVersion {
			return problem.Conflict(
				"WT_CONCURRENT_MODIFICATION",
				"wine ticket lot changed after the renewal quote",
			)
		}
		if err := validateRenewalQuoteFacts(
			quote,
			customerID,
			lot,
			policy,
			now,
		); err != nil {
			return err
		}

		renewalID = s.ids.Next()
		renewalNo := "WTRN" + idString(renewalID)
		newExpiry := renewalNewExpiry(lot.ExpiresAt, policy.Policy.ExtensionDays)
		renewal := Renewal{
			ID: renewalID, RenewalNo: renewalNo,
			LotID: lot.ID, CustomerID: customerID,
			OldExpiresAt:  lot.ExpiresAt.In(shanghaiLocation).Truncate(time.Millisecond),
			NewExpiresAt:  newExpiry,
			ExtensionDays: uint(policy.Policy.ExtensionDays),
			FeeAmount:     policy.Policy.FeeAmount, Currency: "CNY",
			PolicySnapshot:     cloneJSON(policy.PolicySnapshot),
			ExpectedLotVersion: lot.Version,
			Version:            1, CreatedAt: now, UpdatedAt: now,
		}

		if policy.Policy.FeeAmount == 0 {
			renewal.Status = RenewalStatusCompleted
			renewal.CompletedAt = timePtr(now)
			if err := s.repo.createRenewal(ctx, tx, &renewal); err != nil {
				return err
			}
			if err := s.repo.updateLotVersioned(
				ctx,
				tx,
				lot.ID,
				lot.Version,
				map[string]any{
					"expires_at":        newExpiry,
					"expiry_changed_at": now,
					"renewal_count":     lot.RenewalCount + 1,
					"ever_used":         true,
					"version":           gorm.Expr("version + 1"),
					"updated_at":        now,
				},
			); err != nil {
				return err
			}
			if err := s.core.createCustomerAudit(
				ctx,
				tx,
				customerID,
				"wine_ticket.renewal.complete",
				"wine_ticket_renewal",
				renewal.ID,
				nil,
				map[string]any{
					"renewal_no":     renewal.RenewalNo,
					"lot_no":         lot.LotNo,
					"old_expires_at": formatShanghai(renewal.OldExpiresAt),
					"new_expires_at": formatShanghai(renewal.NewExpiresAt),
					"status":         renewal.Status,
				},
			); err != nil {
				return err
			}
			if err := s.createRenewedOutbox(ctx, tx, renewal, lot.LotNo); err != nil {
				return err
			}
			response = renewalRecordDTO(renewalRecord{
				Renewal: renewal,
				LotNo:   lot.LotNo,
			})
			completedInTx = true
			return s.core.idStore.SucceedOwned(
				ctx,
				tx,
				claimID,
				claims.AccountType,
				customerID,
				path,
				key,
				response,
			)
		}

		if s.paymentService == nil {
			return problem.New(
				http.StatusServiceUnavailable,
				"PAYMENT_PROVIDER_UNAVAILABLE",
				"Service Unavailable",
				"payment service is unavailable",
			)
		}
		identity, err := s.repo.customerPurchaseEligibility(
			ctx,
			tx,
			customerID,
			s.wechatAppID,
			now,
		)
		if err != nil {
			return err
		}
		openID, err = renewalPaymentOpenID(identity)
		if err != nil {
			return err
		}

		paymentID := s.ids.Next()
		renewal.PaymentID = &paymentID
		renewal.Status = RenewalStatusPendingPayment
		if err := s.repo.createRenewal(ctx, tx, &renewal); err != nil {
			return renewalCreateConflict(err)
		}
		if err := s.repo.updateLotVersioned(
			ctx,
			tx,
			lot.ID,
			lot.Version,
			map[string]any{
				"ever_used":  true,
				"version":    gorm.Expr("version + 1"),
				"updated_at": now,
			},
		); err != nil {
			return err
		}
		paymentExpiry := now.Add(30 * time.Minute)
		if paymentExpiry.After(renewal.OldExpiresAt) {
			paymentExpiry = renewal.OldExpiresAt
		}
		if !paymentExpiry.After(now) {
			return renewalQuoteExpired()
		}
		if _, err := s.paymentService.CreateBusinessPaymentTx(
			ctx,
			tx,
			order.BusinessPaymentCreateInput{
				PaymentID:      paymentID,
				PaymentNo:      "WTRPAY" + idString(paymentID),
				BizType:        RenewalPaymentBusiness,
				BizID:          renewal.ID,
				CustomerID:     customerID,
				Channel:        "wechat_miniapp",
				Provider:       "wechat",
				Amount:         renewal.FeeAmount,
				Currency:       renewal.Currency,
				ExpiresAt:      paymentExpiry,
				IdempotencyKey: key,
			},
		); err != nil {
			return err
		}
		return s.core.createCustomerAudit(
			ctx,
			tx,
			customerID,
			"wine_ticket.renewal.create",
			"wine_ticket_renewal",
			renewal.ID,
			nil,
			map[string]any{
				"renewal_no":     renewal.RenewalNo,
				"lot_no":         lot.LotNo,
				"fee_amount":     renewal.FeeAmount,
				"old_expires_at": formatShanghai(renewal.OldExpiresAt),
				"new_expires_at": formatShanghai(renewal.NewExpiresAt),
				"status":         renewal.Status,
			},
		)
	})
	if resultErr != nil || replayed || completedInTx {
		return response, resultErr
	}

	payment, err := s.paymentService.BusinessPayment(
		ctx,
		customerID,
		RenewalPaymentBusiness,
		renewalID,
	)
	if err != nil {
		s.core.releaseCustomerIdempotencyOwned(ctx, claimID, claims.AccountType, customerID, path, key)
		return RenewalDTO{}, err
	}
	if payment.Status == "creating" {
		_, _ = s.paymentService.SubmitBusinessPayment(
			ctx,
			payment.ID,
			openID,
			"酒票续期",
		)
		payment, err = s.paymentService.BusinessPayment(
			ctx,
			customerID,
			RenewalPaymentBusiness,
			renewalID,
		)
		if err != nil {
			s.core.releaseCustomerIdempotencyOwned(ctx, claimID, claims.AccountType, customerID, path, key)
			return RenewalDTO{}, err
		}
	}
	// 业务草稿提交后，支付机构仍可能永久拒绝创建支付。
	// 若进程在首次请求把支付终态映射回续期单前停止，
	// 过期幂等租约仍必须关闭恢复出的保护记录，不能永久缓存。
	if err := s.reflectRenewalPaymentSubmission(
		ctx,
		renewalID,
		payment.ID,
		payment.Status,
	); err != nil {
		s.core.releaseCustomerIdempotencyOwned(ctx, claimID, claims.AccountType, customerID, path, key)
		return RenewalDTO{}, err
	}

	response, err = s.customerRenewalDTOByID(ctx, customerID, renewalID)
	if err != nil {
		s.core.releaseCustomerIdempotencyOwned(ctx, claimID, claims.AccountType, customerID, path, key)
		return RenewalDTO{}, err
	}
	if err := s.repo.dbConn().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.core.idStore.SucceedOwned(
			ctx,
			tx,
			claimID,
			claims.AccountType,
			customerID,
			path,
			key,
			response,
		)
	}); err != nil {
		s.core.releaseCustomerIdempotencyOwned(ctx, claimID, claims.AccountType, customerID, path, key)
		return RenewalDTO{}, err
	}
	return response, nil
}

func (s *RenewalService) ListRenewals(
	ctx context.Context,
	claims *auth.Claims,
	query pagination.Query,
	status string,
) ([]RenewalDTO, string, error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_renewal:view")
	if err != nil {
		return nil, "", err
	}
	status = strings.TrimSpace(status)
	if status != "" && !validRenewalStatus(status) {
		return nil, "", problem.InvalidArgument(
			"VALIDATION_FAILED",
			"invalid renewal status",
		)
	}
	rows, err := s.repo.listCustomerRenewals(ctx, customerID, query, status)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		rows = rows[:query.PageSize]
		next = pagination.NextPageTokenWithCursor(
			query,
			idString(rows[len(rows)-1].ID),
		)
	}
	items := make([]RenewalDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, renewalRecordDTO(row))
	}
	return items, next, nil
}

func (s *RenewalService) Renewal(
	ctx context.Context,
	claims *auth.Claims,
	renewalNo string,
) (RenewalDTO, error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_renewal:view")
	if err != nil {
		return RenewalDTO{}, err
	}
	renewalNo = strings.TrimSpace(renewalNo)
	if err := validateBusinessNo(renewalNo, "renewal_no"); err != nil {
		return RenewalDTO{}, err
	}
	row, err := s.repo.customerRenewalByNo(ctx, customerID, renewalNo)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return RenewalDTO{}, problem.NotFound(
			"WT_RENEWAL_NOT_FOUND",
			"wine ticket renewal not found",
		)
	}
	if err != nil {
		return RenewalDTO{}, err
	}
	return renewalRecordDTO(row), nil
}

func (s *RenewalService) ConfirmRenewalPayment(
	ctx context.Context,
	claims *auth.Claims,
	method, path, key, renewalNo string,
) (response RenewalDTO, resultErr error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_payment:confirm")
	if err != nil {
		return RenewalDTO{}, err
	}
	renewalNo = strings.TrimSpace(renewalNo)
	if err := validateBusinessNo(renewalNo, "renewal_no"); err != nil {
		return RenewalDTO{}, err
	}
	row, err := s.repo.customerRenewalByNo(ctx, customerID, renewalNo)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return RenewalDTO{}, problem.NotFound(
			"WT_RENEWAL_NOT_FOUND",
			"wine ticket renewal not found",
		)
	}
	if err != nil {
		return RenewalDTO{}, err
	}
	requestHash := idempotency.ResourceRequestHash(
		"wine_ticket.renewal.payment.confirm",
		renewalNo,
		map[string]any{},
	)
	started := false
	claimID := s.ids.Next()
	resultErr = s.repo.dbConn().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		claimed, err := s.core.claimIdempotencyWithID(
			ctx,
			tx,
			claimID,
			claims.AccountType,
			customerID,
			method,
			path,
			key,
			requestHash,
		)
		if err != nil {
			return err
		}
		started = claimed
		if !claimed {
			return s.core.cachedResponse(
				ctx,
				tx,
				claims.AccountType,
				customerID,
				path,
				key,
				&response,
			)
		}
		return nil
	})
	if resultErr != nil || !started {
		return response, resultErr
	}

	// 免费续期和已进入终态的续期没有需要查询的支付记录。
	if row.PaymentID != nil &&
		row.Status != RenewalStatusCompleted &&
		row.Status != RenewalStatusClosed &&
		row.Status != RenewalStatusRefunded &&
		row.Status != RenewalStatusCompensatingRefund &&
		row.Status != RenewalStatusRefundException {
		if s.paymentService == nil {
			s.core.releaseCustomerIdempotencyOwned(ctx, claimID, claims.AccountType, customerID, path, key)
			return RenewalDTO{}, problem.New(
				http.StatusServiceUnavailable,
				"PAYMENT_PROVIDER_UNAVAILABLE",
				"Service Unavailable",
				"payment service is unavailable",
			)
		}
		if _, err := s.paymentService.ConfirmBusinessPayment(
			ctx,
			customerID,
			RenewalPaymentBusiness,
			row.ID,
		); err != nil {
			_ = s.markRenewalPaymentUnknown(ctx, row.ID)
			s.core.releaseCustomerIdempotencyOwned(ctx, claimID, claims.AccountType, customerID, path, key)
			return RenewalDTO{}, err
		}
	}
	response, err = s.customerRenewalDTOByID(ctx, customerID, row.ID)
	if err != nil {
		s.core.releaseCustomerIdempotencyOwned(ctx, claimID, claims.AccountType, customerID, path, key)
		return RenewalDTO{}, err
	}
	if err := s.repo.dbConn().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.core.idStore.SucceedOwned(
			ctx,
			tx,
			claimID,
			claims.AccountType,
			customerID,
			path,
			key,
			response,
		)
	}); err != nil {
		s.core.releaseCustomerIdempotencyOwned(ctx, claimID, claims.AccountType, customerID, path, key)
		return RenewalDTO{}, err
	}
	return response, nil
}

func (s *RenewalService) customerRenewalDTOByID(
	ctx context.Context,
	customerID, renewalID uint64,
) (RenewalDTO, error) {
	row, err := s.repo.customerRenewalByID(ctx, customerID, renewalID)
	if err != nil {
		return RenewalDTO{}, err
	}
	return renewalRecordDTO(row), nil
}

func (s *RenewalService) reflectRenewalPaymentSubmission(
	ctx context.Context,
	renewalID, paymentID uint64,
	paymentStatus string,
) error {
	return s.repo.dbConn().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		lookup, err := s.repo.renewalByID(ctx, tx, renewalID, false)
		if err != nil {
			return err
		}
		lot, err := s.repo.lockLotByID(ctx, tx, lookup.LotID)
		if err != nil {
			return err
		}
		renewal, err := s.repo.lockRenewalAfterLot(ctx, tx, renewalID, lot.ID)
		if err != nil {
			return err
		}
		if renewal.PaymentID == nil || *renewal.PaymentID != paymentID {
			return problem.Internal("renewal payment link is inconsistent")
		}
		payment, err := s.repo.paymentByID(ctx, tx, paymentID, true)
		if err != nil {
			return err
		}
		if payment.Status != paymentStatus {
			paymentStatus = payment.Status
		}
		switch paymentStatus {
		case "failed", "closed":
			return s.closeUnpaidRenewalTx(ctx, tx, &lot, renewal, paymentStatus)
		case "creating", "exception":
			if renewal.Status != RenewalStatusPendingPayment {
				return nil
			}
			now := s.nowShanghai()
			return s.repo.updateRenewalVersioned(
				ctx,
				tx,
				renewal,
				map[string]any{
					"status":     RenewalStatusPaymentUnknown,
					"version":    gorm.Expr("version + 1"),
					"updated_at": now,
				},
			)
		default:
			return nil
		}
	})
}

func (s *RenewalService) markRenewalPaymentUnknown(
	ctx context.Context,
	renewalID uint64,
) error {
	return s.repo.dbConn().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		lookup, err := s.repo.renewalByID(ctx, tx, renewalID, false)
		if err != nil {
			return err
		}
		if _, err := s.repo.lockLotByID(ctx, tx, lookup.LotID); err != nil {
			return err
		}
		renewal, err := s.repo.lockRenewalAfterLot(ctx, tx, renewalID, lookup.LotID)
		if err != nil {
			return err
		}
		if renewal.Status != RenewalStatusPendingPayment {
			return nil
		}
		now := s.nowShanghai()
		return s.repo.updateRenewalVersioned(
			ctx,
			tx,
			renewal,
			map[string]any{
				"status":     RenewalStatusPaymentUnknown,
				"version":    gorm.Expr("version + 1"),
				"updated_at": now,
			},
		)
	})
}

func (s *RenewalService) closeUnpaidRenewalTx(
	ctx context.Context,
	tx *gorm.DB,
	lot *core.Lot,
	renewal Renewal,
	providerStatus string,
) error {
	if renewal.Status == RenewalStatusClosed {
		return s.expireLotAfterRenewalGuardRelease(
			ctx,
			tx,
			lot,
			renewal,
			s.nowShanghai(),
			"unpaid_closed",
		)
	}
	switch renewal.Status {
	case RenewalStatusPendingPayment, RenewalStatusPaymentUnknown, RenewalStatusApplying:
	default:
		return nil
	}
	now := s.nowShanghai()
	if err := s.repo.updateRenewalVersioned(
		ctx,
		tx,
		renewal,
		map[string]any{
			"status":     RenewalStatusClosed,
			"closed_at":  now,
			"version":    gorm.Expr("version + 1"),
			"updated_at": now,
		},
	); err != nil {
		return err
	}
	if err := s.createRenewalSettlementAudit(
		ctx,
		tx,
		"wine_ticket.renewal.close",
		renewal,
		RenewalStatusClosed,
		map[string]any{
			"renewal_no":      renewal.RenewalNo,
			"provider_status": providerStatus,
		},
	); err != nil {
		return err
	}
	return s.expireLotAfterRenewalGuardRelease(
		ctx,
		tx,
		lot,
		renewal,
		now,
		"unpaid_closed",
	)
}

func (s *RenewalService) expireLotAfterRenewalGuardRelease(
	ctx context.Context,
	tx *gorm.DB,
	lot *core.Lot,
	renewal Renewal,
	now time.Time,
	reason string,
) error {
	if lot.ExpiresAt.After(now) {
		return nil
	}
	_, err := s.assets.Expire(
		ctx,
		core.NewTransactionAssetRepository(tx),
		core.ExpireCommand{
			LotID:           lot.ID,
			OwnerCustomerID: lot.OwnerCustomerID,
			ActionKey: "renewal_guard_release_expiry:" +
				idString(renewal.ID) + ":" +
				strconv.FormatInt(lot.ExpiresAt.UnixMilli(), 10),
			TransactionType: transactionTypeLotExpiry,
			BizType:         "wine_ticket_renewal",
			BizID:           renewal.ID,
			Metadata: map[string]any{
				"reason":     reason,
				"renewal_no": renewal.RenewalNo,
			},
			OccurredAt: now,
		},
	)
	return err
}

func (s *RenewalService) loadRenewalPolicy(
	ctx context.Context,
	db *gorm.DB,
	lot core.Lot,
) (renewalLotPolicy, error) {
	raw, err := s.repo.policySnapshot(ctx, db, lot.PurchaseID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return renewalLotPolicy{}, problem.Internal(
			"wine ticket lot purchase lineage is missing",
		)
	}
	if err != nil {
		return renewalLotPolicy{}, err
	}
	var policy core.RenewalPolicy
	if err := decodePolicyJSON(
		raw,
		&policy,
		"schema_version",
		"enabled",
		"extension_days",
		"max_count",
		"grace_days",
		"fee_amount",
	); err != nil {
		return renewalLotPolicy{}, problem.Internal(
			"wine ticket renewal policy snapshot is invalid",
		)
	}
	if err := validateRenewalPolicyContract(policy); err != nil {
		return renewalLotPolicy{}, problem.Internal(
			"wine ticket renewal policy snapshot violates the V1 contract",
		)
	}
	return renewalLotPolicy{
		Lot:            lot,
		PolicySnapshot: cloneJSON(raw),
		Policy:         policy,
	}, nil
}

func validateRenewalEligibility(
	lot core.Lot,
	policy core.RenewalPolicy,
	blocked, hasActiveRenewal bool,
	now time.Time,
) error {
	if hasActiveRenewal {
		return problem.Conflict(
			"WT_RENEWAL_IN_PROGRESS",
			"a renewal for this wine ticket lot is already in progress",
		)
	}
	switch {
	case !policy.Enabled:
		return renewalUnprocessable(
			"WT_RENEWAL_NOT_ELIGIBLE",
			"renewal_disabled",
			"wine ticket renewal is disabled by its purchase policy",
		)
	case policy.SchemaVersion != 1 || policy.GraceDays != 0:
		return problem.Internal("unsupported wine ticket renewal policy")
	case lot.Status != LotStatusActive || lot.AvailableQuantity == 0:
		return renewalUnprocessable(
			"WT_RENEWAL_NOT_ELIGIBLE",
			"lot_not_active",
			"wine ticket lot is not active",
		)
	case !lot.ExpiresAt.After(now):
		return renewalUnprocessable(
			"WT_RENEWAL_NOT_ELIGIBLE",
			"lot_expired",
			"expired wine tickets cannot be renewed",
		)
	case lot.RenewalCount >= uint(policy.MaxCount):
		return renewalUnprocessable(
			"WT_RENEWAL_LIMIT_REACHED",
			"renewal_limit_reached",
			"wine ticket renewal limit has been reached",
		)
	case blocked:
		return renewalUnprocessable(
			"WT_RENEWAL_NOT_ELIGIBLE",
			"active_hold_exists",
			"wine ticket lot has an active hold",
		)
	case policy.FeeAmount < 0 || policy.FeeAmount > maxWineTicketAmount:
		return renewalUnprocessable(
			"WT_AMOUNT_LIMIT_EXCEEDED",
			"amount_limit_exceeded",
			"renewal fee exceeds the supported amount",
		)
	default:
		return nil
	}
}

func validateRenewalQuoteFacts(
	quote renewalQuoteClaims,
	customerID uint64,
	lot core.Lot,
	policy renewalLotPolicy,
	now time.Time,
) error {
	if quote.SchemaVersion != 1 ||
		quote.CustomerID != idString(customerID) ||
		quote.LotID != idString(lot.ID) ||
		quote.LotNo != lot.LotNo ||
		quote.ExpectedLotVersion != lot.Version ||
		quote.ExtensionDays != uint(policy.Policy.ExtensionDays) ||
		quote.FeeAmount != policy.Policy.FeeAmount ||
		quote.PolicyDigest != renewalPolicyDigest(policy.PolicySnapshot) ||
		quote.OldExpiresAtMS != lot.ExpiresAt.UnixMilli() ||
		quote.NewExpiresAtMS != renewalNewExpiry(
			lot.ExpiresAt,
			policy.Policy.ExtensionDays,
		).UnixMilli() ||
		quote.QuoteExpiresAtMS > quote.OldExpiresAtMS ||
		quote.QuoteExpiresAtMS <= now.UnixMilli() {
		return renewalQuoteExpired()
	}
	return nil
}

func renewalNewExpiry(old time.Time, extensionDays int) time.Time {
	return old.In(shanghaiLocation).
		AddDate(0, 0, extensionDays).
		Truncate(time.Millisecond)
}

func renewalPolicyDigest(raw datatypes.JSON) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *RenewalService) validateQuoteConfiguration() error {
	if len(s.quoteSecret) < 32 || s.ids == nil || s.repo == nil || s.repo.dbConn() == nil {
		return problem.Internal("wine ticket renewal service is not configured")
	}
	return nil
}

func (s *RenewalService) signRenewalQuote(
	claims renewalQuoteClaims,
) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.quoteSecret)
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *RenewalService) verifyRenewalQuote(
	token string,
	customerID uint64,
	lotNo string,
	now time.Time,
) (renewalQuoteClaims, error) {
	if len(token) < 32 || len(token) > 2048 {
		return renewalQuoteClaims{}, renewalQuoteExpired()
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return renewalQuoteClaims{}, renewalQuoteExpired()
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(payload) == 0 || len(payload) > 1536 {
		return renewalQuoteClaims{}, renewalQuoteExpired()
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return renewalQuoteClaims{}, renewalQuoteExpired()
	}
	mac := hmac.New(sha256.New, s.quoteSecret)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return renewalQuoteClaims{}, renewalQuoteExpired()
	}
	var quote renewalQuoteClaims
	if err := json.Unmarshal(payload, &quote); err != nil ||
		quote.SchemaVersion != 1 ||
		quote.CustomerID != idString(customerID) ||
		quote.LotNo != lotNo ||
		quote.ExpectedLotVersion == 0 ||
		quote.ExtensionDays == 0 ||
		quote.FeeAmount < 0 ||
		quote.FeeAmount > maxWineTicketAmount ||
		quote.QuoteExpiresAtMS <= now.UnixMilli() ||
		quote.QuoteExpiresAtMS > quote.OldExpiresAtMS ||
		quote.OldExpiresAtMS <= now.UnixMilli() ||
		quote.NewExpiresAtMS <= quote.OldExpiresAtMS {
		return renewalQuoteClaims{}, renewalQuoteExpired()
	}
	if _, err := strconv.ParseUint(quote.CustomerID, 10, 64); err != nil {
		return renewalQuoteClaims{}, renewalQuoteExpired()
	}
	if _, err := strconv.ParseUint(quote.LotID, 10, 64); err != nil {
		return renewalQuoteClaims{}, renewalQuoteExpired()
	}
	return quote, nil
}

func renewalPaymentOpenID(identity CustomerEligibility) (string, error) {
	if identity.CustomerID == 0 || identity.CustomerStatus != "active" {
		return "", problem.Unauthorized(
			"AUTH_UNAUTHORIZED",
			"customer account is unavailable",
		)
	}
	if identity.IdentityCount != 1 || strings.TrimSpace(identity.OpenID) == "" {
		return "", problem.Conflict(
			"WECHAT_IDENTITY_REQUIRED",
			"one active WeChat mini-program identity is required for payment",
		)
	}
	return strings.TrimSpace(identity.OpenID), nil
}

func validateRenewalPolicyContract(policy core.RenewalPolicy) error {
	if policy.SchemaVersion != 1 ||
		policy.ExtensionDays < 1 ||
		policy.ExtensionDays > 3650 ||
		policy.MaxCount < 0 ||
		policy.MaxCount > 100 ||
		policy.GraceDays != 0 ||
		policy.FeeAmount < 0 ||
		policy.FeeAmount > maxWineTicketAmount {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"renewal_policy violates the V1 policy contract",
		)
	}
	return nil
}

func renewalUnprocessable(code, reason, detail string) error {
	result := problem.New(
		http.StatusUnprocessableEntity,
		code,
		"Unprocessable Entity",
		detail,
	)
	result.Data = map[string]any{"reason_code": reason}
	return result
}

func renewalQuoteExpired() error {
	return problem.Conflict(
		"WT_RENEWAL_QUOTE_EXPIRED",
		"renewal quote is invalid, stale, or expired",
	)
}

func renewalCreateConflict(err error) error {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "unique") ||
		strings.Contains(message, "duplicate") {
		return problem.Conflict(
			"WT_RENEWAL_IN_PROGRESS",
			"a renewal for this wine ticket lot is already in progress",
		)
	}
	return err
}

func renewalRecordDTO(row renewalRecord) RenewalDTO {
	var paymentParameters map[string]any
	if row.Status == RenewalStatusPendingPayment ||
		row.Status == RenewalStatusPaymentUnknown {
		paymentParameters = decodePaymentParameters(row.PaymentClientPayload)
	}
	displayStatus, safeMessage := renewalCompensationDisplay(row)
	return RenewalDTO{
		RenewalNo:                       row.RenewalNo,
		LotNo:                           row.LotNo,
		ExtensionDays:                   row.ExtensionDays,
		FeeAmount:                       row.FeeAmount,
		OldExpiresAt:                    formatShanghai(row.OldExpiresAt),
		NewExpiresAt:                    formatShanghai(row.NewExpiresAt),
		Status:                          row.Status,
		PaymentParameters:               paymentParameters,
		CompensatingRefundDisplayStatus: displayStatus,
		CompensatingRefundSafeMessage:   safeMessage,
		Version:                         row.Version,
		UpdatedAt:                       formatShanghai(row.UpdatedAt),
	}
}

func renewalCompensationDisplay(
	row renewalRecord,
) (*string, *string) {
	if row.CompensatingRefundID == nil || row.RefundStatus == nil {
		return nil, nil
	}
	status := strings.ToLower(strings.TrimSpace(*row.RefundStatus))
	providerStatus := ""
	if row.RefundProviderStatus != nil {
		providerStatus = strings.ToUpper(strings.TrimSpace(*row.RefundProviderStatus))
	}
	display := status
	switch {
	case providerStatus == "PROCESSING":
		display = "processing"
	case status == "failed" && providerStatus == "CLOSED":
		display = "closed"
	case status == "exception":
		display = "exception"
	case status == "creating", status == "pending",
		status == "submission_unknown", status == "succeeded":
	default:
		display = "exception"
	}
	message := "续期未生效，退款处理中"
	switch display {
	case "succeeded":
		message = "续期未生效，款项已原路退回"
	case "closed":
		message = "补偿退款未完成，系统将继续处理"
	case "exception":
		message = "续期未生效，退款状态待确认，请稍后查看"
	}
	return stringPointer(display), stringPointer(message)
}

func (s *RenewalService) createRenewedOutbox(
	ctx context.Context,
	tx *gorm.DB,
	renewal Renewal,
	lotNo string,
) error {
	return s.core.createWineTicketOutbox(
		ctx,
		tx,
		"wine_ticket.renewed",
		"wine_ticket_renewal",
		renewal.ID,
		map[string]any{
			"renewal_no":     renewal.RenewalNo,
			"lot_no":         lotNo,
			"customer_id":    idString(renewal.CustomerID),
			"old_expires_at": formatShanghai(renewal.OldExpiresAt),
			"new_expires_at": formatShanghai(renewal.NewExpiresAt),
		},
	)
}
