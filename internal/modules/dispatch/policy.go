package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

// ListPolicies 查询Policies列表。
func (s *Service) ListPolicies(ctx context.Context, claims *auth.Claims, query pagination.Query) ([]PolicyDTO, string, error) {
	if _, err := adminActor(claims, "dispatch_policy:list"); err != nil {
		return nil, "", err
	}
	db := s.db.WithContext(ctx)
	var err error
	db, err = pagination.ApplyFilter(db, query.Filter, map[string]string{
		"scope_type": "scope_type", "scope_id": "scope_id", "mode": "mode", "status": "status", "created_at": "created_at",
	})
	if err != nil {
		return nil, "", err
	}
	db, err = pagination.ApplyOrder(db, query.OrderBy, map[string]string{
		"created_at": "created_at", "published_at": "published_at", "version": "version", "id": "id",
	}, "created_at DESC,id DESC")
	if err != nil {
		return nil, "", err
	}
	var rows []Policy
	if err := db.Offset(query.Offset).Limit(query.PageSize + 1).Find(&rows).Error; err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		next = pagination.NextPageToken(query)
		rows = rows[:query.PageSize]
	}
	items := make([]PolicyDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, policyDTO(row))
	}
	return items, next, nil
}

// CreatePolicy 创建策略。
func (s *Service) CreatePolicy(ctx context.Context, claims *auth.Claims, method, path, key string, req PolicyCreateReq) (PolicyDTO, error) {
	actor, err := adminActor(claims, "dispatch_policy:create")
	if err != nil {
		return PolicyDTO{}, err
	}
	if err := validatePolicyRequest(req); err != nil {
		return PolicyDTO{}, err
	}
	var out PolicyDTO
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), "admin", actor, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		if !started {
			cached, err := s.idem.CachedResponse(ctx, tx, "admin", actor, path, key, &out)
			if err != nil || cached {
				return err
			}
			return problem.Conflict("IDEMPOTENCY_CONFLICT", "idempotency response missing")
		}
		if allowed, rateErr := s.allowFixedWindow(ctx, idempotencyRateKey("dispatch:admin:policy-writes", actor), time.Minute, 20); rateErr == nil && !allowed {
			return rateLimited("policy writes are limited to twenty requests per minute", time.Minute)
		}
		if allowed, rateErr := s.allowFixedWindow(ctx, "dispatch:policy:scope-writes:"+req.ScopeType+":"+req.ScopeID, time.Minute, 5); rateErr == nil && !allowed {
			return rateLimited("policy writes for this scope are limited to five requests per minute", time.Minute)
		}
		var maxVersion uint
		if err := tx.Model(&Policy{}).Where("policy_code=? AND scope_type=? AND scope_id=?", req.PolicyCode, req.ScopeType, req.ScopeID).Select("COALESCE(MAX(version),0)").Scan(&maxVersion).Error; err != nil {
			return err
		}
		weights, _ := json.Marshal(req.ScoreWeights)
		row := Policy{
			ID: s.ids.Next(), PolicyCode: req.PolicyCode, ScopeType: req.ScopeType, ScopeID: req.ScopeID,
			Version: maxVersion + 1, Mode: req.Mode, AutoRounds: req.AutoRounds,
			OfferTTLSeconds: req.OfferTTLSeconds, GrabTTLSeconds: req.GrabTTLSeconds,
			CandidateLimit: req.CandidateLimit, OfferCandidateLimit: req.OfferCandidateLimit,
			HeartbeatFreshSeconds: req.HeartbeatFreshSeconds, LocationFreshSeconds: req.LocationFreshSeconds,
			MaxLocationAccuracyM: req.MaxLocationAccuracyM, MaxPickupDistanceM: req.MaxPickupDistanceM,
			MaxActiveOrdersDefault: req.MaxActiveOrdersDefault, IdleFullScoreSeconds: req.IdleFullScoreSeconds,
			ScoreWeights: weights, RejectionCooldownSeconds: req.RejectionCooldownSeconds,
			Status: "draft", RowVersion: 1, CreatedBy: actor, UpdatedBy: actor,
		}
		if err := tx.Create(&row).Error; err != nil {
			if isDuplicate(err) {
				return problem.Conflict("VERSION_CONFLICT", "policy version was created concurrently")
			}
			return err
		}
		if err := s.createAudit(ctx, tx, "admin", actor, "dispatch.policy.create", "dispatch_policy", row.ID, nil, row); err != nil {
			return err
		}
		out = policyDTO(row)
		return s.idem.Succeed(ctx, tx, "admin", actor, path, key, out)
	})
	return out, err
}

