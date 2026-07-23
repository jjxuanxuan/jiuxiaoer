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

// ResourceRequestHash binds an idempotent command to the concrete resource it
// targets. Gin's FullPath returns a route template (for example
// /orders/:id/cancel), so hashing only the request body would allow the same
// actor/key/body tuple to replay a response produced for a different order.
//
// action is normalized because it is a protocol discriminator, not user data.
// resourceID and body retain their JSON representation so callers can use a
// numeric ID, a stable business number, or a composite resource reference.
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

// Start 尝试为当前 actor/path 占用一个幂等键。插入和过期锁重领在
// 单条 upsert 中完成，避免多个事务先持有重复键共享锁、再升级为写锁
// 时产生死锁。
func (s *Store) Start(ctx context.Context, tx *gorm.DB, id uint64, actorType string, actorID uint64, method string, path string, key string, requestHash string) (bool, error) {
	if key == "" {
		return false, problem.InvalidArgument("IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key is required")
	}
	if len(key) < 8 || len(key) > 128 {
		return false, problem.InvalidArgument("IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must be between 8 and 128 characters")
	}

	now := time.Now()
	lockedUntil := now.Add(30 * time.Second)
	keyHash := KeyHash(key)

	result := tx.WithContext(ctx).Exec(`
		INSERT INTO idempotency_keys
			(id, actor_type, actor_id, method, path, key_hash, request_hash, status, locked_until, expired_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'processing', ?, ?)
		ON DUPLICATE KEY UPDATE
			status = IF(
				request_hash = VALUES(request_hash)
				AND status <> 'succeeded'
				AND (locked_until IS NULL OR locked_until <= ?),
				'processing', status
			),
			locked_until = IF(
				request_hash = VALUES(request_hash)
				AND status <> 'succeeded'
				AND (locked_until IS NULL OR locked_until <= ?),
				VALUES(locked_until), locked_until
			)
	`, id, actorType, actorID, method, path, keyHash, requestHash, lockedUntil, now.Add(24*time.Hour), now, now)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 1 || result.RowsAffected == 2 {
		return true, nil
	}

	var existing Record
	if err := tx.WithContext(ctx).
		Where("actor_type = ? AND actor_id = ? AND path = ? AND key_hash = ?", actorType, actorID, path, keyHash).
		First(&existing).Error; err != nil {
		return false, err
	}
	return existingClaimResult(existing, requestHash, now)
}

// Succeed 保存最终响应，便于重试请求返回同一结果。
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

// Fail 将当前幂等处理记录标记为失败。
func (s *Store) Fail(ctx context.Context, tx *gorm.DB, actorType string, actorID uint64, path string, key string) error {
	return tx.WithContext(ctx).Model(&Record{}).
		Where("actor_type = ? AND actor_id = ? AND path = ? AND key_hash = ?", actorType, actorID, path, KeyHash(key)).
		Updates(map[string]any{"status": "failed", "locked_until": nil}).Error
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

// ReplayCompleted is a non-mutating fast path used before expensive external
// precomputation. It rejects a key bound to different request data and an
// unexpired processing lease early. The transactional Start call remains the
// authority for new requests and reclaiming expired or failed attempts.
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
	// A no-op upsert for any other non-terminal state means another transaction
	// won the claim before this transaction could inspect it.
	return false, idempotencyInProgress()
}

func idempotencyKeyReused() *problem.Details {
	return problem.Conflict(errorCodeKeyReused, "same idempotency key was used for a different request")
}

func idempotencyInProgress() *problem.Details {
	return problem.Conflict(errorCodeInProgress, "request with the same idempotency key is still processing")
}
