package member

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Service struct {
	cfg  config.Config
	db   *gorm.DB
	ids  *snowflake.Generator
	idem *idempotency.Store
	now  func() time.Time
}

// NewService 创建并初始化服务。
func NewService(cfg config.Config, db *gorm.DB, ids *snowflake.Generator) *Service {
	return &Service{cfg: cfg, db: db, ids: ids, idem: idempotency.NewStore(db), now: time.Now}
}

// Profile 返回资料。
func (s *Service) Profile(ctx context.Context, claims *auth.Claims) (ProfileDTO, error) {
	if !s.cfg.Asset.MemberEnabled || !s.cfg.Asset.ReadEnabled {
		return ProfileDTO{}, problem.New(503, "MEMBER_DISABLED", "Service Unavailable", "member profile is disabled")
	}
	customerID, err := customerActor(claims)
	if err != nil {
		return ProfileDTO{}, err
	}
	return s.profileFor(ctx, customerID)
}

// profileFor 返回指定客户的会员资料。
func (s *Service) profileFor(ctx context.Context, customerID uint64) (ProfileDTO, error) {
	var growth int64
	err := s.db.WithContext(ctx).Table("asset_accounts a").Select("COALESCE(MAX(b.amount),0)").Joins("LEFT JOIN asset_balances b ON b.account_id=a.id AND b.bucket='available'").Where("a.owner_type='customer' AND a.owner_id=? AND a.asset_type='growth_value'", customerID).Scan(&growth).Error
	if err != nil {
		return ProfileDTO{}, err
	}
	ruleSet, rules, err := s.effectiveRules(ctx, s.db)
	if err != nil {
		return ProfileDTO{}, err
	}
	tier, next, remaining := evaluate(growth, rules)
	now := s.now().UTC()
	seed := Profile{CustomerID: customerID, CurrentGrowth: growth, TierCode: tier.TierCode, RuleSetID: ruleSet.ID, TierEffectiveAt: now, Version: 1}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&seed).Error; err != nil {
		return ProfileDTO{}, err
	}
	var profile Profile
	if err := s.db.WithContext(ctx).Where("customer_id=?", customerID).Take(&profile).Error; err != nil {
		return ProfileDTO{}, err
	}
	if profile.CurrentGrowth != growth || profile.TierCode != tier.TierCode || profile.RuleSetID != ruleSet.ID {
		updates := map[string]any{"current_growth": growth, "tier_code": tier.TierCode, "rule_set_id": ruleSet.ID, "version": gorm.Expr("version+1")}
		if profile.TierCode != tier.TierCode {
			updates["tier_effective_at"] = now
		}
		if err := s.db.Model(&Profile{}).Where("customer_id=?", customerID).Updates(updates).Error; err != nil {
			return ProfileDTO{}, err
		}
		profile.CurrentGrowth = growth
		profile.TierCode = tier.TierCode
		profile.RuleSetID = ruleSet.ID
		profile.Version++
	}
	dto := ProfileDTO{CustomerID: idString(customerID), TierCode: tier.TierCode, TierName: tier.TierName, GrowthValue: growth, RuleVersion: ruleSet.Version, EvaluatedAt: now.Format(time.RFC3339Nano), Version: profile.Version}
	if next != nil {
		dto.NextTierCode = next.TierCode
		dto.GrowthToNextTier = remaining
	}
	return dto, nil
}

