package refund

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	wineticketcore "jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type RefundService struct {
	repo        *refundRepository
	core        *serviceCore
	ids         *snowflake.Generator
	quoteSecret []byte
	now         func() time.Time
}

var errRefundCanonicalReentry = errors.New("wine ticket refund requires canonical lock re-entry")

func NewRefundService(
	db *gorm.DB,
	ids *snowflake.Generator,
	quoteTokenSecret string,
) *RefundService {
	repo := newRefundRepository(db)
	return &RefundService{
		repo: repo, core: newServiceCore(repo, ids), ids: ids,
		quoteSecret: []byte(quoteTokenSecret), now: time.Now,
	}
}

func (s *RefundService) WithNow(now func() time.Time) *RefundService {
	if now != nil {
		s.now = now
		s.core.setClock(now)
	}
	return s
}

func (s *RefundService) nowShanghai() time.Time {
	return s.now().In(shanghaiLocation).Truncate(time.Millisecond)
}

func (s *RefundService) Quote(
	ctx context.Context,
	claims *auth.Claims,
	purchaseNo string,
) (RefundQuoteDTO, error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_refund:quote")
	if err != nil {
		return RefundQuoteDTO{}, err
	}
	if err := validateBusinessNo(purchaseNo, "purchase_no"); err != nil {
		return RefundQuoteDTO{}, err
	}
	now := s.nowShanghai()
	if err := s.requireActiveCustomer(ctx, s.repo.dbConn(), customerID); err != nil {
		return RefundQuoteDTO{}, err
	}
	purchase, err := s.repo.purchaseByNo(ctx, nil, customerID, purchaseNo, false)
	if refundNotFound(err) {
		return RefundQuoteDTO{}, problem.NotFound("WT_PURCHASE_NOT_FOUND", "wine ticket purchase not found")
	}
	if err != nil {
		return RefundQuoteDTO{}, err
	}
	lots, err := s.repo.originalLots(ctx, nil, purchase.ID, false)
	if err != nil {
		return RefundQuoteDTO{}, err
	}
	facts, err := s.repo.eligibilityFacts(ctx, nil, purchase, lots)
	if err != nil {
		return RefundQuoteDTO{}, err
	}
	evaluation := evaluateRefundEligibility(facts, now)
	claimsPayload := refundQuoteClaims{
		SchemaVersion: 1, CustomerID: idString(customerID),
		PurchaseID: idString(purchase.ID), PurchaseNo: purchase.PurchaseNo,
		ExpectedPurchaseVersion: purchase.Version,
		Amount:                  evaluation.dto.RefundableAmount, Currency: evaluation.dto.Currency,
		Eligible:             evaluation.dto.Eligible,
		RefundWindowEndsAtMS: facts.WindowEndsAt.UnixMilli(),
		QuoteExpiresAtMS:     evaluation.quoteExpiresAt.UnixMilli(),
		LotDigest:            evaluation.lotDigest,
		PolicyDigest:         evaluation.policyDigest,
	}
	token, err := s.signQuote(claimsPayload)
	if err != nil {
		return RefundQuoteDTO{}, err
	}
	evaluation.dto.QuoteToken = token
	return evaluation.dto, nil
}

