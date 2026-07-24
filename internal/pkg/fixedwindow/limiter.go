package fixedwindow

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

var incrementScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
local ttl = redis.call('PTTL', KEYS[1])
return {count, ttl}
`)

const redisAttemptTimeout = 100 * time.Millisecond

// Result 同时报告限流决策，以及是否因 Redis 不可用而启用了进程内降级方案。
// 调用方可以针对 Degraded 告警，而不会让短暂的 Redis 故障演变为写入中断。
type Result struct {
	Allowed    bool
	Degraded   bool
	RetryAfter time.Duration
}

type localWindow struct {
	count     int64
	expiresAt time.Time
}

// Limiter 实现基于 Redis 的原子固定窗口，并提供有界、软失败的进程内降级方案。
// 降级方案刻意对单进程执行更严格限制，但无法提供集群级保证，因此必须可观测。
type Limiter struct {
	redis *redis.Client
	now   func() time.Time

	mu      sync.Mutex
	windows map[string]localWindow
}

func New(client *redis.Client) *Limiter {
	return &Limiter{redis: client, now: time.Now, windows: make(map[string]localWindow)}
}

func (l *Limiter) Allow(ctx context.Context, key string, window time.Duration, limit int64) Result {
	if l == nil || key == "" || window <= 0 || limit <= 0 {
		return Result{Allowed: false, Degraded: true, RetryAfter: time.Second}
	}
	if l.redis != nil {
		redisCtx, cancel := context.WithTimeout(ctx, redisAttemptTimeout)
		values, err := incrementScript.Run(redisCtx, l.redis, []string{key}, window.Milliseconds()).Int64Slice()
		cancel()
		if err == nil && len(values) == 2 {
			retry := durationFromMilliseconds(values[1], window)
			return Result{Allowed: values[0] <= limit, RetryAfter: retry}
		}
	}
	result := l.allowLocal(key, window, limit)
	result.Degraded = true
	return result
}

func (l *Limiter) allowLocal(key string, window time.Duration, limit int64) Result {
	now := l.now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()

	current, ok := l.windows[key]
	if !ok || !current.expiresAt.After(now) {
		current = localWindow{expiresAt: now.Add(window)}
	}
	current.count++
	l.windows[key] = current
	if len(l.windows) > 4096 {
		for candidate, value := range l.windows {
			if !value.expiresAt.After(now) {
				delete(l.windows, candidate)
			}
		}
	}
	return Result{Allowed: current.count <= limit, RetryAfter: roundRetry(current.expiresAt.Sub(now))}
}

func durationFromMilliseconds(milliseconds int64, fallback time.Duration) time.Duration {
	if milliseconds <= 0 {
		return roundRetry(fallback)
	}
	return roundRetry(time.Duration(milliseconds) * time.Millisecond)
}

func roundRetry(value time.Duration) time.Duration {
	seconds := math.Ceil(value.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return time.Duration(seconds) * time.Second
}
