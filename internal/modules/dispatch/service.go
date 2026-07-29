package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type Service struct {
	cfg                   config.Config
	db                    *gorm.DB
	redis                 *redis.Client
	ids                   *snowflake.Generator
	idem                  *idempotency.Store
	metrics               *metrics.Registry
	log                   *slog.Logger
	assignmentSettlements *assignmentSettlementRegistry
}

// NewService 创建并初始化服务。
func NewService(cfg config.Config, db *gorm.DB, redisClient *redis.Client, ids *snowflake.Generator, registry *metrics.Registry, log *slog.Logger) *Service {
	return &Service{
		cfg: cfg, db: db, redis: redisClient, ids: ids,
		idem: idempotency.NewStore(db), metrics: registry, log: log,
		assignmentSettlements: newAssignmentSettlementRegistry(),
	}
}

type PaidOrderInput struct {
	OrderID          uint64
	ShopID           uint64
	AddressSnapshot  datatypes.JSON
	ScheduledStartAt *time.Time
	ScheduledEndAt   *time.Time
	NotBeforeAt      *time.Time
}

// EnsurePaidOrderTask 确保已支付订单的派单任务存在且可用。
// EnsurePaidOrderTask 在支付成功事务内调用。它同步创建持久的配送和派单事实；
// RabbitMQ 只在提交后唤醒工作进程。
func (s *Service) EnsurePaidOrderTask(ctx context.Context, tx *gorm.DB, input PaidOrderInput) (DeliveryOrder, Job, error) {
	if err := validateDeliverySchedule(input); err != nil {
		return DeliveryOrder{}, Job{}, err
	}
	var existing DeliveryOrder
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("order_id=?", input.OrderID).First(&existing).Error
	if err == nil {
		if err := validateExistingDeliverySchedule(existing, input); err != nil {
			return DeliveryOrder{}, Job{}, err
		}
		var job Job
		jobErr := tx.WithContext(ctx).Where("delivery_order_id=? AND status IN ?", existing.ID, activeJobStatuses()).Order("dispatch_seq DESC").First(&job).Error
		if jobErr == nil {
			return existing, job, nil
		}
		if !errors.Is(jobErr, gorm.ErrRecordNotFound) {
			return DeliveryOrder{}, Job{}, jobErr
		}
		if existing.Status != "pending_assign" || existing.RiderID != nil {
			return existing, Job{}, nil
		}
		var maxSeq uint
		if err := tx.WithContext(ctx).Model(&Job{}).Where("delivery_order_id=?", existing.ID).Select("COALESCE(MAX(dispatch_seq),0)").Scan(&maxSeq).Error; err != nil {
			return DeliveryOrder{}, Job{}, err
		}
		return s.createJobForDelivery(ctx, tx, existing, input.OrderID, input.ShopID, maxSeq+1)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return DeliveryOrder{}, Job{}, err
	}

	var shop shopRow
	if err := tx.WithContext(ctx).Where("id=? AND deleted_at IS NULL", input.ShopID).First(&shop).Error; err != nil {
		return DeliveryOrder{}, Job{}, err
	}
	pickup := jsonData(map[string]any{
		"shop_id": idString(shop.ID), "name": shop.Name, "phone": stringValue(shop.Phone),
		"district": shop.District, "address": shop.Address, "latitude": shop.Latitude, "longitude": shop.Longitude, "coordinate_system": shop.CoordinateSystem,
	})
	delivery := DeliveryOrder{
		ID: s.ids.Next(), OrderID: input.OrderID, ShopID: input.ShopID, Status: "pending_assign",
		AssignmentVersion: 1, DispatchStatus: "pending", PickupReadyStatus: "waiting_store",
		PickupSnapshot: pickup, RecipientSnapshot: input.AddressSnapshot,
		ScheduledStartAt: input.ScheduledStartAt, ScheduledEndAt: input.ScheduledEndAt,
		NotBeforeAt: input.NotBeforeAt,
	}
	if err := tx.WithContext(ctx).Create(&delivery).Error; err != nil {
		if isDuplicate(err) {
			if loadErr := tx.WithContext(ctx).Where("order_id=?", input.OrderID).First(&existing).Error; loadErr == nil {
				return s.createJobForDelivery(ctx, tx, existing, input.OrderID, input.ShopID, 1)
			}
		}
		return DeliveryOrder{}, Job{}, err
	}
	return s.createJobForDelivery(ctx, tx, delivery, input.OrderID, input.ShopID, 1)
}

