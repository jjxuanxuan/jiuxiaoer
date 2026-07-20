package routeplanning

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/coordinate"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
)

type Service struct {
	cfg       config.MapRouteConfig
	db        *gorm.DB
	redis     *redis.Client
	provider  Provider
	log       *slog.Logger
	metrics   *metricState
	group     singleflight.Group
	semaphore chan struct{}
	canaries  map[uint64]bool
}

func NewService(cfg config.MapRouteConfig, db *gorm.DB, redisClient *redis.Client, provider Provider, registry *metrics.Registry, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	if provider == nil {
		provider = &UnavailableProvider{}
	}
	canaries := make(map[uint64]bool, len(cfg.CanaryRiderIDs))
	for _, raw := range cfg.CanaryRiderIDs {
		if id, err := strconv.ParseUint(raw, 10, 64); err == nil && id > 0 {
			canaries[id] = true
		}
	}
	maxConcurrency := cfg.MaxConcurrency
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	return &Service{
		cfg: cfg, db: db, redis: redisClient, provider: provider, log: log,
		metrics: newMetricState(registry), semaphore: make(chan struct{}, maxConcurrency), canaries: canaries,
	}
}

func (s *Service) Current(ctx context.Context, claims *auth.Claims, deliveryIDRaw, clientIP string) (plan RoutePlan, resultErr error) {
	metricStage := "unknown"
	deliveryIDForLog := ""
	started := time.Now()
	defer func() {
		source := "none"
		if resultErr == nil {
			source = plan.Source
		}
		result := routeMetricResult(resultErr)
		s.metrics.incRequest(metricStage, result, source)
		s.log.InfoContext(ctx, "delivery route query",
			slog.String("request_id", requestctx.RequestID(ctx)), slog.String("action", "delivery_route_current"),
			slog.String("actor_type", routeActorType(claims)), slog.String("delivery_id", deliveryIDForLog),
			slog.String("stage", metricStage), slog.String("mode", sourceProvider(s.cfg.Mode)), slog.String("source", source),
			slog.String("result", successOrError(resultErr)), slog.String("error_code", errorCode(resultErr)),
			slog.String("provider_error_code", safeProviderLogCode(resultErr)), slog.Int64("duration_ms", time.Since(started).Milliseconds()))
	}()
	riderID, accountID, err := routeActor(claims)
	if err != nil {
		return RoutePlan{}, err
	}
	if !s.cfg.Enabled || (len(s.canaries) > 0 && !s.canaries[riderID]) {
		return RoutePlan{}, problem.New(http.StatusServiceUnavailable, "MAP_ROUTE_DISABLED", "Service Unavailable", "delivery route planning is disabled")
	}
	if s.db == nil {
		return RoutePlan{}, problem.New(http.StatusServiceUnavailable, "DELIVERY_ROUTE_NOT_AVAILABLE", "Service Unavailable", "delivery route data is unavailable")
	}
	deliveryID, parseErr := strconv.ParseUint(deliveryIDRaw, 10, 64)
	if parseErr != nil || deliveryID == 0 {
		return RoutePlan{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid delivery order id")
	}
	deliveryIDForLog = strconv.FormatUint(deliveryID, 10)
	contextRow, err := s.loadContext(ctx, deliveryID, riderID)
	if err != nil {
		return RoutePlan{}, err
	}
	metricStage = routeStage(contextRow.Status)
	request, stage, err := s.providerRequest(contextRow)
	if err != nil {
		return RoutePlan{}, err
	}
	if s.redis == nil {
		return RoutePlan{}, routeCacheUnavailable()
	}
	cacheKey := s.cacheKey(contextRow, request, stage)
	if cached, ok, cacheErr := s.loadCache(ctx, "route:fresh:"+cacheKey); cacheErr != nil {
		return RoutePlan{}, routeCacheUnavailable()
	} else if ok {
		cached.Source, cached.Degraded = "cache", false
		s.metrics.incCache("hit")
		return cached, nil
	}
	s.metrics.incCache("miss")
	if scope, limitErr := s.allow(ctx, riderID, accountID, clientIP); limitErr != nil {
		return RoutePlan{}, limitErr
	} else if scope != "" {
		s.metrics.incRateLimited(scope)
		return RoutePlan{}, routeRateLimited(scope)
	}

	resultChannel := s.group.DoChan(cacheKey, func() (any, error) {
		if cached, ok, cacheErr := s.loadCache(context.WithoutCancel(ctx), "route:fresh:"+cacheKey); cacheErr != nil {
			return RoutePlan{}, routeCacheUnavailable()
		} else if ok {
			cached.Source, cached.Degraded = "cache", false
			return cached, nil
		}
		select {
		case s.semaphore <- struct{}{}:
			defer func() { <-s.semaphore }()
		default:
			s.metrics.incRateLimited("global_concurrency")
			return RoutePlan{}, routeRateLimited("global_concurrency")
		}
		providerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.Timeout)
		defer cancel()
		started := time.Now()
		providerLabel := s.cfg.Provider
		s.metrics.addInflight(providerLabel, 1)
		providerResult, providerErr := s.provider.Plan(providerCtx, request)
		s.metrics.addInflight(providerLabel, -1)
		s.metrics.observeProvider(providerLabel, request.Mode, providerErr, time.Since(started))
		if providerErr != nil {
			s.log.WarnContext(ctx, "map route provider call failed",
				slog.String("request_id", requestctx.RequestID(ctx)), slog.String("action", "delivery_route_provider_call"),
				slog.String("delivery_id", deliveryIDForLog), slog.String("stage", stage), slog.String("mode", request.Mode),
				slog.String("result", "error"), slog.String("error_code", "ROUTE_PROVIDER_ERROR"),
				slog.String("provider_error_code", safeProvider(providerErr)), slog.Int64("duration_ms", time.Since(started).Milliseconds()))
			if stale, ok, cacheErr := s.loadCache(context.WithoutCancel(ctx), "route:stale:"+cacheKey); cacheErr != nil {
				return RoutePlan{}, routeCacheUnavailable()
			} else if ok {
				stale.Source, stale.Degraded = "stale_cache", true
				s.metrics.incCache("stale")
				s.metrics.incDegraded(safeProvider(providerErr))
				return stale, nil
			}
			return RoutePlan{}, providerProblem(providerErr)
		}
		if err := validateProviderResult(providerResult); err != nil {
			return RoutePlan{}, providerProblem(err)
		}
		now := time.Now().UTC()
		source := "provider"
		if providerResult.Provider == "fake" {
			source = "fake"
		}
		plan := RoutePlan{
			DeliveryOrderID: strconv.FormatUint(contextRow.DeliveryOrderID, 10), OrderID: strconv.FormatUint(contextRow.OrderID, 10),
			Stage: stage, Mode: request.Mode, CoordinateSystem: coordinate.GCJ02, Origin: request.Origin, Destination: request.Destination,
			DistanceM: providerResult.DistanceM, DurationSeconds: providerResult.DurationSeconds, Polyline: providerResult.Polyline,
			Steps: providerResult.Steps, Source: source, Degraded: false, PlannedAt: now, ExpiresAt: now.Add(s.cfg.CacheTTL), Provider: providerResult.Provider,
		}
		if payload, marshalErr := json.Marshal(plan); marshalErr == nil {
			pipe := s.redis.Pipeline()
			pipe.Set(context.WithoutCancel(ctx), "route:fresh:"+cacheKey, payload, s.cfg.CacheTTL)
			pipe.Set(context.WithoutCancel(ctx), "route:stale:"+cacheKey, payload, s.cfg.StaleTTL)
			if _, cacheErr := pipe.Exec(context.WithoutCancel(ctx)); cacheErr != nil {
				s.metrics.incCache("write_error")
				s.log.Warn("map route cache write failed", slog.String("error", cacheErr.Error()))
			}
		}
		return plan, nil
	})

	select {
	case <-ctx.Done():
		return RoutePlan{}, ctx.Err()
	case call := <-resultChannel:
		if call.Err != nil {
			return RoutePlan{}, call.Err
		}
		plan := call.Val.(RoutePlan)
		return plan, nil
	}
}

