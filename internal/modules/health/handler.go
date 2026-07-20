package health

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	goredis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/rabbitmq"
	"jiuxiaoer-admin/backend-go/internal/modules/mq"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type LeaseChecker interface {
	Healthy() bool
}

type Handler struct {
	cfg      config.Config
	db       *gorm.DB
	redis    *goredis.Client
	rabbitMQ *rabbitmq.Manager
	lease    LeaseChecker
}

type CheckResponse struct {
	Status   string            `json:"status"`
	Checks   map[string]string `json:"checks"`
	UnixTime int64             `json:"unix_time"`
}

// RegisterRoutes 注册Routes。
func RegisterRoutes(root gin.IRoutes, api gin.IRoutes, cfg config.Config, db *gorm.DB, redisClient *goredis.Client, rabbitManager *rabbitmq.Manager, lease LeaseChecker, registry *metrics.Registry) {
	handler := Handler{cfg: cfg, db: db, redis: redisClient, rabbitMQ: rabbitManager, lease: lease}
	root.GET("/livez", handler.Live)
	root.GET("/readyz", handler.Ready)
	api.GET("/health", handler.CompatibleHealth)
	if registry != nil {
		registry.AddCollector(handler.collectMetrics)
	}
}

// Live 处理Live相关逻辑。
func (h Handler) Live(c *gin.Context) {
	c.JSON(http.StatusOK, CheckResponse{
		Status:   "ok",
		Checks:   map[string]string{"process": "ok"},
		UnixTime: time.Now().Unix(),
	})
}

// Ready 处理就绪状态相关逻辑。
func (h Handler) Ready(c *gin.Context) {
	result, code := h.readiness(c.Request.Context())
	c.JSON(code, result)
}

// CompatibleHealth 处理Compatible 健康状态相关逻辑。
func (h Handler) CompatibleHealth(c *gin.Context) {
	result, code := h.readiness(c.Request.Context())
	response.WithStatus(c, code, result)
}

// readiness 返回readiness。
func (h Handler) readiness(ctx context.Context) (CheckResponse, int) {
	checks := map[string]string{
		"mysql":             h.checkMySQL(ctx),
		"redis":             h.checkRedis(ctx),
		"rabbitmq":          h.checkRabbitMQ(),
		"rabbitmq_topology": h.checkRabbitMQTopology(ctx),
		"snowflake_lease":   h.checkNodeLease(),
	}

	ready := true
	if h.cfg.MySQL.Required && checks["mysql"] != "ok" {
		ready = false
	}
	if h.cfg.Redis.Required && checks["redis"] != "ok" {
		ready = false
	}
	if (h.cfg.RabbitMQ.Required || h.cfg.Feature.MQPublisherEnabled) && checks["rabbitmq"] != "ok" {
		ready = false
	}
	if h.cfg.MQ.FailOnTopologyDrift && h.mqEnabled() && checks["rabbitmq_topology"] != "ok" {
		ready = false
	}
	if h.cfg.Redis.Required && checks["snowflake_lease"] != "ok" {
		ready = false
	}
	status := "ok"
	code := http.StatusOK
	if !ready {
		status = "not_ready"
		code = http.StatusServiceUnavailable
	}
	return CheckResponse{Status: status, Checks: checks, UnixTime: time.Now().Unix()}, code
}

// checkMySQL 检查My SQL是否满足要求。
func (h Handler) checkMySQL(ctx context.Context) string {
	if h.db == nil {
		return "disabled"
	}
	sqlDB, err := h.db.DB()
	if err != nil {
		return "unhealthy"
	}
	pingCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		return "unhealthy"
	}
	return "ok"
}

// checkRedis 检查Redis是否满足要求。
func (h Handler) checkRedis(ctx context.Context) string {
	if h.redis == nil {
		return "disabled"
	}
	pingCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err := h.redis.Ping(pingCtx).Err(); err != nil {
		return "unhealthy"
	}
	return "ok"
}

// checkRabbitMQ 检查Rabbit 消息队列是否满足要求。
func (h Handler) checkRabbitMQ() string {
	if h.rabbitMQ == nil {
		return "disabled"
	}
	if !h.rabbitMQ.Healthy() {
		return "unhealthy"
	}
	return "ok"
}

// checkRabbitMQTopology 检查Rabbit 消息队列拓扑是否满足要求。
func (h Handler) checkRabbitMQTopology(ctx context.Context) string {
	if !h.mqEnabled() || h.rabbitMQ == nil {
		return "disabled"
	}
	if !h.rabbitMQ.Healthy() {
		return "unhealthy"
	}
	checkCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	connection, err := h.rabbitMQ.Connection(checkCtx)
	if err != nil {
		return "unhealthy"
	}
	drift, _, complete := mq.VerifyManagedTopology(checkCtx, h.rabbitMQ, mq.DefaultTopology())
	if !complete {
		drift = mq.VerifyTopology(connection, mq.DefaultTopology())
	}
	if len(drift) > 0 {
		return "drift"
	}
	return "ok"
}

// mqEnabled 判断消息队列启用状态。
func (h Handler) mqEnabled() bool {
	return h.cfg.Feature.MQPublisherEnabled || h.cfg.MQ.ConsumerNotificationEnabled || h.cfg.MQ.ConsumerPrintEnabled || h.cfg.MQ.ConsumerCacheEnabled || h.cfg.MQ.ConsumerSecurityEnabled || h.cfg.MQ.ConsumerRealtimeEnabled
}

// checkNodeLease 检查节点租约是否满足要求。
func (h Handler) checkNodeLease() string {
	if h.lease == nil {
		return "disabled"
	}
	if !h.lease.Healthy() {
		return "unhealthy"
	}
	return "ok"
}

// collectMetrics 收集指标。
func (h Handler) collectMetrics() []metrics.Sample {
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	result, code := h.readiness(ctx)
	ready := float64(0)
	if code == http.StatusOK {
		ready = 1
	}
	samples := []metrics.Sample{
		{Name: "jxe_readiness", Help: "Whether this instance is ready to serve traffic.", Type: "gauge", Value: ready},
	}
	required := map[string]bool{
		"mysql":             h.cfg.MySQL.Required,
		"redis":             h.cfg.Redis.Required,
		"rabbitmq":          h.cfg.RabbitMQ.Required || h.mqEnabled(),
		"rabbitmq_topology": h.cfg.MQ.FailOnTopologyDrift && h.mqEnabled(),
		"snowflake_lease":   h.cfg.Redis.Required,
	}
	for _, dependency := range []string{"mysql", "redis", "rabbitmq", "rabbitmq_topology", "snowflake_lease"} {
		value := float64(0)
		if result.Checks[dependency] == "ok" {
			value = 1
		}
		samples = append(samples, metrics.Sample{
			Name:   "jxe_dependency_ready",
			Help:   "Whether a configured dependency is healthy.",
			Type:   "gauge",
			Labels: map[string]string{"dependency": dependency, "required": boolString(required[dependency])},
			Value:  value,
		})
	}
	topologyDrift := float64(0)
	if result.Checks["rabbitmq_topology"] == "drift" {
		topologyDrift = 1
	}
	samples = append(samples, metrics.Sample{Name: "jxe_mq_topology_drift", Help: "Whether RabbitMQ topology differs from the declared contract.", Type: "gauge", Value: topologyDrift})
	return samples
}

// boolString 返回布尔值字符串。
func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