func (s *RefundService) Create(
	ctx context.Context,
	claims *auth.Claims,
	method, path, key, purchaseNo string,
	request RefundCreateRequest,
) (response RefundDTO, resultErr error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_refund:create")
	if err != nil {
		return RefundDTO{}, err
	}
	if err := validateBusinessNo(purchaseNo, "purchase_no"); err != nil {
		return RefundDTO{}, err
	}
	request, err = normalizeRefundCreate(request)
	if err != nil {
		return RefundDTO{}, err
	}
	token, err := s.verifyQuote(request.QuoteToken)
	if err != nil {
		return RefundDTO{}, err
	}
	if token.CustomerID != idString(customerID) || token.PurchaseNo != purchaseNo ||
		token.ExpectedPurchaseVersion != request.ExpectedPurchaseVersion {
		return RefundDTO{}, problem.Conflict("WT_CONCURRENT_MODIFICATION", "refund quote no longer matches this purchase")
	}
	requestHash := idempotency.ResourceRequestHash("create_refund", purchaseNo, request)

	for attempt := 0; attempt < 3; attempt++ {
		resultErr = s.repo.dbConn().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			started, err := s.core.claimIdempotency(
				ctx, tx, claims.AccountType, customerID, method, path, key, requestHash,
			)
			if err != nil {
				return err
			}
			if !started {
				return s.core.cachedResponse(ctx, tx, claims.AccountType, customerID, path, key, &response)
			}

			requestedAt, err := s.repo.transactionNow(ctx, tx)
			if err != nil {
				return err
			}
			if requestedAt.UnixMilli() > token.QuoteExpiresAtMS {
				return problem.Conflict("WT_REFUND_QUOTE_EXPIRED", "refund quote has expired")
			}
			if err := s.requireActiveCustomer(ctx, tx, customerID); err != nil {
				return err
			}
			observedPurchase, err := s.repo.purchaseByNo(ctx, tx, customerID, purchaseNo, false)
			if refundNotFound(err) {
				return problem.NotFound("WT_PURCHASE_NOT_FOUND", "wine ticket purchase not found")
			}
			if err != nil {
				return err
			}
			if existing, activeErr := s.repo.activeRefund(ctx, tx, observedPurchase.ID, false); activeErr == nil {
				return s.lockExistingActiveRefund(
					ctx, tx, customerID, observedPurchase.ID, existing.ID,
				)
			} else if !refundNotFound(activeErr) {
				return activeErr
			}

			purchase, err := s.repo.purchaseByNo(ctx, tx, customerID, purchaseNo, true)
			if refundNotFound(err) {
				return problem.NotFound("WT_PURCHASE_NOT_FOUND", "wine ticket purchase not found")
			}
			if err != nil {
				return err
			}
			// READ COMMITTED 可确保等待购买记录后重新读取最新事实。
			// 不得在购买锁之后再获取业务退款锁；应回滚并通过现有退款的
			// 标准锁计划重新进入。
			if _, activeErr := s.repo.activeRefund(ctx, tx, purchase.ID, false); activeErr == nil {
				return errRefundCanonicalReentry
			} else if !refundNotFound(activeErr) {
				return activeErr
			}
			if purchase.Version != request.ExpectedPurchaseVersion ||
				token.PurchaseID != idString(purchase.ID) ||
				token.ExpectedPurchaseVersion != purchase.Version {
				return problem.Conflict("WT_CONCURRENT_MODIFICATION", "wine ticket purchase changed after refund quote")
			}

			// 首次申请的锁顺序：购买记录 -> 原始批次（按 ID 升序）->
			// 支付事实 -> 业务退款和分配记录 -> 公共退款记录。
			lots, err := s.repo.originalLots(ctx, tx, purchase.ID, true)
			if err != nil {
				return err
			}
			payment, err := s.repo.paymentByID(ctx, tx, purchase.PaymentID, true)
			if err != nil {
				return err
			}
			facts, err := s.repo.eligibilityFacts(ctx, tx, purchase, lots)
			if err != nil {
				return err
			}
			facts.Payment = payment
			facts.RefundableAmount = payment.Amount - payment.RefundedAmount
			if payment.PaidAt != nil {
				facts.WindowEndsAt = payment.PaidAt.In(shanghaiLocation).
					Add(time.Duration(facts.Policy.WindowHours) * time.Hour).
					Truncate(time.Millisecond)
			}
			evaluation := evaluateRefundEligibility(facts, requestedAt)
			if !evaluation.dto.Eligible {
				return problem.New(
					http.StatusUnprocessableEntity,
					"WT_REFUND_NOT_ELIGIBLE",
					"Unprocessable Entity",
					"wine ticket purchase is no longer eligible for a refund",
				)
			}
			if token.Eligible != evaluation.dto.Eligible ||
				token.Amount != evaluation.dto.RefundableAmount ||
				token.Currency != evaluation.dto.Currency ||
				token.RefundWindowEndsAtMS != facts.WindowEndsAt.UnixMilli() ||
				token.LotDigest != evaluation.lotDigest ||
				token.PolicyDigest != evaluation.policyDigest {
				return problem.Conflict("WT_CONCURRENT_MODIFICATION", "refund eligibility changed after quote")
			}
			if requestedAt.After(facts.WindowEndsAt) {
				return problem.New(
					http.StatusUnprocessableEntity,
					"WT_REFUND_NOT_ELIGIBLE",
					"Unprocessable Entity",
					"refund window has ended",
				)
			}

			businessID := s.ids.Next()
			commonID := s.ids.Next()
			businessNo := "WTRF" + idString(businessID)
			commonNo := "WTRFC" + idString(commonID)
			snapshot := refundEligibilitySnapshot{
				SchemaVersion: 1, CustomerID: idString(customerID),
				PurchaseNo: purchase.PurchaseNo, PurchaseVersion: purchase.Version,
				PaymentID: idString(payment.ID), PaymentNo: payment.PaymentNo,
				Amount: facts.RefundableAmount, Currency: payment.Currency,
				RefundWindowEndsAt: formatShanghai(facts.WindowEndsAt),
				RequestedAt:        formatShanghai(requestedAt), Policy: facts.Policy,
				EligibilityChecks:      evaluation.dto.EligibilityChecks,
				OriginalLotCount:       len(lots),
				OriginalBottleQuantity: purchase.TotalBottleQuantity,
				LotDigest:              evaluation.lotDigest, PolicyDigest: evaluation.policyDigest,
			}
			var reasonText *string
			if request.ReasonText != "" {
				reasonText = stringPointer(request.ReasonText)
			}
			business := WineTicketRefund{
				ID: businessID, WineTicketRefundNo: businessNo,
				PurchaseID: purchase.ID, CustomerID: customerID,
				CurrentRefundID: commonID, RefundKind: RefundKindUserUnused,
				Amount: facts.RefundableAmount, Currency: payment.Currency,
				ReasonCode: request.ReasonCode, ReasonText: reasonText,
				EligibilitySnapshot: refundJSON(snapshot), Status: RefundStatusHolding,
				Version: 1, RequestedAt: requestedAt, CreatedAt: requestedAt, UpdatedAt: requestedAt,
			}
			if err := s.repo.createBusinessRefund(ctx, tx, &business); err != nil {
				if isRefundDuplicateKey(err) {
					return errRefundCanonicalReentry
				}
				return err
			}

			allocations := make([]RefundAllocation, 0, len(lots))
			for _, lot := range lots {
				allocations = append(allocations, RefundAllocation{
					ID: s.ids.Next(), WineTicketRefundID: businessID, LotID: lot.ID,
					Quantity: lot.TotalQuantity, SourceExpiresAt: lot.ExpiresAt,
					Status: RefundAllocationHeld, CreatedAt: requestedAt, UpdatedAt: requestedAt,
				})
			}
			if err := s.repo.createAllocations(ctx, tx, allocations); err != nil {
				return err
			}

			for _, lot := range lots {
				if _, err := s.core.assets.Freeze(
					ctx,
					wineticketcore.NewTransactionAssetRepository(tx),
					wineticketcore.AssetCommand{
						LotID:           lot.ID,
						OwnerCustomerID: customerID,
						Quantity:        lot.TotalQuantity,
						MarkUsed:        false,
						TransactionType: TransactionTypeRefundHold,
						BizType:         "refund",
						BizID:           businessID,
						ActionKey: "refund_hold:" +
							idString(businessID) + ":" + idString(lot.ID),
						Metadata: refundTransactionMetadata{
							RefundNo: businessNo, PurchaseNo: purchase.PurchaseNo,
							Source: "user_unused", RuleVersion: 1,
						},
						OccurredAt: requestedAt,
					},
				); err != nil {
					return err
				}
			}
			if err := s.repo.updatePurchaseVersioned(ctx, tx, purchase, map[string]any{
				"status": PurchaseStatusRefundHolding, "updated_at": requestedAt,
			}); err != nil {
				return err
			}
			bizType, bizID := WineTicketPurchaseRefundBusiness, businessID
			common := commonRefundRow{
				ID: commonID, PaymentID: payment.ID, RefundNo: commonNo,
				BizType: &bizType, BizID: &bizID, Provider: "wechat",
				Status: "creating", Currency: payment.Currency,
				Reason: safeRefundProviderReason(request.ReasonCode),
				Amount: facts.RefundableAmount, TotalAmount: payment.Amount,
				RequestedAt: requestedAt, NextRetryAt: timePtr(requestedAt), Version: 1,
				CreatedAt: requestedAt, UpdatedAt: requestedAt,
			}
			if err := s.repo.createCommonRefund(ctx, tx, &common); err != nil {
				return err
			}

			response = refundDTO(refundRecord{
				WineTicketRefund: business, PurchaseNo: purchase.PurchaseNo,
				CommonStatus: common.Status,
				HeldCount:    uint(len(allocations)),
			})
			if err := s.core.createCustomerAudit(
				ctx, tx, customerID, "wine_ticket.refund.create",
				"wine_ticket_refund", businessID, nil,
				map[string]any{
					"refund_no": businessNo, "purchase_no": purchase.PurchaseNo,
					"amount": facts.RefundableAmount, "status": RefundStatusHolding,
					"lot_count": len(lots),
				},
			); err != nil {
				return err
			}
			if err := s.core.createWineTicketOutbox(
				ctx, tx, "wine_ticket.refund_created", "wine_ticket_refund", businessID,
				map[string]any{
					"refund_no": businessNo, "purchase_no": purchase.PurchaseNo,
					"customer_id": idString(customerID), "status": RefundStatusHolding,
				},
			); err != nil {
				return err
			}
			return s.core.idStore.Succeed(ctx, tx, claims.AccountType, customerID, path, key, response)
		}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if !errors.Is(resultErr, errRefundCanonicalReentry) {
			return response, resultErr
		}
	}
	return RefundDTO{}, problem.Conflict(
		"WT_CONCURRENT_MODIFICATION",
		"wine ticket refund changed concurrently; request a new quote and retry",
	)
}