type routeContext struct {
	DeliveryOrderID   uint64         `gorm:"column:delivery_order_id"`
	OrderID           uint64         `gorm:"column:order_id"`
	Status            string         `gorm:"column:status"`
	PickupSnapshot    datatypes.JSON `gorm:"column:pickup_snapshot"`
	RecipientSnapshot datatypes.JSON `gorm:"column:recipient_snapshot"`
	Latitude          *float64       `gorm:"column:latitude"`
	Longitude         *float64       `gorm:"column:longitude"`
	CoordinateSystem  string         `gorm:"column:coordinate_system"`
	AccuracyM         *float64       `gorm:"column:accuracy_m"`
	CapturedAt        *time.Time     `gorm:"column:captured_at"`
	RiderStatus       string         `gorm:"column:rider_status"`
	ReviewStatus      string         `gorm:"column:review_status"`
	AccountStatus     string         `gorm:"column:account_status"`
}

func (s *Service) loadContext(ctx context.Context, deliveryID, riderID uint64) (routeContext, error) {
	var row routeContext
	err := s.db.WithContext(ctx).Table("delivery_orders AS d").
		Select("d.id AS delivery_order_id, d.order_id, d.status, d.pickup_snapshot, d.recipient_snapshot, rs.latitude, rs.longitude, rs.coordinate_system, rs.accuracy_m, rs.captured_at, r.status AS rider_status, r.review_status, a.status AS account_status").
		Joins("JOIN riders AS r ON r.id=d.rider_id AND r.deleted_at IS NULL").
		Joins("JOIN accounts AS a ON a.id=r.account_id AND a.deleted_at IS NULL").
		Joins("LEFT JOIN rider_runtime_states AS rs ON rs.rider_id=r.id").
		Where("d.id=? AND d.rider_id=? AND d.deleted_at IS NULL", deliveryID, riderID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return routeContext{}, problem.NotFound("DELIVERY_ROUTE_FORBIDDEN", "delivery route is not available")
	}
	if err != nil {
		return routeContext{}, err
	}
	if row.RiderStatus != "active" || row.ReviewStatus != "approved" || row.AccountStatus != "active" {
		return routeContext{}, problem.Forbidden("DELIVERY_ROUTE_FORBIDDEN", "rider is unavailable")
	}
	return row, nil
}

