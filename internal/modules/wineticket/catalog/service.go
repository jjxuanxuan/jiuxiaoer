package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	mysqlerr "github.com/go-sql-driver/mysql"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

var (
	packageCodePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	cityCodePattern    = regexp.MustCompile(`^[0-9A-Za-z_-]+$`)
	businessNoPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

type Service struct {
	repo     *Repository
	idStore  *idempotency.Store
	ids      *snowflake.Generator
	now      func() time.Time
	instance string
}

func NewService(db *gorm.DB, ids *snowflake.Generator) *Service {
	return &Service{
		repo:    NewRepository(db),
		idStore: idempotency.NewStore(db),
		ids:     ids,
		now:     time.Now,
	}
}

func (s *Service) WithClock(now func() time.Time) *Service {
	if now != nil {
		s.now = now
	}
	return s
}

func (s *Service) WithInstance(instance string) *Service {
	s.instance = strings.TrimSpace(instance)
	return s
}

func (s *Service) ListPublicPackages(ctx context.Context, query pagination.Query, packageType string) ([]PackageDTO, string, error) {
	packageType = strings.TrimSpace(packageType)
	if packageType != "" && !validPackageType(packageType) {
		return nil, "", problem.InvalidArgument("VALIDATION_FAILED", "invalid package_type")
	}
	rows, err := s.repo.ListPublicPackages(ctx, query, PackageListFilter{PackageType: packageType}, s.nowShanghai())
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		rows = rows[:query.PageSize]
		next = pagination.NextPageTokenWithCursor(query, idString(rows[len(rows)-1].ID))
	}
	items := make([]PackageDTO, 0, len(rows))
	for _, row := range rows {
		item, err := packageDTO(row)
		if err != nil {
			return nil, "", err
		}
		items = append(items, item)
	}
	return items, next, nil
}

func (s *Service) PublicPackage(ctx context.Context, packageNo string) (PackageDTO, error) {
	if err := validatePackageNo(packageNo); err != nil {
		return PackageDTO{}, err
	}
	row, err := s.repo.PublicPackageByNo(ctx, packageNo, s.nowShanghai())
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return PackageDTO{}, problem.NotFound("WT_PACKAGE_NOT_FOUND", "wine ticket package not found")
	}
	if err != nil {
		return PackageDTO{}, err
	}
	return packageDTO(row)
}

func (s *Service) ListAdminPackages(ctx context.Context, claims *auth.Claims, query pagination.Query) ([]AdminPackageDTO, string, error) {
	if _, err := adminIDWithPermission(claims, "wine_ticket_package:list"); err != nil {
		return nil, "", err
	}
	rows, err := s.repo.ListAdminPackages(ctx, query)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		rows = rows[:query.PageSize]
		next = pagination.NextPageTokenWithCursor(query, idString(rows[len(rows)-1].ID))
	}
	items := make([]AdminPackageDTO, 0, len(rows))
	for _, row := range rows {
		item, err := adminPackageDTO(row)
		if err != nil {
			return nil, "", err
		}
		items = append(items, item)
	}
	return items, next, nil
}

func (s *Service) AdminPackage(ctx context.Context, claims *auth.Claims, packageNo string) (AdminPackageDTO, error) {
	if _, err := adminIDWithPermission(claims, "wine_ticket_package:view"); err != nil {
		return AdminPackageDTO{}, err
	}
	if err := validatePackageNo(packageNo); err != nil {
		return AdminPackageDTO{}, err
	}
	row, err := s.repo.AdminPackageByNo(ctx, nil, packageNo)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return AdminPackageDTO{}, problem.NotFound("WT_PACKAGE_NOT_FOUND", "wine ticket package not found")
	}
	if err != nil {
		return AdminPackageDTO{}, err
	}
	return adminPackageDTO(row)
}