// createJobForDelivery 为配送单创建派单任务。
func (s *Service) createJobForDelivery(ctx context.Context, tx *gorm.DB, delivery DeliveryOrder, orderID, shopID uint64, seq uint) (DeliveryOrder, Job, error) {
	policy, snapshot, version, err := s.resolvePolicy(ctx, tx, shopID)
	if err != nil {
		return DeliveryOrder{}, Job{}, err
	}
	if !s.cfg.Dispatch.Enabled {
		snapshot.Mode = "manual"
		version += ":dispatch-disabled"
	}
	if s.cfg.Dispatch.ModeOverride != "" {
		snapshot.Mode = s.cfg.Dispatch.ModeOverride
		version += ":override-" + snapshot.Mode
	}
	policyJSON := jsonData(snapshot)
	now := time.Now()
	nextActionAt := now
	if delivery.NotBeforeAt != nil && delivery.NotBeforeAt.After(nextActionAt) {
		nextActionAt = *delivery.NotBeforeAt
	}
	jobID := s.ids.Next()
	job := Job{
		ID: jobID, JobNo: fmt.Sprintf("DJ%d", jobID), DeliveryOrderID: delivery.ID, OrderID: orderID,
		ShopID: shopID, DispatchSeq: seq, PolicyVersion: version, PolicySnapshot: policyJSON,
		Mode: snapshot.Mode, Status: "pending", NextActionAt: nextActionAt, Version: 1,
	}
	if policy != nil {
		job.PolicyID = &policy.ID
	}
	if err := tx.WithContext(ctx).Create(&job).Error; err != nil {
		if isDuplicate(err) {
			var existing Job
			if loadErr := tx.WithContext(ctx).Where("delivery_order_id=? AND status IN ?", delivery.ID, activeJobStatuses()).Order("dispatch_seq DESC").First(&existing).Error; loadErr == nil {
				return delivery, existing, nil
			}
		}
		return DeliveryOrder{}, Job{}, err
	}
	if err := tx.WithContext(ctx).Model(&DeliveryOrder{}).Where("id=?", delivery.ID).Updates(map[string]any{
		"dispatch_status": "pending", "current_dispatch_job_id": job.ID,
		"dispatch_mode_snapshot": snapshot.Mode, "dispatch_policy_version": version,
	}).Error; err != nil {
		return DeliveryOrder{}, Job{}, err
	}
	if err := s.createEvent(ctx, tx, "dispatch.job.ready", "dispatch_job", job.ID, map[string]any{
		"dispatch_job_id": idString(job.ID), "delivery_order_id": idString(delivery.ID),
	}); err != nil {
		return DeliveryOrder{}, Job{}, err
	}
	delivery.DispatchStatus = "pending"
	delivery.CurrentDispatchJobID = &job.ID
	delivery.DispatchModeSnapshot = &snapshot.Mode
	delivery.DispatchPolicyVersion = &version
	return delivery, job, nil
}

func validateDeliverySchedule(input PaidOrderInput) error {
	present := 0
	for _, value := range []*time.Time{
		input.ScheduledStartAt,
		input.ScheduledEndAt,
		input.NotBeforeAt,
	} {
		if value != nil {
			present++
		}
	}
	if present == 0 {
		return nil
	}
	if present != 3 ||
		!input.ScheduledStartAt.Before(*input.ScheduledEndAt) ||
		input.NotBeforeAt.After(*input.ScheduledStartAt) {
		return fmt.Errorf("invalid scheduled delivery window")
	}
	return nil
}

func validateExistingDeliverySchedule(
	existing DeliveryOrder,
	input PaidOrderInput,
) error {
	if input.ScheduledStartAt == nil {
		if existing.ScheduledStartAt != nil ||
			existing.ScheduledEndAt != nil ||
			existing.NotBeforeAt != nil {
			return fmt.Errorf("existing delivery schedule differs from request")
		}
		return nil
	}
	if existing.ScheduledStartAt == nil ||
		existing.ScheduledEndAt == nil ||
		existing.NotBeforeAt == nil ||
		!existing.ScheduledStartAt.Equal(*input.ScheduledStartAt) ||
		!existing.ScheduledEndAt.Equal(*input.ScheduledEndAt) ||
		!existing.NotBeforeAt.Equal(*input.NotBeforeAt) {
		return fmt.Errorf("existing delivery schedule differs from request")
	}
	return nil
}

