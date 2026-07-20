package provisioning

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/coordinate"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/securevalue"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

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

// ProvisionMerchant 开通商户。
func (s *Service) ProvisionMerchant(ctx context.Context, c *auth.Claims, method, path, key string, req MerchantProvisionReq) (OperationDTO, error) {
	actor, e := adminID(c, "merchant:provision")
	if e != nil {
		return OperationDTO{}, e
	}
	if e := s.requireEnabled(); e != nil {
		return OperationDTO{}, e
	}
	if e := normalizeShopCoordinates(&req.Shop); e != nil {
		return OperationDTO{}, e
	}
	var out OperationDTO
	e = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, e := s.idem.Start(ctx, tx, s.ids.Next(), "admin", actor, method, path, key, idempotency.RequestHash(req))
		if e != nil {
			return e
		}
		if !started {
			return cached(ctx, s.idem, tx, "admin", actor, path, key, &out)
		}
		now := time.Now()
		opID := s.ids.Next()
		merchantID := s.ids.Next()
		shopID := s.ids.Next()
		accountID := s.ids.Next()
		userID := s.ids.Next()
		op := Operation{ID: opID, OperationNo: fmt.Sprintf("PO%d", opID), OperationType: "merchant_provision", IdempotencyKeyHash: securevalue.Digest(key), RequestHash: idempotency.RequestHash(req), ActorID: actor, Status: "processing", StartedAt: now}
		if e := tx.Create(&op).Error; e != nil {
			return e
		}
		password, e := randomSecret()
		if e != nil {
			return e
		}
		hash, e := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if e != nil {
			return e
		}
		if e := tx.Table("merchants").Create(map[string]any{"id": merchantID, "code": req.Merchant.Code, "name": req.Merchant.Name, "contact_name": null(req.Merchant.ContactName), "contact_phone": null(req.Merchant.ContactPhone), "license_no": null(req.Merchant.LicenseNo), "status": "active", "review_status": "approved", "reviewed_at": now, "created_by": actor, "updated_by": actor}).Error; e != nil {
			return identityConflict(e)
		}
		if e := tx.Table("shops").Create(map[string]any{"id": shopID, "merchant_id": merchantID, "name": req.Shop.Name, "phone": null(req.Shop.Phone), "province": null(req.Shop.Province), "city": req.Shop.City, "city_code": null(req.Shop.CityCode), "district": req.Shop.District, "address": req.Shop.Address, "latitude": req.Shop.Latitude, "longitude": req.Shop.Longitude, "coordinate_system": req.Shop.CoordinateSystem, "status": "active", "business_status": "open", "created_by": actor, "updated_by": actor}).Error; e != nil {
			return e
		}
		if e := createAccount(tx, accountID, "merchant", req.Account, hash, actor); e != nil {
			return e
		}
		if e := tx.Table("merchant_users").Create(map[string]any{"id": userID, "account_id": accountID, "merchant_id": merchantID, "name": req.MerchantUserName, "status": "active", "created_by": actor, "updated_by": actor}).Error; e != nil {
			return e
		}
		if e := tx.Table("merchant_user_shops").Create(map[string]any{"id": s.ids.Next(), "merchant_user_id": userID, "merchant_id": merchantID, "shop_id": shopID, "created_by": actor, "updated_by": actor}).Error; e != nil {
			return e
		}
		if e := s.createResetToken(tx, accountID, actor, "activate"); e != nil {
			return e
		}
		resources := map[string]string{"merchant_id": idString(merchantID), "shop_id": idString(shopID), "account_id": idString(accountID), "merchant_user_id": idString(userID)}
		raw, _ := json.Marshal(resources)
		target := "merchant"
		finished := time.Now()
		if e := tx.Model(&Operation{}).Where("id=?", opID).Updates(map[string]any{"status": "succeeded", "target_type": target, "target_id": merchantID, "step_state": datatypes.JSON(raw), "finished_at": finished}).Error; e != nil {
			return e
		}
		if e := audit(ctx, tx, s.ids.Next(), actor, "merchant.provision", merchantID, resources); e != nil {
			return e
		}
		if e := outbox(ctx, tx, s.ids.Next(), "account.activation.requested", "account", accountID, map[string]any{"account_id": idString(accountID), "account_type": "merchant"}); e != nil {
			return e
		}
		out = OperationDTO{ID: idString(opID), OperationNo: op.OperationNo, OperationType: op.OperationType, Status: "succeeded", TargetType: target, TargetID: idString(merchantID), ResourceIDs: resources, StartedAt: now.Format(time.RFC3339), FinishedAt: finished.Format(time.RFC3339)}
		return s.idem.Succeed(ctx, tx, "admin", actor, path, key, out)
	})
	return out, e
}

