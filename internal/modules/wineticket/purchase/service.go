package purchase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/catalog"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
)

type purchasePackageSnapshot struct {
	SchemaVersion           int                    `json:"schema_version"`
	PackageNo               string                 `json:"package_no"`
	PackageCode             string                 `json:"package_code"`
	PackageName             string                 `json:"package_name"`
	PackageType             string                 `json:"package_type"`
	PackageVersion          uint                   `json:"package_version"`
	ValidityDays            uint                   `json:"validity_days"`
	BottleQuantity          uint                   `json:"bottle_quantity"`
	UnitPriceAmount         int64                  `json:"unit_price_amount"`
	IssuerMerchantID        string                 `json:"issuer_merchant_id"`
	SettlementShopID        string                 `json:"settlement_shop_id"`
	SettlementShopProductID string                 `json:"settlement_shop_product_id"`
	RedeemCityCode          string                 `json:"redeem_city_code"`
	Product                 core.ProductSummaryDTO `json:"product"`
}

func (s *Service) CreatePurchase(
	ctx context.Context,
	claims *auth.Claims,
	method, path, key string,
	req PurchaseCreateRequest,
) (response PurchaseDTO, resultErr error) {
	if s.paymentService == nil {
		return PurchaseDTO{}, problem.New(http.StatusServiceUnavailable, "PAYMENT_PROVIDER_UNAVAILABLE", "Service Unavailable", "payment service is unavailable")
	}
	customerID, err := customerIDWithPermission(claims, "wine_ticket_purchase:create")
	if err != nil {
		return PurchaseDTO{}, err
	}
	if err := validatePurchaseCreate(req); err != nil {
		return PurchaseDTO{}, err
	}
	requestHash := idempotency.RequestHash(req)

	var (
		purchaseID  uint64
		openID      string
		description string
		replayed    bool
	)
	claimID := s.ids.Next()
	resultErr = s.repo.WithTransaction(ctx, func(tx *gorm.DB) error {
		started, err := s.claimIdempotencyWithID(ctx, tx, claimID, claims.AccountType, customerID, method, path, key, requestHash)
		if err != nil {
			return err
		}
		if !started {
			replayed = true
			return s.cachedResponse(ctx, tx, claims.AccountType, customerID, path, key, &response)
		}

		// 进程可能在提交业务和支付草稿后、保存 HTTP 响应前崩溃。
		// 此时应认领同一草稿，而不是再次预留配额并创建第二笔购买。
		existing, err := s.repo.CustomerPurchaseByPaymentKey(ctx, tx, customerID, key)
		if err == nil {
			purchaseID = existing.ID
			snapshot, snapshotErr := parsePurchaseSnapshot(existing.PackageSnapshot)
			if snapshotErr != nil {
				return snapshotErr
			}
			if existing.PaymentState == "creating" {
				eligibility, eligibilityErr := s.repo.CustomerPurchaseEligibility(
					ctx,
					tx,
					customerID,
					s.wechatAppID,
					s.nowShanghai(),
				)
				if eligibilityErr != nil {
					return eligibilityErr
				}
				if eligibilityErr := validatePurchaseEligibility(eligibility); eligibilityErr != nil {
					return eligibilityErr
				}
				openID = eligibility.OpenID
			}
			description = "酒票-" + snapshot.PackageName
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		now := s.nowShanghai()
		pkg, err := s.repo.LockPackageForPurchase(ctx, tx, req.PackageNo)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("WT_PACKAGE_NOT_FOUND", "wine ticket package not found")
		}
		if err != nil {
			return err
		}
		if err := validatePurchasablePackage(pkg, req.Quantity, now); err != nil {
			return err
		}
		if err := s.validateSettlementForPublish(ctx, tx, pkg); err != nil {
			return err
		}
		payableAmount, totalBottles, err := checkedPurchaseTotals(pkg.SalePriceAmount, pkg.BottleQuantity, req.Quantity)
		if err != nil {
			return err
		}

		eligibility, err := s.repo.CustomerPurchaseEligibility(ctx, tx, customerID, s.wechatAppID, now)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.Unauthorized("AUTH_UNAUTHORIZED", "customer account is unavailable")
		}
		if err != nil {
			return err
		}
		if err := validatePurchaseEligibility(eligibility); err != nil {
			return err
		}
		openID = eligibility.OpenID
		description = "酒票-" + pkg.Name

		quotaSeed := PurchaseQuota{
			ID: s.ids.Next(), CustomerID: customerID, PackageCode: pkg.PackageCode,
			Version: 1, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.repo.EnsurePurchaseQuota(ctx, tx, &quotaSeed); err != nil {
			return err
		}
		quota, err := s.repo.LockPurchaseQuota(ctx, tx, customerID, pkg.PackageCode)
		if err != nil {
			return err
		}
		if err := validatePurchaseQuota(quota, pkg.PerCustomerLimit, req.Quantity); err != nil {
			return err
		}
		if err := s.repo.UpdatePurchaseQuota(ctx, tx, quota.ID, map[string]any{
			"reserved_quantity": quota.ReservedQuantity + req.Quantity,
			"version":           gorm.Expr("version + 1"),
			"updated_at":        now,
		}); err != nil {
			return err
		}

		product, err := s.packageProductSummary(ctx, tx, pkg.ProductID)
		if err != nil {
			return err
		}
		snapshot := purchasePackageSnapshot{
			SchemaVersion: 1, PackageNo: pkg.PackageNo, PackageCode: pkg.PackageCode,
			PackageName: pkg.Name, PackageType: pkg.PackageType, PackageVersion: pkg.PackageVersion,
			ValidityDays: pkg.ValidityDays, BottleQuantity: pkg.BottleQuantity,
			UnitPriceAmount: pkg.SalePriceAmount, IssuerMerchantID: idString(pkg.IssuerMerchantID),
			SettlementShopID:        idString(pkg.SettlementShopID),
			SettlementShopProductID: idString(pkg.SettlementShopProductID),
			RedeemCityCode:          pkg.RedeemCityCode, Product: product,
		}
		snapshotJSON, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}

		purchaseID = s.ids.Next()
		paymentID := s.ids.Next()
		purchase := Purchase{
			ID: purchaseID, PurchaseNo: "WTPU" + idString(purchaseID),
			CustomerID: customerID, PackageID: pkg.ID, PackageVersion: pkg.PackageVersion,
			PaymentID: paymentID, IssuerMerchantID: pkg.IssuerMerchantID,
			SettlementShopID:        pkg.SettlementShopID,
			SettlementShopProductID: pkg.SettlementShopProductID,
			ProductID:               pkg.ProductID, RedeemCityCode: pkg.RedeemCityCode,
			PackageQuantity: req.Quantity, BottleQuantityPerPackage: pkg.BottleQuantity,
			TotalBottleQuantity: totalBottles, UnitPriceAmount: pkg.SalePriceAmount,
			PayableAmount: payableAmount, PaidAmount: 0, Currency: "CNY",
			PackageSnapshot:       datatypes.JSON(snapshotJSON),
			RefundPolicySnapshot:  cloneJSON(pkg.RefundPolicy),
			RenewalPolicySnapshot: cloneJSON(pkg.RenewalPolicy),
			Status:                PurchaseStatusPendingPayment, Version: 1,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.repo.CreatePurchase(ctx, tx, &purchase); err != nil {
			return err
		}
		expiresAt := now.Add(30 * time.Minute)
		if _, err := s.paymentService.CreateBusinessPaymentTx(ctx, tx, order.BusinessPaymentCreateInput{
			PaymentID: paymentID, PaymentNo: "WTPAY" + idString(paymentID),
			BizType: PurchasePaymentBusiness, BizID: purchaseID, CustomerID: customerID,
			Channel: "wechat_miniapp", Provider: "wechat", Amount: payableAmount,
			Currency: "CNY", ExpiresAt: expiresAt, IdempotencyKey: key,
		}); err != nil {
			return err
		}
		if err := s.createCustomerAudit(ctx, tx, customerID, "wine_ticket.purchase.create", "wine_ticket_purchase", purchaseID, nil, map[string]any{
			"purchase_no": purchase.PurchaseNo, "status": purchase.Status,
			"package_no": pkg.PackageNo, "package_quantity": req.Quantity,
			"payable_amount": payableAmount,
		}); err != nil {
			return err
		}
		return s.createWineTicketOutbox(ctx, tx, "wine_ticket.purchase_created", "wine_ticket_purchase", purchaseID, map[string]any{
			"purchase_no": purchase.PurchaseNo, "customer_id": idString(customerID),
			"package_no": pkg.PackageNo, "payment_id": idString(paymentID),
		})
	})
	if resultErr != nil || replayed {
		return response, resultErr
	}

	payment, err := s.paymentService.BusinessPayment(ctx, customerID, PurchasePaymentBusiness, purchaseID)
	if err != nil {
		s.releaseCustomerIdempotencyOwned(ctx, claimID, claims.AccountType, customerID, path, key)
		return PurchaseDTO{}, err
	}
	reflectSubmissionState := payment.Status == "failed" ||
		payment.Status == "closed"
	if payment.Status == "creating" {
		_, submitErr := s.paymentService.SubmitBusinessPayment(ctx, payment.ID, openID, description)
		if submitErr != nil {
			payment, err = s.paymentService.BusinessPayment(ctx, customerID, PurchasePaymentBusiness, purchaseID)
			if err != nil {
				s.releaseCustomerIdempotencyOwned(ctx, claimID, claims.AccountType, customerID, path, key)
				return PurchaseDTO{}, submitErr
			}
			reflectSubmissionState = true
		}
	}
	// 进程可能在支付终态持久化后、购买记录与配额收敛前停止。
	// 重新认领过期幂等记录时，必须先完成该映射，再缓存恢复后的响应。
	if reflectSubmissionState {
		if err := s.reflectPaymentSubmissionState(ctx, purchaseID, payment.Status); err != nil {
			s.releaseCustomerIdempotencyOwned(ctx, claimID, claims.AccountType, customerID, path, key)
			return PurchaseDTO{}, err
		}
	}

	response, err = s.customerPurchaseDTOByID(ctx, customerID, purchaseID)
	if err != nil {
		s.releaseCustomerIdempotencyOwned(ctx, claimID, claims.AccountType, customerID, path, key)
		return PurchaseDTO{}, err
	}
	if err := s.repo.WithTransaction(ctx, func(tx *gorm.DB) error {
		return s.idStore.SucceedOwned(ctx, tx, claimID, claims.AccountType, customerID, path, key, response)
	}); err != nil {
		s.releaseCustomerIdempotencyOwned(ctx, claimID, claims.AccountType, customerID, path, key)
		return PurchaseDTO{}, err
	}
	return response, nil
}

