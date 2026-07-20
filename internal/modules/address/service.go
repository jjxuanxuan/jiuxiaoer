package address

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/customerlocation"
	"jiuxiaoer-admin/backend-go/internal/pkg/coordinate"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

const maxAddressesPerCustomer = 20

var (
	addressCityCodePattern = regexp.MustCompile(`^[0-9]{6}$`)
	addressPhonePattern    = regexp.MustCompile(`^1[3-9][0-9]{9}$`)
)

type Service struct {
	repo      *Repository
	idStore   *idempotency.Store
	idGen     *snowflake.Generator
	lbsMode   string
	locations *customerlocation.Service
}

func (s *Service) WithLocationVerification(mode string, locations *customerlocation.Service) *Service {
	s.lbsMode, s.locations = mode, locations
	return s
}

// NewService 创建并初始化服务。
func NewService(db *gorm.DB, idGen *snowflake.Generator) *Service {
	return &Service{repo: NewRepository(db), idStore: idempotency.NewStore(db), idGen: idGen, lbsMode: "off"}
}

// List 查询地址 DTO列表列表。
func (s *Service) List(ctx context.Context, claims *auth.Claims) ([]AddressDTO, error) {
	customerID, err := customerIDFromClaims(claims)
	if err != nil {
		return nil, err
	}
	rows, err := s.repo.List(ctx, s.repo.DB(), customerID)
	if err != nil {
		return nil, err
	}
	return addressDTOs(rows), nil
}

// Create 创建地址DTO。
func (s *Service) Create(ctx context.Context, claims *auth.Claims, method, path, key string, req AddressUpsertReq) (AddressDTO, error) {
	customerID, err := customerIDFromClaims(claims)
	if err != nil {
		return AddressDTO{}, err
	}
	if err := normalizeCreateCoordinates(&req); err != nil {
		return AddressDTO{}, err
	}
	if err := validateAddress(req.CityCode, req.DistrictCode, req.ContactPhone, req.Latitude, req.Longitude); err != nil {
		return AddressDTO{}, err
	}
	verified, err := s.verifyCreate(ctx, &req)
	if err != nil {
		return AddressDTO{}, err
	}

	var result AddressDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, customerID, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		if !started {
			cached, err := s.idStore.CachedResponse(ctx, tx, claims.AccountType, customerID, path, key, &result)
			if err != nil {
				return err
			}
			if !cached {
				return problem.Conflict("IDEMPOTENCY_IN_PROGRESS", "request is still processing")
			}
			return nil
		}
		if err := s.repo.LockCustomer(ctx, tx, customerID); err != nil {
			return err
		}
		count, err := s.repo.Count(ctx, tx, customerID)
		if err != nil {
			return err
		}
		if count >= maxAddressesPerCustomer {
			return problem.Conflict("ADDRESS_LIMIT_REACHED", "a customer can have at most 20 active addresses")
		}
		isDefault := req.IsDefault || count == 0
		if isDefault {
			if err := s.repo.ClearDefault(ctx, tx, customerID, 0); err != nil {
				return err
			}
		}
		row := customerAddressFromCreate(s.idGen.Next(), customerID, req, isDefault, verified)
		if err := s.repo.Create(ctx, tx, &row); err != nil {
			return err
		}
		result = addressDTO(row)
		applyVerificationDTO(&result, verified)
		return s.idStore.Succeed(ctx, tx, claims.AccountType, customerID, path, key, result)
	})
	return result, err
}