// ListMembers 查询Members列表。
func (s *Service) ListMembers(ctx context.Context, claims *auth.Claims, q ListQuery) ([]ProfileDTO, string, error) {
	if _, err := adminActor(claims, "member:list"); err != nil {
		return nil, "", err
	}
	db := s.db.WithContext(ctx).Table("member_profiles p").Select("p.*")
	if q.TierCode != "" {
		db = db.Where("p.tier_code=?", q.TierCode)
	}
	var profiles []Profile
	if err := db.Order("p.updated_at DESC,p.customer_id DESC").Offset(q.Offset).Limit(q.PageSize + 1).Scan(&profiles).Error; err != nil {
		return nil, "", err
	}
	next := ""
	if len(profiles) > q.PageSize {
		profiles = profiles[:q.PageSize]
		next = pagination.NextPageToken(q.Query)
	}
	out := make([]ProfileDTO, 0, len(profiles))
	for _, p := range profiles {
		dto, err := s.profileFor(ctx, p.CustomerID)
		if err != nil {
			return nil, "", err
		}
		out = append(out, dto)
	}
	return out, next, nil
}

// AdminMember 返回管理端会员。
func (s *Service) AdminMember(ctx context.Context, claims *auth.Claims, raw string) (ProfileDTO, error) {
	if _, err := adminActor(claims, "member:view"); err != nil {
		return ProfileDTO{}, err
	}
	id, err := parseID(raw)
	if err != nil {
		return ProfileDTO{}, problem.NotFound("MEMBER_NOT_FOUND", "member not found")
	}
	var count int64
	if err = s.db.Table("customers").Where("id=? AND deleted_at IS NULL", id).Count(&count).Error; err != nil {
		return ProfileDTO{}, err
	}
	if count != 1 {
		return ProfileDTO{}, problem.NotFound("MEMBER_NOT_FOUND", "member not found")
	}
	return s.profileFor(ctx, id)
}

// CreateRuleSet 创建规则集。
func (s *Service) CreateRuleSet(ctx context.Context, claims *auth.Claims, method, path, key string, req RuleSetCreateReq) (RuleSetDTO, error) {
	adminID, err := adminActor(claims, "member_rule:create")
	if err != nil {
		return RuleSetDTO{}, err
	}
	effective, err := time.Parse(time.RFC3339, req.EffectiveAt)
	if err != nil {
		return RuleSetDTO{}, problem.InvalidArgument("MEMBER_RULE_INVALID", "effective_at must be RFC3339")
	}
	tiers := append([]TierReq(nil), req.Tiers...)
	sort.Slice(tiers, func(i, j int) bool { return tiers[i].MinGrowth < tiers[j].MinGrowth })
	if len(tiers) != 3 || tiers[0].TierCode != "normal" || tiers[0].MinGrowth != 0 || tiers[1].TierCode != "silver" || tiers[2].TierCode != "gold" || tiers[1].MinGrowth >= tiers[2].MinGrowth {
		return RuleSetDTO{}, problem.New(422, "MEMBER_RULE_INVALID", "Unprocessable Entity", "tiers must be normal silver gold with strictly increasing thresholds")
	}
	var out RuleSetDTO
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), "admin", adminID, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		if !started {
			ok, err := s.idem.CachedResponse(ctx, tx, "admin", adminID, path, key, &out)
			if err != nil {
				return err
			}
			if !ok {
				return problem.Conflict("IDEMPOTENCY_CONFLICT", "idempotent response unavailable")
			}
			return nil
		}
		row := RuleSet{ID: s.ids.Next(), Version: req.Version, Status: "draft", EffectiveAt: effective.UTC(), Reason: req.Reason, CreatedBy: adminID}
		if err := tx.Create(&row).Error; err != nil {
			if isDuplicate(err) {
				return problem.Conflict("MEMBER_RULE_CONFLICT", "member rule version already exists")
			}
			return err
		}
		rules := make([]Rule, 0, 3)
		for i, tier := range tiers {
			benefits, _ := json.Marshal(tier.Benefits)
			rules = append(rules, Rule{ID: s.ids.Next(), RuleSetID: row.ID, TierCode: tier.TierCode, TierName: tier.TierName, MinGrowth: tier.MinGrowth, SortOrder: i + 1, BenefitsSnapshot: benefits})
		}
		if err := tx.Create(&rules).Error; err != nil {
			return err
		}
		if err := tx.Table("audit_logs").Create(map[string]any{"id": s.ids.Next(), "actor_type": "admin", "actor_id": adminID, "action": "member_rule.create", "resource_type": "member_rule", "resource_id": row.ID, "after_data": json.RawMessage(fmt.Sprintf(`{"version":%q,"status":"draft"}`, row.Version)), "result": "success"}).Error; err != nil {
			return err
		}
		out = ruleSetDTO(row, rules)
		return s.idem.Succeed(ctx, tx, "admin", adminID, path, key, out)
	})
	return out, err
}