func (s *Service) ListPurchases(ctx context.Context, claims *auth.Claims, query pagination.Query, status string) ([]PurchaseDTO, string, error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_purchase:list")
	if err != nil {
		return nil, "", err
	}
	status = strings.TrimSpace(status)
	if status != "" && !validPurchaseStatus(status) {
		return nil, "", problem.InvalidArgument("VALIDATION_FAILED", "invalid purchase status")
	}
	rows, err := s.repo.ListCustomerPurchases(ctx, customerID, query, status)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		rows = rows[:query.PageSize]
		next = pagination.NextPageTokenWithCursor(query, idString(rows[len(rows)-1].ID))
	}
	items, err := s.purchaseDTOs(ctx, rows)
	return items, next, err
}

func (s *Service) Purchase(ctx context.Context, claims *auth.Claims, purchaseNo string) (PurchaseDTO, error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_purchase:view")
	if err != nil {
		return PurchaseDTO{}, err
	}
	if err := validateBusinessNo(purchaseNo, "purchase_no"); err != nil {
		return PurchaseDTO{}, err
	}
	row, err := s.repo.CustomerPurchaseByNo(ctx, customerID, purchaseNo)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return PurchaseDTO{}, problem.NotFound("WT_PURCHASE_NOT_FOUND", "wine ticket purchase not found")
	}
	if err != nil {
		return PurchaseDTO{}, err
	}
	items, err := s.purchaseDTOs(ctx, []PurchaseRecord{row})
	if err != nil {
		return PurchaseDTO{}, err
	}
	return items[0], nil
}