func normalizeShopCoordinates(input *ShopInput) error {
	if input.Latitude == nil && input.Longitude == nil {
		if input.CoordinateSystem != "" {
			return problem.InvalidArgument("COORDINATE_INVALID", "coordinate_system requires latitude and longitude")
		}
		input.CoordinateSystem = coordinate.GCJ02
		return nil
	}
	if input.Latitude == nil || input.Longitude == nil || input.CoordinateSystem == "" {
		return problem.InvalidArgument("COORDINATE_INVALID", "latitude, longitude, and coordinate_system must be provided together")
	}
	lat, lng, err := coordinate.Normalize(*input.Latitude, *input.Longitude, input.CoordinateSystem)
	if err != nil {
		return problem.InvalidArgument("COORDINATE_INVALID", "coordinate is invalid")
	}
	input.Latitude, input.Longitude, input.CoordinateSystem = &lat, &lng, coordinate.GCJ02
	return nil
}

// CreateMerchantUser 创建商户用户。
func (s *Service) CreateMerchantUser(ctx context.Context, c *auth.Claims, method, path, key, merchantRaw string, req MerchantUserReq) (OperationDTO, error) {
	actor, e := adminID(c, "merchant_user:create")
	if e != nil {
		return OperationDTO{}, e
	}
	if e := s.requireEnabled(); e != nil {
		return OperationDTO{}, e
	}
	merchant, e := parseID(merchantRaw)
	if e != nil {
		return OperationDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid merchant id")
	}
	shopIDs, e := parseIDs(req.ShopIDs)
	if e != nil {
		return OperationDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid shop_ids")
	}
	var out OperationDTO
	e = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, e := s.idem.Start(ctx, tx, s.ids.Next(), "admin", actor, method, path, key, idempotency.RequestHash(req))
		if e != nil {
			return e
		}
		if !started {
			return cached(ctx, s.idem, tx, "admin", actor, path, key, &out)
		}
		var count int64
		if e := tx.Table("shops").Where("merchant_id=? AND id IN ? AND deleted_at IS NULL", merchant, shopIDs).Count(&count).Error; e != nil {
			return e
		}
		if int(count) != len(shopIDs) {
			return problem.Forbidden("PERM_FORBIDDEN", "shop is outside merchant scope")
		}
		accountID, userID, opID := s.ids.Next(), s.ids.Next(), s.ids.Next()
		password, _ := randomSecret()
		hash, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if e := createAccount(tx, accountID, "merchant", req.Account, hash, actor); e != nil {
			return e
		}
		if e := tx.Table("merchant_users").Create(map[string]any{"id": userID, "account_id": accountID, "merchant_id": merchant, "name": req.Name, "status": "active", "created_by": actor, "updated_by": actor}).Error; e != nil {
			return e
		}
		for _, shop := range shopIDs {
			if e := tx.Table("merchant_user_shops").Create(map[string]any{"id": s.ids.Next(), "merchant_user_id": userID, "merchant_id": merchant, "shop_id": shop, "created_by": actor, "updated_by": actor}).Error; e != nil {
				return e
			}
		}
		if e := s.createResetToken(tx, accountID, actor, "activate"); e != nil {
			return e
		}
		out, e = s.finishSimpleOperation(ctx, tx, opID, actor, "merchant_user_create", "merchant_user", userID, map[string]string{"account_id": idString(accountID), "merchant_user_id": idString(userID)})
		if e != nil {
			return e
		}
		return s.idem.Succeed(ctx, tx, "admin", actor, path, key, out)
	})
	return out, e
}

