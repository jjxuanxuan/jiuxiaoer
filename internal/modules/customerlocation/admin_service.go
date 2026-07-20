package customerlocation

import (
	"context"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
)

var policyCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

func (s *Service) ListAdminCities(ctx context.Context, claims *auth.Claims, query pagination.Query) ([]ServiceCityAdminDTO, string, error) {
	if _, err := requireAdminPermission(claims, "service_city:list"); err != nil {
		return nil, "", err
	}
	rows, err := s.repo.AdminCities(ctx, query)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		next = pagination.NextPageToken(query)
		rows = rows[:query.PageSize]
	}
	items := make([]ServiceCityAdminDTO, 0, len(rows))
	for _, row := range rows {
		item, itemErr := s.cityAdminDTO(ctx, s.repo.DB(), row)
		if itemErr != nil {
			return nil, "", itemErr
		}
		items = append(items, item)
	}
	return items, next, nil
}

func (s *Service) CreateAdminCity(ctx context.Context, claims *auth.Claims, method, path, key string, req ServiceCityWriteRequest) (ServiceCityAdminDTO, error) {
	adminID, err := requireAdminPermission(claims, "service_city:create")
	if err != nil {
		return ServiceCityAdminDTO{}, err
	}
	defaultShopID, err := validateCityWrite(req, false)
	if err != nil {
		return ServiceCityAdminDTO{}, err
	}
	row := ServiceCity{ID: s.idGen.Next(), CityCode: req.CityCode, ProvinceCode: req.ProvinceCode, Name: strings.TrimSpace(req.Name), Pinyin: strings.ToLower(strings.TrimSpace(req.Pinyin)), Status: "draft", SortOrder: req.SortOrder, DefaultBrowseShopID: defaultShopID, Version: 1, CreatedBy: adminID, UpdatedBy: adminID}
	var out ServiceCityAdminDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, claimErr := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, adminID, method, path, key, idempotency.RequestHash(req))
		if claimErr != nil {
			return claimErr
		}
		if !started {
			return replayAdmin(ctx, s.idStore, tx, claims.AccountType, adminID, path, key, &out)
		}
		if err := s.repo.CreateCity(ctx, tx, &row); err != nil {
			return err
		}
		if err := s.repo.ReplaceADCodes(ctx, tx, row.ID, adminID, s.idGen.Next, req.CoveredADCodes); err != nil {
			return err
		}
		if err := s.repo.Audit(ctx, tx, s.idGen.Next(), adminID, "service_city.create", "service_city", row.ID, nil, row, requestctx.RequestIDPtr(ctx), s.auditIP(ctx), requestctx.UserAgentPtr(ctx)); err != nil {
			return err
		}
		var dtoErr error
		out, dtoErr = s.cityAdminDTO(ctx, tx, row)
		if dtoErr != nil {
			return dtoErr
		}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, adminID, path, key, out)
	})
	return out, err
}

func (s *Service) UpdateAdminCity(ctx context.Context, claims *auth.Claims, method, path, key, idRaw string, req ServiceCityWriteRequest) (ServiceCityAdminDTO, error) {
	adminID, err := requireAdminPermission(claims, "service_city:update")
	if err != nil {
		return ServiceCityAdminDTO{}, err
	}
	id, err := parsePositiveID(idRaw, "SERVICE_CITY_NOT_FOUND")
	if err != nil {
		return ServiceCityAdminDTO{}, err
	}
	if req.ExpectedVersion == 0 {
		return ServiceCityAdminDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "expected_version is required")
	}
	defaultShopID, err := validateCityWrite(req, true)
	if err != nil {
		return ServiceCityAdminDTO{}, err
	}
	var out ServiceCityAdminDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, claimErr := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, adminID, method, path, key, idempotency.RequestHash(req))
		if claimErr != nil {
			return claimErr
		}
		if !started {
			return replayAdmin(ctx, s.idStore, tx, claims.AccountType, adminID, path, key, &out)
		}
		before, lockErr := s.repo.LockCity(ctx, tx, id)
		if isNotFound(lockErr) {
			return problem.NotFound("SERVICE_CITY_NOT_FOUND", "service city not found")
		}
		if lockErr != nil {
			return lockErr
		}
		if before.Status == "published" {
			return problem.Conflict("SERVICE_CITY_PUBLISHED_IMMUTABLE", "disable the city before editing it")
		}
		ok, updateErr := s.repo.UpdateCity(ctx, tx, id, req.ExpectedVersion, map[string]any{"city_code": req.CityCode, "province_code": req.ProvinceCode, "name": strings.TrimSpace(req.Name), "pinyin": strings.ToLower(strings.TrimSpace(req.Pinyin)), "sort_order": req.SortOrder, "default_browse_shop_id": defaultShopID, "updated_by": adminID})
		if updateErr != nil {
			return updateErr
		}
		if !ok {
			return problem.Conflict("SERVICE_CITY_VERSION_CONFLICT", "service city version changed")
		}
		if err := s.repo.ReplaceADCodes(ctx, tx, id, adminID, s.idGen.Next, req.CoveredADCodes); err != nil {
			return err
		}
		after, lockErr := s.repo.LockCity(ctx, tx, id)
		if lockErr != nil {
			return lockErr
		}
		if err := s.repo.Audit(ctx, tx, s.idGen.Next(), adminID, "service_city.update", "service_city", id, before, after, requestctx.RequestIDPtr(ctx), s.auditIP(ctx), requestctx.UserAgentPtr(ctx)); err != nil {
			return err
		}
		out, lockErr = s.cityAdminDTO(ctx, tx, after)
		if lockErr != nil {
			return lockErr
		}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, adminID, path, key, out)
	})
	if err == nil {
		s.bump("lbs:city-version")
	}
	return out, err
}