// Update 更新地址DTO。
func (s *Service) Update(ctx context.Context, claims *auth.Claims, method, path, key, id string, req AddressUpdateReq) (AddressDTO, error) {
	customerID, addressID, err := s.actorAndAddress(claims, id)
	if err != nil {
		return AddressDTO{}, err
	}
	if err := normalizeUpdateCoordinates(&req); err != nil {
		return AddressDTO{}, err
	}
	if err := validateAddress(req.CityCode, req.DistrictCode, req.ContactPhone, req.Latitude, req.Longitude); err != nil {
		return AddressDTO{}, err
	}
	verified, err := s.verifyUpdate(ctx, &req)
	if err != nil {
		return AddressDTO{}, err
	}
	var result AddressDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		request := struct {
			AddressID uint64           `json:"address_id"`
			Body      AddressUpdateReq `json:"body"`
		}{addressID, req}
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, customerID, method, path, key, idempotency.RequestHash(request))
		if err != nil {
			return err
		}
		if !started {
			return cachedAddress(ctx, s.idStore, tx, claims.AccountType, customerID, path, key, &result)
		}
		if err := s.repo.LockCustomer(ctx, tx, customerID); err != nil {
			return err
		}
		if _, err := s.repo.GetOwned(ctx, tx, customerID, addressID, true, false); errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("ADDRESS_NOT_FOUND", "address not found")
		} else if err != nil {
			return err
		}
		updated, err := s.repo.Update(ctx, tx, customerID, addressID, req.Version, addressValues(req, verified))
		if err != nil {
			return err
		}
		if !updated {
			return problem.Conflict("ADDRESS_VERSION_CONFLICT", "address was changed; refresh and retry")
		}
		row, err := s.repo.GetOwned(ctx, tx, customerID, addressID, false, false)
		if err != nil {
			return err
		}
		result = addressDTO(row)
		applyVerificationDTO(&result, verified)
		return s.idStore.Succeed(ctx, tx, claims.AccountType, customerID, path, key, result)
	})
	return result, err
}

func (s *Service) verifyCreate(ctx context.Context, req *AddressUpsertReq) (*customerlocation.VerifiedAddress, error) {
	return s.verifyLocation(ctx, req.LocationSource, req.POIID, req.CityCode, req.DistrictCode, req.Latitude, req.Longitude, func(value customerlocation.VerifiedAddress) {
		req.Province, req.City, req.CityCode = value.Location.Province, value.Location.City, value.Location.CityCode
		req.District, req.DistrictCode, req.FormattedAddress = value.Location.District, value.Location.DistrictCode, value.Location.FormattedAddress
	})
}

func (s *Service) verifyUpdate(ctx context.Context, req *AddressUpdateReq) (*customerlocation.VerifiedAddress, error) {
	return s.verifyLocation(ctx, req.LocationSource, req.POIID, req.CityCode, req.DistrictCode, req.Latitude, req.Longitude, func(value customerlocation.VerifiedAddress) {
		req.Province, req.City, req.CityCode = value.Location.Province, value.Location.City, value.Location.CityCode
		req.District, req.DistrictCode, req.FormattedAddress = value.Location.District, value.Location.DistrictCode, value.Location.FormattedAddress
	})
}

func (s *Service) verifyLocation(ctx context.Context, source, poiID, cityCode, districtCode string, latitude, longitude *float64, apply func(customerlocation.VerifiedAddress)) (*customerlocation.VerifiedAddress, error) {
	if source == "" && s.lbsMode == "off" {
		return nil, nil
	}
	if source != "amap_poi" && source != "map_pin" && source != "manual_import" {
		return nil, problem.InvalidArgument("VALIDATION_FAILED", "location_source must be amap_poi, map_pin, or manual_import")
	}
	if source == "amap_poi" {
		if strings.TrimSpace(poiID) == "" || len(poiID) > 64 {
			return nil, problem.InvalidArgument("VALIDATION_FAILED", "poi_id is required for amap_poi")
		}
	} else if strings.TrimSpace(poiID) != "" {
		return nil, problem.InvalidArgument("VALIDATION_FAILED", "poi_id is only allowed for amap_poi")
	}
	if latitude == nil || longitude == nil {
		if s.lbsMode == "enforce" || source != "" {
			return nil, problem.New(422, "ADDRESS_INVALID_LOCATION", "Unprocessable Entity", "coordinates are required for location verification")
		}
		return nil, nil
	}
	if s.locations == nil {
		if s.lbsMode == "enforce" {
			return nil, problem.New(503, "LBS_PROVIDER_UNAVAILABLE", "Service Unavailable", "address location verifier unavailable")
		}
		return nil, nil
	}
	verified, err := s.locations.VerifyAddress(ctx, cityCode, districtCode, *latitude, *longitude)
	if err != nil {
		var details *problem.Details
		if s.lbsMode == "observe" && errors.As(err, &details) && (details.ErrorCode == "LBS_PROVIDER_UNAVAILABLE" || details.ErrorCode == "LBS_PROVIDER_TIMEOUT") {
			return nil, nil
		}
		return nil, err
	}
	apply(verified)
	return &verified, nil
}

