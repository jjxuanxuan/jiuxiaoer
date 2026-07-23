package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/customerlocation"
	"jiuxiaoer-admin/backend-go/internal/pkg/fixedwindow"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

const (
	eventPath       = "/api/v1/search/events"
	historyPath     = "/api/v1/search/history"
	globalScopeID   = "*"
	defaultConfig   = "search.hot.default.global"
	blocklistConfig = "search.hot.blocklist"
	configCacheTTL  = 5 * time.Minute
	redisTimeout    = 100 * time.Millisecond
)

var chinaStandardTime = time.FixedZone("Asia/Shanghai", 8*60*60)

type configCacheEntry struct {
	values    []string
	expiresAt time.Time
}

type hotCachePayload struct {
	Items       []hotAggregate `json:"items"`
	GeneratedAt time.Time      `json:"generated_at"`
}

type idempotencyStore interface {
	ReplayCompleted(context.Context, *gorm.DB, string, uint64, string, string, string, any) (bool, error)
	Start(context.Context, *gorm.DB, uint64, string, uint64, string, string, string, string) (bool, error)
	CachedResponse(context.Context, *gorm.DB, string, uint64, string, string, any) (bool, error)
	Succeed(context.Context, *gorm.DB, string, uint64, string, string, any) error
}

type Service struct {
	cfg       config.SearchConfig
	db        *gorm.DB
	repo      *Repository
	redis     *goredis.Client
	ids       *snowflake.Generator
	idem      idempotencyStore
	limiter   *fixedwindow.Limiter
	metrics   *searchMetrics
	log       *slog.Logger
	locations *customerlocation.Service
	now       func() time.Time

	configMu    sync.Mutex
	configCache map[string]configCacheEntry
}

func NewService(cfg config.SearchConfig, db *gorm.DB, redisClient *goredis.Client, ids *snowflake.Generator, registry *metrics.Registry, log *slog.Logger) *Service {
	if ids == nil {
		ids = snowflake.New(1)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		cfg: cfg, db: db, repo: NewRepository(db), redis: redisClient, ids: ids,
		idem: idempotency.NewStore(db), limiter: fixedwindow.New(redisClient), metrics: newSearchMetrics(registry),
		log: log, now: time.Now, configCache: make(map[string]configCacheEntry),
	}
}

func (s *Service) WithLocations(locations *customerlocation.Service) *Service {
	s.locations = locations
	return s
}

func (s *Service) MaxHistory() int { return s.cfg.HistoryMax }

func (s *Service) Discovery(ctx context.Context, claims *auth.Claims, locationContextID, rawSession string, historyLimit, hotLimit int) (DiscoveryResponse, error) {
	now := s.now()
	customerID, customerRaw, err := searchCustomer(claims, false)
	if err != nil {
		return DiscoveryResponse{}, err
	}
	cityCode, err := s.resolveCity(ctx, customerRaw, rawSession, locationContextID)
	if err != nil {
		return DiscoveryResponse{}, err
	}

	history := make([]HistoryDTO, 0)
	if customerID != 0 {
		rows, queryErr := s.repo.ListHistory(ctx, customerID, now.Add(-s.cfg.HistoryRetention), historyLimit)
		if queryErr != nil {
			s.metrics.incHistory("list", "error")
			return DiscoveryResponse{}, queryErr
		}
		for _, row := range rows {
			history = append(history, historyDTO(row))
		}
		s.metrics.incHistory("list", "success")
	}

	blocklist, blockErr := s.blocklist(ctx)
	if blockErr != nil {
		blocklist = map[string]struct{}{}
	}
	cityRows := make([]hotAggregate, 0)
	if blockErr == nil && cityCode != "" {
		cityRows, _ = s.hotScope(ctx, ScopeCity, cityCode, now)
	}
	globalRows := make([]hotAggregate, 0)
	if blockErr == nil {
		globalRows, _ = s.hotScope(ctx, ScopeGlobal, globalScopeID, now)
	}
	defaults := make([]string, 0)
	if blockErr == nil {
		var defaultsErr error
		defaults, defaultsErr = s.configStrings(ctx, defaultConfig)
		if defaultsErr != nil {
			defaults = nil
		}
	}
	hot := mergeHot(cityRows, globalRows, defaults, cityCode, hotLimit, blocklist)
	return DiscoveryResponse{History: history, HotKeywords: hot, GeneratedAt: now.UTC()}, nil
}