func (s *Service) CreateAdminPackage(ctx context.Context, claims *auth.Claims, method, path, key string, req PackageWriteRequest) (resp AdminPackageDTO, resultErr error) {
	actorID, err := adminIDWithPermission(claims, "wine_ticket_package:create")
	if err != nil {
		return AdminPackageDTO{}, err
	}
	input, err := normalizePackageWrite(req, false)
	if err != nil {
		return AdminPackageDTO{}, err
	}
	requestHash := idempotency.RequestHash(req)

	resultErr = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.claimIdempotency(ctx, tx, claims.AccountType, actorID, method, path, key, requestHash)
		if err != nil {
			return err
		}
		if !started {
			return s.cachedResponse(ctx, tx, claims.AccountType, actorID, path, key, &resp)
		}

		packageVersion, err := s.repo.NextPackageVersionLocked(ctx, tx, input.PackageCode)
		if err != nil {
			return err
		}
		now := s.nowShanghai()
		id := s.ids.Next()
		row := Package{
			ID:                      id,
			PackageNo:               "WTP" + idString(id),
			PackageCode:             input.PackageCode,
			PackageVersion:          packageVersion,
			IssuerMerchantID:        input.IssuerMerchantID,
			SettlementShopID:        input.SettlementShopID,
			SettlementShopProductID: input.SettlementShopProductID,
			ProductID:               input.ProductID,
			RedeemCityCode:          input.RedeemCityCode,
			PackageType:             input.PackageType,
			Name:                    input.Name,
			Subtitle:                input.Subtitle,
			CoverImageURL:           input.CoverImageURL,
			BottleQuantity:          input.BottleQuantity,
			SalePriceAmount:         input.SalePriceAmount,
			MinPurchaseQuantity:     input.MinPurchaseQuantity,
			MaxPurchaseQuantity:     input.MaxPurchaseQuantity,
			ValidityDays:            input.ValidityDays,
			PerCustomerLimit:        input.PerCustomerLimit,
			RefundPolicy:            input.RefundPolicyJSON,
			RenewalPolicy:           input.RenewalPolicyJSON,
			DeliveryPolicy:          input.DeliveryPolicyJSON,
			Status:                  PackageStatusDraft,
			SaleStartAt:             input.SaleStartAt,
			SaleEndAt:               input.SaleEndAt,
			Version:                 1,
			CreatedAt:               now,
			UpdatedAt:               now,
			CreatedBy:               uint64Ptr(actorID),
			UpdatedBy:               uint64Ptr(actorID),
		}
		if err := s.repo.CreatePackage(ctx, tx, &row); err != nil {
			return packageWriteDBError(err)
		}
		projected, err := s.repo.AdminPackageByNo(ctx, tx, row.PackageNo)
		if err != nil {
			return err
		}
		resp, err = adminPackageDTO(projected)
		if err != nil {
			return err
		}
		if err := s.createPackageAudit(ctx, tx, actorID, "wine_ticket.package.create", row.ID, nil, &resp, nil); err != nil {
			return err
		}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, actorID, path, key, resp)
	})
	return resp, packageWriteDBError(resultErr)
}

func (s *Service) UpdateAdminPackage(ctx context.Context, claims *auth.Claims, method, path, key, packageNo string, req PackageWriteRequest) (resp AdminPackageDTO, resultErr error) {
	actorID, err := adminIDWithPermission(claims, "wine_ticket_package:update")
	if err != nil {
		return AdminPackageDTO{}, err
	}
	if err := validatePackageNo(packageNo); err != nil {
		return AdminPackageDTO{}, err
	}
	input, err := normalizePackageWrite(req, true)
	if err != nil {
		return AdminPackageDTO{}, err
	}
	requestHash := idempotency.ResourceRequestHash("update", packageNo, req)

	resultErr = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.claimIdempotency(ctx, tx, claims.AccountType, actorID, method, path, key, requestHash)
		if err != nil {
			return err
		}
		if !started {
			return s.cachedResponse(ctx, tx, claims.AccountType, actorID, path, key, &resp)
		}
		row, err := s.repo.LockPackageByNo(ctx, tx, packageNo)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("WT_PACKAGE_NOT_FOUND", "wine ticket package not found")
		}
		if err != nil {
			return err
		}
		if row.Version != *req.ExpectedVersion {
			return problem.Conflict("WT_CONCURRENT_MODIFICATION", "wine ticket package version changed")
		}
		if row.Status != PackageStatusDraft {
			return problem.Conflict("WT_PACKAGE_NOT_AVAILABLE", "only a draft package can be edited; create a new version for a published package")
		}
		if row.PackageCode != input.PackageCode {
			return problem.InvalidArgument("VALIDATION_FAILED", "package_code cannot be changed after draft creation")
		}
		beforeRecord, err := s.repo.AdminPackageByNo(ctx, tx, packageNo)
		if err != nil {
			return err
		}
		before, err := adminPackageDTO(beforeRecord)
		if err != nil {
			return err
		}
		now := s.nowShanghai()
		updated, err := s.repo.UpdateDraftPackage(ctx, tx, row, *req.ExpectedVersion, map[string]any{
			"issuer_merchant_id":         input.IssuerMerchantID,
			"settlement_shop_id":         input.SettlementShopID,
			"settlement_shop_product_id": input.SettlementShopProductID,
			"product_id":                 input.ProductID,
			"redeem_city_code":           input.RedeemCityCode,
			"package_type":               input.PackageType,
			"name":                       input.Name,
			"subtitle":                   input.Subtitle,
			"cover_image_url":            input.CoverImageURL,
			"bottle_quantity":            input.BottleQuantity,
			"sale_price_amount":          input.SalePriceAmount,
			"min_purchase_quantity":      input.MinPurchaseQuantity,
			"max_purchase_quantity":      input.MaxPurchaseQuantity,
			"validity_days":              input.ValidityDays,
			"per_customer_limit":         input.PerCustomerLimit,
			"refund_policy":              input.RefundPolicyJSON,
			"renewal_policy":             input.RenewalPolicyJSON,
			"delivery_policy":            input.DeliveryPolicyJSON,
			"sale_start_at":              input.SaleStartAt,
			"sale_end_at":                input.SaleEndAt,
			"version":                    gorm.Expr("version + 1"),
			"updated_at":                 now,
			"updated_by":                 actorID,
		})
		if err != nil {
			return packageWriteDBError(err)
		}
		if !updated {
			return problem.Conflict("WT_CONCURRENT_MODIFICATION", "wine ticket package changed concurrently")
		}
		afterRecord, err := s.repo.AdminPackageByNo(ctx, tx, packageNo)
		if err != nil {
			return err
		}
		resp, err = adminPackageDTO(afterRecord)
		if err != nil {
			return err
		}
		if err := s.createPackageAudit(ctx, tx, actorID, "wine_ticket.package.update", row.ID, &before, &resp, nil); err != nil {
			return err
		}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, actorID, path, key, resp)
	})
	return resp, packageWriteDBError(resultErr)
}

