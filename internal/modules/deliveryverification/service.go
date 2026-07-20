package deliveryverification

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/securevalue"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// GeneratePair 生成配对。
func GeneratePair(ctx context.Context, tx *gorm.DB, cfg config.CP1Config, ids *snowflake.Generator, deliveryID uint64) error {
	if cfg.PickupVerificationMode == "off" && cfg.DeliveryVerificationMode == "off" {
		return nil
	}
	for _, stage := range []string{"pickup", "delivery"} {
		mode := cfg.PickupVerificationMode
		if stage == "delivery" {
			mode = cfg.DeliveryVerificationMode
		}
		if mode == "off" {
			continue
		}
		code, e := newCode()
		if e != nil {
			return e
		}
		cipher, e := securevalue.Seal(cfg.DataEncryptionKey, code)
		if e != nil {
			return e
		}
		max := cfg.VerificationMaxAttempts
		if max == 0 {
			max = 5
		}
		ttl := cfg.VerificationTTL
		if ttl == 0 {
			ttl = 30 * time.Minute
		}
		row := Verification{ID: ids.Next(), DeliveryOrderID: deliveryID, Stage: stage, CodeHash: hashCode(cfg, deliveryID, stage, code), CodeCiphertext: cipher, CodeMask: "****" + code[len(code)-2:], PolicyVersion: "cp1-v1", Status: "active", MaxAttempts: uint(max), ExpiresAt: time.Now().Add(ttl), Version: 1}
		if e := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; e != nil {
			return e
		}
	}
	return nil
}

// VerifyLocked 核验Locked是否有效。
// VerifyLocked must run while the delivery row is locked. A rejection is
// returned separately so the caller can commit the failed-attempt audit before
// sending the business error to the client.
func VerifyLocked(ctx context.Context, tx *gorm.DB, cfg config.CP1Config, ids *snowflake.Generator, deliveryID uint64, stage, code string, actorID uint64) (*problem.Details, error) {
	mode := cfg.PickupVerificationMode
	if stage == "delivery" {
		mode = cfg.DeliveryVerificationMode
	}
	if mode == "off" || mode == "" {
		return nil, nil
	}
	var row Verification
	e := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("delivery_order_id=? AND stage=?", deliveryID, stage).First(&row).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		if mode == "enforce" {
			return problem.New(422, "VERIFICATION_CODE_REQUIRED", "Unprocessable Entity", "verification code is required"), nil
		}
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	now := time.Now()
	if limited, e := rateLimited(ctx, tx, actorID, now); e != nil {
		return nil, e
	} else if limited {
		return problem.TooManyRequests("VERIFICATION_RATE_LIMITED", "too many verification attempts"), nil
	}
	failure := ""
	status := row.Status
	if status == "locked" && row.LockedUntil != nil && row.LockedUntil.After(now) {
		failure = "VERIFICATION_LOCKED"
	} else if status == "locked" {
		row.Status = "active"
		row.LockedUntil = nil
		if e := tx.Model(&Verification{}).Where("id=?", row.ID).Updates(map[string]any{"status": "active", "locked_until": nil}).Error; e != nil {
			return nil, e
		}
	} else if status == "verified" {
		return nil, nil
	} else if status != "active" && status != "expired" {
		return problem.Conflict("VERIFICATION_INVALID_STATUS", "verification is not active"), nil
	} else if !row.ExpiresAt.After(now) {
		failure = "VERIFICATION_EXPIRED"
		_ = tx.Model(&Verification{}).Where("id=?", row.ID).Update("status", "expired").Error
	} else if code == "" {
		failure = "VERIFICATION_CODE_REQUIRED"
	} else if !securevalue.EqualHMAC(row.CodeHash, hashCode(cfg, deliveryID, stage, code)) {
		failure = "VERIFICATION_CODE_INVALID"
	}
	attemptNo := row.FailedAttempts + 1
	if failure != "" {
		newStatus := row.Status
		var lockedUntil *time.Time
		if failure == "VERIFICATION_CODE_INVALID" {
			row.FailedAttempts++
			attemptNo = row.FailedAttempts
			if row.FailedAttempts >= row.MaxAttempts {
				newStatus = "locked"
				d := cfg.VerificationLockDuration
				if d == 0 {
					d = 15 * time.Minute
				}
				v := now.Add(d)
				lockedUntil = &v
			}
			if e := tx.Model(&Verification{}).Where("id=?", row.ID).Updates(map[string]any{"failed_attempts": row.FailedAttempts, "status": newStatus, "locked_until": lockedUntil, "version": gorm.Expr("version+1")}).Error; e != nil {
				return nil, e
			}
		}
		if e := createAttempt(ctx, tx, ids, row, actorID, "failed", failure, attemptNo); e != nil {
			return nil, e
		}
		if mode == "observe" {
			return nil, nil
		}
		httpCode := 422
		if failure == "VERIFICATION_LOCKED" {
			httpCode = 423
		} else if failure == "VERIFICATION_EXPIRED" {
			httpCode = 410
		}
		detail := problem.New(httpCode, failure, httpTitle(httpCode), "verification failed")
		remaining := uint(0)
		if row.MaxAttempts > row.FailedAttempts {
			remaining = row.MaxAttempts - row.FailedAttempts
		}
		detail.Data = map[string]any{"remaining_attempts": remaining, "locked_until": timeString(lockedUntil)}
		return detail, nil
	}
	result := tx.Model(&Verification{}).Where("id=? AND status='active'", row.ID).Updates(map[string]any{"status": "verified", "verified_at": now, "verified_by_type": "rider", "verified_by_id": actorID, "version": gorm.Expr("version+1")})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return problem.Conflict("VERIFICATION_INVALID_STATUS", "verification is not active"), nil
	}
	if e := createAttempt(ctx, tx, ids, row, actorID, "success", "", row.FailedAttempts+1); e != nil {
		return nil, e
	}
	return nil, nil
}

