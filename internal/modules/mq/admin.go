package mq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/infra/rabbitmq"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
	"jiuxiaoer-admin/backend-go/internal/pkg/securevalue"
)

type DeadLetter struct {
	ID            uint64
	DeadNo        string
	ConsumerName  string
	EventID       string
	EventType     string
	EventVersion  uint
	AggregateType *string
	AggregateID   *string
	ErrorCode     string
	ErrorSafe     *string
	PayloadHash   string
	RetryCount    uint
	Status        string
	Version       uint
	FirstFailedAt time.Time
	DeadAt        time.Time
	LastReplayID  *uint64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (DeadLetter) TableName() string { return "mq_dead_letters" }

type DeadLetterReplay struct {
	ID                 uint64
	DeadLetterID       uint64
	ReplayEventID      string
	ActorAdminID       uint64
	ReasonCode         string
	Reason             string
	IdempotencyKeyHash string
	Status             string
	RequestID          *string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (DeadLetterReplay) TableName() string { return "mq_dead_letter_replays" }

type DeadLetterDTO struct {
	ID            string `json:"id"`
	DeadNo        string `json:"dead_no"`
	ConsumerName  string `json:"consumer_name"`
	EventID       string `json:"event_id"`
	EventType     string `json:"event_type"`
	EventVersion  uint   `json:"event_version"`
	AggregateType string `json:"aggregate_type,omitempty"`
	AggregateID   string `json:"aggregate_id,omitempty"`
	ErrorCode     string `json:"error_code"`
	ErrorSafe     string `json:"error_safe,omitempty"`
	PayloadHash   string `json:"payload_hash"`
	RetryCount    uint   `json:"retry_count"`
	Status        string `json:"status"`
	Version       uint   `json:"version"`
	FirstFailedAt string `json:"first_failed_at"`
	DeadAt        string `json:"dead_at"`
	CreatedAt     string `json:"created_at"`
}

type ReplayRequest struct {
	ReasonCode      string `json:"reason_code" binding:"required,max=64"`
	Reason          string `json:"reason" binding:"required,min=2,max=512"`
	ExpectedVersion uint   `json:"expected_version" binding:"required,min=1"`
}

type ReplayDTO struct {
	ID            string `json:"id"`
	DeadLetterID  string `json:"dead_letter_id"`
	ReplayEventID string `json:"replay_event_id"`
	Status        string `json:"status"`
	CreatedAt     string `json:"created_at"`
}

type QueueHealth struct {
	Queue          string `json:"queue"`
	Ready          int    `json:"ready"`
	Unacknowledged int    `json:"unacked"`
	Consumers      int    `json:"consumers"`
	OldestSeconds  int64  `json:"oldest_seconds"`
	Error          string `json:"error,omitempty"`
}

type MQHealthDTO struct {
	BrokerConnected bool            `json:"broker_connected"`
	TopologyVersion string          `json:"topology_version"`
	Partial         bool            `json:"partial"`
	TopologyDrift   []TopologyDrift `json:"topology_drift"`
	Queues          []QueueHealth   `json:"queues"`
	OutboxPending   int64           `json:"outbox_pending"`
	OutboxDead      int64           `json:"outbox_dead"`
	DeadLettersOpen int64           `json:"dead_letters_open"`
}

type AdminService struct {
	db       *gorm.DB
	rabbit   *rabbitmq.Manager
	registry *EventRegistry
	ids      idSource
}

// NewAdminService 创建并初始化管理端服务。
func NewAdminService(db *gorm.DB, rabbit *rabbitmq.Manager, registry *EventRegistry, ids idSource) *AdminService {
	return &AdminService{db: db, rabbit: rabbit, registry: registry, ids: ids}
}

// Health 返回健康状态。
func (s *AdminService) Health(ctx context.Context, claims *auth.Claims) (MQHealthDTO, error) {
	if _, err := mqAdminID(claims, "mq:health:view"); err != nil {
		return MQHealthDTO{}, err
	}
	result := MQHealthDTO{TopologyVersion: topologyContractVersion, Partial: true}
	if s.db != nil {
		_ = s.db.WithContext(ctx).Table("outbox_events").Where("status='pending'").Count(&result.OutboxPending).Error
		_ = s.db.WithContext(ctx).Table("outbox_events").Where("status='dead'").Count(&result.OutboxDead).Error
		_ = s.db.WithContext(ctx).Table("mq_dead_letters").Where("status='open'").Count(&result.DeadLettersOpen).Error
	}
	if s.rabbit == nil || !s.rabbit.Healthy() {
		return result, nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := s.rabbit.Connection(checkCtx)
	if err != nil {
		return result, nil
	}
	result.BrokerConnected = true
	managedDrift, managedQueues, complete := VerifyManagedTopology(checkCtx, s.rabbit, DefaultTopology())
	if complete {
		result.Partial = false
		result.TopologyDrift = managedDrift
		for _, queue := range DefaultTopology().Queues {
			observation, ok := managedQueues[queue.Name]
			if !ok {
				result.Queues = append(result.Queues, QueueHealth{Queue: queue.Name, Error: "missing_or_unavailable"})
				continue
			}
			oldest := int64(0)
			if observation.HeadMessageTimestamp > 0 {
				oldest = time.Now().Unix() - observation.HeadMessageTimestamp
				if oldest < 0 {
					oldest = 0
				}
			}
			result.Queues = append(result.Queues, QueueHealth{Queue: queue.Name, Ready: observation.Ready, Unacknowledged: observation.Unacknowledged, Consumers: observation.Consumers, OldestSeconds: oldest})
		}
		return result, nil
	}
	result.TopologyDrift = VerifyTopology(conn, DefaultTopology())
	for _, queue := range DefaultTopology().Queues {
		channel, channelErr := conn.Channel()
		if channelErr != nil {
			result.Queues = append(result.Queues, QueueHealth{Queue: queue.Name, Error: "inspect_failed"})
			continue
		}
		inspection, inspectErr := channel.QueueInspect(queue.Name)
		_ = channel.Close()
		item := QueueHealth{Queue: queue.Name}
		if inspectErr != nil {
			item.Error = "missing_or_unavailable"
		} else {
			item.Ready, item.Consumers = inspection.Messages, inspection.Consumers
		}
		result.Queues = append(result.Queues, item)
	}
	return result, nil
}

// ListDeadLetters 查询死信 Letters列表。
func (s *AdminService) ListDeadLetters(ctx context.Context, claims *auth.Claims, query pagination.Query) ([]DeadLetterDTO, string, error) {
	if _, err := mqAdminID(claims, "mq:dead_letter:list"); err != nil {
		return nil, "", err
	}
	db := s.db.WithContext(ctx).Model(&DeadLetter{})
	var err error
	db, err = pagination.ApplyFilter(db, query.Filter, map[string]string{
		"consumer_name": "consumer_name", "event_type": "event_type", "status": "status",
		"error_code": "error_code", "created_at": "created_at",
	})
	if err != nil {
		return nil, "", err
	}
	db, err = pagination.ApplyOrder(db, query.OrderBy, map[string]string{"created_at": "created_at", "id": "id"}, "created_at DESC,id DESC")
	if err != nil {
		return nil, "", err
	}
	var rows []DeadLetter
	if err := db.Offset(query.Offset).Limit(query.PageSize + 1).Find(&rows).Error; err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		rows = rows[:query.PageSize]
		next = pagination.NextPageToken(query)
	}
	items := make([]DeadLetterDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, deadLetterDTO(row))
	}
	return items, next, nil
}

// GetDeadLetter 获取死信 Letter。
func (s *AdminService) GetDeadLetter(ctx context.Context, claims *auth.Claims, idRaw string) (DeadLetterDTO, error) {
	if _, err := mqAdminID(claims, "mq:dead_letter:list"); err != nil {
		return DeadLetterDTO{}, err
	}
	id, err := strconv.ParseUint(idRaw, 10, 64)
	if err != nil {
		return DeadLetterDTO{}, problem.NotFound("MQ_DEAD_NOT_FOUND", "dead letter not found")
	}
	var row DeadLetter
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return DeadLetterDTO{}, problem.NotFound("MQ_DEAD_NOT_FOUND", "dead letter not found")
		}
		return DeadLetterDTO{}, err
	}
	return deadLetterDTO(row), nil
}