func (s *Service) PublishAdminPackage(ctx context.Context, claims *auth.Claims, method, path, key, packageNo string, req ExpectedVersionRequest) (AdminPackageDTO, error) {
	return s.transitionAdminPackage(ctx, claims, method, path, key, packageNo, req, true)
}

func (s *Service) UnpublishAdminPackage(ctx context.Context, claims *auth.Claims, method, path, key, packageNo string, req ExpectedVersionRequest) (AdminPackageDTO, error) {
	return s.transitionAdminPackage(ctx, claims, method, path, key, packageNo, req, false)
}

func (s *Service) transitionAdminPackage(ctx context.Context, claims *auth.Claims, method, path, key, packageNo string, req ExpectedVersionRequest, publish bool) (resp AdminPackageDTO, resultErr error) {
	permission := "wine_ticket_package:unpublish"
	if publish {
		permission = "wine_ticket_package:publish"
	}
	actorID, err := adminIDWithPermission(claims, permission)
	if err != nil {
		return AdminPackageDTO{}, err
	}
	if err := validatePackageNo(packageNo); err != nil {
		return AdminPackageDTO{}, err
	}
	if req.ExpectedVersion == 0 {
		return AdminPackageDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "expected_version must be at least 1")
	}
	action := "unpublish"
	auditAction := "wine_ticket.package.unpublish"
	if publish {
		action = "publish"
		auditAction = "wine_ticket.package.publish"
	}
	requestHash := idempotency.ResourceRequestHash(action, packageNo, req)

	resultErr = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		allowed, authErr := auth.ActiveAdminHasPermission(
			ctx,
			tx,
			actorID,
			permission,
		)
		if authErr != nil {
			return authErr
		}
		if !allowed {
			return problem.Forbidden(
				"PERM_FORBIDDEN",
				"administrator is no longer active or authorized",
			)
		}
		started, err := s.claimIdempotency(ctx, tx, claims.AccountType, actorID, method, path, key, requestHash)
		if err != nil {
			return err
		}
		if !started {
			return s.cachedResponse(ctx, tx, claims.AccountType, actorID, path, key, &resp)
		}
		row, err := s.repo.LockPackageByNo(ctx, tx, packageNo)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("WT_PACKAGE_NOT_FOUND", "wine ticket package not found")
		}
		if err != nil {
			return err
		}
		if row.Version != req.ExpectedVersion {
			return problem.Conflict("WT_CONCURRENT_MODIFICATION", "wine ticket package version changed")
		}
		if publish {
			if row.Status != PackageStatusDraft {
				return problem.Conflict("WT_PACKAGE_NOT_AVAILABLE", "only a draft package can be published")
			}
		}
		beforeRecord, err := s.repo.AdminPackageByNo(ctx, tx, packageNo)
		if err != nil {
			return err
		}
		before, err := adminPackageDTO(beforeRecord)
		if err != nil {
			return err
		}

		now := s.nowShanghai()
		expectedStatus := PackageStatusPublished
		values := map[string]any{
			"status":     PackageStatusUnpublished,
			"version":    gorm.Expr("version + 1"),
			"updated_at": now,
			"updated_by": actorID,
		}
		if publish {
			if err := validatePackageRow(row); err != nil {
				return err
			}
			if err := s.validateSettlementForPublish(ctx, tx, row); err != nil {
				return err
			}
			other, err := s.repo.OtherPublishedPackageForCode(ctx, tx, row.PackageCode, row.ID)
			if err == nil && other.ID != 0 {
				return problem.Conflict("WT_PACKAGE_NOT_AVAILABLE", "another version of this package code is already published")
			}
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			expectedStatus = row.Status
			values["status"] = PackageStatusPublished
			values["published_at"] = now
			values["published_by"] = actorID
		} else if row.Status != PackageStatusPublished {
			return problem.Conflict("WT_PACKAGE_NOT_AVAILABLE", "only a published package can be unpublished")
		}

		updated, err := s.repo.TransitionPackage(ctx, tx, row, expectedStatus, req.ExpectedVersion, values)
		if err != nil {
			return packageWriteDBError(err)
		}
		if !updated {
			return problem.Conflict("WT_CONCURRENT_MODIFICATION", "wine ticket package changed concurrently")
		}
		afterRecord, err := s.repo.AdminPackageByNo(ctx, tx, packageNo)
		if err != nil {
			return err
		}
		resp, err = adminPackageDTO(afterRecord)
		if err != nil {
			return err
		}
		requestID := requestctx.RequestID(ctx)
		if err := s.createPackageAudit(
			ctx,
			tx,
			actorID,
			auditAction,
			row.ID,
			&before,
			&resp,
			map[string]any{
				"permission":           permission,
				"request_id":           requestID,
				"correlation_id":       requestID,
				"idempotency_key_hash": idempotency.KeyHash(key),
				"service_instance":     nonEmptyAuditInstance(s.instance),
			},
		); err != nil {
			return err
		}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, actorID, path, key, resp)
	})
	return resp, packageWriteDBError(resultErr)
}