// rateLimited 返回速率 Limited。
func rateLimited(ctx context.Context, tx *gorm.DB, actorID uint64, now time.Time) (bool, error) {
	window := now.Add(-time.Minute)
	var actorFailures int64
	if e := tx.Model(&Attempt{}).Where("actor_type='rider' AND actor_id=? AND result='failed' AND created_at>=?", actorID, window).Count(&actorFailures).Error; e != nil {
		return false, e
	}
	if actorFailures >= 20 {
		return true, nil
	}
	if ip := requestctx.IPPtr(ctx); ip != nil && *ip != "" {
		hash := securevalue.Digest(*ip)
		var ipFailures int64
		if e := tx.Model(&Attempt{}).Where("ip_hash=? AND result='failed' AND created_at>=?", hash, window).Count(&ipFailures).Error; e != nil {
			return false, e
		}
		if ipFailures >= 30 {
			return true, nil
		}
	}
	return false, nil
}

// createAttempt 创建尝试。
func createAttempt(ctx context.Context, tx *gorm.DB, ids *snowflake.Generator, row Verification, actor uint64, result, failure string, no uint) error {
	var f *string
	if failure != "" {
		f = &failure
	}
	ip := ""
	if v := requestctx.IPPtr(ctx); v != nil {
		ip = securevalue.Digest(*v)
	}
	var ipPtr *string
	if ip != "" {
		ipPtr = &ip
	}
	return tx.Create(&Attempt{ID: ids.Next(), VerificationID: row.ID, DeliveryOrderID: row.DeliveryOrderID, Stage: row.Stage, ActorType: "rider", ActorID: actor, Result: result, FailureCode: f, AttemptNo: no, RequestID: requestctx.RequestIDPtr(ctx), IPHash: ipPtr}).Error
}

type Service struct {
	cfg config.CP1Config
	db  *gorm.DB
	ids *snowflake.Generator
}

// NewService 创建并初始化服务。
func NewService(cfg config.CP1Config, db *gorm.DB, ids *snowflake.Generator) *Service {
	return &Service{cfg: cfg, db: db, ids: ids}
}

// GetStore 获取门店。
func (s *Service) GetStore(ctx context.Context, c *auth.Claims, orderRaw string) (VerificationDTO, error) {
	order, e := parseID(orderRaw)
	if e != nil {
		return VerificationDTO{}, problem.NotFound("ORDER_NOT_FOUND", "order not found")
	}
	shopIDs := claimShops(c)
	if len(shopIDs) == 0 || c.AccountType != "merchant" {
		return VerificationDTO{}, problem.Forbidden("PERM_FORBIDDEN", "merchant required")
	}
	var delivery uint64
	if e := s.db.WithContext(ctx).Table("delivery_orders d").Select("d.id").Joins("JOIN orders o ON o.id=d.order_id").Where("o.id=? AND o.shop_id IN ? AND d.status IN ? AND d.pickup_ready_status='ready'", order, shopIDs, []string{"pending_assign", "accepted"}).Scan(&delivery).Error; e != nil || delivery == 0 {
		return VerificationDTO{}, problem.NotFound("ORDER_NOT_FOUND", "order not found")
	}
	return s.reveal(ctx, c, "merchant", delivery, "pickup")
}

