package customerlocation

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"jiuxiaoer-admin/backend-go/internal/infra/amap"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

type regeoCacheEntry struct {
	Location   amap.AdministrativeLocation `json:"location"`
	FreshUntil time.Time                   `json:"fresh_until"`
	StaleUntil time.Time                   `json:"stale_until"`
}

type routeCacheEntry struct {
	Estimate   amap.RouteEstimate `json:"estimate"`
	FreshUntil time.Time          `json:"fresh_until"`
	StaleUntil time.Time          `json:"stale_until"`
}

type switchReplayEntry struct {
	RequestHash string             `json:"request_hash"`
	Response    SwitchShopResponse `json:"response"`
}

type cityListCacheEntry struct {
	Items []ServiceCityDTO `json:"items"`
	Next  string           `json:"next"`
}

func (s *Service) regeoCacheKey(point amap.Coordinate) string {
	grid := fmt.Sprintf("%.5f,%.5f", point.Latitude, point.Longitude)
	return "lbs:regeo:v1:amap:" + s.digest(grid)
}

func (s *Service) routeCacheKey(shopID uint64, serviceVersion uint32, point amap.Coordinate) string {
	grid := fmt.Sprintf("%.5f,%.5f", point.Latitude, point.Longitude)
	return "lbs:route:v1:" + strconv.FormatUint(uint64(serviceVersion), 10) + ":" + strconv.FormatUint(shopID, 10) + ":" + s.digest(grid)
}

func (s *Service) digest(value string) string {
	mac := hmac.New(sha256.New, []byte(s.cfg.CacheHMACSecret))
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Service) switchReplay(ctx context.Context, contextID, key, requestHash string) (SwitchShopResponse, bool, error) {
	if s.redis == nil {
		return SwitchShopResponse{}, false, contextUnavailable()
	}
	raw, err := s.redis.Get(ctx, s.switchReplayKey(contextID, key)).Bytes()
	if errors.Is(err, goredis.Nil) {
		return SwitchShopResponse{}, false, nil
	}
	if err != nil {
		return SwitchShopResponse{}, false, contextUnavailable()
	}
	var entry switchReplayEntry
	if json.Unmarshal(raw, &entry) != nil || entry.RequestHash == "" || entry.Response.Version == 0 {
		return SwitchShopResponse{}, false, contextUnavailable()
	}
	if entry.RequestHash != requestHash {
		return SwitchShopResponse{}, false, problem.Conflict("IDEMPOTENCY_KEY_REUSED", "Idempotency-Key was already used for a different request")
	}
	return entry.Response, true, nil
}

func (s *Service) setSwitchReplay(ctx context.Context, contextID, key, requestHash string, response SwitchShopResponse, expiresAt time.Time) error {
	payload, err := json.Marshal(switchReplayEntry{RequestHash: requestHash, Response: response})
	if err != nil {
		return err
	}
	ttl := time.Until(expiresAt)
	if ttl <= 0 {
		return problem.New(410, "LOCATION_CONTEXT_EXPIRED", "Gone", "location context expired")
	}
	if err := s.redis.Set(ctx, s.switchReplayKey(contextID, key), payload, ttl).Err(); err != nil {
		return contextUnavailable()
	}
	return nil
}

func (s *Service) switchReplayKey(contextID, key string) string {
	return "lbs:switch:v1:" + s.digest(contextID+"\x00"+key)
}

func (s *Service) getCityListCache(ctx context.Context, keyword string, offset, pageSize int) ([]ServiceCityDTO, string, bool) {
	if s.redis == nil {
		return nil, "", false
	}
	key := s.cityListCacheKey(ctx, keyword, offset, pageSize)
	raw, err := s.redis.Get(ctx, key).Bytes()
	if err != nil {
		return nil, "", false
	}
	var entry cityListCacheEntry
	if json.Unmarshal(raw, &entry) != nil || entry.Items == nil {
		_ = s.redis.Del(ctx, key).Err()
		return nil, "", false
	}
	return entry.Items, entry.Next, true
}

func (s *Service) setCityListCache(ctx context.Context, keyword string, offset, pageSize int, items []ServiceCityDTO, next string) {
	if s.redis == nil {
		return
	}
	payload, err := json.Marshal(cityListCacheEntry{Items: items, Next: next})
	if err == nil {
		_ = s.redis.Set(ctx, s.cityListCacheKey(ctx, keyword, offset, pageSize), payload, 5*time.Minute).Err()
	}
}

