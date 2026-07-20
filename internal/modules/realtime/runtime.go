package realtime

import (
	"context"
	"log/slog"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type Runtime struct {
	Service *Service
	Hub     *Hub
	Handler *Handler
	Relay   *RelayWorker
}

// NewRuntime 创建并初始化运行时。
func NewRuntime(cfg config.Config, db *gorm.DB, redisClient *redis.Client, ids *snowflake.Generator, registry *metrics.Registry, log *slog.Logger) *Runtime {
	metricState := newMetricState(registry, cfg.App.InstanceID)
	registerDatabaseMetrics(db, registry, cfg.App.InstanceID)
	service := NewService(cfg, db, redisClient, ids, metricState)
	hub := NewHub(cfg.Realtime, service, redisClient, metricState, log)
	return &Runtime{
		Service: service, Hub: hub, Handler: NewHandler(cfg.Realtime, service, hub),
		Relay: NewRelayWorker(cfg.Realtime, db, service, cfg.App.InstanceID+":realtime-relay", log),
	}
}

// RunSubscriber 运行Subscriber处理流程。
func (r *Runtime) RunSubscriber(ctx context.Context) {
	if r != nil && r.Hub != nil {
		r.Hub.RunSubscriber(ctx)
	}
}

// Shutdown 平滑停止当前实例并释放相关资源。
func (r *Runtime) Shutdown(ctx context.Context) {
	if r != nil && r.Hub != nil {
		r.Hub.Shutdown(ctx)
	}
}