// resolvePolicy 解析并返回派单策略。
func (s *Service) resolvePolicy(ctx context.Context, tx *gorm.DB, shopID uint64) (*Policy, PolicySnapshot, string, error) {
	var shop shopRow
	if err := tx.WithContext(ctx).Select("id,city_code").Where("id=?", shopID).First(&shop).Error; err != nil {
		return nil, PolicySnapshot{}, "", err
	}
	scopes := [][2]string{{"shop", idString(shopID)}}
	if shop.CityCode != nil && *shop.CityCode != "" {
		scopes = append(scopes, [2]string{"city", *shop.CityCode})
	}
	scopes = append(scopes, [2]string{"global", "0"})
	for _, scope := range scopes {
		var policy Policy
		err := tx.WithContext(ctx).Where("scope_type=? AND scope_id=? AND status='published'", scope[0], scope[1]).Order("version DESC").First(&policy).Error
		if err == nil {
			snapshot := snapshotFromPolicy(policy)
			return &policy, snapshot, fmt.Sprintf("%s:%s/v%d", scope[0], scope[1], policy.Version), nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, PolicySnapshot{}, "", err
		}
	}
	snapshot := defaultPolicySnapshot()
	snapshot.Mode = "manual"
	return nil, snapshot, "built-in/manual-v1", nil
}

// defaultPolicySnapshot 返回默认项策略快照。
func defaultPolicySnapshot() PolicySnapshot {
	return PolicySnapshot{
		Mode: "hybrid", AutoRounds: 3, OfferTTLSeconds: 10, GrabTTLSeconds: 30,
		CandidateLimit: 100, OfferCandidateLimit: 3, HeartbeatFreshSeconds: 60,
		LocationFreshSeconds: 120, MaxLocationAccuracyM: 200, MaxPickupDistanceM: 5000,
		MaxActiveOrdersDefault: 3, IdleFullScoreSeconds: 1800,
		ScoreWeights:             ScoreWeights{Distance: .45, Load: .30, Idle: .20, Freshness: .05},
		RejectionCooldownSeconds: 120, ScoreVersion: "dispatch-score-v1",
	}
}

// snapshotFromPolicy 根据策略构造快照。
func snapshotFromPolicy(policy Policy) PolicySnapshot {
	weights := defaultPolicySnapshot().ScoreWeights
	_ = json.Unmarshal(policy.ScoreWeights, &weights)
	return PolicySnapshot{
		Mode: policy.Mode, AutoRounds: policy.AutoRounds, OfferTTLSeconds: policy.OfferTTLSeconds,
		GrabTTLSeconds: policy.GrabTTLSeconds, CandidateLimit: policy.CandidateLimit,
		OfferCandidateLimit: policy.OfferCandidateLimit, HeartbeatFreshSeconds: policy.HeartbeatFreshSeconds,
		LocationFreshSeconds: policy.LocationFreshSeconds, MaxLocationAccuracyM: policy.MaxLocationAccuracyM,
		MaxPickupDistanceM: policy.MaxPickupDistanceM, MaxActiveOrdersDefault: policy.MaxActiveOrdersDefault,
		IdleFullScoreSeconds: policy.IdleFullScoreSeconds, ScoreWeights: weights,
		RejectionCooldownSeconds: policy.RejectionCooldownSeconds, ScoreVersion: "dispatch-score-v1",
	}
}

// decodeSnapshot 解码快照。
func decodeSnapshot(job Job) PolicySnapshot {
	snapshot := defaultPolicySnapshot()
	_ = json.Unmarshal(job.PolicySnapshot, &snapshot)
	if job.Mode != "" {
		snapshot.Mode = job.Mode
	}
	return snapshot
}

// createEvent 创建事件。
func (s *Service) createEvent(ctx context.Context, tx *gorm.DB, eventType, aggregateType string, aggregateID uint64, payload any) error {
	return tx.WithContext(ctx).Table("outbox_events").Create(map[string]any{
		"id": s.ids.Next(), "event_id": uuid.NewString(), "event_type": eventType,
		"aggregate_type": aggregateType, "aggregate_id": aggregateID, "payload": jsonData(payload),
		"status": "pending", "retry_count": 0, "request_id": requestctx.RequestIDPtr(ctx),
	}).Error
}