func (s *Service) cityListCacheKey(ctx context.Context, keyword string, offset, pageSize int) string {
	version, _ := s.redis.Get(ctx, "lbs:city-version").Result()
	if version == "" {
		version = "0"
	}
	page := strconv.Itoa(offset) + ":" + strconv.Itoa(pageSize)
	return "lbs:cities:v1:" + version + ":" + s.digest(strings.ToLower(strings.TrimSpace(keyword))) + ":" + page
}

func (s *Service) getRegeoCache(ctx context.Context, key string) (regeoCacheEntry, bool) {
	if s.redis == nil {
		return regeoCacheEntry{}, false
	}
	raw, err := s.redis.Get(ctx, key).Bytes()
	if err != nil {
		s.metrics.incCache("regeo", "miss")
		return regeoCacheEntry{}, false
	}
	var entry regeoCacheEntry
	if json.Unmarshal(raw, &entry) != nil || entry.StaleUntil.Before(s.now().UTC()) {
		_ = s.redis.Del(ctx, key).Err()
		s.metrics.incCache("regeo", "invalid")
		return regeoCacheEntry{}, false
	}
	if entry.FreshUntil.After(s.now().UTC()) {
		s.metrics.incCache("regeo", "fresh")
	} else {
		s.metrics.incCache("regeo", "stale")
	}
	return entry, true
}

func (s *Service) setRegeoCache(ctx context.Context, key string, location amap.AdministrativeLocation) {
	if s.redis == nil {
		return
	}
	now := s.now().UTC()
	entry := regeoCacheEntry{Location: location, FreshUntil: now.Add(s.cfg.RegeocodeFreshTTL), StaleUntil: now.Add(s.cfg.RegeocodeStaleTTL)}
	if raw, err := json.Marshal(entry); err == nil {
		_ = s.redis.Set(ctx, key, raw, s.cfg.RegeocodeStaleTTL).Err()
	}
}

func (s *Service) getRouteCache(ctx context.Context, key string) (routeCacheEntry, bool) {
	if s.redis == nil {
		return routeCacheEntry{}, false
	}
	raw, err := s.redis.Get(ctx, key).Bytes()
	if err != nil {
		s.metrics.incCache("route", "miss")
		return routeCacheEntry{}, false
	}
	var entry routeCacheEntry
	if json.Unmarshal(raw, &entry) != nil || entry.StaleUntil.Before(s.now().UTC()) {
		_ = s.redis.Del(ctx, key).Err()
		s.metrics.incCache("route", "invalid")
		return routeCacheEntry{}, false
	}
	if entry.FreshUntil.After(s.now().UTC()) {
		s.metrics.incCache("route", "fresh")
	} else {
		s.metrics.incCache("route", "stale")
	}
	return entry, true
}

func (s *Service) setRouteCache(ctx context.Context, key string, estimate amap.RouteEstimate) {
	if s.redis == nil {
		return
	}
	now := s.now().UTC()
	entry := routeCacheEntry{Estimate: estimate, FreshUntil: now.Add(s.cfg.RouteFreshTTL), StaleUntil: now.Add(s.cfg.RouteStaleTTL)}
	if raw, err := json.Marshal(entry); err == nil {
		_ = s.redis.Set(ctx, key, raw, s.cfg.RouteStaleTTL).Err()
	}
}

func (s *Service) rateLimit(ctx context.Context, actor Actor, meta ClientMeta) error {
	if s.redis == nil {
		return contextUnavailable()
	}
	type dimension struct {
		name, value string
		limit       int
	}
	dimensions := make([]dimension, 0, 2)
	if actor.Type == "customer" {
		dimensions = append(dimensions, dimension{"customer", actor.ID, s.cfg.CustomerRate})
	} else {
		dimensions = append(dimensions,
			dimension{"session", actor.SessionHash, s.cfg.SessionRate},
			dimension{"ip", meta.IP, s.cfg.AnonymousIPRate},
		)
	}
	bucket := s.now().UTC().Unix() / 60
	for _, item := range dimensions {
		key := "lbs:rate:v1:" + item.name + ":" + s.digest(item.value) + ":" + strconv.FormatInt(bucket, 10)
		count, err := s.redis.Incr(ctx, key).Result()
		if err != nil {
			return contextUnavailable()
		}
		if count == 1 {
			_ = s.redis.Expire(ctx, key, 70*time.Second).Err()
		}
		if count > int64(item.limit) {
			return rateLimited()
		}
	}
	return nil
}