// lockExistingActiveRefund 遵循与支付机构结算相同的锁顺序：
// 公共退款记录（按 ID 升序）-> 酒票退款 -> 购买记录 ->
// 批次（按 ID 升序）-> 分配记录（按 ID 升序）-> 支付。
// 调用方已持有购买记录锁时，绝不能进入该路径。
func (s *RefundService) lockExistingActiveRefund(
	ctx context.Context,
	tx *gorm.DB,
	customerID, expectedPurchaseID, businessID uint64,
) error {
	if _, err := s.repo.lockCommonRefunds(ctx, tx, businessID); err != nil {
		return err
	}
	business, err := s.repo.lockBusinessRefund(ctx, tx, businessID)
	if refundNotFound(err) {
		return errRefundCanonicalReentry
	}
	if err != nil {
		return err
	}
	purchase, err := s.repo.lockPurchaseByID(ctx, tx, business.PurchaseID)
	if err != nil {
		return err
	}
	if _, err := s.repo.originalLots(ctx, tx, purchase.ID, true); err != nil {
		return err
	}
	if _, err := s.repo.lockAllocations(ctx, tx, business.ID); err != nil {
		return err
	}
	if _, err := s.repo.paymentByID(ctx, tx, purchase.PaymentID, true); err != nil {
		return err
	}
	if business.PurchaseID != expectedPurchaseID ||
		business.CustomerID != customerID ||
		purchase.ID != expectedPurchaseID ||
		purchase.CustomerID != customerID ||
		!isActiveWineTicketRefundStatus(business.Status) {
		return errRefundCanonicalReentry
	}
	return problem.Conflict(
		"WT_REFUND_IN_PROGRESS",
		"wine ticket refund is already in progress: "+business.WineTicketRefundNo,
	)
}