// createAudit 创建审计。
func (s *Service) createAudit(ctx context.Context, tx *gorm.DB, actorType string, actorID uint64, action, resourceType string, resourceID uint64, before, after any) error {
	return tx.WithContext(ctx).Table("audit_logs").Create(map[string]any{
		"id": s.ids.Next(), "actor_type": actorType, "actor_id": actorID, "action": action,
		"resource_type": resourceType, "resource_id": resourceID, "before_data": jsonData(before),
		"after_data": jsonData(after), "result": "success", "request_id": requestctx.RequestIDPtr(ctx),
		"ip_hash": requestctx.IPHashPtr(ctx), "user_agent": requestctx.UserAgentPtr(ctx),
	}).Error
}

// createHeartbeatFailureAudit 使用服务的基础数据库句柄记录被拒绝的心跳。
// 它不得使用业务事务：校验、限流和资格失败在事务回滚后仍需要审计事实。
// 此处只接受受控维度；心跳载荷中的坐标和设备标识符绝不会持久化。
func (s *Service) createHeartbeatFailureAudit(ctx context.Context, claims *auth.Claims, spec heartbeatAuditSpec) {
	if s == nil || s.db == nil || s.ids == nil {
		return
	}
	actorType, actorID := heartbeatAuditActor(claims)
	var resourceID any
	if actorID != 0 {
		resourceID = actorID
	}
	var accountID any
	if id := requestctx.AccountID(ctx); id != 0 {
		accountID = id
	}
	after := map[string]any{
		"error_code":    spec.ErrorCode,
		"reason_code":   spec.ReasonCode,
		"request_stage": spec.RequestStage,
	}
	if spec.RateLimitDimension != "" {
		after["rate_limit_dimension"] = spec.RateLimitDimension
	}

	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	err := s.db.Session(&gorm.Session{NewDB: true}).WithContext(auditCtx).Table("audit_logs").Create(map[string]any{
		"id":            s.ids.Next(),
		"event_id":      uuid.NewString(),
		"actor_type":    actorType,
		"actor_id":      actorID,
		"account_id":    accountID,
		"action":        spec.Action,
		"resource_type": "rider",
		"resource_id":   resourceID,
		"after_data":    jsonData(after),
		"result":        "failed",
		"error_code":    spec.ErrorCode,
		"reason_code":   spec.ReasonCode,
		"request_id":    requestctx.RequestIDPtr(auditCtx),
		"ip_hash":       requestctx.IPHashPtr(auditCtx),
		"created_at":    time.Now(),
	}).Error
	if err != nil && s.log != nil {
		// 不要记录 err：驱动错误可能回显 SQL 值。
		// 稳定的审计维度已足以提供运维信号。
		s.log.Warn("heartbeat failure audit write failed", "action", spec.Action, "error_code", spec.ErrorCode, "reason_code", spec.ReasonCode)
	}
}

func heartbeatAuditActor(claims *auth.Claims) (string, uint64) {
	if claims == nil {
		return "unknown", 0
	}
	actorType := claims.AccountType
	switch actorType {
	case "admin", "applicant", "customer", "merchant", "rider":
	default:
		actorType = "unknown"
	}
	if actorType != "rider" {
		return actorType, 0
	}
	actorID, _ := parseID(claims.RiderID)
	return actorType, actorID
}

// activeJobStatuses 返回启用状态任务 Statuses。
func activeJobStatuses() []string {
	return []string{"pending", "scoring", "offering", "grab_open", "manual_required"}
}

// jsonData 将输入值序列化为 JSON 数据。
func jsonData(value any) datatypes.JSON {
	if value == nil {
		return nil
	}
	payload, _ := json.Marshal(value)
	return datatypes.JSON(payload)
}

// idString 将数字 ID 转换为字符串。
func idString(id uint64) string { return strconv.FormatUint(id, 10) }

// optionalID 返回可选 ID 指针。
func optionalID(id *uint64) string {
	if id == nil {
		return ""
	}
	return idString(*id)
}

// parseID 解析并校验字符串形式的 ID。
func parseID(raw string) (uint64, error) {
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, fmt.Errorf("invalid id")
	}
	return id, nil
}

// isDuplicate 判断重复项是否成立。
func isDuplicate(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate")
}

// stringValue 安全读取字符串指针的值。
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// timeValue 返回时间值。
func timeValue(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.Format(time.RFC3339Nano)
}

// stringPtr 将非空字符串转换为字符串指针。
func stringPtr(value string) *string { return &value }
