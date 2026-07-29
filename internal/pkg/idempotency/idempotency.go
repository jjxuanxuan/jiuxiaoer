package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

type Record struct {
	ID             uint64
	ActorType      string
	ActorID        uint64
	Method         string
	Path           string
	KeyHash        string
	RequestHash    string
	ResponseStatus *int
	ResponseBody   datatypes.JSON
	Status         string
	LockedUntil    *time.Time
	ExpiredAt      time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Record) TableName() string { return "idempotency_keys" }

type Store struct {
	db *gorm.DB
}

const (
	errorCodeKeyReused  = "IDEMPOTENCY_KEY_REUSED"
	errorCodeInProgress = "IDEMPOTENCY_IN_PROGRESS"
	errorCodeClaimLost  = "IDEMPOTENCY_CLAIM_LOST"
)

// NewStore 让幂等操作和业务事务保持在同一个事务边界内。
// 调用方必须传入执行受保护写操作的同一个事务。
func NewStore(db *gorm.DB) *Store {
	return &Store{db: db}
}

// RequestHash 将幂等键绑定到一次确定的请求内容。
// 同一个幂等键携带不同 JSON 时会被视为冲突。
func RequestHash(value any) string {
	payload, _ := json.Marshal(value)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// ResourceRequestHash 将幂等命令绑定到其目标具体资源。Gin 的 FullPath
// 返回路由模板（例如 /orders/:id/cancel），因此如果只对请求体做哈希，
// 同一参与者、键和请求体组合可能重放为其他订单生成的响应。
//
// action 是协议判别值而非用户数据，因此会被规范化。resourceID 和 body
// 保留其 JSON 表示，使调用方可以使用数字 ID、稳定业务编号或复合资源引用。
func ResourceRequestHash(action string, resourceID any, body any) string {
	return RequestHash(struct {
		Action     string `json:"action"`
		ResourceID any    `json:"resource_id"`
		Body       any    `json:"body"`
	}{
		Action:     strings.ToLower(strings.TrimSpace(action)),
		ResourceID: resourceID,
		Body:       body,
	})
}

// KeyHash 避免直接存储客户端原始幂等键，同时保留查询能力。
func KeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Start 尝试为当前 actor/path 占用一个幂等键。
func (s *Store) Start(ctx context.Context, tx *gorm.DB, id uint64, actorType string, actorID uint64, method string, path string, key string, requestHash string) (bool, error) {
	return s.StartAt(
		ctx,
		tx,
		id,
		actorType,
		actorID,
		method,
		path,
		key,
		requestHash,
		time.Now(),
	)
}

// StartAt 是显式传入租约时钟的 Start。
// 需要确定性边界测试的服务可以传入配置时钟，而无需修改共享状态。
func (s *Store) StartAt(
	ctx context.Context,
	tx *gorm.DB,
	id uint64,
	actorType string,
	actorID uint64,
	method string,
	path string,
	key string,
	requestHash string,
	now time.Time,
) (bool, error) {
	if key == "" {
		return false, problem.InvalidArgument("IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key is required")
	}
	if len(key) < 8 || len(key) > 128 {
		return false, problem.InvalidArgument("IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must be between 8 and 128 characters")
	}

	lockedUntil := now.Add(30 * time.Second)
	keyHash := KeyHash(key)

	candidate := Record{
		ID:          id,
		ActorType:   actorType,
		ActorID:     actorID,
		Method:      method,
		Path:        path,
		KeyHash:     keyHash,
		RequestHash: requestHash,
		Status:      "processing",
		LockedUntil: &lockedUntil,
		ExpiredAt:   now.Add(24 * time.Hour),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	// 重复插入会获取唯一键写锁，不依赖受影响行数报告。
	// 随后的受保护 UPDATE 要么采用本次尝试的新 ID 作为隔离令牌，
	// 要么保持当前持有者不变。
	// GORM 会根据当前数据库生成冲突子句，因此生产环境保持 MySQL 语义，
	// 无需测试方言分支。
	if err := tx.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&candidate).Error; err != nil {
		return false, err
	}
	if err := tx.WithContext(ctx).
		Model(&Record{}).
		Where(
			"actor_type = ? AND actor_id = ? AND path = ? AND key_hash = ?",
			actorType,
			actorID,
			path,
			keyHash,
		).
		Where(
			"request_hash = ? AND status <> 'succeeded' AND (locked_until IS NULL OR locked_until <= ?)",
			requestHash,
			now,
		).
		Updates(map[string]any{
			"id":           id,
			"status":       "processing",
			"locked_until": lockedUntil,
			"updated_at":   now,
		}).Error; err != nil {
		return false, err
	}

	var existing Record
	if err := tx.WithContext(ctx).
		Where("actor_type = ? AND actor_id = ? AND path = ? AND key_hash = ?", actorType, actorID, path, keyHash).
		First(&existing).Error; err != nil {
		return false, err
	}
	// 插入成功或重新认领过期租约时，会采用本次尝试的新 Snowflake ID。
	// 被拒绝或无变化的重复操作会保留原 ID。
	// 该认领令牌在 MySQL 各种受影响行数模式下均保持稳定，
	// 包括配置 clientFoundRows=true 的 DSN。
	if existing.ID == id {
		return true, nil
	}
	return existingClaimResult(existing, requestHash, now)
}

// Succeed 保存最终响应，便于重试请求返回同一结果。
//
// 该兼容方法只适用于 Start 与业务写、Succeed 位于同一个数据库事务的调用方；
// 跨事务或跨外部调用的流程必须使用 SucceedOwned 并携带 claim ID。
func (s *Store) Succeed(ctx context.Context, tx *gorm.DB, actorType string, actorID uint64, path string, key string, response any) error {
	status := http.StatusOK
	payload, _ := json.Marshal(response)
	return tx.WithContext(ctx).Model(&Record{}).
		Where("actor_type = ? AND actor_id = ? AND path = ? AND key_hash = ?", actorType, actorID, path, KeyHash(key)).
		Updates(map[string]any{
			"status":          "succeeded",
			"response_status": status,
			"response_body":   datatypes.JSON(payload),
			"locked_until":    nil,
		}).Error
}

// SucceedOwned 只允许当前 claim owner 持久化最终响应。过期 owner 在新
// owner 重领后得到 IDEMPOTENCY_CLAIM_LOST，不会覆盖新 owner 的响应。
func (s *Store) SucceedOwned(
	ctx context.Context,
	tx *gorm.DB,
	expectedClaimID uint64,
	actorType string,
	actorID uint64,
	path string,
	key string,
	response any,
) error {
	status := http.StatusOK
	payload, _ := json.Marshal(response)
	result := tx.WithContext(ctx).Model(&Record{}).
		Where(
			"id = ? AND actor_type = ? AND actor_id = ? AND path = ? AND key_hash = ? AND status = 'processing'",
			expectedClaimID,
			actorType,
			actorID,
			path,
			KeyHash(key),
		).
		Updates(map[string]any{
			"status":          "succeeded",
			"response_status": status,
			"response_body":   datatypes.JSON(payload),
			"locked_until":    nil,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return idempotencyClaimLost()
	}
	return nil
}

// Fail 将当前幂等处理记录标记为失败。
//
// 该兼容方法只适用于 Start 与 Fail 位于同一个数据库事务的调用方；跨事务
// 清理必须使用 FailOwned。即使误用于已完成记录，也绝不会把 succeeded
// 降级为 failed。
func (s *Store) Fail(ctx context.Context, tx *gorm.DB, actorType string, actorID uint64, path string, key string) error {
	return tx.WithContext(ctx).Model(&Record{}).
		Where(
			"actor_type = ? AND actor_id = ? AND path = ? AND key_hash = ? AND status <> 'succeeded'",
			actorType,
			actorID,
			path,
			KeyHash(key),
		).
		Updates(map[string]any{"status": "failed", "locked_until": nil}).Error
}

// FailOwned 只允许当前 claim owner 释放自己的处理租约。它既不能清除新
// owner 的 lease，也不能把任何 succeeded 记录降级为 failed。
func (s *Store) FailOwned(
	ctx context.Context,
	tx *gorm.DB,
	expectedClaimID uint64,
	actorType string,
	actorID uint64,
	path string,
	key string,
) error {
	result := tx.WithContext(ctx).Model(&Record{}).
		Where(
			"id = ? AND actor_type = ? AND actor_id = ? AND path = ? AND key_hash = ? AND status = 'processing'",
			expectedClaimID,
			actorType,
			actorID,
			path,
			KeyHash(key),
		).
		Updates(map[string]any{"status": "failed", "locked_until": nil})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return idempotencyClaimLost()
	}
	return nil
}

// CachedResponse 读取已完成幂等请求的缓存响应。
func (s *Store) CachedResponse(ctx context.Context, tx *gorm.DB, actorType string, actorID uint64, path string, key string, out any) (bool, error) {
	var record Record
	err := tx.WithContext(ctx).
		Where("actor_type = ? AND actor_id = ? AND path = ? AND key_hash = ? AND status = 'succeeded'", actorType, actorID, path, KeyHash(key)).
		First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(record.ResponseBody) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(record.ResponseBody, out); err != nil {
		return false, err
	}
	return true, nil
}

// ReplayCompleted 是昂贵外部预计算前使用的无修改快速路径。它会提前拒绝
// 已绑定其他请求数据的键和未过期的处理租约。事务内的 Start 调用仍是
// 新请求以及重新认领已过期或失败尝试的权威入口。
func (s *Store) ReplayCompleted(ctx context.Context, db *gorm.DB, actorType string, actorID uint64, path, key, requestHash string, out any) (bool, error) {
	if key == "" || db == nil {
		return false, nil
	}
	var record Record
	err := db.WithContext(ctx).
		Where("actor_type = ? AND actor_id = ? AND path = ? AND key_hash = ?", actorType, actorID, path, KeyHash(key)).
		First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if record.RequestHash != requestHash {
		return false, idempotencyKeyReused()
	}
	if record.Status != "succeeded" {
		if record.Status == "processing" && record.LockedUntil != nil && record.LockedUntil.After(time.Now()) {
			return false, idempotencyInProgress()
		}
		return false, nil
	}
	if len(record.ResponseBody) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(record.ResponseBody, out); err != nil {
		return false, err
	}
	return true, nil
}

func existingClaimResult(existing Record, requestHash string, now time.Time) (bool, error) {
	if existing.RequestHash != requestHash {
		return false, idempotencyKeyReused()
	}
	if existing.Status == "succeeded" {
		return false, nil
	}
	if existing.Status == "processing" && existing.LockedUntil != nil && existing.LockedUntil.After(now) {
		return false, idempotencyInProgress()
	}
	// 对其他非终态执行无操作更新插入，表示另一事务已在本事务检查前赢得认领。
	return false, idempotencyInProgress()
}

func idempotencyKeyReused() *problem.Details {
	return problem.Conflict(errorCodeKeyReused, "same idempotency key was used for a different request")
}

func idempotencyInProgress() *problem.Details {
	return problem.Conflict(errorCodeInProgress, "request with the same idempotency key is still processing")
}

func idempotencyClaimLost() *problem.Details {
	return problem.Conflict(errorCodeClaimLost, "idempotency claim is no longer owned by this request")
}

// IsClaimLost 判断带持有者隔离的完成或释放操作，
// 是否因其他请求已重新认领或完成认领而被拒绝。
func IsClaimLost(err error) bool {
	var details *problem.Details
	return errors.As(err, &details) && details.ErrorCode == errorCodeClaimLost
}