func (s *RefundService) Detail(
	ctx context.Context,
	claims *auth.Claims,
	refundNo string,
) (RefundDTO, error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_refund:view")
	if err != nil {
		return RefundDTO{}, err
	}
	if err := validateBusinessNo(refundNo, "refund_no"); err != nil {
		return RefundDTO{}, err
	}
	row, err := s.repo.customerRefundByNo(ctx, customerID, refundNo)
	if refundNotFound(err) {
		return RefundDTO{}, problem.NotFound("WT_REFUND_NOT_FOUND", "wine ticket refund not found")
	}
	if err != nil {
		return RefundDTO{}, err
	}
	return refundDTO(row), nil
}

func (s *RefundService) List(
	ctx context.Context,
	claims *auth.Claims,
	query pagination.Query,
	status string,
) ([]RefundDTO, string, error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_refund:view")
	if err != nil {
		return nil, "", err
	}
	status = strings.TrimSpace(status)
	if status != "" && !validWineTicketRefundStatus(status) {
		return nil, "", problem.InvalidArgument("VALIDATION_FAILED", "invalid refund status")
	}
	rows, err := s.repo.listCustomerRefunds(ctx, customerID, query, status)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		rows = rows[:query.PageSize]
		next = pagination.NextPageTokenWithCursor(query, idString(rows[len(rows)-1].ID))
	}
	items := make([]RefundDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, refundDTO(row))
	}
	return items, next, nil
}