// AuthorizeShops 为Shops授予访问权限。
func (s *Service) AuthorizeShops(ctx context.Context, c *auth.Claims, method, path, key, userRaw string, req ShopAuthorizationReq) (OperationDTO, error) {
	actor, e := adminID(c, "merchant_user:authorize_shop")
	if e != nil {
		return OperationDTO{}, e
	}
	if e := s.requireEnabled(); e != nil {
		return OperationDTO{}, e
	}
	user, e := parseID(userRaw)
	if e != nil {
		return OperationDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid merchant user id")
	}
	shops, e := parseIDs(req.ShopIDs)
	if e != nil {
		return OperationDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid shop ids")
	}
	var out OperationDTO
	e = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, e := s.idem.Start(ctx, tx, s.ids.Next(), "admin", actor, method, path, key, idempotency.RequestHash(req))
		if e != nil {
			return e
		}
		if !started {
			return cached(ctx, s.idem, tx, "admin", actor, path, key, &out)
		}
		var merchant uint64
		if e := tx.Table("merchant_users").Select("merchant_id").Where("id=?", user).Scan(&merchant).Error; e != nil || merchant == 0 {
			return problem.NotFound("MERCHANT_USER_NOT_FOUND", "merchant user not found")
		}
		var count int64
		if len(shops) > 0 {
			if e := tx.Table("shops").Where("merchant_id=? AND id IN ?", merchant, shops).Count(&count).Error; e != nil {
				return e
			}
			if int(count) != len(shops) {
				return problem.Forbidden("PERM_FORBIDDEN", "shop is outside merchant scope")
			}
		}
		var before []uint64
		if e := tx.Table("merchant_user_shops").Select("shop_id").Where("merchant_user_id=? AND deleted_at IS NULL", user).Scan(&before).Error; e != nil {
			return e
		}
		now := time.Now()
		if e := tx.Table("merchant_user_shops").Where("merchant_user_id=? AND deleted_at IS NULL", user).Updates(map[string]any{"deleted_at": now, "updated_by": actor}).Error; e != nil {
			return e
		}
		for _, shop := range shops {
			if e := tx.Exec(`
				INSERT INTO merchant_user_shops (id, merchant_user_id, merchant_id, shop_id, created_by, updated_by)
				VALUES (?, ?, ?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE merchant_id=VALUES(merchant_id), deleted_at=NULL, updated_by=VALUES(updated_by)
			`, s.ids.Next(), user, merchant, shop, actor, actor).Error; e != nil {
				return e
			}
		}
		beforeJSON, _ := json.Marshal(before)
		afterJSON, _ := json.Marshal(shops)
		out, e = s.finishSimpleOperation(ctx, tx, s.ids.Next(), actor, "merchant_shop_authorize", "merchant_user", user, map[string]string{"merchant_user_id": idString(user), "before_shop_ids": string(beforeJSON), "after_shop_ids": string(afterJSON)})
		if e != nil {
			return e
		}
		return s.idem.Succeed(ctx, tx, "admin", actor, path, key, out)
	})
	return out, e
}

// AccountStatus 返回账户状态。
func (s *Service) AccountStatus(ctx context.Context, c *auth.Claims, method, path, key, idRaw string, req AccountStatusReq) (OperationDTO, error) {
	actor, e := adminID(c, "account:status_update")
	if e != nil {
		return OperationDTO{}, e
	}
	if e := s.requireEnabled(); e != nil {
		return OperationDTO{}, e
	}
	id, e := parseID(idRaw)
	if e != nil {
		return OperationDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid account id")
	}
	var out OperationDTO
	e = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, e := s.idem.Start(ctx, tx, s.ids.Next(), "admin", actor, method, path, key, idempotency.RequestHash(req))
		if e != nil {
			return e
		}
		if !started {
			return cached(ctx, s.idem, tx, "admin", actor, path, key, &out)
		}
		now := time.Now()
		res := tx.Table("accounts").Where("id=? AND deleted_at IS NULL", id).Updates(map[string]any{"status": req.Status, "token_invalid_before": now, "credential_version": gorm.Expr("credential_version+1"), "updated_by": actor})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return problem.NotFound("ACCOUNT_NOT_FOUND", "account not found")
		}
		out, e = s.finishSimpleOperation(ctx, tx, s.ids.Next(), actor, "account_status", "account", id, map[string]string{"account_id": idString(id), "status": req.Status})
		if e != nil {
			return e
		}
		if e := audit(ctx, tx, s.ids.Next(), actor, "account.status_update", id, map[string]any{"status": req.Status, "reason": req.Reason}); e != nil {
			return e
		}
		return s.idem.Succeed(ctx, tx, "admin", actor, path, key, out)
	})
	return out, e
}