// Replay 返回Replay。
func (s *AdminService) Replay(ctx context.Context, claims *auth.Claims, idRaw, key string, request ReplayRequest) (ReplayDTO, error) {
	actorID, err := mqAdminID(claims, "mq:dead_letter:replay")
	if err != nil {
		return ReplayDTO{}, err
	}
	if key == "" {
		return ReplayDTO{}, problem.InvalidArgument("IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key header is required")
	}
	deadID, err := strconv.ParseUint(idRaw, 10, 64)
	if err != nil {
		return ReplayDTO{}, problem.NotFound("MQ_DEAD_NOT_FOUND", "dead letter not found")
	}
	hash := securevalue.Digest(key)
	var output ReplayDTO
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing DeadLetterReplay
		if err := tx.Where("actor_admin_id=? AND idempotency_key_hash=?", actorID, hash).First(&existing).Error; err == nil {
			if existing.DeadLetterID != deadID {
				return problem.Conflict("IDEMPOTENCY_KEY_REUSED", "idempotency key was used for another dead letter")
			}
			output = replayDTO(existing)
			return nil
		} else if err != nil && err != gorm.ErrRecordNotFound {
			return err
		}
		var dead DeadLetter
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&dead, deadID).Error; err != nil {
			return problem.NotFound("MQ_DEAD_NOT_FOUND", "dead letter not found")
		}
		if dead.Version != request.ExpectedVersion {
			return problem.Conflict("VERSION_CONFLICT", "dead letter version changed")
		}
		if dead.Status != "open" {
			return problem.Conflict("MQ_DEAD_ALREADY_REPLAYED", "dead letter is not open")
		}
		definition, registered := s.registry.Lookup(dead.EventType)
		if !registered || !definition.Routable() || dead.EventVersion != definition.Version {
			return problem.New(http.StatusUnprocessableEntity, "SCHEMA_NOT_SUPPORTED", "Schema Not Supported", "event contract is no longer replayable")
		}
		var source OutboxEvent
		if err := tx.Where("event_id=?", dead.EventID).First(&source).Error; err != nil {
			return problem.New(http.StatusUnprocessableEntity, "REPLAY_SOURCE_NOT_FOUND", "Replay Source Not Found", "source outbox event is unavailable")
		}
		replayEventID := uuid.NewString()
		outboxID := s.ids.Next()
		if err := tx.Table("outbox_events").Create(map[string]any{
			"id": outboxID, "event_id": replayEventID, "event_type": source.EventType,
			"event_version": definition.Version, "spec_version": envelopeSpecVersion,
			"aggregate_type": source.AggregateType, "aggregate_id": source.AggregateID,
			"producer": definition.Owner, "schema_ref": fmt.Sprintf("event://%s/%d", source.EventType, definition.Version),
			"partition_key": partitionKey(source), "replay_of_event_id": source.EventID,
			"payload": source.Payload, "status": "pending", "request_id": requestctx.RequestIDPtr(ctx),
		}).Error; err != nil {
			return err
		}
		replay := DeadLetterReplay{ID: s.ids.Next(), DeadLetterID: dead.ID, ReplayEventID: replayEventID, ActorAdminID: actorID, ReasonCode: request.ReasonCode, Reason: request.Reason, IdempotencyKeyHash: hash, Status: "requested", RequestID: requestctx.RequestIDPtr(ctx)}
		if err := tx.Create(&replay).Error; err != nil {
			return err
		}
		if err := tx.Model(&DeadLetter{}).Where("id=? AND version=?", dead.ID, dead.Version).Updates(map[string]any{"status": "replayed", "version": gorm.Expr("version + 1"), "last_replay_id": replay.ID}).Error; err != nil {
			return err
		}
		after, _ := json.Marshal(map[string]any{"dead_letter_id": idRaw, "replay_event_id": replayEventID, "reason_code": request.ReasonCode})
		if err := tx.Table("audit_logs").Create(map[string]any{"id": s.ids.Next(), "actor_type": "admin", "actor_id": actorID, "action": "mq.dead_letter.replay", "resource_type": "mq_dead_letter", "resource_id": dead.ID, "after_data": datatypes.JSON(after), "result": "success", "request_id": requestctx.RequestIDPtr(ctx)}).Error; err != nil {
			return err
		}
		output = replayDTO(replay)
		return nil
	})
	return output, err
}