func (s *Service) RecordEvent(ctx context.Context, claims *auth.Claims, method, path, idempotencyKey, locationContextID, rawSession string, req EventRequest) (EventResponse, error) {
	customerID, customerRaw, err := searchCustomer(claims, true)
	if err != nil {
		return EventResponse{}, err
	}
	display, normalized, err := normalizeKeyword(req.Keyword)
	if err != nil {
		s.metrics.incEvent(metricSource(req.Source), "invalid", false)
		return EventResponse{}, err
	}
	if !validSource(req.Source) {
		s.metrics.incEvent("invalid", "invalid", false)
		return EventResponse{}, problem.InvalidArgument("SEARCH_SOURCE_INVALID", "source must be manual, history, or hot")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		s.metrics.incEvent(req.Source, "invalid", false)
		return EventResponse{}, err
	}
	if path == "" {
		path = eventPath
	}
	if s.db == nil {
		return EventResponse{}, errors.New("search database is unavailable")
	}
	requestHash := idempotency.RequestHash(map[string]string{
		"keyword": req.Keyword, "source": req.Source, "location_context_id": strings.TrimSpace(locationContextID),
	})
	var replay EventResponse
	if found, replayErr := s.idem.ReplayCompleted(ctx, s.db, "customer", customerID, path, idempotencyKey, requestHash, &replay); replayErr != nil {
		return EventResponse{}, replayErr
	} else if found {
		return replay, nil
	}

	cityCode, err := s.resolveCity(ctx, customerRaw, rawSession, locationContextID)
	if err != nil {
		return EventResponse{}, err
	}
	limit := s.limiter.Allow(ctx, fmt.Sprintf("search:event:v1:customer:%d", customerID), time.Minute, int64(s.cfg.EventRatePerMinute))
	if limit.Degraded {
		details := problem.New(503, "SEARCH_RATE_LIMIT_UNAVAILABLE", "Service Unavailable", "search event rate limit is temporarily unavailable")
		details.Data = map[string]any{"retry_after_seconds": int64(limit.RetryAfter.Seconds())}
		return EventResponse{}, details
	}
	if !limit.Allowed {
		s.metrics.incRateLimited("customer")
		details := problem.TooManyRequests("SEARCH_EVENT_RATE_LIMITED", "too many search events")
		details.Data = map[string]any{"retry_after_seconds": int64(limit.RetryAfter.Seconds())}
		return EventResponse{}, details
	}
	blocklist, blockErr := s.blocklist(ctx)
	counted := req.Source == SourceManual && blockErr == nil && eligibleForHot(normalized, blocklist)
	now := s.now()
	var responseValue EventResponse
	replayedAfterStart := false
	err = s.dbTransaction(ctx, func(tx *gorm.DB) error {
		if lockErr := s.repo.LockCustomer(ctx, tx, customerID); lockErr != nil {
			if errors.Is(lockErr, gorm.ErrRecordNotFound) {
				return problem.Forbidden("SEARCH_CUSTOMER_REQUIRED", "an active customer account is required")
			}
			return lockErr
		}
		started, startErr := s.idem.Start(ctx, tx, s.ids.Next(), "customer", customerID, method, path, idempotencyKey, requestHash)
		if startErr != nil {
			return startErr
		}
		if !started {
			replayedAfterStart = true
			return cachedResponse(ctx, s.idem, tx, customerID, path, idempotencyKey, &responseValue)
		}
		stored, writeErr := s.repo.UpsertHistory(ctx, tx, &History{
			ID: s.ids.Next(), CustomerID: customerID, Keyword: display, NormalizedKeyword: normalized,
			SearchCount: 1, LastSearchedAt: now, CreatedAt: now, UpdatedAt: now,
		})
		if writeErr != nil {
			return writeErr
		}
		if trimErr := s.repo.TrimHistory(ctx, tx, customerID, s.cfg.HistoryMax); trimErr != nil {
			return trimErr
		}
		if counted {
			if statErr := s.upsertStat(ctx, tx, ScopeGlobal, globalScopeID, display, normalized, now); statErr != nil {
				return statErr
			}
			if cityCode != "" {
				if statErr := s.upsertStat(ctx, tx, ScopeCity, cityCode, display, normalized, now); statErr != nil {
					return statErr
				}
			}
		}
		responseValue = EventResponse{HistoryItem: historyDTO(stored), CountedForHot: counted}
		return s.idem.Succeed(ctx, tx, "customer", customerID, path, idempotencyKey, responseValue)
	})
	if err != nil {
		s.metrics.incEvent(req.Source, problem.FromError(err).ErrorCode, false)
		return EventResponse{}, err
	}
	if replayedAfterStart {
		return responseValue, nil
	}
	s.metrics.incEvent(req.Source, "success", counted)
	scopeType := ScopeGlobal
	if cityCode != "" {
		scopeType = ScopeCity
	}
	s.log.InfoContext(ctx, "customer search event recorded", slog.String("account_type", "customer"), slog.String("source", req.Source), slog.String("result", "success"), slog.String("keyword_hash", keywordHash(normalized)), slog.Int("keyword_length", len([]rune(normalized))), slog.Bool("counted_for_hot", counted), slog.String("scope_type", scopeType))
	return responseValue, nil
}