type normalizedPackageWrite struct {
	PackageCode             string
	IssuerMerchantID        uint64
	SettlementShopID        uint64
	SettlementShopProductID uint64
	ProductID               uint64
	RedeemCityCode          string
	Name                    string
	Subtitle                *string
	CoverImageURL           *string
	PackageType             string
	BottleQuantity          uint
	SalePriceAmount         int64
	MinPurchaseQuantity     uint
	MaxPurchaseQuantity     uint
	ValidityDays            uint
	SaleStartAt             *time.Time
	SaleEndAt               *time.Time
	PerCustomerLimit        *uint
	RefundPolicy            core.RefundPolicy
	RenewalPolicy           core.RenewalPolicy
	DeliveryPolicy          core.DeliveryPolicy
	RefundPolicyJSON        datatypes.JSON
	RenewalPolicyJSON       datatypes.JSON
	DeliveryPolicyJSON      datatypes.JSON
}

func normalizePackageWrite(req PackageWriteRequest, requireExpectedVersion bool) (normalizedPackageWrite, error) {
	if requireExpectedVersion {
		if req.ExpectedVersion == nil || *req.ExpectedVersion == 0 {
			return normalizedPackageWrite{}, problem.InvalidArgument("VALIDATION_FAILED", "expected_version must be at least 1")
		}
	} else if req.ExpectedVersion != nil {
		return normalizedPackageWrite{}, problem.InvalidArgument("VALIDATION_FAILED", "expected_version is not allowed when creating a package")
	}

	input := normalizedPackageWrite{
		PackageCode:         strings.TrimSpace(req.PackageCode),
		RedeemCityCode:      strings.TrimSpace(req.RedeemCityCode),
		Name:                strings.TrimSpace(req.Name),
		Subtitle:            cleanOptionalString(req.Subtitle),
		CoverImageURL:       cleanOptionalString(req.CoverImageURL),
		PackageType:         strings.TrimSpace(req.PackageType),
		BottleQuantity:      req.BottleQuantity,
		SalePriceAmount:     req.SalePriceAmount,
		MinPurchaseQuantity: req.MinPurchaseQuantity,
		MaxPurchaseQuantity: req.MaxPurchaseQuantity,
		ValidityDays:        req.ValidityDays,
		PerCustomerLimit:    req.PerCustomerLimit,
	}
	if len(input.PackageCode) == 0 || len(input.PackageCode) > 64 || !packageCodePattern.MatchString(input.PackageCode) {
		return normalizedPackageWrite{}, problem.InvalidArgument("VALIDATION_FAILED", "package_code must match ^[A-Za-z0-9_-]+$ and be at most 64 characters")
	}
	if len(input.RedeemCityCode) == 0 || len(input.RedeemCityCode) > 32 || !cityCodePattern.MatchString(input.RedeemCityCode) {
		return normalizedPackageWrite{}, problem.InvalidArgument("VALIDATION_FAILED", "redeem_city_code is invalid")
	}
	if utf8.RuneCountInString(input.Name) == 0 || utf8.RuneCountInString(input.Name) > 128 {
		return normalizedPackageWrite{}, problem.InvalidArgument("VALIDATION_FAILED", "name must contain 1 to 128 characters")
	}
	if input.Subtitle != nil && utf8.RuneCountInString(*input.Subtitle) > 255 {
		return normalizedPackageWrite{}, problem.InvalidArgument("VALIDATION_FAILED", "subtitle must not exceed 255 characters")
	}
	if input.CoverImageURL != nil {
		if len(*input.CoverImageURL) > 512 || !validHTTPURL(*input.CoverImageURL) {
			return normalizedPackageWrite{}, problem.InvalidArgument("VALIDATION_FAILED", "cover_image_url must be a valid HTTP(S) URI of at most 512 characters")
		}
	}
	if !validPackageType(input.PackageType) {
		return normalizedPackageWrite{}, problem.InvalidArgument("VALIDATION_FAILED", "package_type must be stockpile, corporate, or gift")
	}

	var err error
	if input.IssuerMerchantID, err = parseExternalID(req.IssuerMerchantID, "issuer_merchant_id"); err != nil {
		return normalizedPackageWrite{}, err
	}
	if input.SettlementShopID, err = parseExternalID(req.SettlementShopID, "settlement_shop_id"); err != nil {
		return normalizedPackageWrite{}, err
	}
	if input.SettlementShopProductID, err = parseExternalID(req.SettlementShopProductID, "settlement_shop_product_id"); err != nil {
		return normalizedPackageWrite{}, err
	}
	if input.ProductID, err = parseExternalID(req.ProductID, "product_id"); err != nil {
		return normalizedPackageWrite{}, err
	}
	if input.BottleQuantity < 1 || input.BottleQuantity > 10000 {
		return normalizedPackageWrite{}, problem.InvalidArgument("VALIDATION_FAILED", "bottle_quantity must be between 1 and 10000")
	}
	if input.SalePriceAmount < 1 || input.SalePriceAmount > 100000000 {
		return normalizedPackageWrite{}, problem.InvalidArgument("VALIDATION_FAILED", "sale_price_amount must be between 1 and 100000000")
	}
	if input.MinPurchaseQuantity < 1 || input.MinPurchaseQuantity > 10000 ||
		input.MaxPurchaseQuantity < input.MinPurchaseQuantity || input.MaxPurchaseQuantity > 10000 {
		return normalizedPackageWrite{}, problem.InvalidArgument("VALIDATION_FAILED", "purchase quantities must satisfy 1 <= min_purchase_quantity <= max_purchase_quantity <= 10000")
	}
	if input.ValidityDays < 1 || input.ValidityDays > 3650 {
		return normalizedPackageWrite{}, problem.InvalidArgument("VALIDATION_FAILED", "validity_days must be between 1 and 3650")
	}
	if input.PerCustomerLimit != nil && (*input.PerCustomerLimit < input.MinPurchaseQuantity || *input.PerCustomerLimit > 10000) {
		return normalizedPackageWrite{}, problem.InvalidArgument("VALIDATION_FAILED", "per_customer_limit must be at least min_purchase_quantity and at most 10000")
	}
	if input.SaleStartAt, err = parseOptionalDateTime(req.SaleStartAt, "sale_start_at"); err != nil {
		return normalizedPackageWrite{}, err
	}
	if input.SaleEndAt, err = parseOptionalDateTime(req.SaleEndAt, "sale_end_at"); err != nil {
		return normalizedPackageWrite{}, err
	}
	if input.SaleStartAt != nil && input.SaleEndAt != nil && !input.SaleStartAt.Before(*input.SaleEndAt) {
		return normalizedPackageWrite{}, problem.InvalidArgument("VALIDATION_FAILED", "sale_start_at must be before sale_end_at")
	}
	if input.RefundPolicy, err = normalizeRefundPolicy(req.RefundPolicy); err != nil {
		return normalizedPackageWrite{}, err
	}
	if input.RenewalPolicy, err = normalizeRenewalPolicy(req.RenewalPolicy); err != nil {
		return normalizedPackageWrite{}, err
	}
	if input.DeliveryPolicy, err = normalizeDeliveryPolicy(req.DeliveryPolicy); err != nil {
		return normalizedPackageWrite{}, err
	}
	input.RefundPolicyJSON = jsonData(input.RefundPolicy)
	input.RenewalPolicyJSON = jsonData(input.RenewalPolicy)
	input.DeliveryPolicyJSON = jsonData(input.DeliveryPolicy)
	return input, nil
}