// Verify 核验映射是否有效。
func (s *AdminService) Verify(ctx context.Context, claims *auth.Claims) (map[string]any, error) {
	if _, err := mqAdminID(claims, "mq:topology:verify"); err != nil {
		return nil, err
	}
	if s.rabbit == nil || !s.rabbit.Healthy() {
		return map[string]any{"topology_version": topologyContractVersion, "broker_connected": false, "drift": []TopologyDrift{}}, nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := s.rabbit.Connection(checkCtx)
	if err != nil {
		return nil, err
	}
	drift, _, complete := VerifyManagedTopology(checkCtx, s.rabbit, DefaultTopology())
	if !complete {
		drift = VerifyTopology(conn, DefaultTopology())
	}
	return map[string]any{"topology_version": topologyContractVersion, "broker_connected": true, "valid": len(drift) == 0, "partial": !complete, "drift": drift}, nil
}

type AdminHandler struct{ service *AdminService }

// NewAdminHandler 创建并初始化管理端 Handler。
func NewAdminHandler(service *AdminService) *AdminHandler { return &AdminHandler{service: service} }

// RegisterAdminRoutes 注册管理端 Routes。
func RegisterAdminRoutes(group *gin.RouterGroup, handler *AdminHandler) {
	group.GET("/health", handler.Health)
	group.GET("/dead-letters", handler.ListDeadLetters)
	group.GET("/dead-letters/:id", handler.GetDeadLetter)
	group.POST("/dead-letters/:id/replay", handler.Replay)
	group.POST("/topology/verify", handler.Verify)
}

// Health 处理健康状态相关逻辑。
func (h *AdminHandler) Health(c *gin.Context) {
	claims, ok := mqClaims(c)
	if !ok {
		return
	}
	item, err := h.service.Health(c.Request.Context(), claims)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

// ListDeadLetters 查询死信 Letters列表。
func (h *AdminHandler) ListDeadLetters(c *gin.Context) {
	claims, ok := mqClaims(c)
	if !ok {
		return
	}
	query, err := pagination.FromGin(c)
	if err != nil {
		response.Error(c, err)
		return
	}
	items, next, err := h.service.ListDeadLetters(c.Request.Context(), claims, query)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.Page(c, items, next)
}

// GetDeadLetter 获取死信 Letter。
func (h *AdminHandler) GetDeadLetter(c *gin.Context) {
	claims, ok := mqClaims(c)
	if !ok {
		return
	}
	item, err := h.service.GetDeadLetter(c.Request.Context(), claims, c.Param("id"))
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

// Replay 处理Replay相关逻辑。
func (h *AdminHandler) Replay(c *gin.Context) {
	claims, ok := mqClaims(c)
	if !ok {
		return
	}
	var request ReplayRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		response.Error(c, problem.InvalidArgument("VALIDATION_FAILED", err.Error()))
		return
	}
	item, err := h.service.Replay(c.Request.Context(), claims, c.Param("id"), c.GetHeader("Idempotency-Key"), request)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

// Verify 核验消息队列是否有效。
func (h *AdminHandler) Verify(c *gin.Context) {
	claims, ok := mqClaims(c)
	if !ok {
		return
	}
	item, err := h.service.Verify(c.Request.Context(), claims)
	if err != nil {
		response.Error(c, err)
		return
	}
	response.OK(c, item)
}

// mqClaims 返回消息队列认证声明。
func mqClaims(c *gin.Context) (*auth.Claims, bool) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_UNAUTHORIZED", "unauthorized"))
	}
	return claims, ok
}

// mqAdminID 返回消息队列管理端ID。
func mqAdminID(claims *auth.Claims, permission string) (uint64, error) {
	if claims == nil || claims.AccountType != "admin" || !hasPermission(claims.Permissions, permission) {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "MQ operations permission required")
	}
	id, err := strconv.ParseUint(claims.AdminUserID, 10, 64)
	if err != nil || id == 0 {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "admin identity is invalid")
	}
	return id, nil
}