// ValidatePolicy 校验策略是否合法。
func (s *Service) ValidatePolicy(ctx context.Context, claims *auth.Claims, idRaw string, req VersionReq) (PolicyDTO, error) {
	actor, err := adminActor(claims, "dispatch_policy:validate")
	if err != nil {
		return PolicyDTO{}, err
	}
	id, err := parseID(idRaw)
	if err != nil {
		return PolicyDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid policy id")
	}
	if allowed, rateErr := s.allowFixedWindow(ctx, idempotencyRateKey("dispatch:admin:policy-writes", actor), time.Minute, 20); rateErr == nil && !allowed {
		return PolicyDTO{}, rateLimited("policy writes are limited to twenty requests per minute", time.Minute)
	}
	var out PolicyDTO
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Probe the scope without a lock, then lock every policy in that scope in
		// id order. Locking only the target draft allows two concurrent publishes
		// to deadlock or race on the generated published-scope unique key.
		var probe Policy
		if err := tx.Select("id,scope_type,scope_id").First(&probe, id).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("DISPATCH_POLICY_NOT_FOUND", "dispatch policy not found")
		} else if err != nil {
			return err
		}
		var scopeRows []Policy
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("scope_type=? AND scope_id=?", probe.ScopeType, probe.ScopeID).Order("id").Find(&scopeRows).Error; err != nil {
			return err
		}
		if allowed, rateErr := s.allowFixedWindow(ctx, "dispatch:policy:scope-writes:"+probe.ScopeType+":"+probe.ScopeID, time.Minute, 5); rateErr == nil && !allowed {
			return rateLimited("policy writes for this scope are limited to five requests per minute", time.Minute)
		}
		var row Policy
		for _, candidate := range scopeRows {
			if candidate.ID == id {
				row = candidate
				break
			}
		}
		if row.ID == 0 {
			return problem.NotFound("DISPATCH_POLICY_NOT_FOUND", "dispatch policy not found")
		}
		if row.RowVersion != req.ExpectedVersion {
			return problem.Conflict("VERSION_CONFLICT", "policy version changed")
		}
		if row.Status != "draft" && row.Status != "validated" {
			return problem.Conflict("DISPATCH_POLICY_INVALID", "policy status cannot be validated")
		}
		if err := validatePolicySnapshot(snapshotFromPolicy(row)); err != nil {
			return err
		}
		if row.Status == "draft" {
			if err := tx.Model(&Policy{}).Where("id=? AND row_version=?", row.ID, row.RowVersion).Updates(map[string]any{"status": "validated", "row_version": gorm.Expr("row_version+1"), "updated_by": actor}).Error; err != nil {
				return err
			}
			row.Status = "validated"
			row.RowVersion++
		}
		if err := s.createAudit(ctx, tx, "admin", actor, "dispatch.policy.validate", "dispatch_policy", row.ID, nil, map[string]any{"status": row.Status}); err != nil {
			return err
		}
		out = policyDTO(row)
		return nil
	})
	return out, err
}