func (s *Service) ClearHistory(ctx context.Context, claims *auth.Claims, method, path, idempotencyKey string) (ClearResponse, error) {
	customerID, _, err := searchCustomer(claims, true)
	if err != nil {
		return ClearResponse{}, err
	}
	if path == "" {
		path = historyPath
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return ClearResponse{}, err
	}
	if s.db == nil {
		return ClearResponse{}, errors.New("search database is unavailable")
	}
	requestHash := idempotency.RequestHash(map[string]string{"action": "clear_history"})
	var replay ClearResponse
	if found, replayErr := s.idem.ReplayCompleted(ctx, s.db, "customer", customerID, path, idempotencyKey, requestHash, &replay); replayErr != nil {
		return ClearResponse{}, replayErr
	} else if found {
		return replay, nil
	}
	var responseValue ClearResponse
	replayedAfterStart := false
	err = s.dbTransaction(ctx, func(tx *gorm.DB) error {
		started, startErr := s.idem.Start(ctx, tx, s.ids.Next(), "customer", customerID, method, path, idempotencyKey, requestHash)
		if startErr != nil {
			return startErr
		}
		if !started {
			replayedAfterStart = true
			return cachedResponse(ctx, s.idem, tx, customerID, path, idempotencyKey, &responseValue)
		}
		deleted, deleteErr := s.repo.ClearHistory(ctx, tx, customerID)
		if deleteErr != nil {
			return deleteErr
		}
		responseValue = ClearResponse{DeletedCount: deleted}
		return s.idem.Succeed(ctx, tx, "customer", customerID, path, idempotencyKey, responseValue)
	})
	if err != nil {
		s.metrics.incHistory("clear", "error")
		return ClearResponse{}, err
	}
	if replayedAfterStart {
		return responseValue, nil
	}
	s.metrics.incHistory("clear", "success")
	return responseValue, nil
}

func cachedResponse(ctx context.Context, store idempotencyStore, tx *gorm.DB, customerID uint64, path, key string, target any) error {
	found, err := store.CachedResponse(ctx, tx, "customer", customerID, path, key, target)
	if err != nil {
		return err
	}
	if !found {
		return problem.Conflict("IDEMPOTENCY_IN_PROGRESS", "request with the same idempotency key is still processing")
	}
	return nil
}

func (s *Service) upsertStat(ctx context.Context, tx *gorm.DB, scopeType, scopeID, display, normalized string, now time.Time) error {
	local := now.In(chinaStandardTime)
	statDate := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, chinaStandardTime)
	return s.repo.UpsertDailyStat(ctx, tx, &DailyStat{
		ID: s.ids.Next(), StatDate: statDate, ScopeType: scopeType, ScopeID: scopeID,
		NormalizedKeyword: normalized, DisplayKeyword: display, SearchCount: 1,
		LastSearchedAt: now, CreatedAt: now, UpdatedAt: now,
	})
}

func (s *Service) dbTransaction(ctx context.Context, callback func(*gorm.DB) error) error {
	if s.db == nil {
		return errors.New("search database is unavailable")
	}
	return s.db.WithContext(ctx).Transaction(callback)
}

func (s *Service) resolveCity(ctx context.Context, customerID, rawSession, locationContextID string) (string, error) {
	locationContextID = strings.TrimSpace(locationContextID)
	if locationContextID == "" {
		return "", nil
	}
	if s.locations == nil {
		return "", problem.InvalidArgument("LOCATION_CONTEXT_INVALID", "location context is unavailable")
	}
	actor, err := s.locations.BuildActor(customerID, rawSession)
	if err != nil {
		return "", err
	}
	value, err := s.locations.GetContext(ctx, actor, locationContextID)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(value.Location.CityCode), nil
}

