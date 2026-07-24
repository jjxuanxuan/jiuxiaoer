package customerlocation

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/amap"
	"jiuxiaoer-admin/backend-go/internal/modules/servicearea"
	"jiuxiaoer-admin/backend-go/internal/pkg/coordinate"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

var (
	locationContextIDPattern = regexp.MustCompile(`^loc_[A-Za-z0-9_-]{32}$`)
	locationCityCodePattern  = regexp.MustCompile(`^[0-9]{6}$`)
)

type Service struct {
	cfg          config.CustomerLBSConfig
	repo         *Repository
	redis        *goredis.Client
	store        *ContextStore
	provider     amap.Provider
	serviceArea  *servicearea.Service
	metrics      *lbsMetrics
	log          *slog.Logger
	flight       singleflight.Group
	providerSlot chan struct{}
	now          func() time.Time
	idStore      *idempotency.Store
	idGen        *snowflake.Generator
}

func NewService(cfg config.CustomerLBSConfig, db *gorm.DB, redisClient *goredis.Client, provider amap.Provider, resolver *servicearea.Service, registry *metrics.Registry, log *slog.Logger, generators ...*snowflake.Generator) *Service {
	if provider == nil {
		provider = &amap.UnavailableProvider{}
	}
	if log == nil {
		log = slog.Default()
	}
	idGen := snowflake.New(1)
	if len(generators) > 0 && generators[0] != nil {
		idGen = generators[0]
	}
	maxConcurrency := cfg.MaxConcurrency
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	return &Service{
		cfg: cfg, repo: NewRepository(db), redis: redisClient, store: NewContextStore(redisClient, cfg.ContextTTL),
		provider: provider, serviceArea: resolver, metrics: newLBSMetrics(registry), log: log,
		providerSlot: make(chan struct{}, maxConcurrency), now: time.Now, idStore: idempotency.NewStore(db), idGen: idGen,
	}
}

func (s *Service) HashSession(raw string) string { return s.digest(raw) }

// BuildActor 对所有使用 X-Location-Context 的接口应用相同的
// 客户或匿名主体绑定契约。
func (s *Service) BuildActor(customerID, rawSession string) (Actor, error) {
	if strings.TrimSpace(customerID) != "" {
		if value, err := strconv.ParseUint(customerID, 10, 64); err != nil || value == 0 {
			return Actor{}, problem.Forbidden("PERM_FORBIDDEN", "invalid customer identity")
		}
		return Actor{Type: "customer", ID: customerID}, nil
	}
	rawSession = strings.TrimSpace(rawSession)
	if len(rawSession) < 16 || len(rawSession) > 256 || strings.ContainsAny(rawSession, "\r\n") {
		return Actor{}, problem.InvalidArgument("ANONYMOUS_SESSION_REQUIRED", s.cfg.AnonymousSessionHeader+" is required for anonymous location contexts")
	}
	return Actor{Type: "anonymous", SessionHash: s.HashSession(rawSession)}, nil
}

func (s *Service) SessionHeader() string { return s.cfg.AnonymousSessionHeader }

func (s *Service) Resolve(ctx context.Context, actor Actor, meta ClientMeta, req ResolveRequest) (ResolveResponse, error) {
	started := s.now()
	if err := s.rateLimit(ctx, actor, meta); err != nil {
		return ResolveResponse{}, err
	}
	value, err := s.resolve(ctx, actor, req)
	if err == nil {
		value, err = s.store.Create(ctx, value)
		if err == nil {
			err = s.store.Index(ctx, s.actorIndexKey(actor), value.ID)
		}
	}
	result := "success"
	level := value.LocationLevel
	if err != nil {
		result = problem.FromError(err).ErrorCode
	}
	s.metrics.incResolve(req.Source, result, level, value.Degraded)
	s.logResolve(ctx, actor, req.Source, value, err, time.Since(started))
	if err != nil {
		return ResolveResponse{}, err
	}
	return responseFromContext(value), nil
}