// ResetPassword 重置密码。
func (s *Service) ResetPassword(ctx context.Context, c *auth.Claims, method, path, key, idRaw string, req ResetPasswordReq) (OperationDTO, error) {
	actor, e := adminID(c, "account:reset_password")
	if e != nil {
		return OperationDTO{}, e
	}
	if e := s.requireEnabled(); e != nil {
		return OperationDTO{}, e
	}
	id, e := parseID(idRaw)
	if e != nil {
		return OperationDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid account id")
	}
	var out OperationDTO
	e = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, e := s.idem.Start(ctx, tx, s.ids.Next(), "admin", actor, method, path, key, idempotency.RequestHash(req))
		if e != nil {
			return e
		}
		if !started {
			return cached(ctx, s.idem, tx, "admin", actor, path, key, &out)
		}
		var account struct {
			AccountType string
		}
		if e := tx.Table("accounts").Select("account_type").Where("id=? AND deleted_at IS NULL", id).Take(&account).Error; errors.Is(e, gorm.ErrRecordNotFound) {
			return problem.NotFound("ACCOUNT_NOT_FOUND", "account not found")
		} else if e != nil {
			return e
		}
		if account.AccountType == "rider" {
			return problem.Conflict("ACCOUNT_PASSWORD_UNSUPPORTED", "rider accounts use sms login and do not support password reset")
		}
		now := time.Now()
		res := tx.Table("accounts").Where("id=? AND deleted_at IS NULL", id).Updates(map[string]any{"token_invalid_before": now, "credential_version": gorm.Expr("credential_version+1")})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return problem.NotFound("ACCOUNT_NOT_FOUND", "account not found")
		}
		if e := s.createResetToken(tx, id, actor, "reset_password"); e != nil {
			return e
		}
		out, e = s.finishSimpleOperation(ctx, tx, s.ids.Next(), actor, "account_reset_password", "account", id, map[string]string{"account_id": idString(id)})
		if e != nil {
			return e
		}
		if e := outbox(ctx, tx, s.ids.Next(), "account.password_reset.requested", "account", id, map[string]any{"account_id": idString(id)}); e != nil {
			return e
		}
		return s.idem.Succeed(ctx, tx, "admin", actor, path, key, out)
	})
	return out, e
}

// CreateRider 创建骑手。
func (s *Service) CreateRider(ctx context.Context, c *auth.Claims, method, path, key string, req RiderCreateReq) (RiderDTO, error) {
	actor, e := adminID(c, "rider:create")
	if e != nil {
		return RiderDTO{}, e
	}
	if e := s.requireEnabled(); e != nil {
		return RiderDTO{}, e
	}
	req, shopIDs, e := normalizeRiderCreate(req)
	if e != nil {
		return RiderDTO{}, e
	}
	var out RiderDTO
	e = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, e := s.idem.Start(ctx, tx, s.ids.Next(), "admin", actor, method, path, key, idempotency.RequestHash(req))
		if e != nil {
			return e
		}
		if !started {
			return cached(ctx, s.idem, tx, "admin", actor, path, key, &out)
		}
		if e := validateProvisioningRiderShops(ctx, tx, shopIDs); e != nil {
			return e
		}
		accountID, riderID := s.ids.Next(), s.ids.Next()
		if e := identityConflict(tx.Table("accounts").Create(map[string]any{
			"id": accountID, "account_type": "rider", "phone": req.Phone,
			"status": "disabled", "credential_version": 1, "created_by": actor, "updated_by": actor,
		}).Error); e != nil {
			return e
		}
		scope, _ := json.Marshal(req.ServiceScope)
		if e := tx.Table("riders").Create(map[string]any{"id": riderID, "account_id": accountID, "name": req.Name, "phone": req.Phone, "status": "disabled", "work_status": "offline", "review_status": "pending", "service_scope": datatypes.JSON(scope), "created_by": actor, "updated_by": actor}).Error; e != nil {
			return e
		}
		for _, shopID := range shopIDs {
			if e := tx.Table("rider_service_shops").Create(map[string]any{"id": s.ids.Next(), "rider_id": riderID, "shop_id": shopID, "status": "active", "source": "provisioning", "created_by": actor, "updated_by": actor}).Error; e != nil {
				return e
			}
		}
		if e := tx.Table("rider_runtime_states").Create(map[string]any{"rider_id": riderID, "work_status": "offline", "last_sequence": 0, "version": 1}).Error; e != nil {
			return e
		}
		out = RiderDTO{ID: idString(riderID), AccountID: idString(accountID), Name: req.Name, Phone: req.Phone, Status: "disabled", ReviewStatus: "pending", ServiceScope: req.ServiceScope}
		if e := audit(ctx, tx, s.ids.Next(), actor, "rider.create", riderID, out); e != nil {
			return e
		}
		return s.idem.Succeed(ctx, tx, "admin", actor, path, key, out)
	})
	return out, e
}