// PublishPolicy 发布策略。
func (s *Service) PublishPolicy(ctx context.Context, claims *auth.Claims, method, path, key, idRaw string, req VersionReq) (PolicyDTO, error) {
	actor, err := adminActor(claims, "dispatch_policy:publish")
	if err != nil {
		return PolicyDTO{}, err
	}
	return s.changePolicyStatus(ctx, actor, method, path, key, idRaw, req, "published")
}

// RetirePolicy 返回Retire 策略。
func (s *Service) RetirePolicy(ctx context.Context, claims *auth.Claims, method, path, key, idRaw string, req VersionReq) (PolicyDTO, error) {
	actor, err := adminActor(claims, "dispatch_policy:publish")
	if err != nil {
		return PolicyDTO{}, err
	}
	return s.changePolicyStatus(ctx, actor, method, path, key, idRaw, req, "retired")
}

// changePolicyStatus 返回change 策略状态。
func (s *Service) changePolicyStatus(ctx context.Context, actor uint64, method, path, key, idRaw string, req VersionReq, target string) (PolicyDTO, error) {
	id, err := parseID(idRaw)
	if err != nil {
		return PolicyDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid policy id")
	}
	var out PolicyDTO
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), "admin", actor, method, path, key, idempotency.RequestHash(map[string]any{"id": id, "target": target, "version": req.ExpectedVersion}))
		if err != nil {
			return err
		}
		if !started {
			cached, err := s.idem.CachedResponse(ctx, tx, "admin", actor, path, key, &out)
			if err != nil || cached {
				return err
			}
			return problem.Conflict("IDEMPOTENCY_CONFLICT", "idempotency response missing")
		}
		if allowed, rateErr := s.allowFixedWindow(ctx, idempotencyRateKey("dispatch:admin:policy-status", actor), time.Minute, 2); rateErr == nil && !allowed {
			return rateLimited("policy publish and retire actions are limited to two requests per minute", time.Minute)
		}
		var probe Policy
		if err := tx.Select("id,scope_type,scope_id").First(&probe, id).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("DISPATCH_POLICY_NOT_FOUND", "dispatch policy not found")
		} else if err != nil {
			return err
		}
		var scopeRows []Policy
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("scope_type=? AND scope_id=?", probe.ScopeType, probe.ScopeID).Order("id").Find(&scopeRows).Error; err != nil {
			return err
		}
		var row Policy
		for _, candidate := range scopeRows {
			if candidate.ID == id {
				row = candidate
				break
			}
		}
		if row.ID == 0 {
			return problem.NotFound("DISPATCH_POLICY_NOT_FOUND", "dispatch policy not found")
		}
		if row.RowVersion != req.ExpectedVersion {
			return problem.Conflict("VERSION_CONFLICT", "policy version changed")
		}
		now := time.Now()
		updates := map[string]any{"status": target, "row_version": gorm.Expr("row_version+1"), "updated_by": actor}
		if target == "published" {
			if row.Status != "validated" {
				return problem.Conflict("DISPATCH_POLICY_INVALID", "only validated policy can be published")
			}
			if err := tx.Model(&Policy{}).Where("scope_type=? AND scope_id=? AND status='published' AND id<>?", row.ScopeType, row.ScopeID, row.ID).Updates(map[string]any{"status": "retired", "row_version": gorm.Expr("row_version+1"), "updated_by": actor}).Error; err != nil {
				return err
			}
			updates["published_at"] = now
			updates["published_by"] = actor
		} else if row.Status != "published" && row.Status != "validated" {
			return problem.Conflict("DISPATCH_POLICY_INVALID", "policy cannot be retired")
		}
		result := tx.Model(&Policy{}).Where("id=? AND row_version=?", row.ID, row.RowVersion).Updates(updates)
		if result.Error != nil {
			if isDuplicate(result.Error) {
				return problem.Conflict("VERSION_CONFLICT", "another policy was published for this scope")
			}
			return result.Error
		}
		if result.RowsAffected != 1 {
			return problem.Conflict("VERSION_CONFLICT", "policy version changed")
		}
		row.Status = target
		row.RowVersion++
		if target == "published" {
			row.PublishedAt, row.PublishedBy = &now, &actor
			if err := s.createEvent(ctx, tx, "dispatch.policy.published", "dispatch_policy", row.ID, map[string]any{"policy_id": idString(row.ID), "scope_type": row.ScopeType, "scope_id": row.ScopeID, "version": row.Version}); err != nil {
				return err
			}
		}
		if err := s.createAudit(ctx, tx, "admin", actor, "dispatch.policy."+target, "dispatch_policy", row.ID, nil, updates); err != nil {
			return err
		}
		out = policyDTO(row)
		return s.idem.Succeed(ctx, tx, "admin", actor, path, key, out)
	})
	return out, err
}