func (s *Service) hotScope(ctx context.Context, scopeType, scopeID string, now time.Time) ([]hotAggregate, error) {
	cacheKey := "search:hot:v1:" + scopeType + ":" + scopeID
	cacheResult := "disabled"
	degraded := s.redis == nil
	if s.redis != nil {
		cacheCtx, cancel := context.WithTimeout(ctx, redisTimeout)
		payload, err := s.redis.Get(cacheCtx, cacheKey).Bytes()
		cancel()
		if err == nil {
			var cached hotCachePayload
			if json.Unmarshal(payload, &cached) == nil {
				s.metrics.incHot(scopeType, "hit", false)
				return cached.Items, nil
			}
			cacheResult = "corrupt"
			degraded = true
		} else if errors.Is(err, goredis.Nil) {
			cacheResult = "miss"
			degraded = false
		} else {
			cacheResult = "error"
			degraded = true
		}
	}
	local := now.In(chinaStandardTime)
	through := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, chinaStandardTime)
	from := through.AddDate(0, 0, -(s.cfg.HotWindowDays - 1))
	refreshStarted := time.Now()
	rows, err := s.repo.HotKeywords(ctx, scopeType, scopeID, from, through, 20)
	if err != nil {
		s.metrics.incHot(scopeType, cacheResult, true)
		s.metrics.observeHotRefresh(scopeType, "error", time.Since(refreshStarted))
		return nil, err
	}
	s.metrics.observeHotRefresh(scopeType, "success", time.Since(refreshStarted))
	if s.redis != nil {
		payload, marshalErr := json.Marshal(hotCachePayload{Items: rows, GeneratedAt: now.UTC()})
		if marshalErr == nil {
			jitter := time.Duration(now.UnixNano() % int64(time.Minute))
			cacheCtx, cancel := context.WithTimeout(ctx, redisTimeout)
			if setErr := s.redis.Set(cacheCtx, cacheKey, payload, s.cfg.HotCacheTTL+jitter).Err(); setErr != nil {
				degraded = true
			}
			cancel()
		}
	}
	s.metrics.incHot(scopeType, cacheResult, degraded)
	return rows, nil
}

func (s *Service) blocklist(ctx context.Context) (map[string]struct{}, error) {
	values, err := s.configStrings(ctx, blocklistConfig)
	if err != nil {
		return nil, err
	}
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		_, normalized, normalizeErr := normalizeKeyword(value)
		if normalizeErr == nil {
			result[normalized] = struct{}{}
		}
	}
	return result, nil
}

func (s *Service) configStrings(ctx context.Context, key string) ([]string, error) {
	now := s.now()
	s.configMu.Lock()
	entry, ok := s.configCache[key]
	if ok && entry.expiresAt.After(now) {
		values := append([]string(nil), entry.values...)
		s.configMu.Unlock()
		return values, nil
	}
	s.configMu.Unlock()
	values, err := s.repo.ConfigStrings(ctx, key)
	if err != nil {
		return nil, err
	}
	s.configMu.Lock()
	s.configCache[key] = configCacheEntry{values: append([]string(nil), values...), expiresAt: now.Add(configCacheTTL)}
	s.configMu.Unlock()
	return values, nil
}

func searchCustomer(claims *auth.Claims, required bool) (uint64, string, error) {
	if claims == nil {
		if required {
			return 0, "", problem.Unauthorized("AUTH_UNAUTHORIZED", "customer authentication is required")
		}
		return 0, "", nil
	}
	if claims.AccountType != "customer" {
		return 0, "", problem.Forbidden("SEARCH_CUSTOMER_REQUIRED", "a customer account is required")
	}
	raw := strings.TrimSpace(claims.CustomerID)
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, "", problem.Forbidden("SEARCH_CUSTOMER_REQUIRED", "invalid customer identity")
	}
	return id, raw, nil
}

func metricSource(value string) string {
	if validSource(value) {
		return value
	}
	return "invalid"
}

func validateIdempotencyKey(value string) error {
	if value == "" {
		return problem.InvalidArgument("IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key is required")
	}
	if len(value) < 8 || len(value) > 128 || strings.ContainsAny(value, "\r\n") {
		return problem.InvalidArgument("IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must be between 8 and 128 safe characters")
	}
	return nil
}

func historyDTO(row History) HistoryDTO {
	return HistoryDTO{ID: strconv.FormatUint(row.ID, 10), Keyword: row.Keyword, LastSearchedAt: row.LastSearchedAt.UTC()}
}

func mergeHot(cityRows, globalRows []hotAggregate, defaults []string, cityCode string, limit int, blocklist map[string]struct{}) []HotKeywordDTO {
	if limit < 1 {
		return []HotKeywordDTO{}
	}
	items := make([]HotKeywordDTO, 0, limit)
	seen := make(map[string]struct{}, limit)
	appendKeyword := func(display, normalized, scope string) {
		if len(items) >= limit || normalized == "" {
			return
		}
		if _, exists := seen[normalized]; exists || !eligibleForHot(normalized, blocklist) {
			return
		}
		seen[normalized] = struct{}{}
		item := HotKeywordDTO{Rank: len(items) + 1, Keyword: display, SourceScope: scope}
		if scope == ScopeCity {
			item.CityCode = cityCode
		}
		items = append(items, item)
	}
	for _, row := range cityRows {
		appendKeyword(row.DisplayKeyword, row.NormalizedKeyword, ScopeCity)
	}
	for _, row := range globalRows {
		appendKeyword(row.DisplayKeyword, row.NormalizedKeyword, ScopeGlobal)
	}
	for _, value := range defaults {
		display, normalized, err := normalizeKeyword(value)
		if err == nil {
			appendKeyword(display, normalized, "default")
		}
	}
	return items
}