func (s *Service) providerRequest(row routeContext) (ProviderRequest, string, error) {
	if row.Status != "accepted" && row.Status != "delivering" {
		return ProviderRequest{}, "", problem.Conflict("DELIVERY_ROUTE_NOT_AVAILABLE", "delivery status has no routable stage")
	}
	if row.Latitude == nil || row.Longitude == nil || row.AccuracyM == nil || row.CapturedAt == nil || row.CoordinateSystem != coordinate.GCJ02 {
		return ProviderRequest{}, "", problem.Conflict("RIDER_LOCATION_STALE", "rider location is missing or stale")
	}
	if time.Since(*row.CapturedAt) > s.cfg.LocationFreshness || row.CapturedAt.After(time.Now().Add(5*time.Second)) || *row.AccuracyM > s.cfg.MaxAccuracyM {
		return ProviderRequest{}, "", problem.Conflict("RIDER_LOCATION_STALE", "rider location is missing or stale")
	}
	origin := Coordinate{Latitude: *row.Latitude, Longitude: *row.Longitude}
	if !coordinate.Valid(origin.Latitude, origin.Longitude) {
		return ProviderRequest{}, "", problem.New(http.StatusUnprocessableEntity, "COORDINATE_INVALID", "Unprocessable Entity", "rider coordinate is invalid")
	}
	stage, snapshot := "pickup", row.PickupSnapshot
	if row.Status == "delivering" {
		stage, snapshot = "delivery", row.RecipientSnapshot
	}
	var destination struct {
		Latitude         *float64 `json:"latitude"`
		Longitude        *float64 `json:"longitude"`
		CoordinateSystem string   `json:"coordinate_system"`
	}
	if len(snapshot) == 0 || json.Unmarshal(snapshot, &destination) != nil || destination.Latitude == nil || destination.Longitude == nil {
		return ProviderRequest{}, "", problem.New(http.StatusUnprocessableEntity, "ROUTE_DESTINATION_MISSING", "Unprocessable Entity", "route destination is missing")
	}
	if destination.CoordinateSystem != coordinate.GCJ02 || !coordinate.Valid(*destination.Latitude, *destination.Longitude) {
		return ProviderRequest{}, "", problem.New(http.StatusUnprocessableEntity, "COORDINATE_INVALID", "Unprocessable Entity", "route destination coordinate is invalid")
	}
	return ProviderRequest{
		Origin: roundCoordinate(origin), Destination: roundCoordinate(Coordinate{Latitude: *destination.Latitude, Longitude: *destination.Longitude}),
		Mode: s.cfg.Mode, Strategy: s.cfg.Strategy,
	}, stage, nil
}

