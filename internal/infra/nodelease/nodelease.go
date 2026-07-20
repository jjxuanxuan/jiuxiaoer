package nodelease

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

var refreshScript = goredis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0
`)

var releaseScript = goredis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

// 租约可防止两个活动实例使用相同的 Snowflake 节点 ID。
type Lease struct {
	redis   *goredis.Client
	key     string
	owner   string
	ttl     time.Duration
	healthy atomic.Bool
}

// Acquire 返回Acquire。
func Acquire(ctx context.Context, client *goredis.Client, nodeID int64, owner string, ttl time.Duration) (*Lease, error) {
	if client == nil {
		return nil, nil
	}
	lease := &Lease{
		redis: client,
		key:   fmt.Sprintf("jxe:infra:snowflake:node:%d", nodeID),
		owner: owner,
		ttl:   ttl,
	}
	acquired, err := client.SetNX(ctx, lease.key, owner, ttl).Result()
	if err != nil {
		return nil, fmt.Errorf("acquire snowflake node lease: %w", err)
	}
	if !acquired {
		current, _ := client.Get(ctx, lease.key).Result()
		return nil, fmt.Errorf("snowflake node %d is already leased by %q", nodeID, current)
	}
	lease.healthy.Store(true)
	return lease, nil
}

// Healthy 判断Healthy。
func (l *Lease) Healthy() bool {
	return l != nil && l.healthy.Load()
}

// 运行会刷新租约直至取消。失去所有权是致命的
// 因为c继续生成 ID 可能会产生冲突。
func (l *Lease) Run(ctx context.Context) error {
	if l == nil {
		return nil
	}
	interval := l.ttl / 3
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastRefresh := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			refreshCtx, cancel := context.WithTimeout(ctx, interval)
			updated, err := refreshScript.Run(refreshCtx, l.redis, []string{l.key}, l.owner, l.ttl.Milliseconds()).Int64()
			cancel()
			if err != nil {
				l.healthy.Store(false)
				if time.Since(lastRefresh) >= 2*l.ttl/3 {
					return fmt.Errorf("refresh snowflake node lease before expiry: %w", err)
				}
				continue
			}
			if updated != 1 {
				l.healthy.Store(false)
				return fmt.Errorf("snowflake node lease ownership lost")
			}
			lastRefresh = time.Now()
			l.healthy.Store(true)
		}
	}
}

// Release 释放nodelease。
func (l *Lease) Release(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.healthy.Store(false)
	_, err := releaseScript.Run(ctx, l.redis, []string{l.key}, l.owner).Result()
	return err
}