func (s *Service) ConfirmPurchasePayment(
	ctx context.Context,
	claims *auth.Claims,
	method, path, key, purchaseNo string,
) (response PurchaseDTO, resultErr error) {
	if s.paymentService == nil {
		return PurchaseDTO{}, problem.New(http.StatusServiceUnavailable, "PAYMENT_PROVIDER_UNAVAILABLE", "Service Unavailable", "payment service is unavailable")
	}
	customerID, err := customerIDWithPermission(claims, "wine_ticket_payment:confirm")
	if err != nil {
		return PurchaseDTO{}, err
	}
	if err := validateBusinessNo(purchaseNo, "purchase_no"); err != nil {
		return PurchaseDTO{}, err
	}
	row, err := s.repo.CustomerPurchaseByNo(ctx, customerID, purchaseNo)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return PurchaseDTO{}, problem.NotFound("WT_PURCHASE_NOT_FOUND", "wine ticket purchase not found")
	}
	if err != nil {
		return PurchaseDTO{}, err
	}
	requestHash := idempotency.ResourceRequestHash("wine_ticket.purchase.payment.confirm", purchaseNo, map[string]any{})
	started := false
	claimID := s.ids.Next()
	resultErr = s.repo.WithTransaction(ctx, func(tx *gorm.DB) error {
		claimed, err := s.claimIdempotencyWithID(ctx, tx, claimID, claims.AccountType, customerID, method, path, key, requestHash)
		if err != nil {
			return err
		}
		started = claimed
		if !claimed {
			return s.cachedResponse(ctx, tx, claims.AccountType, customerID, path, key, &response)
		}
		return nil
	})
	if resultErr != nil || !started {
		return response, resultErr
	}

	if _, err := s.paymentService.ConfirmBusinessPayment(ctx, customerID, PurchasePaymentBusiness, row.ID); err != nil {
		s.releaseCustomerIdempotencyOwned(ctx, claimID, claims.AccountType, customerID, path, key)
		return PurchaseDTO{}, err
	}
	response, err = s.customerPurchaseDTOByID(ctx, customerID, row.ID)
	if err != nil {
		s.releaseCustomerIdempotencyOwned(ctx, claimID, claims.AccountType, customerID, path, key)
		return PurchaseDTO{}, err
	}
	if err := s.repo.WithTransaction(ctx, func(tx *gorm.DB) error {
		return s.idStore.SucceedOwned(ctx, tx, claimID, claims.AccountType, customerID, path, key, response)
	}); err != nil {
		s.releaseCustomerIdempotencyOwned(ctx, claimID, claims.AccountType, customerID, path, key)
		return PurchaseDTO{}, err
	}
	return response, nil
}