func (s *Service) SetAdminCityStatus(ctx context.Context, claims *auth.Claims, method, path, key, idRaw string, req ResourceStatusRequest) (ServiceCityAdminDTO, error) {
	adminID, err := requireAdminPermission(claims, "service_city:publish")
	if err != nil {
		return ServiceCityAdminDTO{}, err
	}
	id, err := parsePositiveID(idRaw, "SERVICE_CITY_NOT_FOUND")
	if err != nil {
		return ServiceCityAdminDTO{}, err
	}
	if req.Status != "published" && req.Status != "disabled" {
		return ServiceCityAdminDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "status must be published or disabled")
	}
	var out ServiceCityAdminDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, claimErr := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, adminID, method, path, key, idempotency.RequestHash(req))
		if claimErr != nil {
			return claimErr
		}
		if !started {
			return replayAdmin(ctx, s.idStore, tx, claims.AccountType, adminID, path, key, &out)
		}
		before, lockErr := s.repo.LockCity(ctx, tx, id)
		if isNotFound(lockErr) {
			return problem.NotFound("SERVICE_CITY_NOT_FOUND", "service city not found")
		}
		if lockErr != nil {
			return lockErr
		}
		if req.Status == "published" {
			ok, checkErr := s.repo.CityPublishable(ctx, tx, before)
			if checkErr != nil {
				return checkErr
			}
			if !ok {
				return problem.Conflict("SERVICE_CITY_NOT_PUBLISHABLE", "published city requires adcode mappings and an active service shop")
			}
		}
		values := map[string]any{"status": req.Status, "updated_by": adminID}
		if req.Status == "published" {
			values["published_at"] = s.now().UTC()
		}
		ok, updateErr := s.repo.UpdateCity(ctx, tx, id, req.ExpectedVersion, values)
		if updateErr != nil {
			return updateErr
		}
		if !ok {
			return problem.Conflict("SERVICE_CITY_VERSION_CONFLICT", "service city version changed")
		}
		after, lockErr := s.repo.LockCity(ctx, tx, id)
		if lockErr != nil {
			return lockErr
		}
		if err := s.repo.Audit(ctx, tx, s.idGen.Next(), adminID, "service_city.status", "service_city", id, before, after, requestctx.RequestIDPtr(ctx), s.auditIP(ctx), requestctx.UserAgentPtr(ctx)); err != nil {
			return err
		}
		out, lockErr = s.cityAdminDTO(ctx, tx, after)
		if lockErr != nil {
			return lockErr
		}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, adminID, path, key, out)
	})
	if err == nil {
		s.bump("lbs:city-version")
	}
	return out, err
}

func (s *Service) ListAdminPolicies(ctx context.Context, claims *auth.Claims, query pagination.Query) ([]PromisePolicyAdminDTO, string, error) {
	if _, err := requireAdminPermission(claims, "promise_policy:list"); err != nil {
		return nil, "", err
	}
	rows, err := s.repo.AdminPolicies(ctx, query)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		next = pagination.NextPageToken(query)
		rows = rows[:query.PageSize]
	}
	items := make([]PromisePolicyAdminDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, policyAdminDTO(row))
	}
	return items, next, nil
}