// ActivateRuleSet 返回Activate Rule Set。
func (s *Service) ActivateRuleSet(ctx context.Context, claims *auth.Claims, method, path, rawID, key string, req ActivateReq) (RuleSetDTO, error) {
	adminID, err := adminActor(claims, "member_rule:activate")
	if err != nil {
		return RuleSetDTO{}, err
	}
	id, err := parseID(rawID)
	if err != nil {
		return RuleSetDTO{}, problem.NotFound("MEMBER_RULE_NOT_FOUND", "member rule not found")
	}
	var out RuleSetDTO
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		request := struct {
			RuleSetID      uint64 `json:"rule_set_id"`
			ExpectedStatus string `json:"expected_status,omitempty"`
		}{RuleSetID: id, ExpectedStatus: req.ExpectedStatus}
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), "admin", adminID, method, path, key, idempotency.RequestHash(request))
		if err != nil {
			return err
		}
		if !started {
			ok, err := s.idem.CachedResponse(ctx, tx, "admin", adminID, path, key, &out)
			if err != nil {
				return err
			}
			if !ok {
				return problem.Conflict("IDEMPOTENCY_CONFLICT", "idempotent response unavailable")
			}
			return nil
		}
		var row RuleSet
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=?", id).Take(&row).Error; err != nil {
			return problem.NotFound("MEMBER_RULE_NOT_FOUND", "member rule not found")
		}
		if row.Status == "active" {
			return problem.Conflict("MEMBER_RULE_CONFLICT", "member rule was already activated")
		}
		if row.Status != "draft" {
			return problem.Conflict("MEMBER_RULE_CONFLICT", "member rule is not draft")
		}
		now := s.now().UTC()
		if !row.EffectiveAt.After(now) {
			if err := tx.Model(&RuleSet{}).Where("status='active' AND effective_at<=? AND id<>?", now, id).Update("status", "retired").Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&RuleSet{}).Where("id=? AND status='draft'", id).Updates(map[string]any{"status": "active", "activated_by": adminID, "activated_at": now}).Error; err != nil {
			return err
		}
		row.Status = "active"
		row.ActivatedBy = &adminID
		row.ActivatedAt = &now
		if err := tx.Table("audit_logs").Create(map[string]any{"id": s.ids.Next(), "actor_type": "admin", "actor_id": adminID, "action": "member_rule.activate", "resource_type": "member_rule", "resource_id": row.ID, "after_data": json.RawMessage(fmt.Sprintf(`{"version":%q,"status":"active"}`, row.Version)), "result": "success"}).Error; err != nil {
			return err
		}
		if err := tx.Table("outbox_events").Create(map[string]any{"id": s.ids.Next(), "event_id": uuid.NewString(), "event_type": "member.rule.activated", "aggregate_type": "member_rule", "aggregate_id": row.ID, "payload": json.RawMessage(fmt.Sprintf(`{"rule_set_id":"%d","version":%q}`, row.ID, row.Version)), "status": "pending"}).Error; err != nil {
			return err
		}
		var rules []Rule
		if err := tx.Where("rule_set_id=?", row.ID).Order("sort_order").Find(&rules).Error; err != nil {
			return err
		}
		out = ruleSetDTO(row, rules)
		return s.idem.Succeed(ctx, tx, "admin", adminID, path, key, out)
	})
	if err != nil {
		return RuleSetDTO{}, err
	}
	return out, nil
}