func applyVerificationDTO(result *AddressDTO, verified *customerlocation.VerifiedAddress) {
	if result == nil || verified == nil {
		return
	}
	serviceability := verified.Serviceability
	result.Serviceability, result.ServiceShop, result.DeliveryPromise = &serviceability, verified.ServiceShop, verified.DeliveryPromise
}

func normalizeCreateCoordinates(req *AddressUpsertReq) error {
	if req.Latitude == nil && req.Longitude == nil {
		if req.CoordinateSystem != "" {
			return problem.InvalidArgument("COORDINATE_INVALID", "coordinate_system requires latitude and longitude")
		}
		req.CoordinateSystem = coordinate.GCJ02
		return nil
	}
	if req.Latitude == nil || req.Longitude == nil || req.CoordinateSystem == "" {
		return problem.InvalidArgument("COORDINATE_INVALID", "latitude, longitude, and coordinate_system must be provided together")
	}
	lat, lng, err := coordinate.Normalize(*req.Latitude, *req.Longitude, req.CoordinateSystem)
	if err != nil {
		return problem.InvalidArgument("COORDINATE_INVALID", "coordinate is invalid")
	}
	req.Latitude, req.Longitude, req.CoordinateSystem = &lat, &lng, coordinate.GCJ02
	return nil
}

func normalizeUpdateCoordinates(req *AddressUpdateReq) error {
	input := AddressUpsertReq{Latitude: req.Latitude, Longitude: req.Longitude, CoordinateSystem: req.CoordinateSystem}
	if err := normalizeCreateCoordinates(&input); err != nil {
		return err
	}
	req.Latitude, req.Longitude, req.CoordinateSystem = input.Latitude, input.Longitude, input.CoordinateSystem
	return nil
}

// Delete 删除地址。
func (s *Service) Delete(ctx context.Context, claims *auth.Claims, method, path, key, id string) error {
	customerID, addressID, err := s.actorAndAddress(claims, id)
	if err != nil {
		return err
	}
	var result struct{}
	return s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, customerID, method, path, key, idempotency.RequestHash(struct{ ID uint64 }{addressID}))
		if err != nil {
			return err
		}
		if !started {
			cached, err := s.idStore.CachedResponse(ctx, tx, claims.AccountType, customerID, path, key, &result)
			if err != nil || cached {
				return err
			}
			return problem.Conflict("IDEMPOTENCY_IN_PROGRESS", "request is still processing")
		}
		if err := s.repo.LockCustomer(ctx, tx, customerID); err != nil {
			return err
		}
		row, err := s.repo.GetOwned(ctx, tx, customerID, addressID, true, true)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("ADDRESS_NOT_FOUND", "address not found")
		}
		if err != nil {
			return err
		}
		if row.DeletedAt == nil {
			if err := s.repo.SoftDelete(ctx, tx, customerID, addressID); err != nil {
				return err
			}
		}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, customerID, path, key, result)
	})
}

