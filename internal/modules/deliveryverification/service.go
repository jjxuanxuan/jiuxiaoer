package deliveryverification

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/securevalue"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

const (
	verificationPolicyVersion    = "cp1-v1"
	verificationSecretKeyVersion = "v1"
)

// GeneratePair is retained for callers compiled against the original CP1 API.
// The phase-one contract intentionally creates only the pickup code when the
// store becomes ready; the delivery code is activated after successful pickup.
func GeneratePair(ctx context.Context, tx *gorm.DB, cfg config.CP1Config, ids *snowflake.Generator, deliveryID uint64) error {
	return GeneratePickup(ctx, tx, cfg, ids, deliveryID)
}

// GeneratePickup creates the pickup credential at the store-ready boundary.
func GeneratePickup(ctx context.Context, tx *gorm.DB, cfg config.CP1Config, ids *snowflake.Generator, deliveryID uint64) error {
	return generateStage(ctx, tx, cfg, ids, deliveryID, "pickup", false, "pickup_ready")
}

// ActivateDelivery rotates any legacy early-created credential and starts the
// delivery credential TTL only after pickup has been verified successfully.
func ActivateDelivery(ctx context.Context, tx *gorm.DB, cfg config.CP1Config, ids *snowflake.Generator, deliveryID uint64) error {
	return generateStage(ctx, tx, cfg, ids, deliveryID, "delivery", true, "pickup_verified")
}