func normalizeRefundPolicy(input RefundPolicyInput) (core.RefundPolicy, error) {
	if input.SchemaVersion == nil || input.Enabled == nil || input.WindowHours == nil || input.RequireNeverUsed == nil || input.FeeAmount == nil {
		return core.RefundPolicy{}, problem.InvalidArgument("VALIDATION_FAILED", "refund_policy requires schema_version, enabled, window_hours, require_never_used, and fee_amount")
	}
	policy := core.RefundPolicy{SchemaVersion: *input.SchemaVersion, Enabled: *input.Enabled, WindowHours: *input.WindowHours, RequireNeverUsed: *input.RequireNeverUsed, FeeAmount: *input.FeeAmount}
	if policy.SchemaVersion != 1 || policy.WindowHours < 0 || policy.WindowHours > 8760 || !policy.RequireNeverUsed || policy.FeeAmount != 0 {
		return core.RefundPolicy{}, problem.InvalidArgument("VALIDATION_FAILED", "refund_policy violates the V1 policy contract")
	}
	return policy, nil
}

func normalizeRenewalPolicy(input RenewalPolicyInput) (core.RenewalPolicy, error) {
	if input.SchemaVersion == nil || input.Enabled == nil || input.ExtensionDays == nil || input.MaxCount == nil || input.GraceDays == nil || input.FeeAmount == nil {
		return core.RenewalPolicy{}, problem.InvalidArgument("VALIDATION_FAILED", "renewal_policy requires all V1 fields")
	}
	policy := core.RenewalPolicy{SchemaVersion: *input.SchemaVersion, Enabled: *input.Enabled, ExtensionDays: *input.ExtensionDays, MaxCount: *input.MaxCount, GraceDays: *input.GraceDays, FeeAmount: *input.FeeAmount}
	if policy.SchemaVersion != 1 || policy.ExtensionDays < 1 || policy.ExtensionDays > 3650 || policy.MaxCount < 0 || policy.MaxCount > 100 || policy.GraceDays != 0 || policy.FeeAmount < 0 || policy.FeeAmount > 100000000 {
		return core.RenewalPolicy{}, problem.InvalidArgument("VALIDATION_FAILED", "renewal_policy violates the V1 policy contract")
	}
	return policy, nil
}