// validatePolicyRequest 校验策略请求是否合法。
func validatePolicyRequest(req PolicyCreateReq) error {
	if req.OfferCandidateLimit > req.CandidateLimit {
		return problem.New(422, "DISPATCH_POLICY_INVALID", "Unprocessable Entity", "offer_candidate_limit cannot exceed candidate_limit")
	}
	return validatePolicySnapshot(PolicySnapshot{
		Mode: req.Mode, AutoRounds: req.AutoRounds, OfferTTLSeconds: req.OfferTTLSeconds,
		GrabTTLSeconds: req.GrabTTLSeconds, CandidateLimit: req.CandidateLimit,
		OfferCandidateLimit: req.OfferCandidateLimit, HeartbeatFreshSeconds: req.HeartbeatFreshSeconds,
		LocationFreshSeconds: req.LocationFreshSeconds, MaxLocationAccuracyM: req.MaxLocationAccuracyM,
		MaxPickupDistanceM: req.MaxPickupDistanceM, MaxActiveOrdersDefault: req.MaxActiveOrdersDefault,
		IdleFullScoreSeconds: req.IdleFullScoreSeconds, ScoreWeights: req.ScoreWeights,
		RejectionCooldownSeconds: req.RejectionCooldownSeconds,
	})
}

// validatePolicySnapshot 校验策略快照是否合法。
func validatePolicySnapshot(snapshot PolicySnapshot) error {
	sum := snapshot.ScoreWeights.Distance + snapshot.ScoreWeights.Load + snapshot.ScoreWeights.Idle + snapshot.ScoreWeights.Freshness
	if math.Abs(sum-1) > .0001 || snapshot.ScoreWeights.Distance < 0 || snapshot.ScoreWeights.Load < 0 || snapshot.ScoreWeights.Idle < 0 || snapshot.ScoreWeights.Freshness < 0 {
		return problem.New(422, "DISPATCH_POLICY_INVALID", "Unprocessable Entity", "score weights must be non-negative and sum to 1")
	}
	return nil
}

// policyDTO 返回策略DTO。
func policyDTO(row Policy) PolicyDTO {
	return PolicyDTO{
		ID: idString(row.ID), PolicyCode: row.PolicyCode, ScopeType: row.ScopeType, ScopeID: row.ScopeID,
		Version: row.Version, Mode: row.Mode, Status: row.Status, Snapshot: snapshotFromPolicy(row),
		RowVersion: row.RowVersion, PublishedAt: timeValue(row.PublishedAt),
		CreatedAt: row.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: row.UpdatedAt.Format(time.RFC3339Nano),
	}
}

// adminActor 返回管理端 Actor。
func adminActor(claims *auth.Claims, permission string) (uint64, error) {
	if claims == nil || claims.AccountType != "admin" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "admin permission required")
	}
	allowed := false
	for _, item := range claims.Permissions {
		if item == permission {
			allowed = true
			break
		}
	}
	if !allowed {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "admin permission required")
	}
	id, err := parseID(claims.AdminUserID)
	if err != nil {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "invalid admin identity")
	}
	return id, nil
}
