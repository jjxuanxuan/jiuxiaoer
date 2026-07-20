package redis

import (
	"context"
	"fmt"
	"log/slog"

	goredis "github.com/redis/go-redis/v9"

	"jiuxiaoer-admin/backend-go/internal/config"
)

// Open 解密并返回客户端。
func Open(ctx context.Context, cfg config.RedisConfig, log *slog.Logger) (*goredis.Client, error) {
	if cfg.Addr == "" {
		// Redis 对只读本地运行是可选的，但认证会话和缓存能力会依赖它。
		if cfg.Required {
			return nil, fmt.Errorf("JXE_REDIS_ADDR is required")
		}
		log.Warn("redis disabled because JXE_REDIS_ADDR is empty")
		return nil, nil
	}

	client := goredis.NewClient(&goredis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})

	// Redis 启动 Ping 保持短超时，避免缓存启动慢导致 API 长时间卡住。
	pingCtx, cancel := context.WithTimeout(ctx, cfg.PingTimeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		if cfg.Required {
			_ = client.Close()
			return nil, err
		}
		log.Warn("redis ping failed; continuing because redis is optional", slog.Any("error", err))
		_ = client.Close()
		return nil, nil
	}

	return client, nil
}