func (s *Service) CreateAdminPolicy(ctx context.Context, claims *auth.Claims, method, path, key string, req PromisePolicyWriteRequest) (PromisePolicyAdminDTO, error) {
	adminID, err := requireAdminPermission(claims, "promise_policy:create")
	if err != nil {
		return PromisePolicyAdminDTO{}, err
	}
	from, to, err := validatePolicyWrite(req)
	if err != nil {
		return PromisePolicyAdminDTO{}, err
	}
	row := DeliveryPromisePolicy{ID: s.idGen.Next(), PolicyCode: req.PolicyCode, Version: req.Version, Title: strings.TrimSpace(req.Title), Summary: strings.TrimSpace(req.Summary), TermsURL: optionalText(req.TermsURL), Status: "draft", EffectiveFrom: from, EffectiveTo: to, CreatedBy: adminID, UpdatedBy: adminID}
	var out PromisePolicyAdminDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, claimErr := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, adminID, method, path, key, idempotency.RequestHash(req))
		if claimErr != nil {
			return claimErr
		}
		if !started {
			return replayAdmin(ctx, s.idStore, tx, claims.AccountType, adminID, path, key, &out)
		}
		if err := s.repo.CreatePolicy(ctx, tx, &row); err != nil {
			return err
		}
		if err := s.repo.Audit(ctx, tx, s.idGen.Next(), adminID, "promise_policy.create", "delivery_promise_policy", row.ID, nil, row, requestctx.RequestIDPtr(ctx), s.auditIP(ctx), requestctx.UserAgentPtr(ctx)); err != nil {
			return err
		}
		out = policyAdminDTO(row)
		return s.idStore.Succeed(ctx, tx, claims.AccountType, adminID, path, key, out)
	})
	return out, err
}

func (s *Service) UpdateAdminPolicy(ctx context.Context, claims *auth.Claims, method, path, key, idRaw string, req PromisePolicyWriteRequest) (PromisePolicyAdminDTO, error) {
	adminID, err := requireAdminPermission(claims, "promise_policy:create")
	if err != nil {
		return PromisePolicyAdminDTO{}, err
	}
	id, err := parsePositiveID(idRaw, "PROMISE_POLICY_NOT_FOUND")
	if err != nil {
		return PromisePolicyAdminDTO{}, err
	}
	from, to, err := validatePolicyWrite(req)
	if err != nil {
		return PromisePolicyAdminDTO{}, err
	}
	var out PromisePolicyAdminDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, claimErr := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, adminID, method, path, key, idempotency.RequestHash(req))
		if claimErr != nil {
			return claimErr
		}
		if !started {
			return replayAdmin(ctx, s.idStore, tx, claims.AccountType, adminID, path, key, &out)
		}
		before, lockErr := s.repo.LockPolicy(ctx, tx, id)
		if isNotFound(lockErr) {
			return problem.NotFound("PROMISE_POLICY_NOT_FOUND", "promise policy not found")
		}
		if lockErr != nil {
			return lockErr
		}
		if before.Status != "draft" {
			return problem.Conflict("PROMISE_POLICY_IMMUTABLE", "published or retired policy is immutable")
		}
		if req.ExpectedVersion != before.Version {
			return problem.Conflict("PROMISE_POLICY_VERSION_CONFLICT", "promise policy version changed")
		}
		if req.PolicyCode != before.PolicyCode || req.Version != before.Version {
			return problem.Conflict("PROMISE_POLICY_IDENTITY_IMMUTABLE", "policy code and version cannot be changed")
		}
		if err := s.repo.UpdatePolicy(ctx, tx, id, map[string]any{"title": strings.TrimSpace(req.Title), "summary": strings.TrimSpace(req.Summary), "terms_url": optionalText(req.TermsURL), "effective_from": from, "effective_to": to, "updated_by": adminID}); err != nil {
			return err
		}
		after, lockErr := s.repo.LockPolicy(ctx, tx, id)
		if lockErr != nil {
			return lockErr
		}
		if err := s.repo.Audit(ctx, tx, s.idGen.Next(), adminID, "promise_policy.update", "delivery_promise_policy", id, before, after, requestctx.RequestIDPtr(ctx), s.auditIP(ctx), requestctx.UserAgentPtr(ctx)); err != nil {
			return err
		}
		out = policyAdminDTO(after)
		return s.idStore.Succeed(ctx, tx, claims.AccountType, adminID, path, key, out)
	})
	return out, err
}