// ListRuleSets 查询Rule Sets列表。
func (s *Service) ListRuleSets(ctx context.Context, claims *auth.Claims, q pagination.Query) ([]RuleSetDTO, string, error) {
	if _, err := adminActor(claims, "member_rule:list"); err != nil {
		return nil, "", err
	}
	var rows []RuleSet
	if err := s.db.WithContext(ctx).Order("created_at DESC,id DESC").Offset(q.Offset).Limit(q.PageSize + 1).Find(&rows).Error; err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > q.PageSize {
		rows = rows[:q.PageSize]
		next = pagination.NextPageToken(q)
	}
	out := make([]RuleSetDTO, 0, len(rows))
	for _, row := range rows {
		var rules []Rule
		if err := s.db.Where("rule_set_id=?", row.ID).Order("sort_order").Find(&rules).Error; err != nil {
			return nil, "", err
		}
		out = append(out, ruleSetDTO(row, rules))
	}
	return out, next, nil
}

// effectiveRules 返回当前生效的规则。
func (s *Service) effectiveRules(ctx context.Context, db *gorm.DB) (RuleSet, []Rule, error) {
	var set RuleSet
	err := db.WithContext(ctx).Where("status='active' AND effective_at<=?", s.now().UTC()).Order("effective_at DESC,id DESC").Take(&set).Error
	if err != nil {
		return RuleSet{}, nil, err
	}
	var rules []Rule
	err = db.WithContext(ctx).Where("rule_set_id=?", set.ID).Order("min_growth").Find(&rules).Error
	return set, rules, err
}

// evaluate 计算会员等级。
func evaluate(growth int64, rules []Rule) (Rule, *Rule, int64) {
	current := rules[0]
	var next *Rule
	for i := range rules {
		if growth >= rules[i].MinGrowth {
			current = rules[i]
			continue
		}
		copy := rules[i]
		next = &copy
		break
	}
	if next == nil {
		return current, nil, 0
	}
	return current, next, next.MinGrowth - growth
}

// ruleSetDTO 返回规则集 DTO。
func ruleSetDTO(row RuleSet, rules []Rule) RuleSetDTO {
	dto := RuleSetDTO{ID: idString(row.ID), Version: row.Version, Status: row.Status, EffectiveAt: row.EffectiveAt.UTC().Format(time.RFC3339Nano), Reason: row.Reason, CreatedBy: idString(row.CreatedBy), CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: row.UpdatedAt.UTC().Format(time.RFC3339Nano)}
	if row.ActivatedBy != nil {
		dto.ActivatedBy = idString(*row.ActivatedBy)
	}
	for _, rule := range rules {
		benefits := map[string]any{}
		_ = json.Unmarshal(rule.BenefitsSnapshot, &benefits)
		dto.Tiers = append(dto.Tiers, TierDTO{TierCode: rule.TierCode, TierName: rule.TierName, MinGrowth: rule.MinGrowth, Benefits: benefits})
	}
	return dto
}

// customerActor 返回客户审计主体。
func customerActor(c *auth.Claims) (uint64, error) {
	if c == nil || c.AccountType != "customer" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "customer account required")
	}
	id, err := parseID(c.CustomerID)
	if err != nil {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "invalid customer identity")
	}
	return id, nil
}

// adminActor 返回管理端审计主体。
func adminActor(c *auth.Claims, permission string) (uint64, error) {
	if c == nil || c.AccountType != "admin" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "admin account required")
	}
	for _, p := range c.Permissions {
		if p == permission {
			return parseID(c.AdminUserID)
		}
	}
	return 0, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
}

// parseID 解析并校验字符串形式的 ID。
func parseID(raw string) (uint64, error) {
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

// idString 将数字 ID 转换为字符串。
func idString(id uint64) string { return strconv.FormatUint(id, 10) }

// isDuplicate 判断重复项是否成立。
func isDuplicate(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate")
}