func routeActor(claims *auth.Claims) (uint64, uint64, error) {
	if claims == nil || claims.AccountType != "rider" || !hasPermission(claims.Permissions, "delivery:route") {
		return 0, 0, problem.Forbidden("DELIVERY_ROUTE_FORBIDDEN", "delivery route permission is required")
	}
	riderID, riderErr := strconv.ParseUint(claims.RiderID, 10, 64)
	accountID, accountErr := strconv.ParseUint(claims.AccountID, 10, 64)
	if riderErr != nil || accountErr != nil || riderID == 0 || accountID == 0 {
		return 0, 0, problem.Forbidden("DELIVERY_ROUTE_FORBIDDEN", "invalid rider identity")
	}
	return riderID, accountID, nil
}

func hasPermission(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func (s *Service) cacheKey(row routeContext, req ProviderRequest, stage string) string {
	canonical := fmt.Sprintf("v1|%s|%d|%s|%s|%s|%.4f,%.4f|%.7f,%.7f", s.cfg.Provider, row.DeliveryOrderID, stage, req.Mode, req.Strategy, req.Origin.Latitude, req.Origin.Longitude, req.Destination.Latitude, req.Destination.Longitude)
	mac := hmac.New(sha256.New, []byte(s.cfg.CacheHMACSecret))
	_, _ = mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Service) loadCache(ctx context.Context, key string) (RoutePlan, bool, error) {
	payload, err := s.redis.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return RoutePlan{}, false, nil
	}
	if err != nil {
		return RoutePlan{}, false, err
	}
	var plan RoutePlan
	if json.Unmarshal(payload, &plan) != nil || plan.DeliveryOrderID == "" || plan.CoordinateSystem != coordinate.GCJ02 {
		_ = s.redis.Del(context.WithoutCancel(ctx), key).Err()
		return RoutePlan{}, false, nil
	}
	return plan, true, nil
}

func (s *Service) allow(ctx context.Context, riderID, accountID uint64, clientIP string) (string, error) {
	const script = `
for i, key in ipairs(KEYS) do
  local current = redis.call('INCR', key)
  if current == 1 then redis.call('PEXPIRE', key, ARGV[1]) end
  if current > tonumber(ARGV[i + 1]) then return i end
end
return 0`
	keys := []string{
		"route:limit:rider:" + s.secureKey(strconv.FormatUint(riderID, 10)),
		"route:limit:account:" + s.secureKey(strconv.FormatUint(accountID, 10)),
		"route:limit:ip:" + s.secureKey(clientIP),
	}
	index, err := s.redis.Eval(ctx, script, keys, time.Minute.Milliseconds(), s.cfg.RiderRatePerMinute, s.cfg.AccountRatePerMinute, s.cfg.IPRatePerMinute).Int()
	if err != nil {
		return "", routeCacheUnavailable()
	}
	if index == 0 {
		return "", nil
	}
	return []string{"rider", "account", "ip"}[index-1], nil
}