func (s *Service) resolve(ctx context.Context, actor Actor, req ResolveRequest) (LocationContext, error) {
	switch req.Source {
	case "device_location":
		point, err := s.validateDevice(req)
		if err != nil {
			return LocationContext{}, err
		}
		return s.resolvePoint(ctx, actor, "device_location", "exact", point, req.AccuracyM, "", 0, req.CityCodeHint, nil)
	case "saved_address":
		if actor.Type != "customer" {
			return LocationContext{}, problem.Unauthorized("AUTH_UNAUTHORIZED", "saved address resolve requires customer authentication")
		}
		customerID, addressID, err := parseCustomerAddressIDs(actor.ID, req.AddressID)
		if err != nil {
			return LocationContext{}, err
		}
		row, err := s.repo.SavedAddress(ctx, customerID, addressID)
		if isNotFound(err) {
			return LocationContext{}, problem.NotFound("ADDRESS_NOT_FOUND", "address not found")
		}
		if err != nil {
			return LocationContext{}, err
		}
		return s.resolveSavedAddress(ctx, actor, row)
	default:
		return LocationContext{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid location source")
	}
}

func (s *Service) validateDevice(req ResolveRequest) (amap.Coordinate, error) {
	if req.Latitude == nil || req.Longitude == nil || req.AccuracyM == nil || req.CapturedAt == nil || strings.TrimSpace(req.CoordinateSystem) == "" {
		return amap.Coordinate{}, problem.InvalidArgument("VALIDATION_FAILED", "device location requires coordinate, accuracy, coordinate_system, and captured_at")
	}
	if math.IsNaN(*req.AccuracyM) || math.IsInf(*req.AccuracyM, 0) || *req.AccuracyM < 0 || *req.AccuracyM > s.cfg.MaxAccuracyM {
		return amap.Coordinate{}, problem.New(422, "LOCATION_ACCURACY_LOW", "Unprocessable Entity", "location accuracy is below the required threshold")
	}
	age := s.now().Sub(req.CapturedAt.UTC())
	if age < -5*time.Minute || age > 5*time.Minute {
		return amap.Coordinate{}, problem.InvalidArgument("VALIDATION_FAILED", "captured_at must be within five minutes of server time")
	}
	lat, lng, err := coordinate.Normalize(*req.Latitude, *req.Longitude, req.CoordinateSystem)
	if err != nil {
		return amap.Coordinate{}, problem.InvalidArgument("VALIDATION_FAILED", "coordinate is invalid")
	}
	if req.CityCodeHint != "" && !locationCityCodePattern.MatchString(req.CityCodeHint) {
		return amap.Coordinate{}, problem.InvalidArgument("VALIDATION_FAILED", "city_code_hint must be six digits")
	}
	return amap.Coordinate{Latitude: lat, Longitude: lng}, nil
}

func (s *Service) resolveSavedAddress(ctx context.Context, actor Actor, row SavedAddress) (LocationContext, error) {
	if row.Latitude == nil || row.Longitude == nil || row.GeocodeStatus == "missing" || row.GeocodeStatus == "conflict" {
		return LocationContext{}, problem.New(422, "ADDRESS_LOCATION_MISMATCH", "Unprocessable Entity", "address does not have a verified location")
	}
	point := amap.Coordinate{Latitude: *row.Latitude, Longitude: *row.Longitude}
	fallback := AdministrativeLocationDTO{Province: row.Province, City: row.City, District: row.District, FormattedAddress: stringValue(row.FormattedAddress)}
	if row.CityCode != nil {
		fallback.CityCode = *row.CityCode
	}
	if row.DistrictCode != nil {
		fallback.DistrictCode = *row.DistrictCode
	}
	value, err := s.resolvePoint(ctx, actor, "saved_address", "saved_address", point, nil, strconv.FormatUint(row.ID, 10), row.Version, "", &fallback)
	if err != nil {
		return LocationContext{}, err
	}
	if (fallback.CityCode != "" && fallback.CityCode != value.Location.CityCode) || (fallback.DistrictCode != "" && fallback.DistrictCode != value.Location.DistrictCode) {
		return LocationContext{}, problem.New(422, "ADDRESS_LOCATION_MISMATCH", "Unprocessable Entity", "address administrative area conflicts with its coordinate")
	}
	return value, nil
}

func (s *Service) resolvePoint(ctx context.Context, actor Actor, source, level string, point amap.Coordinate, accuracy *float64, addressID string, addressVersion uint32, cityCodeHint string, fallback *AdministrativeLocationDTO) (LocationContext, error) {
	resolveCtx, cancel := context.WithTimeout(ctx, s.cfg.ResolveTimeout)
	defer cancel()
	location, resolutionSource, regeoDegraded, err := s.resolveAdministrative(resolveCtx, point)
	if err != nil && fallback != nil && fallback.CityCode != "" {
		city, cityErr := s.repo.CityByCode(resolveCtx, fallback.CityCode, true)
		if cityErr == nil {
			location = *fallback
			location.City, location.CityCode = city.Name, city.CityCode
			resolutionSource, regeoDegraded, err = "local_distance", true, nil
		}
	}
	if err != nil {
		return LocationContext{}, err
	}
	if cityCodeHint != "" && cityCodeHint != location.CityCode {
		s.metrics.incHintMismatch()
	}
	if s.serviceArea == nil {
		return LocationContext{}, problem.Internal("service area resolver unavailable")
	}
	rows, err := s.serviceArea.Candidates(resolveCtx, servicearea.ResolveInput{CityCode: location.CityCode, Latitude: point.Latitude, Longitude: point.Longitude}, 5)
	if err != nil {
		return LocationContext{}, err
	}
	shops, selected, routeSource, routeDegraded := s.rankCandidates(resolveCtx, point, rows)
	if selected == nil {
		return LocationContext{}, problem.New(422, "OUT_OF_SERVICE_AREA", "Unprocessable Entity", "location is outside the delivery area")
	}
	if resolutionSource == "provider" || resolutionSource == "cache" {
		if routeSource == "local_distance" {
			resolutionSource = routeSource
		} else if resolutionSource != "cache" {
			resolutionSource = routeSource
		}
	}
	degraded := regeoDegraded || routeDegraded
	selection := "automatic"
	if degraded && routeSource == "local_distance" {
		selection = "degraded"
	}
	selected.SelectionSource = selection
	selected.Selected = true
	for index := range shops {
		shops[index].Selected = shops[index].ID == selected.ID
	}
	promise := selected.DeliveryPromise
	return LocationContext{
		ActorType: actor.Type, ActorID: actor.ID, SessionHash: actor.SessionHash,
		Source: source, LocationLevel: level, Latitude: floatPtr(point.Latitude), Longitude: floatPtr(point.Longitude), CoordinateSystem: coordinate.GCJ02, AccuracyM: accuracy,
		Location: location, AddressID: addressID, AddressVersion: addressVersion,
		ServiceShop: selected, CandidateShops: shops, SelectionSource: selection, ServiceAreaVersion: selected.ServiceAreaVersion,
		DeliveryPromise: &promise, Serviceability: ServiceabilityDTO{Serviceable: true}, Degraded: degraded, ResolutionSource: resolutionSource,
	}, nil
}

func (s *Service) resolveAdministrative(ctx context.Context, point amap.Coordinate) (AdministrativeLocationDTO, string, bool, error) {
	if !s.cfg.RegeocodeEnabled {
		return AdministrativeLocationDTO{}, "", false, providerProblem(&amap.ProviderError{Kind: amap.ErrorFailure})
	}
	key := s.regeoCacheKey(point)
	cached, found := s.getRegeoCache(ctx, key)
	if found && cached.FreshUntil.After(s.now().UTC()) {
		location, err := s.mapAdministrative(ctx, cached.Location)
		return location, "cache", false, err
	}
	value, err, _ := s.flight.Do(key, func() (any, error) {
		if err := s.acquireProvider(ctx); err != nil {
			return nil, err
		}
		defer s.releaseProvider()
		location, callErr := s.provider.Reverse(ctx, point)
		if callErr != nil {
			return nil, callErr
		}
		s.setRegeoCache(ctx, key, location)
		return location, nil
	})
	if err != nil {
		s.metrics.incProvider("regeo", "error", providerKind(err))
		if found {
			location, mapErr := s.mapAdministrative(ctx, cached.Location)
			return location, "cache", true, mapErr
		}
		return AdministrativeLocationDTO{}, "", false, providerProblem(err)
	}
	s.metrics.incProvider("regeo", "success", "")
	location, mapErr := s.mapAdministrative(ctx, value.(amap.AdministrativeLocation))
	return location, "provider", false, mapErr
}

func (s *Service) mapAdministrative(ctx context.Context, value amap.AdministrativeLocation) (AdministrativeLocationDTO, error) {
	mapping, err := s.repo.CityByADCode(ctx, value.DistrictCode)
	if isNotFound(err) {
		hasName, nameErr := s.repo.HasPublishedCityName(ctx, value.City)
		if nameErr != nil {
			return AdministrativeLocationDTO{}, nameErr
		}
		if hasName {
			return AdministrativeLocationDTO{}, problem.New(422, "CITY_MAPPING_NOT_FOUND", "Unprocessable Entity", "administrative area mapping is not configured")
		}
		return AdministrativeLocationDTO{}, problem.New(422, "CITY_UNSUPPORTED", "Unprocessable Entity", "city is not open for service")
	}
	if err != nil {
		return AdministrativeLocationDTO{}, err
	}
	if mapping.Status != "published" {
		return AdministrativeLocationDTO{}, problem.New(422, "CITY_UNSUPPORTED", "Unprocessable Entity", "city is not open for service")
	}
	cityName := value.City
	if cityName == "" {
		cityName = mapping.Name
	}
	return AdministrativeLocationDTO{
		Province: value.Province, City: cityName, CityCode: mapping.CityCode, District: value.District,
		DistrictCode: value.DistrictCode, Township: value.Township, TownCode: value.TownCode, FormattedAddress: value.FormattedAddress,
	}, nil
}

type rankedCandidate struct {
	row       servicearea.ResolvedShop
	dto       servicearea.ShopDTO
	routeOK   bool
	routeFrom string
	routeErr  error
}

func (s *Service) rankCandidates(ctx context.Context, destination amap.Coordinate, rows []servicearea.ResolvedShop) ([]servicearea.ShopDTO, *servicearea.ShopDTO, string, bool) {
	ranked := make([]rankedCandidate, len(rows))
	for index, row := range rows {
		ranked[index] = rankedCandidate{row: row, dto: servicearea.ToDTO(row)}
		ranked[index].dto.Degraded = true
		distance := uint64(max64(row.DistanceM, 0))
		ranked[index].dto.RouteDistanceM = &distance
		ranked[index].dto.DeliveryPromise.RouteDistanceM = &distance
		ranked[index].dto.DeliveryPromise.RouteSource = "local_distance"
	}
	if !s.cfg.RouteRefineEnabled {
		sortLocal(ranked)
		items := candidateDTOs(ranked)
		return items, firstShop(items), "local_distance", true
	}
	limit := minInt(len(ranked), s.cfg.MaxRouteCandidates)
	var wait sync.WaitGroup
	for index := 0; index < limit; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			row := ranked[index].row
			origin := amap.Coordinate{Latitude: row.Latitude, Longitude: row.Longitude}
			estimate, source, degraded, err := s.routeEstimate(ctx, row, origin, destination)
			if err != nil {
				ranked[index].routeErr = err
				return
			}
			ranked[index].routeOK, ranked[index].routeFrom = true, source
			distance, duration := estimate.DistanceM, estimate.DurationSeconds
			ranked[index].dto.RouteDistanceM, ranked[index].dto.RouteDurationSeconds = &distance, &duration
			ranked[index].dto.DeliveryPromise.RouteDistanceM = &distance
			ranked[index].dto.DeliveryPromise.RouteDurationSeconds = &duration
			ranked[index].dto.DeliveryPromise.RouteSource = source
			ranked[index].dto.Degraded = degraded
		}(index)
	}
	wait.Wait()
	successes, failures, usedCache := 0, 0, false
	for index := 0; index < limit; index++ {
		if ranked[index].routeOK {
			successes++
			usedCache = usedCache || ranked[index].routeFrom == "cache"
		} else {
			failures++
		}
	}
	if successes == 0 {
		sortLocal(ranked)
		items := candidateDTOs(ranked)
		return items, firstShop(items), "local_distance", true
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].routeOK != ranked[j].routeOK {
			return ranked[i].routeOK
		}
		if ranked[i].routeOK && *ranked[i].dto.RouteDurationSeconds != *ranked[j].dto.RouteDurationSeconds {
			return *ranked[i].dto.RouteDurationSeconds < *ranked[j].dto.RouteDurationSeconds
		}
		if ranked[i].row.Priority != ranked[j].row.Priority {
			return ranked[i].row.Priority > ranked[j].row.Priority
		}
		return ranked[i].row.ID < ranked[j].row.ID
	})
	items := candidateDTOs(ranked)
	source := "provider"
	if usedCache {
		source = "cache"
	}
	return items, firstShop(items), source, failures > 0
}

