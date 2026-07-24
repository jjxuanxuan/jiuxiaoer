package mq

import (
	"context"
	"encoding/json"
	"fmt"

	goredis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

const (
	maxCachePatterns = 20
	maxCacheKeys     = 5000
)

type CacheInvalidationHandler struct {
	redis *goredis.Client
}

type cacheInvalidationPayload struct {
	Keys     []string `json:"keys"`
	Patterns []string `json:"patterns"`
	Reason   string   `json:"reason"`
}

// NewCacheInvalidationHandler 创建并初始化缓存失效处理器。
func NewCacheInvalidationHandler(redisClient *goredis.Client) *CacheInvalidationHandler {
	return &CacheInvalidationHandler{redis: redisClient}
}

// Handle 处理消费者结果请求。
func (h *CacheInvalidationHandler) Handle(ctx context.Context, _ *gorm.DB, envelope EventEnvelope) (ConsumerResult, error) {
	if h.redis == nil {
		return ConsumerResult{}, TemporaryConsumerError("REDIS_UNAVAILABLE", "cache store is unavailable", nil)
	}
	var payload cacheInvalidationPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return ConsumerResult{}, TerminalConsumerError("CACHE_PAYLOAD_INVALID", "cache invalidation payload is invalid", err)
	}
	if len(payload.Keys) == 0 && len(payload.Patterns) == 0 {
		return ConsumerResult{}, TerminalConsumerError("CACHE_TARGET_EMPTY", "cache invalidation has no target", nil)
	}
	if len(payload.Patterns) > maxCachePatterns {
		return ConsumerResult{}, TerminalConsumerError("CACHE_PATTERN_LIMIT", "cache invalidation pattern limit exceeded", nil)
	}
	keys := uniqueStrings(payload.Keys)
	if len(keys) > maxCacheKeys {
		return ConsumerResult{}, TerminalConsumerError("CACHE_KEY_LIMIT", "cache invalidation key limit exceeded", nil)
	}
	for _, pattern := range payload.Patterns {
		if pattern == "" || pattern == "*" {
			return ConsumerResult{}, TerminalConsumerError("CACHE_PATTERN_FORBIDDEN", "unbounded cache invalidation is forbidden", nil)
		}
		var cursor uint64
		for {
			batch, next, err := h.redis.Scan(ctx, cursor, pattern, 100).Result()
			if err != nil {
				return ConsumerResult{}, TemporaryConsumerError("CACHE_SCAN_FAILED", "cache scan failed", err)
			}
			keys = append(keys, batch...)
			keys = uniqueStrings(keys)
			if len(keys) > maxCacheKeys {
				return ConsumerResult{}, TerminalConsumerError("CACHE_KEY_LIMIT", "cache invalidation key limit exceeded", nil)
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
	}
	if len(keys) > 0 {
		if err := h.redis.Del(ctx, keys...).Err(); err != nil {
			return ConsumerResult{}, TemporaryConsumerError("CACHE_DELETE_FAILED", "cache delete failed", err)
		}
	}
	return ConsumerResult{RefType: "cache_invalidation"}, nil
}

// uniqueStrings 返回唯一值 Strings。
func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if len(value) > 512 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

// String 返回当前值的字符串表示。
func (h *CacheInvalidationHandler) String() string {
	return fmt.Sprintf("cache handler(max_patterns=%d,max_keys=%d)", maxCachePatterns, maxCacheKeys)
}