func normalizeDeliveryPolicy(input DeliveryPolicyInput) (core.DeliveryPolicy, error) {
	if input.SchemaVersion == nil || input.DeliveryFeeIncluded == nil || input.DispatchLeadMinutes == nil {
		return core.DeliveryPolicy{}, problem.InvalidArgument("VALIDATION_FAILED", "delivery_policy requires all V1 fields")
	}
	policy := core.DeliveryPolicy{SchemaVersion: *input.SchemaVersion, DeliveryFeeIncluded: *input.DeliveryFeeIncluded, DispatchLeadMinutes: *input.DispatchLeadMinutes}
	if policy.SchemaVersion != 1 || !policy.DeliveryFeeIncluded || policy.DispatchLeadMinutes < 0 || policy.DispatchLeadMinutes > 1440 {
		return core.DeliveryPolicy{}, problem.InvalidArgument("VALIDATION_FAILED", "delivery_policy violates the V1 policy contract")
	}
	return policy, nil
}

func validatePackageRow(row Package) error {
	if len(row.PackageCode) == 0 || len(row.PackageCode) > 64 || !packageCodePattern.MatchString(row.PackageCode) ||
		len(row.RedeemCityCode) == 0 || len(row.RedeemCityCode) > 32 || !cityCodePattern.MatchString(row.RedeemCityCode) ||
		utf8.RuneCountInString(strings.TrimSpace(row.Name)) == 0 || utf8.RuneCountInString(strings.TrimSpace(row.Name)) > 128 ||
		(row.Subtitle != nil && utf8.RuneCountInString(*row.Subtitle) > 255) ||
		(row.CoverImageURL != nil && (len(*row.CoverImageURL) > 512 || !validHTTPURL(*row.CoverImageURL))) ||
		row.IssuerMerchantID == 0 || row.SettlementShopID == 0 || row.SettlementShopProductID == 0 || row.ProductID == 0 ||
		!validPackageType(row.PackageType) || row.PackageVersion == 0 ||
		row.BottleQuantity < 1 || row.BottleQuantity > 10000 ||
		row.SalePriceAmount < 1 || row.SalePriceAmount > 100000000 ||
		row.MinPurchaseQuantity < 1 || row.MinPurchaseQuantity > 10000 ||
		row.MaxPurchaseQuantity < row.MinPurchaseQuantity || row.MaxPurchaseQuantity > 10000 ||
		row.ValidityDays < 1 || row.ValidityDays > 3650 ||
		(row.PerCustomerLimit != nil && (*row.PerCustomerLimit < row.MinPurchaseQuantity || *row.PerCustomerLimit > 10000)) ||
		(row.SaleStartAt != nil && row.SaleEndAt != nil && !row.SaleStartAt.Before(*row.SaleEndAt)) {
		return problem.InvalidArgument("VALIDATION_FAILED", "wine ticket package configuration cannot be published")
	}
	refund, renewal, delivery, err := policiesFromRecord(row)
	if err != nil {
		return problem.InvalidArgument("VALIDATION_FAILED", "wine ticket package policy JSON is invalid")
	}
	if _, err := normalizeRefundPolicy(refundPolicyInput(refund)); err != nil {
		return err
	}
	if _, err := normalizeRenewalPolicy(renewalPolicyInput(renewal)); err != nil {
		return err
	}
	if _, err := normalizeDeliveryPolicy(deliveryPolicyInput(delivery)); err != nil {
		return err
	}
	return nil
}