func (s *Service) reflectPaymentSubmissionState(ctx context.Context, purchaseID uint64, paymentStatus string) error {
	return s.repo.WithTransaction(ctx, func(tx *gorm.DB) error {
		purchase, err := s.repo.LockPurchaseByID(ctx, tx, purchaseID)
		if err != nil {
			return err
		}
		if purchase.Status != PurchaseStatusPendingPayment {
			return nil
		}
		snapshot, err := parsePurchaseSnapshot(purchase.PackageSnapshot)
		if err != nil {
			return err
		}
		quota, err := s.repo.LockPurchaseQuota(ctx, tx, purchase.CustomerID, snapshot.PackageCode)
		if err != nil {
			return err
		}
		now := s.nowShanghai()
		switch paymentStatus {
		case "failed", "closed":
			if quota.ReservedQuantity < purchase.PackageQuantity {
				return problem.Internal("wine ticket purchase quota reservation is inconsistent")
			}
			if err := s.repo.UpdatePurchaseQuota(ctx, tx, quota.ID, map[string]any{
				"reserved_quantity": quota.ReservedQuantity - purchase.PackageQuantity,
				"version":           gorm.Expr("version + 1"), "updated_at": now,
			}); err != nil {
				return err
			}
			return s.repo.UpdatePurchase(ctx, tx, purchase.ID, map[string]any{
				"status": PurchaseStatusClosed, "version": gorm.Expr("version + 1"), "updated_at": now,
			})
		default:
			return s.repo.UpdatePurchase(ctx, tx, purchase.ID, map[string]any{
				"status": PurchaseStatusPaymentUnknown, "version": gorm.Expr("version + 1"), "updated_at": now,
			})
		}
	})
}

func (s *Service) customerPurchaseDTOByID(ctx context.Context, customerID, purchaseID uint64) (PurchaseDTO, error) {
	row, err := s.repo.CustomerPurchaseByID(ctx, customerID, purchaseID)
	if err != nil {
		return PurchaseDTO{}, err
	}
	items, err := s.purchaseDTOs(ctx, []PurchaseRecord{row})
	if err != nil {
		return PurchaseDTO{}, err
	}
	return items[0], nil
}