type evaluatedRefundEligibility struct {
	dto            RefundQuoteDTO
	quoteExpiresAt time.Time
	lotDigest      string
	policyDigest   string
}

func evaluateRefundEligibility(
	facts refundEligibilityFacts,
	now time.Time,
) evaluatedRefundEligibility {
	purchase := facts.Purchase
	payment := facts.Payment
	paymentLinkOK := payment.ID == purchase.PaymentID &&
		payment.BizType != nil && *payment.BizType == PurchasePaymentBusiness &&
		payment.BizID != nil && *payment.BizID == purchase.ID &&
		payment.OrderID == nil && payment.CustomerID == purchase.CustomerID &&
		payment.Provider == "wechat" && payment.Status == "succeeded" &&
		payment.ProviderTradeNo != nil &&
		strings.TrimSpace(*payment.ProviderTradeNo) != "" &&
		payment.PaidAt != nil && payment.Currency == "CNY" &&
		payment.RefundedAmount == 0 &&
		payment.Amount == purchase.PaidAmount &&
		purchase.PaidAmount == purchase.PayableAmount &&
		purchase.Status == PurchaseStatusIssued
	withinWindow := payment.PaidAt != nil && !now.After(facts.WindowEndsAt)

	var totalQuantity uint
	intact := len(facts.OriginalLots) > 0
	neverUsed := len(facts.OriginalLots) > 0 && facts.HistoryCount == 0
	lotSummaries := make([]RefundLotSummaryDTO, 0, len(facts.OriginalLots))
	for _, lot := range facts.OriginalLots {
		held := facts.HeldByLot[lot.ID]
		totalQuantity += lot.TotalQuantity
		if lot.OwnerCustomerID != purchase.CustomerID ||
			lot.PurchaseID != purchase.ID || lot.SourceType != LotSourcePurchase ||
			lot.AvailableQuantity != lot.TotalQuantity ||
			lot.Status != LotStatusActive || !lot.ExpiresAt.After(now) {
			intact = false
		}
		if lot.EverUsed || lot.RenewalCount != 0 {
			neverUsed = false
		}
		lotSummaries = append(lotSummaries, RefundLotSummaryDTO{
			LotNo: lot.LotNo, TotalQuantity: lot.TotalQuantity,
			AvailableQuantity: lot.AvailableQuantity, HeldQuantity: held,
			ExpiresAt: formatShanghai(lot.ExpiresAt),
		})
	}
	if totalQuantity != purchase.TotalBottleQuantity ||
		facts.IssueCount != int64(len(facts.OriginalLots)) ||
		facts.IssueQuantity != int64(purchase.TotalBottleQuantity) {
		intact = false
	}
	noActiveAllocation := facts.ActiveHoldCount == 0
	noActiveRefund := facts.ActiveRefundCount == 0
	refundable := facts.RefundableAmount
	if refundable < 0 {
		refundable = 0
	}
	enabled := facts.Policy.SchemaVersion == 1 && facts.Policy.Enabled &&
		facts.Policy.RequireNeverUsed && facts.Policy.FeeAmount == 0

	checks := []RefundEligibilityCheckDTO{
		{Code: "within_refund_window", Passed: withinWindow, SafeMessage: refundCheckMessage("在退款期限内", withinWindow)},
		{Code: "purchase_paid", Passed: paymentLinkOK, SafeMessage: refundCheckMessage("原支付已确认", paymentLinkOK)},
		{Code: "never_used", Passed: neverUsed, SafeMessage: refundCheckMessage("酒票从未提取、转赠或续期", neverUsed)},
		{Code: "original_quantity_intact", Passed: intact, SafeMessage: refundCheckMessage("原始酒票数量完整且未过期", intact)},
		{Code: "no_active_allocation", Passed: noActiveAllocation, SafeMessage: refundCheckMessage("没有进行中的权益托管", noActiveAllocation)},
		{Code: "no_active_refund", Passed: noActiveRefund, SafeMessage: refundCheckMessage("没有进行中的退款", noActiveRefund)},
	}
	reasons := make([]RefundIneligibleReasonDTO, 0, 7)
	if !enabled {
		reasons = append(reasons, RefundIneligibleReasonDTO{Code: "refund_disabled", SafeMessage: "当前购买版本未开放退款"})
	}
	if !withinWindow {
		reasons = append(reasons, RefundIneligibleReasonDTO{Code: "outside_refund_window", SafeMessage: "已超过退款期限"})
	}
	if !paymentLinkOK || refundable != purchase.PaidAmount {
		reasons = append(reasons, RefundIneligibleReasonDTO{Code: "payment_not_succeeded", SafeMessage: "原支付状态暂不支持退款"})
	}
	if !neverUsed {
		reasons = append(reasons, RefundIneligibleReasonDTO{Code: "entitlement_used", SafeMessage: "酒票已有提取、转赠或续期记录"})
	}
	if !intact {
		reasons = append(reasons, RefundIneligibleReasonDTO{Code: "quantity_not_intact", SafeMessage: "原始酒票数量不完整或已过期"})
	}
	if !noActiveAllocation {
		reasons = append(reasons, RefundIneligibleReasonDTO{Code: "active_allocation_exists", SafeMessage: "酒票存在进行中的托管"})
	}
	if !noActiveRefund {
		reasons = append(reasons, RefundIneligibleReasonDTO{Code: "refund_in_progress", SafeMessage: "该购买已有退款处理中"})
	}
	eligible := enabled && withinWindow && paymentLinkOK &&
		refundable == purchase.PaidAmount && refundable > 0 &&
		neverUsed && intact && noActiveAllocation && noActiveRefund
	quoteExpiresAt := now.Add(refundQuoteTTL)
	if eligible && facts.WindowEndsAt.Before(quoteExpiresAt) {
		quoteExpiresAt = facts.WindowEndsAt
	}
	if !quoteExpiresAt.After(now) {
		quoteExpiresAt = now
	}
	lotDigest := digestRefundLots(facts.OriginalLots, facts.HeldByLot)
	policyDigest := digestRefundPolicy(facts.Purchase.RefundPolicySnapshot)
	return evaluatedRefundEligibility{
		dto: RefundQuoteDTO{
			Eligible: eligible, EligibilityChecks: checks, IneligibleReasons: reasons,
			AllowedReasonCodes: []string{"changed_mind", "duplicate_purchase", "other"},
			RefundableAmount:   refundable, Currency: "CNY",
			ExpectedPurchaseVersion: purchase.Version,
			RefundWindowEndsAt:      optionalRefundTime(facts.WindowEndsAt),
			QuoteExpiresAt:          formatShanghai(quoteExpiresAt),
			LotSummaries:            lotSummaries, PolicySummary: refundPolicySummary(facts.Policy),
			RefundRouteSummary:      "原路退回微信支付",
			EstimatedArrivalSummary: "退款成功后将按微信支付原路退回，到账时间以支付渠道为准",
		},
		quoteExpiresAt: quoteExpiresAt, lotDigest: lotDigest, policyDigest: policyDigest,
	}
}