// SetDefault 设置默认项。
func (s *Service) SetDefault(ctx context.Context, claims *auth.Claims, method, path, key, id string) (AddressDTO, error) {
	customerID, addressID, err := s.actorAndAddress(claims, id)
	if err != nil {
		return AddressDTO{}, err
	}
	var result AddressDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		request := struct {
			AddressID uint64 `json:"address_id"`
		}{addressID}
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, customerID, method, path, key, idempotency.RequestHash(request))
		if err != nil {
			return err
		}
		if !started {
			return cachedAddress(ctx, s.idStore, tx, claims.AccountType, customerID, path, key, &result)
		}
		if err := s.repo.LockCustomer(ctx, tx, customerID); err != nil {
			return err
		}
		row, err := s.repo.GetOwned(ctx, tx, customerID, addressID, true, false)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("ADDRESS_NOT_FOUND", "address not found")
		}
		if err != nil {
			return err
		}
		if !row.IsDefault {
			if err := s.repo.ClearDefault(ctx, tx, customerID, addressID); err != nil {
				return err
			}
			if err := s.repo.SetDefault(ctx, tx, customerID, addressID); err != nil {
				return err
			}
			row, err = s.repo.GetOwned(ctx, tx, customerID, addressID, false, false)
			if err != nil {
				return err
			}
		}
		result = addressDTO(row)
		return s.idStore.Succeed(ctx, tx, claims.AccountType, customerID, path, key, result)
	})
	return result, err
}

// cachedAddress 返回缓存地址。
func cachedAddress(ctx context.Context, store *idempotency.Store, tx *gorm.DB, actorType string, actorID uint64, path, key string, out *AddressDTO) error {
	cached, err := store.CachedResponse(ctx, tx, actorType, actorID, path, key, out)
	if err != nil {
		return err
	}
	if !cached {
		return problem.Conflict("IDEMPOTENCY_IN_PROGRESS", "request is still processing")
	}
	return nil
}

// actorAndAddress 返回actor And 地址。
func (s *Service) actorAndAddress(claims *auth.Claims, id string) (uint64, uint64, error) {
	customerID, err := customerIDFromClaims(claims)
	if err != nil {
		return 0, 0, err
	}
	addressID, err := strconv.ParseUint(id, 10, 64)
	if err != nil || addressID == 0 {
		return 0, 0, problem.NotFound("ADDRESS_NOT_FOUND", "address not found")
	}
	return customerID, addressID, nil
}

// customerIDFromClaims 从认证声明中解析并返回用户 ID。
func customerIDFromClaims(claims *auth.Claims) (uint64, error) {
	if claims == nil || claims.AccountType != "customer" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "customer account required")
	}
	id, err := strconv.ParseUint(claims.CustomerID, 10, 64)
	if err != nil || id == 0 {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "invalid customer identity")
	}
	return id, nil
}

// validateAddress 校验地址是否合法。
func validateAddress(cityCode, districtCode, phone string, latitude, longitude *float64) error {
	if !addressCityCodePattern.MatchString(cityCode) || (districtCode != "" && !addressCityCodePattern.MatchString(districtCode)) || !addressPhonePattern.MatchString(phone) {
		return problem.InvalidArgument("VALIDATION_FAILED", "city_code, district_code or contact_phone is invalid")
	}
	if (latitude == nil) != (longitude == nil) {
		return problem.New(422, "ADDRESS_INVALID_LOCATION", "Unprocessable Entity", "latitude and longitude must be provided together")
	}
	if latitude != nil && (*latitude < -90 || *latitude > 90 || *longitude < -180 || *longitude > 180) {
		return problem.New(422, "ADDRESS_INVALID_LOCATION", "Unprocessable Entity", "coordinates are out of range")
	}
	return nil
}