func (s *Service) purchaseDTOs(ctx context.Context, rows []PurchaseRecord) ([]PurchaseDTO, error) {
	ids := make([]uint64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	facts, err := s.repo.PurchaseLotFacts(ctx, ids)
	if err != nil {
		return nil, err
	}
	byPurchase := make(map[uint64][]LotFactRecord, len(rows))
	for _, fact := range facts {
		byPurchase[fact.PurchaseID] = append(byPurchase[fact.PurchaseID], fact)
	}
	items := make([]PurchaseDTO, 0, len(rows))
	for _, row := range rows {
		item, err := purchaseRecordDTO(row, byPurchase[row.ID])
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

// DTOsFromRecords 用于根模块的运营投影。
// 面向客户的调用方应使用 ListPurchases 或 Purchase。
func (s *Service) DTOsFromRecords(
	ctx context.Context,
	rows []PurchaseRecord,
) ([]PurchaseDTO, error) {
	return s.purchaseDTOs(ctx, rows)
}

func purchaseRecordDTO(row PurchaseRecord, facts []LotFactRecord) (PurchaseDTO, error) {
	snapshot, err := parsePurchaseSnapshot(row.PackageSnapshot)
	if err != nil {
		return PurchaseDTO{}, err
	}
	var refundPolicy core.RefundPolicy
	if err := json.Unmarshal(row.RefundPolicySnapshot, &refundPolicy); err != nil {
		return PurchaseDTO{}, problem.Internal("wine ticket purchase refund policy snapshot is invalid")
	}
	var remaining, held uint
	lotSummaries := make([]PurchaseLotDTO, 0, minInt(len(facts), 20))
	allUnused := len(facts) > 0
	for index, fact := range facts {
		remaining += fact.AvailableQuantity
		held += fact.HeldQuantity()
		if fact.EverUsed || fact.AvailableQuantity != fact.TotalQuantity || fact.HeldQuantity() != 0 {
			allUnused = false
		}
		if index < 20 {
			lotSummaries = append(lotSummaries, PurchaseLotDTO{
				LotNo: fact.LotNo, AvailableQuantity: fact.AvailableQuantity,
				HeldQuantity: fact.HeldQuantity(), ExpiresAt: formatShanghai(fact.ExpiresAt),
				Status: fact.Status,
			})
		}
	}
	var safeMessage *string
	switch row.Status {
	case PurchaseStatusPaymentUnknown:
		safeMessage = stringPointer("支付结果确认中，请稍后刷新")
	case PurchaseStatusSettlementException:
		safeMessage = stringPointer("支付已确认，酒票正在安全补发，请勿重复支付")
	case PurchaseStatusClosed:
		safeMessage = stringPointer("支付未完成，购买已关闭")
	}
	parameters := map[string]any(nil)
	if row.PaymentStatus == "pending" {
		parameters = decodePaymentParameters(row.ClientPayload)
	}
	return PurchaseDTO{
		PurchaseNo: row.PurchaseNo, PackageNo: row.PackageNo,
		PackageName: row.PackageName, PackageVersion: row.PackageVersion,
		PackageSnapshotSummary: fmt.Sprintf("%s，共%d瓶，有效期%d天", snapshot.PackageName, row.TotalBottleQuantity, snapshot.ValidityDays),
		Product: core.ProductSummaryDTO{
			ProductID: idString(row.ProductID), Name: row.ProductName,
			BrandName: row.ProductBrandName, Spec: row.ProductSpec, ImageURL: row.ProductImageURL,
		},
		PackageType: row.PackageType, PackageQuantity: row.PackageQuantity,
		TotalBottleQuantity: row.TotalBottleQuantity, PayableAmount: row.PayableAmount,
		PaidAmount: row.PaidAmount, RemainingBottleQuantity: remaining,
		HeldBottleQuantity: held, LotCount: uint(len(facts)), LotSummaries: lotSummaries,
		RefundPolicySummary: refundPolicySummary(refundPolicy),
		RefundEligible: row.Status == PurchaseStatusIssued && allUnused &&
			remaining == row.TotalBottleQuantity,
		RefundStatus: row.RefundStatus, RefundKind: row.RefundKind,
		RefundNo: row.RefundNo,
		Currency: row.Currency, Status: row.Status, PaymentStatus: row.PaymentStatus,
		PaymentParameters: parameters, Version: row.Version,
		PaidAt: optionalTimeString(row.PaidAt), IssuedAt: optionalTimeString(row.IssuedAt),
		CreatedAt: formatShanghai(row.CreatedAt), UpdatedAt: formatShanghai(row.UpdatedAt),
		SafeStatusMessage: safeMessage,
	}, nil
}

func validatePurchaseCreate(req PurchaseCreateRequest) error {
	if err := validateBusinessNo(req.PackageNo, "package_no"); err != nil {
		return err
	}
	if req.Quantity == 0 || req.Quantity > maxPurchaseQuantity {
		return problem.InvalidArgument("VALIDATION_FAILED", "quantity must be between 1 and 10000")
	}
	return nil
}

func validatePurchasablePackage(pkg catalog.Package, quantity uint, now time.Time) error {
	if pkg.Status != PackageStatusPublished ||
		(pkg.SaleStartAt != nil && pkg.SaleStartAt.After(now)) ||
		(pkg.SaleEndAt != nil && !pkg.SaleEndAt.After(now)) {
		return problem.Conflict("WT_PACKAGE_NOT_AVAILABLE", "wine ticket package is not currently available")
	}
	if quantity < pkg.MinPurchaseQuantity || quantity > pkg.MaxPurchaseQuantity {
		return problem.InvalidArgument("VALIDATION_FAILED", "quantity is outside the package purchase range")
	}
	if err := validatePackageRow(pkg); err != nil {
		return problem.Conflict("WT_PACKAGE_NOT_AVAILABLE", "wine ticket package configuration is invalid")
	}
	return nil
}

func checkedPurchaseTotals(unitPrice int64, bottlesPerPackage, quantity uint) (int64, uint, error) {
	if unitPrice <= 0 || unitPrice > maxWineTicketAmount || quantity == 0 || bottlesPerPackage == 0 {
		return 0, 0, problem.InvalidArgument("VALIDATION_FAILED", "invalid purchase amount inputs")
	}
	if int64(quantity) > math.MaxInt64/unitPrice {
		return 0, 0, problem.New(http.StatusUnprocessableEntity, "WT_AMOUNT_LIMIT_EXCEEDED", "Unprocessable Entity", "purchase amount exceeds the supported limit")
	}
	amount := unitPrice * int64(quantity)
	if amount > maxWineTicketAmount {
		return 0, 0, problem.New(http.StatusUnprocessableEntity, "WT_AMOUNT_LIMIT_EXCEEDED", "Unprocessable Entity", "purchase amount exceeds 100000000 cents")
	}
	if quantity > ^uint(0)/bottlesPerPackage {
		return 0, 0, problem.InvalidArgument("VALIDATION_FAILED", "total bottle quantity overflow")
	}
	return amount, quantity * bottlesPerPackage, nil
}

func CheckedPurchaseTotals(
	unitPrice int64,
	bottlesPerPackage uint,
	quantity uint,
) (int64, uint, error) {
	return checkedPurchaseTotals(unitPrice, bottlesPerPackage, quantity)
}

func validatePurchaseQuota(quota PurchaseQuota, limit *uint, requested uint) error {
	if requested > ^uint(0)-quota.ReservedQuantity ||
		quota.ReservedQuantity+requested > ^uint(0)-quota.ConsumedQuantity {
		return problem.Conflict("WT_PACKAGE_NOT_AVAILABLE", "purchase quota is inconsistent")
	}
	if limit != nil && quota.ReservedQuantity+quota.ConsumedQuantity+requested > *limit {
		return problem.Conflict("WT_PACKAGE_NOT_AVAILABLE", "per-customer package purchase limit exceeded")
	}
	return nil
}

func ValidatePurchaseQuota(
	quota PurchaseQuota,
	limit *uint,
	requested uint,
) error {
	return validatePurchaseQuota(quota, limit, requested)
}

func validatePurchaseEligibility(row CustomerEligibility) error {
	if row.CustomerID == 0 || row.CustomerStatus != "active" {
		return problem.Unauthorized("AUTH_UNAUTHORIZED", "customer account is unavailable")
	}
	if strings.TrimSpace(row.Phone) == "" {
		return problem.Conflict("PHONE_BINDING_REQUIRED", "phone binding is required for payment")
	}
	if row.IdentityCount != 1 || strings.TrimSpace(row.OpenID) == "" {
		return problem.Conflict("WECHAT_IDENTITY_REQUIRED", "one active mini-program identity is required for payment")
	}
	if row.RealnameStatus == nil || *row.RealnameStatus != "verified" || row.RevokedAt != nil {
		if row.PendingVerificationCount > 0 {
			return problem.Conflict("IDENTITY_VERIFICATION_PENDING", "identity verification is still processing")
		}
		return problem.New(http.StatusUnprocessableEntity, "REALNAME_REQUIRED", "Unprocessable Entity", "valid real-name verification required")
	}
	if row.AdultResult == nil || *row.AdultResult != "adult" {
		return problem.New(http.StatusUnprocessableEntity, "UNDERAGE_RESTRICTED", "Unprocessable Entity", "wine ticket purchase requires an adult customer")
	}
	return nil
}

func ValidatePurchaseEligibility(row CustomerEligibility) error {
	return validatePurchaseEligibility(row)
}

func validPurchaseStatus(status string) bool {
	switch status {
	case PurchaseStatusPendingPayment, PurchaseStatusPaymentUnknown,
		PurchaseStatusSettlementException, PurchaseStatusIssued,
		PurchaseStatusClosed, "refund_holding", "refund_exception", "refunded":
		return true
	default:
		return false
	}
}

func ValidPurchaseStatus(status string) bool { return validPurchaseStatus(status) }

func parsePurchaseSnapshot(raw datatypes.JSON) (purchasePackageSnapshot, error) {
	var snapshot purchasePackageSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil ||
		snapshot.SchemaVersion != 1 || snapshot.PackageCode == "" ||
		snapshot.ValidityDays == 0 || snapshot.PackageNo == "" {
		return purchasePackageSnapshot{}, problem.Internal("wine ticket purchase package snapshot is invalid")
	}
	return snapshot, nil
}

func (s *Service) packageProductSummary(ctx context.Context, tx *gorm.DB, productID uint64) (core.ProductSummaryDTO, error) {
	row, err := s.repo.PackageProduct(ctx, tx, productID)
	if err != nil {
		return core.ProductSummaryDTO{}, err
	}
	return core.ProductSummaryDTO{
		ProductID: idString(row.ID), Name: row.Name, BrandName: row.BrandName,
		Spec: row.Spec, ImageURL: row.ImageURL,
	}, nil
}

func customerIDWithPermission(claims *auth.Claims, permission string) (uint64, error) {
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

func validateBusinessNo(value, field string) error {
	value = strings.TrimSpace(value)
	if len(value) < 1 || len(value) > 64 || !businessNoPattern.MatchString(value) {
		return problem.InvalidArgument("VALIDATION_FAILED", "invalid "+field)
	}
	return nil
}

func cloneJSON(value datatypes.JSON) datatypes.JSON {
	return append(datatypes.JSON(nil), value...)
}

func (c *serviceCore) releaseCustomerIdempotencyOwned(ctx context.Context, claimID uint64, actorType string, actorID uint64, path, key string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_ = c.repo.WithTransaction(cleanupCtx, func(tx *gorm.DB) error {
		return c.idStore.FailOwned(cleanupCtx, tx, claimID, actorType, actorID, path, key)
	})
}

func (c *serviceCore) createCustomerAudit(ctx context.Context, tx *gorm.DB, actorID uint64, action, resourceType string, resourceID uint64, before, after any) error {
	return c.repo.CreateAudit(ctx, tx, map[string]any{
		"id": c.ids.Next(), "actor_type": "customer", "actor_id": actorID,
		"action": action, "resource_type": resourceType, "resource_id": resourceID,
		"before_data": jsonData(before), "after_data": jsonData(after), "result": "success",
		"request_id": requestctx.RequestIDPtr(ctx), "ip_hash": requestctx.IPHashPtr(ctx),
		"user_agent": requestctx.UserAgentPtr(ctx),
	})
}

func (c *serviceCore) createWineTicketOutbox(ctx context.Context, tx *gorm.DB, eventType, aggregateType string, aggregateID uint64, payload any) error {
	return c.repo.CreateOutbox(ctx, tx, map[string]any{
		"id": c.ids.Next(), "event_id": uuid.NewString(), "event_type": eventType,
		"event_version": 1, "spec_version": "1.0", "aggregate_type": aggregateType,
		"aggregate_id": aggregateID, "producer": "wine-ticket",
		"payload": jsonData(payload), "status": "pending", "retry_count": 0,
		"request_id": requestctx.RequestIDPtr(ctx), "created_at": c.nowShanghai(),
	})
}

func stringPointer(value string) *string { return &value }

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