func (s *Service) routeEstimate(ctx context.Context, row servicearea.ResolvedShop, origin, destination amap.Coordinate) (amap.RouteEstimate, string, bool, error) {
	key := s.routeCacheKey(row.ID, row.ServiceAreaVersion, destination)
	cached, found := s.getRouteCache(ctx, key)
	if found && cached.FreshUntil.After(s.now().UTC()) {
		return cached.Estimate, "cache", false, nil
	}
	value, err, _ := s.flight.Do(key, func() (any, error) {
		if err := s.acquireProvider(ctx); err != nil {
			return nil, err
		}
		defer s.releaseProvider()
		estimate, callErr := s.provider.Estimate(ctx, origin, destination)
		if callErr != nil {
			return nil, callErr
		}
		s.setRouteCache(ctx, key, estimate)
		return estimate, nil
	})
	if err != nil {
		s.metrics.incProvider("route_estimate", "error", providerKind(err))
		if found {
			return cached.Estimate, "cache", true, nil
		}
		return amap.RouteEstimate{}, "", false, err
	}
	s.metrics.incProvider("route_estimate", "success", "")
	return value.(amap.RouteEstimate), "amap", false, nil
}

func (s *Service) CreateCityContext(ctx context.Context, actor Actor, meta ClientMeta, req CityContextRequest) (ResolveResponse, error) {
	if err := s.rateLimit(ctx, actor, meta); err != nil {
		return ResolveResponse{}, err
	}
	if req.Source != "manual_city" || !locationCityCodePattern.MatchString(req.CityCode) {
		return ResolveResponse{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid manual city request")
	}
	city, err := s.repo.CityByCode(ctx, req.CityCode, true)
	if isNotFound(err) {
		return ResolveResponse{}, problem.New(422, "CITY_UNSUPPORTED", "Unprocessable Entity", "city is not open for service")
	}
	if err != nil {
		return ResolveResponse{}, err
	}
	value, err := s.store.Create(ctx, LocationContext{
		ActorType: actor.Type, ActorID: actor.ID, SessionHash: actor.SessionHash,
		Source: "manual_city", LocationLevel: "city", Location: AdministrativeLocationDTO{City: city.Name, CityCode: city.CityCode},
		Serviceability: ServiceabilityDTO{Serviceable: false, ReasonCode: "PRECISE_LOCATION_REQUIRED"}, ResolutionSource: "manual_city",
	})
	if err != nil {
		return ResolveResponse{}, err
	}
	if err := s.store.Index(ctx, s.actorIndexKey(actor), value.ID); err != nil {
		return ResolveResponse{}, err
	}
	s.metrics.incResolve("manual_city", "success", "city", false)
	return responseFromContext(value), nil
}

func (s *Service) ListCities(ctx context.Context, keyword string, query pagination.Query) ([]ServiceCityDTO, string, error) {
	if items, next, ok := s.getCityListCache(ctx, keyword, query.Offset, query.PageSize); ok {
		return items, next, nil
	}
	rows, err := s.repo.PublishedCities(ctx, keyword, query)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		next = pagination.NextPageToken(query)
		rows = rows[:query.PageSize]
	}
	items := make([]ServiceCityDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, ServiceCityDTO{CityCode: row.CityCode, Name: row.Name, Pinyin: row.Pinyin, ProvinceCode: row.ProvinceCode, LocationRequired: true})
	}
	s.setCityListCache(ctx, keyword, query.Offset, query.PageSize, items, next)
	return items, next, nil
}