func (s *Service) secureKey(value string) string {
	mac := hmac.New(sha256.New, []byte(s.cfg.CacheHMACSecret))
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func validateProviderResult(result ProviderResult) error {
	if result.Provider == "" || len(result.Polyline) > maxPolylineBytes || len(result.Steps) > maxRouteSteps {
		return &ProviderError{Kind: ProviderInvalid}
	}
	for _, step := range result.Steps {
		if len(step.Instruction) > 512 || len(step.Polyline) > maxStepPolylineBytes {
			return &ProviderError{Kind: ProviderInvalid}
		}
	}
	return nil
}

func providerProblem(err error) error {
	switch {
	case IsProviderError(err, ProviderTimeout):
		return problem.New(http.StatusServiceUnavailable, "ROUTE_PROVIDER_TIMEOUT", "Service Unavailable", "route provider timed out")
	case IsProviderError(err, ProviderQuota):
		return problem.New(http.StatusServiceUnavailable, "ROUTE_PROVIDER_QUOTA_EXCEEDED", "Service Unavailable", "route provider quota is unavailable")
	case providerHTTP5xx(err):
		return problem.New(http.StatusServiceUnavailable, "ROUTE_PROVIDER_ERROR", "Service Unavailable", "route provider is unavailable")
	default:
		return problem.New(http.StatusBadGateway, "ROUTE_PROVIDER_ERROR", "Bad Gateway", "route provider is unavailable")
	}
}

func providerHTTP5xx(err error) bool {
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) || providerErr.Kind != ProviderFailure || len(providerErr.Code) != 3 {
		return false
	}
	status, parseErr := strconv.Atoi(providerErr.Code)
	return parseErr == nil && status >= 500 && status <= 599
}

func routeCacheUnavailable() error {
	return problem.New(http.StatusServiceUnavailable, "ROUTE_CACHE_UNAVAILABLE", "Service Unavailable", "route cache is unavailable")
}

func routeRateLimited(scope string) *problem.Details {
	detail := "route request rate exceeded"
	if scope == "global_concurrency" {
		detail = "route provider concurrency is full"
	}
	err := problem.TooManyRequests("ROUTE_RATE_LIMITED", detail)
	err.Data = map[string]any{"retry_after_seconds": 60, "scope": scope}
	return err
}

func safeProvider(err error) string {
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		return string(providerErr.Kind)
	}
	return "unknown"
}

func sourceProvider(source string) string {
	if strings.TrimSpace(source) == "" {
		return "unknown"
	}
	return source
}

func routeStage(status string) string {
	switch status {
	case "accepted":
		return "pickup"
	case "delivering":
		return "delivery"
	default:
		return "unknown"
	}
}

func routeActorType(claims *auth.Claims) string {
	if claims == nil || claims.AccountType == "" {
		return "unknown"
	}
	return claims.AccountType
}

func successOrError(err error) string {
	if err == nil {
		return "success"
	}
	return "error"
}

func errorCode(err error) string {
	if err == nil {
		return ""
	}
	return routeMetricResult(err)
}

func safeProviderLogCode(err error) string {
	switch errorCode(err) {
	case "ROUTE_PROVIDER_TIMEOUT":
		return "timeout"
	case "ROUTE_PROVIDER_QUOTA_EXCEEDED":
		return "quota"
	case "ROUTE_PROVIDER_ERROR":
		return "failure"
	default:
		return ""
	}
}

func roundCoordinate(value Coordinate) Coordinate {
	const scale = 1_000_000.0
	return Coordinate{Latitude: math.Round(value.Latitude*scale) / scale, Longitude: math.Round(value.Longitude*scale) / scale}
}