// GetCustomer 获取用户。
func (s *Service) GetCustomer(ctx context.Context, c *auth.Claims, orderRaw string) (VerificationDTO, error) {
	customer, e := claimCustomer(c)
	if e != nil {
		return VerificationDTO{}, e
	}
	order, e := parseID(orderRaw)
	if e != nil {
		return VerificationDTO{}, problem.NotFound("ORDER_NOT_FOUND", "order not found")
	}
	var delivery uint64
	if e := s.db.WithContext(ctx).Table("delivery_orders d").Select("d.id").Joins("JOIN orders o ON o.id=d.order_id").Where("o.id=? AND o.customer_id=? AND d.status='delivering'", order, customer).Scan(&delivery).Error; e != nil || delivery == 0 {
		return VerificationDTO{}, problem.NotFound("ORDER_NOT_FOUND", "order not found")
	}
	return s.reveal(ctx, c, "customer", delivery, "delivery")
}

// reveal 返回reveal。
func (s *Service) reveal(ctx context.Context, c *auth.Claims, actorType string, delivery uint64, stage string) (VerificationDTO, error) {
	var row Verification
	if e := s.db.WithContext(ctx).Where("delivery_order_id=? AND stage=?", delivery, stage).First(&row).Error; e != nil {
		return VerificationDTO{}, problem.NotFound("VERIFICATION_NOT_FOUND", "verification not found")
	}
	code, e := securevalue.Open(s.cfg.DataEncryptionKey, row.CodeCiphertext)
	if e != nil {
		return VerificationDTO{}, e
	}
	actor := uint64(0)
	if actorType == "customer" {
		actor, _ = parseID(c.CustomerID)
	} else {
		actor, _ = parseID(c.MerchantUserID)
	}
	if e := sensitiveAudit(ctx, s.db, s.ids.Next(), actorType, actor, delivery, stage); e != nil {
		return VerificationDTO{}, e
	}
	return dto(row, code), nil
}

// Unlock 解锁核验DTO。
func (s *Service) Unlock(ctx context.Context, c *auth.Claims, deliveryRaw string, req UnlockReq) (VerificationDTO, error) {
	actor, e := adminID(c, "delivery_verification:unlock")
	if e != nil {
		return VerificationDTO{}, e
	}
	delivery, e := parseID(deliveryRaw)
	if e != nil {
		return VerificationDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid delivery id")
	}
	var out VerificationDTO
	e = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row Verification
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("delivery_order_id=? AND stage=?", delivery, req.Stage).First(&row).Error; e != nil {
			return problem.NotFound("VERIFICATION_NOT_FOUND", "verification not found")
		}
		code, e := newCode()
		if e != nil {
			return e
		}
		cipher, e := securevalue.Seal(s.cfg.DataEncryptionKey, code)
		if e != nil {
			return e
		}
		now := time.Now()
		ttl := s.cfg.VerificationTTL
		if ttl <= 0 {
			ttl = 30 * time.Minute
		}
		if e := tx.Model(&Verification{}).Where("id=?", row.ID).Updates(map[string]any{"code_hash": hashCode(s.cfg, delivery, req.Stage, code), "code_ciphertext": cipher, "code_mask": "****" + code[4:], "status": "active", "failed_attempts": 0, "locked_until": nil, "expires_at": now.Add(ttl), "version": gorm.Expr("version+1")}).Error; e != nil {
			return e
		}
		if e := audit(ctx, tx, s.ids.Next(), "admin", actor, "delivery_verification.unlock", delivery, map[string]any{"stage": req.Stage, "reason_code": req.ReasonCode, "reason": req.Reason}); e != nil {
			return e
		}
		row.Status = "active"
		row.FailedAttempts = 0
		row.LockedUntil = nil
		row.ExpiresAt = now.Add(ttl)
		row.CodeMask = "****" + code[4:]
		out = dto(row, "")
		return nil
	})
	return out, e
}

// InvalidateAndRegenerate 使And Regenerate失效。
func InvalidateAndRegenerate(ctx context.Context, tx *gorm.DB, cfg config.CP1Config, ids *snowflake.Generator, delivery uint64) error {
	for _, stage := range []string{"pickup", "delivery"} {
		mode := cfg.PickupVerificationMode
		if stage == "delivery" {
			mode = cfg.DeliveryVerificationMode
		}
		if mode == "off" || mode == "" {
			continue
		}
		code, e := newCode()
		if e != nil {
			return e
		}
		cipher, e := securevalue.Seal(cfg.DataEncryptionKey, code)
		if e != nil {
			return e
		}
		ttl := cfg.VerificationTTL
		if ttl <= 0 {
			ttl = 30 * time.Minute
		}
		max := cfg.VerificationMaxAttempts
		if max <= 0 {
			max = 5
		}
		updates := map[string]any{"code_hash": hashCode(cfg, delivery, stage, code), "code_ciphertext": cipher, "code_mask": "****" + code[4:], "policy_version": "cp1-v1", "status": "active", "failed_attempts": 0, "max_attempts": max, "expires_at": time.Now().Add(ttl), "locked_until": nil, "verified_at": nil, "verified_by_type": nil, "verified_by_id": nil, "version": gorm.Expr("version+1")}
		res := tx.WithContext(ctx).Model(&Verification{}).Where("delivery_order_id=? AND stage=?", delivery, stage).Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			row := Verification{ID: ids.Next(), DeliveryOrderID: delivery, Stage: stage, CodeHash: hashCode(cfg, delivery, stage, code), CodeCiphertext: cipher, CodeMask: "****" + code[4:], PolicyVersion: "cp1-v1", Status: "active", MaxAttempts: uint(max), ExpiresAt: time.Now().Add(ttl), Version: 1}
			if e := tx.Create(&row).Error; e != nil {
				return e
			}
		}
	}
	return nil
}