// ReviewRider 审核骑手。
func (s *Service) ReviewRider(ctx context.Context, c *auth.Claims, method, path, key, idRaw string, req RiderReviewReq) (RiderDTO, error) {
	actor, e := adminID(c, "rider:review")
	if e != nil {
		return RiderDTO{}, e
	}
	if e := s.requireEnabled(); e != nil {
		return RiderDTO{}, e
	}
	return s.updateRider(ctx, actor, method, path, key, idRaw, "review", req.Decision, req.Reason)
}

// RiderStatus 返回骑手状态。
func (s *Service) RiderStatus(ctx context.Context, c *auth.Claims, method, path, key, idRaw string, req RiderStatusReq) (RiderDTO, error) {
	actor, e := adminID(c, "rider:status_update")
	if e != nil {
		return RiderDTO{}, e
	}
	if e := s.requireEnabled(); e != nil {
		return RiderDTO{}, e
	}
	return s.updateRider(ctx, actor, method, path, key, idRaw, "status", req.Status, req.Reason)
}

// requireEnabled 校验并确保启用状态满足要求。
func (s *Service) requireEnabled() error {
	if !s.cfg.ProvisioningEnabled {
		return problem.New(503, "PROVISIONING_DISABLED", "Service Unavailable", "provisioning is disabled")
	}
	return nil
}

// updateRider 更新骑手。
func (s *Service) updateRider(ctx context.Context, actor uint64, method, path, key, idRaw, kind, value, reason string) (RiderDTO, error) {
	id, e := parseID(idRaw)
	if e != nil {
		return RiderDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid rider id")
	}
	var out RiderDTO
	e = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, e := s.idem.Start(ctx, tx, s.ids.Next(), "admin", actor, method, path, key, idempotency.RequestHash(map[string]string{"kind": kind, "value": value, "reason": reason}))
		if e != nil {
			return e
		}
		if !started {
			return cached(ctx, s.idem, tx, "admin", actor, path, key, &out)
		}
		var row struct {
			ID           uint64
			AccountID    uint64
			Name         string
			Phone        *string
			Status       string
			ReviewStatus string
			ServiceScope datatypes.JSON
		}
		if e := tx.Table("riders").Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=?", id).First(&row).Error; e != nil {
			return problem.NotFound("RIDER_NOT_FOUND", "rider not found")
		}
		updates := map[string]any{}
		if kind == "review" {
			if row.ReviewStatus != "pending" {
				return problem.Conflict("RIDER_REVIEW_STATE_CONFLICT", "rider is no longer pending review")
			}
			if value == "approved" {
				updates["review_status"] = "approved"
				updates["status"] = "active"
			} else {
				updates["review_status"] = "rejected"
				updates["status"] = "disabled"
			}
		} else {
			if value == "active" && row.ReviewStatus != "approved" {
				return problem.Conflict("RIDER_REVIEW_REQUIRED", "rider must be approved before activation")
			}
			updates["status"] = value
		}
		if e := tx.Table("riders").Where("id=?", id).Updates(updates).Error; e != nil {
			return e
		}
		if status, ok := updates["status"]; ok {
			now := time.Now()
			invalidBefore := now
			if status == "active" {
				// JWT issued-at precision is one second. Keep a freshly activated
				// rider's first login valid while still revoking pre-activation tokens.
				invalidBefore = now.Add(-time.Second)
			}
			if e := tx.Table("accounts").Where("id=?", row.AccountID).Updates(map[string]any{"status": status, "token_invalid_before": invalidBefore}).Error; e != nil {
				return e
			}
			row.Status = status.(string)
		}
		if review, ok := updates["review_status"]; ok {
			row.ReviewStatus = review.(string)
		}
		scope := map[string]any{}
		_ = json.Unmarshal(row.ServiceScope, &scope)
		out = RiderDTO{ID: idString(row.ID), AccountID: idString(row.AccountID), Name: row.Name, Phone: stringValue(row.Phone), Status: row.Status, ReviewStatus: row.ReviewStatus, ServiceScope: scope}
		if e := audit(ctx, tx, s.ids.Next(), actor, "rider."+kind, id, map[string]any{"value": value, "reason": reason}); e != nil {
			return e
		}
		return s.idem.Succeed(ctx, tx, "admin", actor, path, key, out)
	})
	return out, e
}