// VerifyLocked 核验Locked是否有效。
// VerifyLocked must run while the delivery row is locked. A rejection is
// returned separately so the caller can commit the failed-attempt audit before
// sending the business error to the client.
func VerifyLocked(ctx context.Context, tx *gorm.DB, cfg config.CP1Config, ids *snowflake.Generator, deliveryID uint64, stage, code string, actorID, accountID uint64, deviceSubject string) (*problem.Details, error) {
	mode := verificationMode(cfg, stage)
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
	mode = effectiveMode(mode, row.ModeSnapshot)
	now := time.Now()
	deviceIDHash := ""
	if strings.TrimSpace(deviceSubject) != "" {
		deviceIDHash = securevalue.Digest("verification-device-v1\x00" + deviceSubject)
	}
	if limited, e := rateLimited(ctx, tx, actorID, accountID, deviceIDHash, now); e != nil {
		return nil, e
	} else if limited {
		if e := createAttempt(ctx, tx, ids, row, actorID, accountID, deviceIDHash, mode, "failed", "VERIFICATION_RATE_LIMITED", row.FailedAttempts+1); e != nil {
			return nil, e
		}
		if e := auditResult(ctx, tx, ids.Next(), "rider", actorID, "delivery_verification.failed", deliveryID, map[string]any{
			"stage": stage, "error_code": "VERIFICATION_RATE_LIMITED", "status": row.Status, "version": row.Version,
		}, "failed"); e != nil {
			return nil, e
		}
		return problem.TooManyRequests("VERIFICATION_RATE_LIMITED", "too many verification attempts"), nil
	}
	failure := ""
	status := row.Status
	if status == "locked" {
		failure = "VERIFICATION_LOCKED"
	} else if status == "verified" {
		return nil, nil
	} else if status == "invalidated" {
		failure = "VERIFICATION_INVALIDATED"
	} else if status != "active" && status != "expired" {
		failure = "VERIFICATION_INVALID_STATUS"
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
		lockedUntil := row.LockedUntil
		becameLocked := false
		if failure == "VERIFICATION_CODE_INVALID" {
			row.FailedAttempts++
			attemptNo = row.FailedAttempts
			if row.FailedAttempts >= row.MaxAttempts {
				newStatus = "locked"
				becameLocked = row.Status != "locked"
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
		if e := createAttempt(ctx, tx, ids, row, actorID, accountID, deviceIDHash, mode, "failed", failure, attemptNo); e != nil {
			return nil, e
		}
		failureVersion := row.Version
		if failure == "VERIFICATION_CODE_INVALID" {
			failureVersion++
		}
		if e := auditResult(ctx, tx, ids.Next(), "rider", actorID, "delivery_verification.failed", deliveryID, map[string]any{
			"stage": stage, "error_code": failure, "before_status": row.Status, "status": newStatus,
			"failed_attempts": row.FailedAttempts, "version": failureVersion, "locked_until": timeString(lockedUntil),
		}, "failed"); e != nil {
			return nil, e
		}
		if becameLocked {
			if e := audit(ctx, tx, ids.Next(), "system", 0, "delivery_verification.locked", deliveryID, map[string]any{
				"stage": stage, "reason_code": "max_attempts_exceeded", "before_status": row.Status,
				"status": "locked", "failed_attempts": row.FailedAttempts, "version": failureVersion,
				"locked_until": timeString(lockedUntil),
			}); e != nil {
				return nil, e
			}
		}
		if mode == "observe" {
			return nil, nil
		}
		httpCode := 422
		if failure == "VERIFICATION_LOCKED" {
			httpCode = 423
		} else if failure == "VERIFICATION_EXPIRED" {
			httpCode = 410
		} else if failure == "VERIFICATION_INVALIDATED" || failure == "VERIFICATION_INVALID_STATUS" {
			httpCode = 409
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
	if e := createAttempt(ctx, tx, ids, row, actorID, accountID, deviceIDHash, mode, "success", "", row.FailedAttempts+1); e != nil {
		return nil, e
	}
	if e := audit(ctx, tx, ids.Next(), "rider", actorID, "delivery_verification.verified", deliveryID, map[string]any{
		"stage": stage, "before_status": row.Status, "status": "verified", "version": row.Version + 1,
	}); e != nil {
		return nil, e
	}
	return nil, nil
}

// rateLimited 返回速率 Limited。
func rateLimited(ctx context.Context, tx *gorm.DB, actorID, accountID uint64, deviceIDHash string, now time.Time) (bool, error) {
	window := now.Add(-time.Minute)
	var actorFailures int64
	if e := tx.Model(&Attempt{}).Where("actor_type='rider' AND actor_id=? AND result='failed' AND created_at>=?", actorID, window).Count(&actorFailures).Error; e != nil {
		return false, e
	}
	if actorFailures >= 20 {
		return true, nil
	}
	if accountID != 0 {
		var accountFailures int64
		if e := tx.Model(&Attempt{}).Where("account_id=? AND result='failed' AND created_at>=?", accountID, window).Count(&accountFailures).Error; e != nil {
			return false, e
		}
		if accountFailures >= 20 {
			return true, nil
		}
	}
	if deviceIDHash != "" {
		var deviceFailures int64
		if e := tx.Model(&Attempt{}).Where("device_id_hash=? AND result='failed' AND created_at>=?", deviceIDHash, window).Count(&deviceFailures).Error; e != nil {
			return false, e
		}
		if deviceFailures >= 20 {
			return true, nil
		}
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
func createAttempt(ctx context.Context, tx *gorm.DB, ids *snowflake.Generator, row Verification, actor, account uint64, deviceIDHash, mode, result, failure string, no uint) error {
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
	var accountPtr *uint64
	if account != 0 {
		accountPtr = &account
	}
	var deviceIDHashPtr *string
	if deviceIDHash != "" {
		deviceIDHashPtr = &deviceIDHash
	}
	return tx.Create(&Attempt{ID: ids.Next(), VerificationID: row.ID, DeliveryOrderID: row.DeliveryOrderID, Stage: row.Stage, ModeSnapshot: mode, ActorType: "rider", ActorID: actor, AccountID: accountPtr, Result: result, FailureCode: f, AttemptNo: no, RequestID: requestctx.RequestIDPtr(ctx), IPHash: ipPtr, DeviceIDHash: deviceIDHashPtr}).Error
}

type Service struct {
	cfg  config.CP1Config
	db   *gorm.DB
	ids  *snowflake.Generator
	idem *idempotency.Store
}

// NewService 创建并初始化服务。
func NewService(cfg config.CP1Config, db *gorm.DB, ids *snowflake.Generator) *Service {
	return &Service{cfg: cfg, db: db, ids: ids, idem: idempotency.NewStore(db)}
}

// GetStore 获取门店。
func (s *Service) GetStore(ctx context.Context, c *auth.Claims, orderRaw string) (VerificationDTO, error) {
	actor := uint64(0)
	if c != nil {
		actor, _ = parseID(c.MerchantUserID)
	}
	if c == nil || c.AccountType != "merchant" || !has(c.Permissions, "delivery_verification:view_shop") {
		err := problem.Forbidden("PERM_FORBIDDEN", "permission denied")
		return VerificationDTO{}, s.sensitiveDenied(ctx, "merchant", actor, parseIDOrZero(orderRaw), "pickup", err)
	}
	order, e := parseID(orderRaw)
	if e != nil {
		err := problem.NotFound("ORDER_NOT_FOUND", "order not found")
		return VerificationDTO{}, s.sensitiveDenied(ctx, "merchant", actor, 0, "pickup", err)
	}
	shopIDs := claimShops(c)
	if len(shopIDs) == 0 {
		err := problem.Forbidden("PERM_FORBIDDEN", "merchant required")
		return VerificationDTO{}, s.sensitiveDenied(ctx, "merchant", actor, order, "pickup", err)
	}
	var delivery uint64
	if e := s.db.WithContext(ctx).Table("delivery_orders d").Select("d.id").Joins("JOIN orders o ON o.id=d.order_id").Where("o.id=? AND o.shop_id IN ? AND d.status IN ? AND d.pickup_ready_status='ready'", order, shopIDs, []string{"pending_assign", "accepted"}).Scan(&delivery).Error; e != nil {
		return VerificationDTO{}, e
	} else if delivery == 0 {
		err := problem.NotFound("ORDER_NOT_FOUND", "order not found")
		return VerificationDTO{}, s.sensitiveDenied(ctx, "merchant", actor, order, "pickup", err)
	}
	return s.reveal(ctx, "merchant", actor, delivery, "pickup")
}

// GetCustomer 获取用户。
func (s *Service) GetCustomer(ctx context.Context, c *auth.Claims, orderRaw string) (VerificationDTO, error) {
	actor := uint64(0)
	if c != nil {
		actor, _ = parseID(c.CustomerID)
	}
	if c == nil || !has(c.Permissions, "delivery_verification:view_customer") {
		err := problem.Forbidden("PERM_FORBIDDEN", "permission denied")
		return VerificationDTO{}, s.sensitiveDenied(ctx, "customer", actor, parseIDOrZero(orderRaw), "delivery", err)
	}
	customer, e := claimCustomer(c)
	if e != nil {
		return VerificationDTO{}, s.sensitiveDenied(ctx, "customer", actor, parseIDOrZero(orderRaw), "delivery", e)
	}
	order, e := parseID(orderRaw)
	if e != nil {
		err := problem.NotFound("ORDER_NOT_FOUND", "order not found")
		return VerificationDTO{}, s.sensitiveDenied(ctx, "customer", customer, 0, "delivery", err)
	}
	var delivery uint64
	if e := s.db.WithContext(ctx).Table("delivery_orders d").Select("d.id").Joins("JOIN orders o ON o.id=d.order_id").Where("o.id=? AND o.customer_id=? AND d.status='delivering'", order, customer).Scan(&delivery).Error; e != nil {
		return VerificationDTO{}, e
	} else if delivery == 0 {
		err := problem.NotFound("ORDER_NOT_FOUND", "order not found")
		return VerificationDTO{}, s.sensitiveDenied(ctx, "customer", customer, order, "delivery", err)
	}
	return s.reveal(ctx, "customer", customer, delivery, "delivery")
}

// reveal 返回reveal。
func (s *Service) reveal(ctx context.Context, actorType string, actor, delivery uint64, stage string) (VerificationDTO, error) {
	limited, e := s.sensitiveViewRateLimited(ctx, actorType, actor, delivery, time.Now())
	if e != nil {
		return VerificationDTO{}, e
	}
	if limited {
		err := problem.TooManyRequests("VERIFICATION_VIEW_RATE_LIMITED", "too many verification code views")
		return VerificationDTO{}, s.sensitiveDenied(ctx, actorType, actor, delivery, stage, err)
	}
	var row Verification
	if e := s.db.WithContext(ctx).Where("delivery_order_id=? AND stage=?", delivery, stage).First(&row).Error; e != nil {
		if !errors.Is(e, gorm.ErrRecordNotFound) {
			return VerificationDTO{}, e
		}
		err := problem.NotFound("VERIFICATION_NOT_FOUND", "verification not found")
		return VerificationDTO{}, s.sensitiveDenied(ctx, actorType, actor, delivery, stage, err)
	}
	if row.Status != "active" && row.Status != "locked" {
		err := problem.NotFound("VERIFICATION_NOT_FOUND", "verification not found")
		return VerificationDTO{}, s.sensitiveDenied(ctx, actorType, actor, delivery, stage, err)
	}
	if !row.ExpiresAt.After(time.Now()) {
		err := problem.NotFound("VERIFICATION_NOT_FOUND", "verification not found")
		return VerificationDTO{}, s.sensitiveDenied(ctx, actorType, actor, delivery, stage, err)
	}
	code, e := securevalue.Open(s.cfg.DataEncryptionKey, row.CodeCiphertext)
	if e != nil {
		return VerificationDTO{}, s.sensitiveDenied(ctx, actorType, actor, delivery, stage, problem.Internal("verification credential unavailable"))
	}
	if e := sensitiveAudit(ctx, s.db, s.ids.Next(), actorType, actor, delivery, stage); e != nil {
		return VerificationDTO{}, e
	}
	return dto(row, code), nil
}

func (s *Service) sensitiveViewRateLimited(ctx context.Context, actorType string, actor, delivery uint64, now time.Time) (bool, error) {
	if actor == 0 {
		return true, nil
	}
	base := s.db.WithContext(ctx).Table("audit_logs").Where("action='delivery_verification.view' AND actor_type=? AND actor_id=? AND created_at>=?", actorType, actor, now.Add(-time.Minute))
	var accountViews int64
	if e := base.Session(&gorm.Session{}).Count(&accountViews).Error; e != nil {
		return false, e
	}
	if accountViews >= 60 {
		return true, nil
	}
	var deliveryViews int64
	if e := base.Session(&gorm.Session{}).Where("resource_id=?", delivery).Count(&deliveryViews).Error; e != nil {
		return false, e
	}
	return deliveryViews >= 10, nil
}

func (s *Service) sensitiveDenied(ctx context.Context, actorType string, actor, resource uint64, stage string, cause error) error {
	detail := problem.FromError(cause)
	if e := sensitiveAuditResult(ctx, s.db, s.ids.Next(), actorType, actor, resource, stage, "denied", detail.ErrorCode); e != nil {
		return e
	}
	return cause
}

// Unlock 解锁核验DTO。
func (s *Service) Unlock(ctx context.Context, c *auth.Claims, method, path, key, deliveryRaw string, req UnlockReq) (VerificationDTO, error) {
	actor, e := adminID(c, "delivery_verification:unlock")
	if e != nil {
		return VerificationDTO{}, e
	}
	delivery, e := parseID(deliveryRaw)
	if e != nil {
		return VerificationDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid delivery id")
	}
	requestHash := idempotency.RequestHash(struct {
		DeliveryID string    `json:"delivery_id"`
		Request    UnlockReq `json:"request"`
	}{DeliveryID: deliveryRaw, Request: req})
	var out VerificationDTO
	if replayed, replayErr := s.idem.ReplayCompleted(ctx, s.db, "admin", actor, path, key, requestHash, &out); replayErr != nil {
		return VerificationDTO{}, replayErr
	} else if replayed {
		return out, nil
	}
	e = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, startErr := s.idem.Start(ctx, tx, s.ids.Next(), "admin", actor, method, path, key, requestHash)
		if startErr != nil {
			return startErr
		}
		if !started {
			cached, cachedErr := s.idem.CachedResponse(ctx, tx, "admin", actor, path, key, &out)
			if cachedErr != nil {
				return cachedErr
			}
			if cached {
				return nil
			}
			return problem.Conflict("IDEMPOTENCY_CONFLICT", "idempotency response missing")
		}
		var phase struct {
			ID                uint64
			Status            string
			PickupReadyStatus string
		}
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Table("delivery_orders").
			Select("id, status, pickup_ready_status").
			Where("id=? AND deleted_at IS NULL", delivery).
			Take(&phase).Error; errors.Is(e, gorm.ErrRecordNotFound) {
			return problem.NotFound("DELIVERY_NOT_FOUND", "delivery not found")
		} else if e != nil {
			return e
		}
		var row Verification
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("delivery_order_id=? AND stage=?", delivery, req.Stage).First(&row).Error; e != nil {
			return problem.NotFound("VERIFICATION_NOT_FOUND", "verification not found")
		}
		if row.Version != req.ExpectedVersion {
			return problem.Conflict("VERIFICATION_VERSION_CONFLICT", "verification version changed")
		}
		now := time.Now()
		if e := validateUnlockState(phase.Status, phase.PickupReadyStatus, req.Stage, row.Status, row.ExpiresAt, now); e != nil {
			return e
		}
		code, e := newCode()
		if e != nil {
			return e
		}
		cipher, e := securevalue.Seal(s.cfg.DataEncryptionKey, code)
		if e != nil {
			return e
		}
		ttl := verificationTTL(s.cfg, req.Stage)
		if req.Stage == "delivery" {
			ttl, e = deliveryVerificationTTL(ctx, tx, s.cfg, delivery)
			if e != nil {
				return e
			}
		}
		mode := verificationMode(s.cfg, req.Stage)
		updated := tx.Model(&Verification{}).Where("id=? AND version=?", row.ID, row.Version).Updates(map[string]any{"mode_snapshot": mode, "code_hash": hashCode(s.cfg, delivery, req.Stage, code), "code_ciphertext": cipher, "code_mask": "****" + code[4:], "secret_key_version": verificationSecretKeyVersion, "status": "active", "failed_attempts": 0, "locked_until": nil, "expires_at": now.Add(ttl), "activated_at": now, "version": gorm.Expr("version+1")})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return problem.Conflict("VERIFICATION_VERSION_CONFLICT", "verification changed concurrently")
		}
		if e := audit(ctx, tx, s.ids.Next(), "admin", actor, "delivery_verification.unlock", delivery, map[string]any{"stage": req.Stage, "reason_code": req.ReasonCode, "reason": req.Reason, "previous_status": row.Status, "previous_version": row.Version, "previous_code_digest": securevalue.Digest(row.CodeHash), "previous_expires_at": row.ExpiresAt.UTC().Format(time.RFC3339Nano), "version": row.Version + 1}); e != nil {
			return e
		}
		row.Status = "active"
		row.ModeSnapshot = mode
		row.FailedAttempts = 0
		row.LockedUntil = nil
		row.ExpiresAt = now.Add(ttl)
		row.ActivatedAt = &now
		row.CodeMask = "****" + code[4:]
		row.Version++
		out = dto(row, "")
		return s.idem.Succeed(ctx, tx, "admin", actor, path, key, out)
	})
	return out, e
}

func validateUnlockState(deliveryStatus, pickupReadyStatus, stage, verificationStatus string, expiresAt, now time.Time) error {
	if deliveryStatus == "completed" || deliveryStatus == "cancelled" {
		return problem.Conflict("DELIVERY_TERMINAL", "terminal delivery verification cannot be unlocked")
	}
	if stage == "pickup" {
		if pickupReadyStatus != "ready" || deliveryStatus != "pending_assign" && deliveryStatus != "accepted" {
			return problem.Conflict("VERIFICATION_STAGE_INACTIVE", "pickup verification is not active for the current delivery phase")
		}
	} else if stage == "delivery" {
		if deliveryStatus != "delivering" {
			return problem.Conflict("VERIFICATION_STAGE_INACTIVE", "delivery verification is not active for the current delivery phase")
		}
	} else {
		return problem.InvalidArgument("VALIDATION_FAILED", "invalid verification stage")
	}
	if verificationStatus == "locked" || verificationStatus == "expired" || verificationStatus == "active" && !expiresAt.After(now) {
		return nil
	}
	return problem.Conflict("VERIFICATION_INVALID_STATUS", "only a locked or expired verification can be unlocked")
}

// InvalidateAndRegenerate invalidates credentials that are not valid for the
// current fulfilment phase and rotates the one credential still needed after a
// reassignment. Before pickup this means pickup only; after pickup it means
// delivery only.
func InvalidateAndRegenerate(ctx context.Context, tx *gorm.DB, cfg config.CP1Config, ids *snowflake.Generator, delivery uint64) error {
	var status string
	if err := tx.WithContext(ctx).Table("delivery_orders").Select("status").Where("id=?", delivery).Scan(&status).Error; err != nil {
		return err
	}
	stage := "pickup"
	if status == "delivering" {
		stage = "delivery"
	} else if status == "completed" || status == "cancelled" {
		return Invalidate(ctx, tx, ids, delivery, "delivery_terminal")
	}
	var obsolete []Verification
	if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("delivery_order_id=? AND stage<>? AND status IN ?", delivery, stage, []string{"active", "locked"}).
		Find(&obsolete).Error; err != nil {
		return err
	}
	if err := invalidateVerificationRows(ctx, tx, ids, obsolete, "assignment_changed"); err != nil {
		return err
	}
	return generateStage(ctx, tx, cfg, ids, delivery, stage, true, "assignment_changed")
}

// Invalidate makes every unused credential for a delivery permanently
// unusable. It is called by terminal order flows such as cancellation.
func Invalidate(ctx context.Context, tx *gorm.DB, ids *snowflake.Generator, delivery uint64, reasonCode string) error {
	return InvalidateMany(ctx, tx, ids, []uint64{delivery}, reasonCode)
}

// InvalidateMany permanently invalidates every still-usable credential for
// the supplied deliveries. Restricting the update to active/locked rows makes
// the operation safe to repeat and preserves the terminal fact for credentials
// that were already verified, expired, overridden, or invalidated.
func InvalidateMany(ctx context.Context, tx *gorm.DB, ids *snowflake.Generator, deliveries []uint64, reasonCode string) error {
	if len(deliveries) == 0 {
		return nil
	}
	if reasonCode == "" {
		reasonCode = "delivery_invalidated"
	}
	var rows []Verification
	if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("delivery_order_id IN ? AND status IN ?", deliveries, []string{"active", "locked"}).
		Find(&rows).Error; err != nil {
		return err
	}
	return invalidateVerificationRows(ctx, tx, ids, rows, reasonCode)
}

func invalidateVerificationRows(ctx context.Context, tx *gorm.DB, ids *snowflake.Generator, rows []Verification, reasonCode string) error {
	if len(rows) == 0 {
		return nil
	}
	verificationIDs := make([]uint64, 0, len(rows))
	for _, row := range rows {
		verificationIDs = append(verificationIDs, row.ID)
	}
	now := time.Now()
	updated := tx.WithContext(ctx).Model(&Verification{}).
		Where("id IN ? AND status IN ?", verificationIDs, []string{"active", "locked"}).
		Updates(map[string]any{"status": "invalidated", "invalidated_at": now, "invalidation_reason_code": reasonCode, "locked_until": nil, "version": gorm.Expr("version+1")})
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != int64(len(rows)) {
		return problem.Conflict("VERIFICATION_INVALID_STATUS", "verification changed concurrently")
	}
	for _, row := range rows {
		if err := audit(ctx, tx, ids.Next(), "system", 0, "delivery_verification.invalidated", row.DeliveryOrderID, map[string]any{
			"stage": row.Stage, "reason_code": reasonCode, "before_status": row.Status,
			"status": "invalidated", "version": row.Version + 1,
		}); err != nil {
			return err
		}
	}
	return nil
}

// InvalidateByOrder is the order-terminal boundary used by cancellation and
// full-refund flows. Looking up all delivery attempts also covers a historical
// retry/re-dispatch without requiring those modules to know verification table
// details.
func InvalidateByOrder(ctx context.Context, tx *gorm.DB, ids *snowflake.Generator, orderID uint64, reasonCode string) error {
	var deliveryIDs []uint64
	if err := tx.WithContext(ctx).Table("delivery_orders").
		Where("order_id=? AND deleted_at IS NULL", orderID).
		Order("id").Pluck("id", &deliveryIDs).Error; err != nil {
		return err
	}
	return InvalidateMany(ctx, tx, ids, deliveryIDs, reasonCode)
}

func generateStage(ctx context.Context, tx *gorm.DB, cfg config.CP1Config, ids *snowflake.Generator, delivery uint64, stage string, rotate bool, reasonCode string) error {
	mode := verificationMode(cfg, stage)
	if mode == "" || mode == "off" {
		return nil
	}
	code, err := newCode()
	if err != nil {
		return err
	}
	cipher, err := securevalue.Seal(cfg.DataEncryptionKey, code)
	if err != nil {
		return err
	}
	now := time.Now()
	maxAttempts := cfg.VerificationMaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	ttl := verificationTTL(cfg, stage)
	if stage == "delivery" {
		var err error
		ttl, err = deliveryVerificationTTL(ctx, tx, cfg, delivery)
		if err != nil {
			return err
		}
	}
	expiresAt := now.Add(ttl)
	updates := map[string]any{
		"mode_snapshot": mode, "code_hash": hashCode(cfg, delivery, stage, code),
		"code_ciphertext": cipher, "code_mask": "****" + code[4:],
		"policy_version": verificationPolicyVersion, "secret_key_version": verificationSecretKeyVersion,
		"status": "active", "failed_attempts": 0, "max_attempts": maxAttempts,
		"expires_at": expiresAt, "activated_at": now, "invalidated_at": nil,
		"invalidation_reason_code": nil, "locked_until": nil, "verified_at": nil,
		"verified_by_type": nil, "verified_by_id": nil,
	}
	if rotate {
		var previous Verification
		findErr := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("delivery_order_id=? AND stage=?", delivery, stage).First(&previous).Error
		if findErr != nil && !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		if findErr == nil {
			rotated := tx.WithContext(ctx).Model(&Verification{}).
				Where("id=? AND version=?", previous.ID, previous.Version).
				Updates(withVersionIncrement(updates))
			if rotated.Error != nil {
				return rotated.Error
			}
			if rotated.RowsAffected != 1 {
				return problem.Conflict("VERIFICATION_VERSION_CONFLICT", "verification changed concurrently")
			}
			return audit(ctx, tx, ids.Next(), "system", 0, "delivery_verification.regenerated", delivery, map[string]any{
				"stage": stage, "reason_code": reasonCode, "before_status": previous.Status,
				"status": "active", "version": previous.Version + 1,
			})
		}
	}
	row := Verification{
		ID: ids.Next(), DeliveryOrderID: delivery, Stage: stage, ModeSnapshot: mode,
		CodeHash: hashCode(cfg, delivery, stage, code), CodeCiphertext: cipher,
		CodeMask: "****" + code[4:], PolicyVersion: verificationPolicyVersion,
		SecretKeyVersion: verificationSecretKeyVersion, Status: "active",
		MaxAttempts: uint(maxAttempts), ExpiresAt: expiresAt, ActivatedAt: &now, Version: 1,
	}
	created := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if created.Error != nil {
		return created.Error
	}
	if created.RowsAffected == 0 {
		return nil
	}
	return audit(ctx, tx, ids.Next(), "system", 0, "delivery_verification.generated", delivery, map[string]any{
		"stage": stage, "reason_code": reasonCode, "status": "active", "version": 1,
	})
}

func withVersionIncrement(values map[string]any) map[string]any {
	result := make(map[string]any, len(values)+1)
	for key, value := range values {
		result[key] = value
	}
	result["version"] = gorm.Expr("version+1")
	return result
}

func verificationMode(cfg config.CP1Config, stage string) string {
	if stage == "delivery" {
		return cfg.DeliveryVerificationMode
	}
	return cfg.PickupVerificationMode
}

func effectiveMode(configured, snapshot string) string {
	if configured == "enforce" || snapshot == "enforce" {
		return "enforce"
	}
	if snapshot != "" {
		return snapshot
	}
	return configured
}

func verificationTTL(cfg config.CP1Config, stage string) time.Duration {
	if stage == "delivery" {
		ttl := cfg.DeliveryVerificationTTL
		if ttl < 2*time.Hour {
			return 2 * time.Hour
		}
		return ttl
	}
	if cfg.VerificationTTL <= 0 {
		return 30 * time.Minute
	}
	return cfg.VerificationTTL
}

// deliveryVerificationTTL starts from the configured two-hour floor, extends
// it to route ETA plus one hour, and caps the result. A missing or malformed
// historical promise snapshot safely falls back to the configured floor.
func deliveryVerificationTTL(ctx context.Context, tx *gorm.DB, cfg config.CP1Config, delivery uint64) (time.Duration, error) {
	var row struct {
		Snapshot datatypes.JSON `gorm:"column:delivery_promise_snapshot"`
	}
	if err := tx.WithContext(ctx).Table("delivery_orders AS d").
		Select("o.delivery_promise_snapshot").
		Joins("JOIN orders AS o ON o.id=d.order_id AND o.deleted_at IS NULL").
		Where("d.id=? AND d.deleted_at IS NULL", delivery).
		Scan(&row).Error; err != nil {
		return 0, err
	}
	return deliveryVerificationTTLFromSnapshot(cfg, row.Snapshot), nil
}

func deliveryVerificationTTLFromSnapshot(cfg config.CP1Config, snapshot []byte) time.Duration {
	minimum := cfg.DeliveryVerificationTTL
	if minimum < 2*time.Hour {
		minimum = 2 * time.Hour
	}
	maximum := cfg.DeliveryVerificationMaxTTL
	if maximum <= 0 {
		maximum = 6 * time.Hour
	}
	if maximum < minimum {
		maximum = minimum
	}
	var payload struct {
		Route struct {
			DurationSeconds json.Number `json:"duration_seconds"`
		} `json:"route"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(snapshot)))
	decoder.UseNumber()
	if len(snapshot) == 0 || decoder.Decode(&payload) != nil {
		return minimum
	}
	seconds, err := payload.Route.DurationSeconds.Int64()
	if err != nil || seconds <= 0 {
		return minimum
	}
	// Compare in seconds before converting to time.Duration so a corrupt
	// oversized snapshot cannot overflow and accidentally shorten validity.
	maxSeconds := int64(maximum / time.Second)
	if maxSeconds <= 3600 || seconds >= maxSeconds-3600 {
		return maximum
	}
	candidate := time.Duration(seconds+3600) * time.Second
	if candidate < minimum {
		return minimum
	}
	if candidate > maximum {
		return maximum
	}
	return candidate
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
	return securevalue.HMAC(cfg.VerificationPepper, idString(delivery), stage, verificationPolicyVersion, code)
}

// dto 返回DTO。
func dto(r Verification, code string) VerificationDTO {
	remaining := uint(0)
	if r.MaxAttempts > r.FailedAttempts {
		remaining = r.MaxAttempts - r.FailedAttempts
	}
	return VerificationDTO{DeliveryOrderID: idString(r.DeliveryOrderID), Stage: r.Stage, Status: r.Status, Code: code, CodeMask: r.CodeMask, FailedAttempts: r.FailedAttempts, RemainingAttempts: remaining, ExpiresAt: r.ExpiresAt.Format(time.RFC3339), LockedUntil: timeString(r.LockedUntil), VerifiedAt: timeString(r.VerifiedAt), Version: r.Version}
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

func parseIDOrZero(v string) uint64 {
	id, _ := parseID(v)
	return id
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
	if code == 409 {
		return "Conflict"
	}
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
	return sensitiveAuditResult(ctx, db, id, actorType, actor, delivery, stage, "success", "")
}

func sensitiveAuditResult(ctx context.Context, db *gorm.DB, id uint64, actorType string, actor, delivery uint64, stage, result, errorCode string) error {
	after := map[string]any{"stage": stage, "sensitive_access": true}
	if errorCode != "" {
		after["error_code"] = errorCode
	}
	return auditResult(ctx, db, id, actorType, actor, "delivery_verification.view", delivery, after, result)
}

// audit 返回审计。
func audit(ctx context.Context, db *gorm.DB, id uint64, actorType string, actor uint64, action string, resource uint64, after any) error {
	return auditResult(ctx, db, id, actorType, actor, action, resource, after, "success")
}

func auditResult(ctx context.Context, db *gorm.DB, id uint64, actorType string, actor uint64, action string, resource uint64, after any, result string) error {
	raw, _ := json.Marshal(after)
	return db.WithContext(ctx).Table("audit_logs").Create(map[string]any{"id": id, "actor_type": actorType, "actor_id": actor, "action": action, "resource_type": "delivery_verification", "resource_id": resource, "after_data": datatypes.JSON(raw), "result": result, "request_id": requestctx.RequestIDPtr(ctx), "created_at": time.Now()}).Error
}