func (s *Service) GetContext(ctx context.Context, actor Actor, id string) (LocationContext, error) {
	if !locationContextIDPattern.MatchString(id) {
		return LocationContext{}, problem.New(410, "LOCATION_CONTEXT_EXPIRED", "Gone", "location context expired")
	}
	value, err := s.store.Get(ctx, id)
	if err != nil {
		return LocationContext{}, err
	}
	if err := verifyActor(value, actor); err != nil {
		return LocationContext{}, err
	}
	return value, nil
}

func (s *Service) RevokeCustomer(ctx context.Context, customerID string) error {
	if id, err := strconv.ParseUint(customerID, 10, 64); err != nil || id == 0 {
		return problem.Forbidden("PERM_FORBIDDEN", "invalid customer identity")
	}
	return s.store.RevokeActor(ctx, s.actorIndexKey(Actor{Type: "customer", ID: customerID}))
}

func (s *Service) actorIndexKey(actor Actor) string {
	subject := actor.ID
	if actor.Type == "anonymous" {
		subject = actor.SessionHash
	}
	return "lbs:actor:v1:" + s.digest(actor.Type+"\x00"+subject)
}

func (s *Service) ServiceShops(ctx context.Context, actor Actor, id string) ([]servicearea.ShopDTO, error) {
	value, err := s.GetContext(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if value.LocationLevel == "city" {
		return nil, problem.New(422, "PRECISE_LOCATION_REQUIRED", "Unprocessable Entity", "a precise location is required")
	}
	return append([]servicearea.ShopDTO(nil), value.CandidateShops...), nil
}

func (s *Service) SwitchShop(ctx context.Context, actor Actor, id, idempotencyKey string, req SwitchShopRequest) (SwitchShopResponse, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return SwitchShopResponse{}, problem.InvalidArgument("IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key is required")
	}
	if len(idempotencyKey) > 128 || strings.ContainsAny(idempotencyKey, "\r\n") {
		return SwitchShopResponse{}, problem.InvalidArgument("VALIDATION_FAILED", "Idempotency-Key is invalid")
	}
	value, err := s.GetContext(ctx, actor, id)
	if err != nil {
		return SwitchShopResponse{}, err
	}
	requestHash := idempotency.RequestHash(req)
	if replay, found, replayErr := s.switchReplay(ctx, id, idempotencyKey, requestHash); replayErr != nil {
		return SwitchShopResponse{}, replayErr
	} else if found {
		return replay, nil
	}
	if value.LocationLevel == "city" || value.Latitude == nil || value.Longitude == nil {
		return SwitchShopResponse{}, problem.New(422, "PRECISE_LOCATION_REQUIRED", "Unprocessable Entity", "a precise location is required")
	}
	shopID, err := strconv.ParseUint(req.ShopID, 10, 64)
	if err != nil || shopID == 0 {
		return SwitchShopResponse{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid shop_id")
	}
	rows, err := s.serviceArea.Candidates(ctx, servicearea.ResolveInput{CityCode: value.Location.CityCode, Latitude: *value.Latitude, Longitude: *value.Longitude}, 5)
	if err != nil {
		return SwitchShopResponse{}, err
	}
	shops, _, _, degraded := s.rankCandidates(ctx, amap.Coordinate{Latitude: *value.Latitude, Longitude: *value.Longitude}, rows)
	var selected *servicearea.ShopDTO
	for index := range shops {
		if shops[index].ID == req.ShopID {
			copy := shops[index]
			selected = &copy
			break
		}
	}
	if selected == nil {
		return SwitchShopResponse{}, problem.New(422, "OUT_OF_SERVICE_AREA", "Unprocessable Entity", "shop is not serviceable for this location")
	}
	selected.SelectionSource, selected.Selected = "manual", true
	for index := range shops {
		shops[index].Selected = shops[index].ID == selected.ID
		if shops[index].Selected {
			shops[index].SelectionSource = "manual"
		}
	}
	updated, err := s.store.Update(ctx, id, req.ExpectedVersion, func(current *LocationContext) error {
		if err := verifyActor(*current, actor); err != nil {
			return err
		}
		current.ServiceShop, current.CandidateShops = selected, shops
		current.SelectionSource, current.ServiceAreaVersion = "manual", selected.ServiceAreaVersion
		promise := selected.DeliveryPromise
		current.DeliveryPromise = &promise
		current.Degraded = degraded
		return nil
	})
	if err != nil {
		return SwitchShopResponse{}, err
	}
	response := SwitchShopResponse{Version: updated.Version, ServiceShop: *updated.ServiceShop, DeliveryPromise: *updated.DeliveryPromise, SelectionSource: "manual", CartImpact: "price_or_stock_may_change"}
	if err := s.setSwitchReplay(ctx, id, idempotencyKey, requestHash, response, updated.ExpiresAt); err != nil {
		return SwitchShopResponse{}, err
	}
	return response, nil
}

func (s *Service) ResolveOrder(ctx context.Context, customerID, addressID uint64) (OrderResolution, error) {
	row, err := s.repo.SavedAddress(ctx, customerID, addressID)
	if isNotFound(err) {
		return OrderResolution{}, problem.InvalidArgument("ORDER_INVALID_ADDRESS", "address not found")
	}
	if err != nil {
		return OrderResolution{}, err
	}
	if row.GeocodeStatus != "verified" && !s.cfg.RegeocodeEnabled {
		return OrderResolution{}, problem.New(422, "ADDRESS_LOCATION_MISMATCH", "Unprocessable Entity", "address requires location verification")
	}
	value, err := s.resolveSavedAddress(ctx, Actor{Type: "customer", ID: strconv.FormatUint(customerID, 10)}, row)
	if err != nil {
		return OrderResolution{}, err
	}
	return OrderResolution{Context: value, AddressVersion: row.Version}, nil
}

type VerifiedAddress struct {
	Location        AdministrativeLocationDTO
	GeocodedAt      time.Time
	Serviceability  ServiceabilityDTO
	ServiceShop     *servicearea.ShopDTO
	DeliveryPromise *servicearea.DeliveryPromiseDTO
}

func (s *Service) VerifyAddress(ctx context.Context, cityCode, districtCode string, latitude, longitude float64) (VerifiedAddress, error) {
	point := amap.Coordinate{Latitude: latitude, Longitude: longitude}
	location, _, _, err := s.resolveAdministrative(ctx, point)
	if err != nil {
		return VerifiedAddress{}, err
	}
	if cityCode != location.CityCode || districtCode != "" && districtCode != location.DistrictCode {
		return VerifiedAddress{}, problem.New(422, "ADDRESS_LOCATION_MISMATCH", "Unprocessable Entity", "address administrative area conflicts with its coordinate")
	}
	result := VerifiedAddress{Location: location, GeocodedAt: s.now().UTC(), Serviceability: ServiceabilityDTO{Serviceable: false, ReasonCode: "OUT_OF_SERVICE_AREA"}}
	rows, resolveErr := s.serviceArea.Candidates(ctx, servicearea.ResolveInput{CityCode: location.CityCode, Latitude: latitude, Longitude: longitude}, 5)
	if resolveErr != nil {
		var details *problem.Details
		if errors.As(resolveErr, &details) {
			switch details.ErrorCode {
			case "OUT_OF_SERVICE_AREA", "NO_OPEN_SHOP", "CITY_UNSUPPORTED":
				result.Serviceability.ReasonCode = details.ErrorCode
				return result, nil
			}
		}
		return VerifiedAddress{}, resolveErr
	}
	shops, selected, _, degraded := s.rankCandidates(ctx, point, rows)
	_ = shops
	if selected != nil {
		result.Serviceability = ServiceabilityDTO{Serviceable: true}
		result.ServiceShop = selected
		promise := selected.DeliveryPromise
		result.DeliveryPromise = &promise
		_ = degraded
	}
	return result, nil
}

func (s *Service) acquireProvider(ctx context.Context) error {
	select {
	case s.providerSlot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return &amap.ProviderError{Kind: amap.ErrorTimeout}
	default:
		return &amap.ProviderError{Kind: amap.ErrorFailure}
	}
}

func (s *Service) releaseProvider() { <-s.providerSlot }

func providerProblem(err error) error {
	kind := providerKind(err)
	switch kind {
	case string(amap.ErrorTimeout):
		return problem.GatewayTimeout("LBS_PROVIDER_TIMEOUT", "location provider timed out")
	default:
		return problem.New(503, "LBS_PROVIDER_UNAVAILABLE", "Service Unavailable", "location provider unavailable")
	}
}

func providerKind(err error) string {
	var target *amap.ProviderError
	if errors.As(err, &target) {
		return string(target.Kind)
	}
	return string(amap.ErrorFailure)
}

func (s *Service) logResolve(ctx context.Context, actor Actor, source string, value LocationContext, err error, elapsed time.Duration) {
	errorCode := ""
	if err != nil {
		errorCode = problem.FromError(err).ErrorCode
	}
	shopID := ""
	if value.ServiceShop != nil {
		shopID = value.ServiceShop.ID
	}
	s.log.Info("customer location resolved",
		slog.String("request_id", requestctx.RequestID(ctx)), slog.String("actor_type", actor.Type), slog.String("source", source),
		slog.String("result", map[bool]string{true: "error", false: "success"}[err != nil]), slog.String("error_code", errorCode),
		slog.String("city_code", value.Location.CityCode), slog.String("district_code", value.Location.DistrictCode),
		slog.String("selected_shop_id", shopID), slog.Bool("degraded", value.Degraded), slog.String("cache_source", value.ResolutionSource), slog.Int64("duration_ms", elapsed.Milliseconds()),
	)
}

func parseCustomerAddressIDs(customerRaw, addressRaw string) (uint64, uint64, error) {
	customerID, err := strconv.ParseUint(customerRaw, 10, 64)
	if err != nil || customerID == 0 {
		return 0, 0, problem.Forbidden("PERM_FORBIDDEN", "invalid customer identity")
	}
	addressID, err := strconv.ParseUint(addressRaw, 10, 64)
	if err != nil || addressID == 0 {
		return 0, 0, problem.NotFound("ADDRESS_NOT_FOUND", "address not found")
	}
	return customerID, addressID, nil
}

func candidateDTOs(values []rankedCandidate) []servicearea.ShopDTO {
	items := make([]servicearea.ShopDTO, 0, len(values))
	for _, value := range values {
		items = append(items, value.dto)
	}
	return items
}

func firstShop(items []servicearea.ShopDTO) *servicearea.ShopDTO {
	if len(items) == 0 {
		return nil
	}
	value := items[0]
	return &value
}

func sortLocal(values []rankedCandidate) {
	sort.SliceStable(values, func(i, j int) bool {
		if values[i].row.DistanceM != values[j].row.DistanceM {
			return values[i].row.DistanceM < values[j].row.DistanceM
		}
		if values[i].row.Priority != values[j].row.Priority {
			return values[i].row.Priority > values[j].row.Priority
		}
		return values[i].row.ID < values[j].row.ID
	})
}

func contextUnavailable() error {
	return problem.New(503, "LOCATION_CONTEXT_UNAVAILABLE", "Service Unavailable", "location context store unavailable")
}

func rateLimited() error {
	detail := problem.TooManyRequests("LOCATION_RATE_LIMITED", "location requests are rate limited")
	detail.Data = map[string]any{"retry_after_seconds": 60}
	return detail
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
func floatPtr(value float64) *float64 { return &value }
func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