// newCode 创建并初始化代码。
func newCode() (string, error) {
	var b [8]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", e
	}
	return fmt.Sprintf("%06d", binary.BigEndian.Uint64(b[:])%1000000), nil
}

// hashCode 计算代码的哈希值。
func hashCode(cfg config.CP1Config, delivery uint64, stage, code string) string {
	return securevalue.HMAC(cfg.VerificationPepper, idString(delivery), stage, "cp1-v1", code)
}

// dto 返回DTO。
func dto(r Verification, code string) VerificationDTO {
	remaining := uint(0)
	if r.MaxAttempts > r.FailedAttempts {
		remaining = r.MaxAttempts - r.FailedAttempts
	}
	return VerificationDTO{DeliveryOrderID: idString(r.DeliveryOrderID), Stage: r.Stage, Status: r.Status, Code: code, CodeMask: r.CodeMask, FailedAttempts: r.FailedAttempts, RemainingAttempts: remaining, ExpiresAt: r.ExpiresAt.Format(time.RFC3339), LockedUntil: timeString(r.LockedUntil), VerifiedAt: timeString(r.VerifiedAt)}
}

// claimShops 认领Shops。
func claimShops(c *auth.Claims) []uint64 {
	if c == nil {
		return nil
	}
	out := []uint64{}
	for _, v := range c.AuthorizedShopIDs {
		id, e := parseID(v)
		if e == nil {
			out = append(out, id)
		}
	}
	return out
}

// claimCustomer 认领用户。
func claimCustomer(c *auth.Claims) (uint64, error) {
	if c == nil || c.AccountType != "customer" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "customer required")
	}
	return parseID(c.CustomerID)
}

// adminID 从认证声明中解析并返回管理员 ID。
func adminID(c *auth.Claims, p string) (uint64, error) {
	if c == nil || c.AccountType != "admin" || !has(c.Permissions, p) {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
	}
	return parseID(c.AdminUserID)
}

// has 判断是否存在deliveryverification。
func has(v []string, x string) bool {
	for _, s := range v {
		if s == x {
			return true
		}
	}
	return false
}

// parseID 解析并校验字符串形式的 ID。
func parseID(v string) (uint64, error) {
	id, e := strconv.ParseUint(v, 10, 64)
	if e != nil || id == 0 {
		return 0, fmt.Errorf("invalid id")
	}
	return id, nil
}

// idString 将数字 ID 转换为字符串。
func idString(v uint64) string { return strconv.FormatUint(v, 10) }

// timeString 将可选时间转换为字符串。
func timeString(v *time.Time) string {
	if v == nil {
		return ""
	}
	return v.Format(time.RFC3339)
}

// httpTitle 返回HTTP Title。
func httpTitle(code int) string {
	if code == 423 {
		return "Locked"
	}
	if code == 410 {
		return "Gone"
	}
	return "Unprocessable Entity"
}

// sensitiveAudit 返回敏感信息审计。
func sensitiveAudit(ctx context.Context, db *gorm.DB, id uint64, actorType string, actor, delivery uint64, stage string) error {
	return audit(ctx, db, id, actorType, actor, "delivery_verification.view", delivery, map[string]any{"stage": stage, "sensitive_access": true})
}

// audit 返回审计。
func audit(ctx context.Context, db *gorm.DB, id uint64, actorType string, actor uint64, action string, resource uint64, after any) error {
	raw, _ := json.Marshal(after)
	return db.WithContext(ctx).Table("audit_logs").Create(map[string]any{"id": id, "actor_type": actorType, "actor_id": actor, "action": action, "resource_type": "delivery_verification", "resource_id": resource, "after_data": datatypes.JSON(raw), "result": "success", "request_id": requestctx.RequestIDPtr(ctx)}).Error
}