// GetOperation 获取操作。
func (s *Service) GetOperation(ctx context.Context, c *auth.Claims, idRaw string) (OperationDTO, error) {
	if _, e := adminIDAny(c); e != nil {
		return OperationDTO{}, e
	}
	id, e := parseID(idRaw)
	if e != nil {
		return OperationDTO{}, problem.NotFound("PROVISIONING_OPERATION_NOT_FOUND", "operation not found")
	}
	var row Operation
	if e := s.db.WithContext(ctx).First(&row, id).Error; e != nil {
		return OperationDTO{}, problem.NotFound("PROVISIONING_OPERATION_NOT_FOUND", "operation not found")
	}
	resources := map[string]string{}
	_ = json.Unmarshal(row.StepState, &resources)
	return OperationDTO{ID: idString(row.ID), OperationNo: row.OperationNo, OperationType: row.OperationType, Status: row.Status, TargetType: stringValue(row.TargetType), TargetID: uintString(row.TargetID), ResourceIDs: resources, StartedAt: row.StartedAt.Format(time.RFC3339), FinishedAt: timeString(row.FinishedAt)}, nil
}

// finishSimpleOperation 完成简单操作操作。
func (s *Service) finishSimpleOperation(ctx context.Context, tx *gorm.DB, id, actor uint64, kind, target string, targetID uint64, resources map[string]string) (OperationDTO, error) {
	now := time.Now()
	raw, _ := json.Marshal(resources)
	row := Operation{ID: id, OperationNo: fmt.Sprintf("PO%d", id), OperationType: kind, IdempotencyKeyHash: securevalue.Digest(uuid.NewString()), RequestHash: securevalue.Digest(kind), ActorID: actor, Status: "succeeded", TargetType: &target, TargetID: &targetID, StepState: raw, StartedAt: now, FinishedAt: &now}
	if e := tx.Create(&row).Error; e != nil {
		return OperationDTO{}, e
	}
	return OperationDTO{ID: idString(id), OperationNo: row.OperationNo, OperationType: kind, Status: "succeeded", TargetType: target, TargetID: idString(targetID), ResourceIDs: resources, StartedAt: now.Format(time.RFC3339), FinishedAt: now.Format(time.RFC3339)}, nil
}

// createResetToken 创建重置令牌。
func (s *Service) createResetToken(tx *gorm.DB, account, actor uint64, purpose string) error {
	secret, e := randomSecret()
	if e != nil {
		return e
	}
	ciphertext, e := securevalue.Seal(s.cfg.DataEncryptionKey, secret)
	if e != nil {
		return e
	}
	return tx.Table("credential_reset_tokens").Create(map[string]any{"id": s.ids.Next(), "account_id": account, "token_hash": securevalue.HMAC(s.cfg.VerificationPepper, secret), "token_ciphertext": ciphertext, "purpose": purpose, "expires_at": time.Now().Add(30 * time.Minute), "created_by": actor}).Error
}

// createAccount 创建账户。
func createAccount(tx *gorm.DB, id uint64, kind string, input AccountInput, hash []byte, actor uint64) error {
	return createAccountWithStatus(tx, id, kind, input, hash, "active", actor)
}