func (s *Service) SetAdminPolicyStatus(ctx context.Context, claims *auth.Claims, method, path, key, idRaw string, req ResourceStatusRequest) (PromisePolicyAdminDTO, error) {
	adminID, err := requireAdminPermission(claims, "promise_policy:publish")
	if err != nil {
		return PromisePolicyAdminDTO{}, err
	}
	id, err := parsePositiveID(idRaw, "PROMISE_POLICY_NOT_FOUND")
	if err != nil {
		return PromisePolicyAdminDTO{}, err
	}
	if req.Status != "published" && req.Status != "retired" {
		return PromisePolicyAdminDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "status must be published or retired")
	}
	var out PromisePolicyAdminDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, claimErr := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, adminID, method, path, key, idempotency.RequestHash(req))
		if claimErr != nil {
			return claimErr
		}
		if !started {
			return replayAdmin(ctx, s.idStore, tx, claims.AccountType, adminID, path, key, &out)
		}
		before, lockErr := s.repo.LockPolicy(ctx, tx, id)
		if isNotFound(lockErr) {
			return problem.NotFound("PROMISE_POLICY_NOT_FOUND", "promise policy not found")
		}
		if lockErr != nil {
			return lockErr
		}
		if before.Version != req.ExpectedVersion {
			return problem.Conflict("PROMISE_POLICY_VERSION_CONFLICT", "promise policy version changed")
		}
		if req.Status == "published" {
			if before.Status != "draft" {
				return problem.Conflict("PROMISE_POLICY_INVALID_STATUS", "only draft policy can be published")
			}
			if err := s.repo.RetirePublishedPolicy(ctx, tx, before.PolicyCode, id, adminID); err != nil {
				return err
			}
			if err := s.repo.UpdatePolicy(ctx, tx, id, map[string]any{"status": "published", "published_at": s.now().UTC(), "published_by": adminID, "updated_by": adminID}); err != nil {
				return err
			}
			if err := s.repo.RebindShopPolicy(ctx, tx, before.PolicyCode, before.Version); err != nil {
				return err
			}
		} else {
			if before.Status != "published" {
				return problem.Conflict("PROMISE_POLICY_INVALID_STATUS", "only published policy can be retired")
			}
			references, referenceErr := s.repo.PolicyShopReferences(ctx, tx, before.PolicyCode, before.Version)
			if referenceErr != nil {
				return referenceErr
			}
			if references > 0 {
				return problem.Conflict("PROMISE_POLICY_IN_USE", "published policy is still referenced by service shops")
			}
			if err := s.repo.UpdatePolicy(ctx, tx, id, map[string]any{"status": "retired", "updated_by": adminID}); err != nil {
				return err
			}
		}
		after, lockErr := s.repo.LockPolicy(ctx, tx, id)
		if lockErr != nil {
			return lockErr
		}
		if err := s.repo.Audit(ctx, tx, s.idGen.Next(), adminID, "promise_policy.status", "delivery_promise_policy", id, before, after, requestctx.RequestIDPtr(ctx), s.auditIP(ctx), requestctx.UserAgentPtr(ctx)); err != nil {
			return err
		}
		out = policyAdminDTO(after)
		return s.idStore.Succeed(ctx, tx, claims.AccountType, adminID, path, key, out)
	})
	if err == nil {
		s.bump("lbs:policy-version")
	}
	return out, err
}

func requireAdminPermission(claims *auth.Claims, permission string) (uint64, error) {
	if claims == nil || claims.AccountType != "admin" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "admin account required")
	}
	allowed := claims.RoleCode == "super_admin"
	for _, value := range claims.Permissions {
		if value == permission || value == "*" {
			allowed = true
			break
		}
	}
	if !allowed {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
	}
	id, err := strconv.ParseUint(claims.AdminUserID, 10, 64)
	if err != nil || id == 0 {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "invalid admin identity")
	}
	return id, nil
}

func validateCityWrite(req ServiceCityWriteRequest, update bool) (*uint64, error) {
	if !locationCityCodePattern.MatchString(req.CityCode) || !locationCityCodePattern.MatchString(req.ProvinceCode) || strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Pinyin) == "" {
		return nil, problem.InvalidArgument("VALIDATION_FAILED", "invalid service city fields")
	}
	if update && req.ExpectedVersion == 0 {
		return nil, problem.InvalidArgument("VALIDATION_FAILED", "expected_version is required")
	}
	seen := map[string]bool{}
	for _, value := range req.CoveredADCodes {
		if !locationCityCodePattern.MatchString(value.ADCode) || seen[value.ADCode] || strings.TrimSpace(value.StandardName) == "" || (value.Level != "city" && value.Level != "district" && value.Level != "county") {
			return nil, problem.InvalidArgument("VALIDATION_FAILED", "covered_adcodes must be valid and unique")
		}
		seen[value.ADCode] = true
	}
	if req.DefaultBrowseShopID == "" {
		return nil, nil
	}
	id, err := strconv.ParseUint(req.DefaultBrowseShopID, 10, 64)
	if err != nil || id == 0 {
		return nil, problem.InvalidArgument("VALIDATION_FAILED", "invalid default_browse_shop_id")
	}
	return &id, nil
}