// hasPermission 判断是否存在权限。
func hasPermission(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// deadLetterDTO 返回死信 Letter DTO。
func deadLetterDTO(row DeadLetter) DeadLetterDTO {
	return DeadLetterDTO{ID: fmt.Sprint(row.ID), DeadNo: row.DeadNo, ConsumerName: row.ConsumerName, EventID: row.EventID, EventType: row.EventType, EventVersion: row.EventVersion, AggregateType: stringValue(row.AggregateType), AggregateID: stringValue(row.AggregateID), ErrorCode: row.ErrorCode, ErrorSafe: stringValue(row.ErrorSafe), PayloadHash: row.PayloadHash, RetryCount: row.RetryCount, Status: row.Status, Version: row.Version, FirstFailedAt: row.FirstFailedAt.UTC().Format(time.RFC3339Nano), DeadAt: row.DeadAt.UTC().Format(time.RFC3339Nano), CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339Nano)}
}

// replayDTO 返回replay DTO。
func replayDTO(row DeadLetterReplay) ReplayDTO {
	return ReplayDTO{ID: fmt.Sprint(row.ID), DeadLetterID: fmt.Sprint(row.DeadLetterID), ReplayEventID: row.ReplayEventID, Status: row.Status, CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339Nano)}
}

// stringValue 安全读取字符串指针的值。
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