func (s *Service) validateSettlementForPublish(ctx context.Context, tx *gorm.DB, row Package) error {
	relation, err := s.repo.SettlementRelation(ctx, tx, row)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.Conflict("WT_PACKAGE_NOT_AVAILABLE", "settlement merchant, shop, shop product, or product does not exist")
	}
	if err != nil {
		return err
	}
	if relation.MerchantID != row.IssuerMerchantID ||
		relation.ShopID != row.SettlementShopID ||
		relation.ShopMerchantID != row.IssuerMerchantID ||
		relation.ShopProductID != row.SettlementShopProductID ||
		relation.ShopProductMerchant != row.IssuerMerchantID ||
		relation.ShopProductShop != row.SettlementShopID ||
		relation.ShopProductProduct != row.ProductID ||
		relation.ProductID != row.ProductID {
		return problem.Conflict("WT_PACKAGE_NOT_AVAILABLE", "settlement merchant, shop, shop product, and product relationship is invalid")
	}
	if relation.MerchantStatus != "active" || relation.MerchantReviewStatus != "approved" ||
		relation.ShopStatus != "active" || relation.ShopBusinessStatus != "open" ||
		relation.ShopProductStatus != "on_sale" || relation.ProductStatus != "on_sale" ||
		relation.ProductCategoryStatus != "active" ||
		!relation.ProductAgeRestricted {
		return problem.Conflict("WT_PACKAGE_NOT_AVAILABLE", "settlement configuration is not eligible for wine ticket publication")
	}
	return nil
}

func (s *Service) createPackageAudit(
	ctx context.Context,
	tx *gorm.DB,
	actorID uint64,
	action string,
	resourceID uint64,
	before, after *AdminPackageDTO,
	metadata map[string]any,
) error {
	var beforeData, afterData datatypes.JSON
	var beforeStatus, afterStatus *string
	var version uint64
	if before != nil {
		beforeData = jsonData(before)
		value := before.Status
		beforeStatus = &value
	}
	if after != nil {
		afterData = packageAuditData(after, metadata)
		value := after.Status
		afterStatus = &value
		version = uint64(after.Version)
	}
	values := map[string]any{
		"id":            s.ids.Next(),
		"actor_type":    "admin",
		"actor_id":      actorID,
		"action":        action,
		"resource_type": "wine_ticket_package",
		"resource_id":   resourceID,
		"before_data":   beforeData,
		"after_data":    afterData,
		"result":        "success",
		"before_status": beforeStatus,
		"after_status":  afterStatus,
		"version":       version,
		"request_id":    requestctx.RequestIDPtr(ctx),
		"ip_hash":       requestctx.IPHashPtr(ctx),
		"user_agent":    requestctx.UserAgentPtr(ctx),
	}
	if accountID := requestctx.AccountID(ctx); accountID != 0 {
		values["account_id"] = accountID
	}
	return s.repo.CreateAudit(ctx, tx, values)
}

func packageAuditData(after *AdminPackageDTO, metadata map[string]any) datatypes.JSON {
	if after == nil {
		return nil
	}
	if len(metadata) == 0 {
		return jsonData(after)
	}
	payload := map[string]any{}
	raw := jsonData(after)
	if err := json.Unmarshal(raw, &payload); err != nil {
		payload = map[string]any{}
	}
	for key, value := range metadata {
		payload[key] = value
	}
	return jsonData(payload)
}

func nonEmptyAuditInstance(instance string) string {
	if instance = strings.TrimSpace(instance); instance != "" {
		return instance
	}
	return "unknown"
}

func (c *Service) claimIdempotency(ctx context.Context, tx *gorm.DB, actorType string, actorID uint64, method, path, key, requestHash string) (bool, error) {
	return c.claimIdempotencyWithID(
		ctx,
		tx,
		c.ids.Next(),
		actorType,
		actorID,
		method,
		path,
		key,
		requestHash,
	)
}

func (c *Service) claimIdempotencyWithID(ctx context.Context, tx *gorm.DB, claimID uint64, actorType string, actorID uint64, method, path, key, requestHash string) (bool, error) {
	return c.idStore.StartAt(
		ctx,
		tx,
		claimID,
		actorType,
		actorID,
		method,
		path,
		key,
		requestHash,
		c.now(),
	)
}

func (c *Service) cachedResponse(ctx context.Context, tx *gorm.DB, actorType string, actorID uint64, path, key string, out any) error {
	found, err := c.idStore.CachedResponse(ctx, tx, actorType, actorID, path, key, out)
	if err != nil {
		return err
	}
	if !found {
		return problem.Conflict("IDEMPOTENCY_IN_PROGRESS", "request with the same idempotency key is still processing")
	}
	return nil
}