func validatePolicyWrite(req PromisePolicyWriteRequest) (*time.Time, *time.Time, error) {
	if !policyCodePattern.MatchString(req.PolicyCode) || strings.TrimSpace(req.Title) == "" || strings.TrimSpace(req.Summary) == "" {
		return nil, nil, problem.InvalidArgument("VALIDATION_FAILED", "invalid promise policy fields")
	}
	if req.TermsURL != "" {
		parsed, err := url.Parse(req.TermsURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return nil, nil, problem.InvalidArgument("VALIDATION_FAILED", "terms_url must be HTTPS")
		}
	}
	parse := func(raw string) (*time.Time, error) {
		if raw == "" {
			return nil, nil
		}
		value, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, err
		}
		value = value.UTC()
		return &value, nil
	}
	from, err := parse(req.EffectiveFrom)
	if err != nil {
		return nil, nil, problem.InvalidArgument("VALIDATION_FAILED", "invalid effective_from")
	}
	to, err := parse(req.EffectiveTo)
	if err != nil {
		return nil, nil, problem.InvalidArgument("VALIDATION_FAILED", "invalid effective_to")
	}
	if from != nil && to != nil && !to.After(*from) {
		return nil, nil, problem.InvalidArgument("VALIDATION_FAILED", "effective_to must be after effective_from")
	}
	return from, to, nil
}

func (s *Service) cityAdminDTO(ctx context.Context, db *gorm.DB, row ServiceCity) (ServiceCityAdminDTO, error) {
	adcodes, err := s.repo.CityADCodes(ctx, db, row.ID)
	if err != nil {
		return ServiceCityAdminDTO{}, err
	}
	items := make([]CoveredADCodeRequest, 0, len(adcodes))
	for _, value := range adcodes {
		items = append(items, CoveredADCodeRequest{ADCode: value.ADCode, StandardName: value.StandardName, Level: value.Level})
	}
	out := ServiceCityAdminDTO{ID: strconv.FormatUint(row.ID, 10), CityCode: row.CityCode, ProvinceCode: row.ProvinceCode, Name: row.Name, Pinyin: row.Pinyin, Status: row.Status, SortOrder: row.SortOrder, Version: row.Version, CoveredADCodes: items}
	if row.DefaultBrowseShopID != nil {
		out.DefaultBrowseShopID = strconv.FormatUint(*row.DefaultBrowseShopID, 10)
	}
	return out, nil
}

func policyAdminDTO(row DeliveryPromisePolicy) PromisePolicyAdminDTO {
	out := PromisePolicyAdminDTO{ID: strconv.FormatUint(row.ID, 10), PolicyCode: row.PolicyCode, Version: row.Version, Title: row.Title, Summary: row.Summary, TermsURL: stringValue(row.TermsURL), Status: row.Status}
	if row.EffectiveFrom != nil {
		out.EffectiveFrom = row.EffectiveFrom.UTC().Format(time.RFC3339)
	}
	if row.EffectiveTo != nil {
		out.EffectiveTo = row.EffectiveTo.UTC().Format(time.RFC3339)
	}
	return out
}

func parsePositiveID(raw, code string) (uint64, error) {
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, problem.NotFound(code, "resource not found")
	}
	return id, nil
}

func optionalText(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func replayAdmin(ctx context.Context, store *idempotency.Store, tx *gorm.DB, actorType string, actorID uint64, path, key string, out any) error {
	cached, err := store.CachedResponse(ctx, tx, actorType, actorID, path, key, out)
	if err != nil {
		return err
	}
	if !cached {
		return problem.Conflict("IDEMPOTENCY_IN_PROGRESS", "request is still processing")
	}
	return nil
}

func (s *Service) bump(key string) {
	if s.redis != nil {
		_ = s.redis.Incr(context.Background(), key).Err()
	}
}

func (s *Service) auditIP(ctx context.Context) *string {
	value := strings.TrimSpace(requestctx.IP(ctx))
	if value == "" {
		return nil
	}
	hash := s.digest(value)
	return &hash
}