func (s *RefundService) requireActiveCustomer(
	ctx context.Context,
	db *gorm.DB,
	customerID uint64,
) error {
	status, err := s.repo.customerStatus(ctx, db, customerID)
	if refundNotFound(err) || (err == nil && status != "active") {
		return problem.Forbidden("PERM_FORBIDDEN", "customer account is not active")
	}
	return err
}

func isActiveWineTicketRefundStatus(status string) bool {
	for _, candidate := range wineTicketRefundActiveStatuses {
		if status == candidate {
			return true
		}
	}
	return false
}

func isRefundDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "duplicate entry") ||
		strings.Contains(message, "unique constraint") ||
		strings.Contains(message, "is not unique")
}

func (s *RefundService) signQuote(claims refundQuoteClaims) (string, error) {
	if len(s.quoteSecret) < 32 {
		return "", problem.Internal("wine ticket quote signing key is not configured")
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.quoteSecret)
	_, _ = mac.Write([]byte("wine-ticket-refund-quote:v1:" + encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *RefundService) verifyQuote(raw string) (refundQuoteClaims, error) {
	if len(raw) < 32 || len(raw) > 2048 || len(s.quoteSecret) < 32 {
		return refundQuoteClaims{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid refund quote token")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 2 {
		return refundQuoteClaims{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid refund quote token")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return refundQuoteClaims{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid refund quote token")
	}
	mac := hmac.New(sha256.New, s.quoteSecret)
	_, _ = mac.Write([]byte("wine-ticket-refund-quote:v1:" + parts[0]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return refundQuoteClaims{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid refund quote token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return refundQuoteClaims{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid refund quote token")
	}
	var claims refundQuoteClaims
	if err := json.Unmarshal(payload, &claims); err != nil ||
		claims.SchemaVersion != 1 || claims.CustomerID == "" || claims.PurchaseID == "" ||
		claims.PurchaseNo == "" || claims.ExpectedPurchaseVersion == 0 ||
		claims.Currency != "CNY" || claims.QuoteExpiresAtMS <= 0 ||
		claims.LotDigest == "" || claims.PolicyDigest == "" {
		return refundQuoteClaims{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid refund quote token")
	}
	return claims, nil
}

func normalizeRefundCreate(request RefundCreateRequest) (RefundCreateRequest, error) {
	request.ReasonCode = strings.TrimSpace(request.ReasonCode)
	request.ReasonText = strings.TrimSpace(request.ReasonText)
	request.QuoteToken = strings.TrimSpace(request.QuoteToken)
	switch request.ReasonCode {
	case "changed_mind", "duplicate_purchase":
		if request.ReasonText != "" {
			return RefundCreateRequest{}, problem.InvalidArgument("VALIDATION_FAILED", "reason_text is only allowed for other")
		}
	case "other":
		if request.ReasonText == "" || utf8.RuneCountInString(request.ReasonText) > 256 {
			return RefundCreateRequest{}, problem.InvalidArgument("VALIDATION_FAILED", "reason_text is required and must be at most 256 characters")
		}
	default:
		return RefundCreateRequest{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid reason_code")
	}
	if request.ExpectedPurchaseVersion == 0 || len(request.QuoteToken) < 32 || len(request.QuoteToken) > 2048 {
		return RefundCreateRequest{}, problem.InvalidArgument("VALIDATION_FAILED", "expected_purchase_version and quote_token are required")
	}
	return request, nil
}

func refundDTO(row refundRecord) RefundDTO {
	status := effectiveRefundStatus(row.Status, row.CommonStatus)
	entitlement := "held"
	switch {
	case row.RefundKind == RefundKindIssueCompensation:
		entitlement = "not_applicable"
	case row.ExceptionCount > 0:
		entitlement = "exception"
	case row.ConsumedCount > 0 && row.HeldCount == 0 && row.RestoredCount == 0:
		entitlement = "consumed"
	case row.RestoredCount > 0 && row.HeldCount == 0 && row.ConsumedCount == 0:
		entitlement = "restored"
	}
	providerStatus := row.CommonProviderStatus
	dto := RefundDTO{
		RefundNo: row.WineTicketRefundNo, PurchaseNo: row.PurchaseNo,
		RefundKind: row.RefundKind, Amount: row.Amount, Currency: row.Currency,
		Status: status, ProviderStatus: providerStatus,
		SafeStatusMessage: refundSafeStatusMessage(status),
		EntitlementStatus: entitlement, Version: row.Version,
		RequestedAt: formatShanghai(row.RequestedAt),
		UpdatedAt:   formatShanghai(laterRefundTime(row.UpdatedAt, row.CommonUpdatedAt)),
	}
	if row.SucceededAt != nil {
		value := formatShanghai(*row.SucceededAt)
		dto.SucceededAt = &value
	}
	return dto
}

func laterRefundTime(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}

func effectiveRefundStatus(businessStatus, commonStatus string) string {
	if businessStatus == RefundStatusSucceeded || businessStatus == RefundStatusCancelled {
		return businessStatus
	}
	switch commonStatus {
	case "submission_unknown":
		return RefundStatusSubmissionUnknown
	case "pending":
		return RefundStatusProcessing
	case "failed":
		return RefundStatusRetryPending
	case "exception":
		return RefundStatusException
	case "succeeded":
		return RefundStatusSucceeded
	default:
		return businessStatus
	}
}

func refundSafeStatusMessage(status string) string {
	switch status {
	case RefundStatusHolding, RefundStatusSubmitting:
		return "退款申请已受理，相关酒票已暂时托管"
	case RefundStatusProcessing:
		return "微信退款处理中，请稍后查看"
	case RefundStatusSubmissionUnknown:
		return "退款提交结果确认中，相关酒票继续托管"
	case RefundStatusRetryPending:
		return "当前退款尝试已关闭，系统将安全重试"
	case RefundStatusException:
		return "退款状态需要核查，酒票仍保持托管"
	case RefundStatusSucceeded:
		return "退款成功，相关酒票已核销"
	case RefundStatusCancelled:
		return "退款已安全终止，相关酒票已恢复"
	default:
		return "退款状态处理中"
	}
}

func validWineTicketRefundStatus(status string) bool {
	switch status {
	case RefundStatusHolding, RefundStatusSubmitting, RefundStatusProcessing,
		RefundStatusSubmissionUnknown, RefundStatusRetryPending, RefundStatusException,
		RefundStatusSucceeded, RefundStatusCancelled:
		return true
	default:
		return false
	}
}

func digestRefundLots(lots []core.Lot, held map[uint64]uint) string {
	type digestLot struct {
		ID                 string `json:"id"`
		Version            uint   `json:"version"`
		Owner              string `json:"owner"`
		Total              uint   `json:"total"`
		Available          uint   `json:"available"`
		Held               uint   `json:"held"`
		EverUsed           bool   `json:"ever_used"`
		RenewalCount       uint   `json:"renewal_count"`
		Status             string `json:"status"`
		ExpiresAtUnixMilli int64  `json:"expires_at_ms"`
	}
	rows := make([]digestLot, 0, len(lots))
	for _, lot := range lots {
		rows = append(rows, digestLot{
			ID: idString(lot.ID), Version: lot.Version,
			Owner: idString(lot.OwnerCustomerID), Total: lot.TotalQuantity,
			Available: lot.AvailableQuantity, Held: held[lot.ID],
			EverUsed: lot.EverUsed, RenewalCount: lot.RenewalCount,
			Status: lot.Status, ExpiresAtUnixMilli: lot.ExpiresAt.UnixMilli(),
		})
	}
	raw, _ := json.Marshal(rows)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func digestRefundPolicy(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func refundCheckMessage(label string, passed bool) string {
	if passed {
		return "符合：" + label
	}
	return "不符合：" + label
}

func optionalRefundTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return formatShanghai(value)
}

func safeRefundProviderReason(code string) string {
	switch code {
	case "changed_mind":
		return "用户申请未使用酒票退款"
	case "duplicate_purchase":
		return "用户重复购买未使用酒票"
	default:
		return "用户申请未使用酒票退款"
	}
}

func refundBusinessID(raw string) (uint64, error) {
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || value == 0 {
		return 0, errors.New("invalid wine ticket refund business id")
	}
	return value, nil
}