// customerAddressFromCreate 返回用户地址 From Create。
func customerAddressFromCreate(id, customerID uint64, req AddressUpsertReq, isDefault bool, verified *customerlocation.VerifiedAddress) CustomerAddress {
	status := "missing"
	provider := (*string)(nil)
	var geocodedAt *time.Time
	if req.Latitude != nil {
		status = "unverified"
	}
	if verified != nil {
		status = "verified"
		value := "amap"
		provider = &value
		now := verified.GeocodedAt.UTC()
		geocodedAt = &now
	}
	source := req.LocationSource
	if source == "" {
		source = "legacy"
	}
	return CustomerAddress{
		ID: id, CustomerID: customerID, ContactName: strings.TrimSpace(req.ContactName), ContactPhone: req.ContactPhone,
		Province: req.Province, City: req.City, CityCode: optionalString(req.CityCode), District: req.District,
		DistrictCode: optionalString(req.DistrictCode), AddressDetail: req.AddressDetail, Doorplate: optionalString(req.Doorplate),
		POIID: optionalString(req.POIID), FormattedAddress: optionalString(req.FormattedAddress), Latitude: req.Latitude, Longitude: req.Longitude, CoordinateSystem: req.CoordinateSystem,
		LocationSource: source, GeocodeProvider: provider, GeocodeStatus: status, GeocodedAt: geocodedAt, IsDefault: isDefault, Version: 1,
	}
}

// addressValues 返回地址 Values。
func addressValues(req AddressUpdateReq, verified *customerlocation.VerifiedAddress) map[string]any {
	status := "missing"
	var provider any
	var geocodedAt any
	if req.Latitude != nil {
		status = "unverified"
	}
	if verified != nil {
		status = "verified"
		provider = "amap"
		geocodedAt = verified.GeocodedAt.UTC()
	}
	source := req.LocationSource
	if source == "" {
		source = "legacy"
	}
	return map[string]any{
		"contact_name": strings.TrimSpace(req.ContactName), "contact_phone": req.ContactPhone,
		"province": req.Province, "city": req.City, "city_code": optionalString(req.CityCode),
		"district": req.District, "district_code": optionalString(req.DistrictCode),
		"address_detail": req.AddressDetail, "doorplate": optionalString(req.Doorplate), "poi_id": optionalString(req.POIID), "formatted_address": optionalString(req.FormattedAddress),
		"latitude": req.Latitude, "longitude": req.Longitude, "coordinate_system": req.CoordinateSystem,
		"location_source": source, "geocode_provider": provider, "geocode_status": status, "geocoded_at": geocodedAt,
	}
}

// addressDTOs 返回地址 DT Os。
func addressDTOs(rows []CustomerAddress) []AddressDTO {
	items := make([]AddressDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, addressDTO(row))
	}
	return items
}

// addressDTO 返回地址DTO。
func addressDTO(row CustomerAddress) AddressDTO {
	result := AddressDTO{
		ID: strconv.FormatUint(row.ID, 10), ContactName: row.ContactName, ContactPhoneMasked: maskPhone(row.ContactPhone),
		Province: row.Province, City: row.City, CityCode: stringValue(row.CityCode), District: row.District,
		DistrictCode: stringValue(row.DistrictCode), AddressDetail: row.AddressDetail, Doorplate: stringValue(row.Doorplate), POIID: stringValue(row.POIID), FormattedAddress: stringValue(row.FormattedAddress),
		Latitude: row.Latitude, Longitude: row.Longitude, CoordinateSystem: row.CoordinateSystem, LocationSource: row.LocationSource, GeocodeProvider: stringValue(row.GeocodeProvider), GeocodeStatus: row.GeocodeStatus, IsDefault: row.IsDefault, Version: row.Version,
	}
	if row.GeocodedAt != nil {
		result.GeocodedAt = row.GeocodedAt.UTC().Format(time.RFC3339Nano)
	}
	return result
}

// optionalString 将空字符串转换为空指针，否则返回字符串指针。
func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// stringValue 安全读取字符串指针的值。
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// maskPhone 对手机号进行脱敏。
func maskPhone(phone string) string {
	if len(phone) < 7 {
		return phone
	}
	return phone[:3] + "****" + phone[len(phone)-4:]
}