// createAccountWithStatus 创建指定初始状态的账户。
func createAccountWithStatus(tx *gorm.DB, id uint64, kind string, input AccountInput, hash []byte, status string, actor uint64) error {
	e := tx.Table("accounts").Create(map[string]any{"id": id, "account_type": kind, "username": input.Username, "phone": null(input.Phone), "password_hash": string(hash), "status": status, "created_by": actor, "updated_by": actor}).Error
	return identityConflict(e)
}

// validateProvisioningRiderShops 防止管理员把骑手绑定到不存在或已停用的门店。
func validateProvisioningRiderShops(ctx context.Context, tx *gorm.DB, shopIDs []uint64) error {
	var activeIDs []uint64
	if e := tx.WithContext(ctx).Table("shops").Clauses(clause.Locking{Strength: "SHARE"}).
		Select("id").Where("id IN ? AND status = 'active' AND deleted_at IS NULL", shopIDs).Find(&activeIDs).Error; e != nil {
		return e
	}
	if len(activeIDs) != len(shopIDs) {
		return problem.InvalidArgument("VALIDATION_FAILED", "service_scope contains a missing or inactive shop")
	}
	return nil
}

// identityConflict 返回身份冲突。
func identityConflict(e error) error {
	if e == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(e.Error()), "duplicate") {
		return problem.Conflict("IDENTITY_CONFLICT", "username, phone, or business code already exists")
	}
	return e
}

// randomSecret 返回random 密钥。
func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// adminID 从认证声明中解析并返回管理员 ID。
func adminID(c *auth.Claims, p string) (uint64, error) {
	if c == nil || c.AccountType != "admin" || !has(c.Permissions, p) {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
	}
	return parseID(c.AdminUserID)
}

// adminIDAny 返回管理端ID Any。
func adminIDAny(c *auth.Claims) (uint64, error) {
	if c == nil || c.AccountType != "admin" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "admin required")
	}
	return parseID(c.AdminUserID)
}

// has 判断是否存在开通。
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

// parseIDs 解析I Ds。
func parseIDs(v []string) ([]uint64, error) {
	out := make([]uint64, 0, len(v))
	for _, x := range v {
		id, e := parseID(x)
		if e != nil {
			return nil, e
		}
		out = append(out, id)
	}
	return out, nil
}

// idString 将数字 ID 转换为字符串。
func idString(v uint64) string { return strconv.FormatUint(v, 10) }

// null 返回null。
func null(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

// stringValue 安全读取字符串指针的值。
func stringValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// uintString 将无符号整数转换为字符串。
func uintString(v *uint64) string {
	if v == nil {
		return ""
	}
	return idString(*v)
}

// timeString 将可选时间转换为字符串。
func timeString(v *time.Time) string {
	if v == nil {
		return ""
	}
	return v.Format(time.RFC3339)
}

// cached 返回缓存。
func cached(ctx context.Context, s *idempotency.Store, tx *gorm.DB, t string, id uint64, path, key string, target any) error {
	ok, e := s.CachedResponse(ctx, tx, t, id, path, key, target)
	if e != nil {
		return e
	}
	if !ok {
		return problem.Conflict("IDEMPOTENCY_CONFLICT", "idempotency response missing")
	}
	return nil
}

// audit 返回审计。
func audit(ctx context.Context, tx *gorm.DB, id, actor uint64, action string, resource uint64, after any) error {
	raw, _ := json.Marshal(after)
	return tx.Table("audit_logs").Create(map[string]any{"id": id, "actor_type": "admin", "actor_id": actor, "action": action, "resource_type": "provisioning", "resource_id": resource, "after_data": datatypes.JSON(raw), "result": "success", "request_id": requestctx.RequestIDPtr(ctx)}).Error
}

// outbox 返回发件箱事件。
func outbox(ctx context.Context, tx *gorm.DB, id uint64, event, aggregate string, aggregateID uint64, payload any) error {
	raw, _ := json.Marshal(payload)
	return tx.Table("outbox_events").Create(map[string]any{"id": id, "event_id": uuid.NewString(), "event_type": event, "aggregate_type": aggregate, "aggregate_id": aggregateID, "payload": datatypes.JSON(raw), "status": "pending", "request_id": requestctx.RequestIDPtr(ctx)}).Error
}