func (c *Service) nowShanghai() time.Time {
	return c.now().In(shanghaiLocation).Truncate(time.Millisecond)
}

func adminIDWithPermission(claims *auth.Claims, permission string) (uint64, error) {
	if claims == nil || claims.AccountType != "admin" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "admin account required")
	}
	actorID, err := strconv.ParseUint(claims.AdminUserID, 10, 64)
	if err != nil || actorID == 0 {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "invalid admin identity")
	}
	for _, granted := range claims.Permissions {
		if granted == permission {
			return actorID, nil
		}
	}
	return 0, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
}

func validatePackageNo(packageNo string) error {
	if len(packageNo) < 1 || len(packageNo) > 64 || !businessNoPattern.MatchString(packageNo) {
		return problem.InvalidArgument("VALIDATION_FAILED", "invalid package_no")
	}
	return nil
}

func validPackageType(value string) bool {
	return value == PackageTypeStockpile || value == PackageTypeCorporate || value == PackageTypeGift
}

func parseExternalID(raw string, field string) (uint64, error) {
	if len(raw) < 1 || len(raw) > 20 || raw[0] == '0' {
		return 0, problem.InvalidArgument("VALIDATION_FAILED", field+" must be a positive decimal string")
	}
	for _, char := range raw {
		if char < '0' || char > '9' {
			return 0, problem.InvalidArgument("VALIDATION_FAILED", field+" must be a positive decimal string")
		}
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || value == 0 {
		return 0, problem.InvalidArgument("VALIDATION_FAILED", field+" must be a positive decimal string")
	}
	return value, nil
}

func parseOptionalDateTime(raw *string, field string) (*time.Time, error) {
	if raw == nil {
		return nil, nil
	}
	value := strings.TrimSpace(*raw)
	if value == "" {
		return nil, problem.InvalidArgument("VALIDATION_FAILED", field+" must be RFC3339 date-time or null")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, problem.InvalidArgument("VALIDATION_FAILED", field+" must be RFC3339 date-time")
	}
	parsed = parsed.In(shanghaiLocation).Truncate(time.Millisecond)
	return &parsed, nil
}

func validHTTPURL(raw string) bool {
	parsed, err := url.ParseRequestURI(raw)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != ""
}

func cleanOptionalString(value *string) *string {
	if value == nil {
		return nil
	}
	cleaned := strings.TrimSpace(*value)
	return &cleaned
}

func jsonData(value any) datatypes.JSON {
	payload, _ := json.Marshal(value)
	return datatypes.JSON(payload)
}

func packageWriteDBError(err error) error {
	if err == nil {
		return nil
	}
	var mysqlError *mysqlerr.MySQLError
	if errors.As(err, &mysqlError) {
		switch mysqlError.Number {
		case 1062, 1205, 1213:
			return problem.Conflict(
				"WT_CONCURRENT_MODIFICATION",
				"wine ticket package changed concurrently",
			)
		}
	}
	if strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
		return problem.Conflict("WT_CONCURRENT_MODIFICATION", "wine ticket package changed concurrently")
	}
	return err
}

func refundPolicyInput(policy core.RefundPolicy) RefundPolicyInput {
	return RefundPolicyInput{
		SchemaVersion: intPtr(policy.SchemaVersion), Enabled: boolPtr(policy.Enabled), WindowHours: intPtr(policy.WindowHours),
		RequireNeverUsed: boolPtr(policy.RequireNeverUsed), FeeAmount: int64Ptr(policy.FeeAmount),
	}
}

func renewalPolicyInput(policy core.RenewalPolicy) RenewalPolicyInput {
	return RenewalPolicyInput{
		SchemaVersion: intPtr(policy.SchemaVersion), Enabled: boolPtr(policy.Enabled), ExtensionDays: intPtr(policy.ExtensionDays),
		MaxCount: intPtr(policy.MaxCount), GraceDays: intPtr(policy.GraceDays), FeeAmount: int64Ptr(policy.FeeAmount),
	}
}

func deliveryPolicyInput(policy core.DeliveryPolicy) DeliveryPolicyInput {
	return DeliveryPolicyInput{
		SchemaVersion: intPtr(policy.SchemaVersion), DeliveryFeeIncluded: boolPtr(policy.DeliveryFeeIncluded),
		DispatchLeadMinutes: intPtr(policy.DispatchLeadMinutes),
	}
}

func intPtr(value int) *int              { return &value }
func int64Ptr(value int64) *int64        { return &value }
func boolPtr(value bool) *bool           { return &value }
func uint64Ptr(value uint64) *uint64     { return &value }
func timePtr(value time.Time) *time.Time { return &value }
