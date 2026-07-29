package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/deliveryverification"
	"jiuxiaoer-admin/backend-go/internal/modules/dispatch"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type Service struct {
	cfg       config.Config
	db        *gorm.DB
	ids       *snowflake.Generator
	idem      *idempotency.Store
	orders    *order.Service
	dispatch  *dispatch.Service
	incidents IncidentResolver
	returns   ReturnGuard
}

type IncidentResolver interface {
	ResolveActiveLocked(ctx context.Context, tx *gorm.DB, deliveryID uint64, stage, resolutionCode string) error
}

type ReturnGuard interface {
	HasActiveLocked(ctx context.Context, tx *gorm.DB, deliveryID uint64) (bool, error)
}

// NewService 创建并初始化服务。
func NewService(cfg config.Config, db *gorm.DB, ids *snowflake.Generator, orders *order.Service) *Service {
	return &Service{cfg: cfg, db: db, ids: ids, idem: idempotency.NewStore(db), orders: orders}
}

// WithDispatch 设置调度并返回更新后的值。
func (s *Service) WithDispatch(service *dispatch.Service) *Service { s.dispatch = service; return s }

func (s *Service) WithIncidentResolver(resolver IncidentResolver) *Service {
	s.incidents = resolver
	return s
}

func (s *Service) WithReturnGuard(guard ReturnGuard) *Service {
	s.returns = guard
	return s
}

// Cancel 取消订单DTO。
func (s *Service) Cancel(ctx context.Context, c *auth.Claims, method, path, key, idRaw string, req CancelReq) (order.OrderDTO, error) {
	actor, e := adminID(c, "order:cancel_all")
	if e != nil {
		return order.OrderDTO{}, e
	}
	_ = actor
	expectedVersion := req.ExpectedVersion
	return s.orders.CancelAdmin(ctx, c, method, path, key, idRaw, order.OrderCancelReq{Reason: req.Reason, ReasonCode: req.ReasonCode, ExpectedVersion: &expectedVersion})
}

// Assign 分配配送DTO。
func (s *Service) Assign(ctx context.Context, c *auth.Claims, method, path, key, idRaw string, req AssignmentReq) (DeliveryDTO, error) {
	return s.assign(ctx, c, method, path, key, idRaw, req, "manual", "delivery:assign")
}

// Reassign 重新分配配送DTO。
func (s *Service) Reassign(ctx context.Context, c *auth.Claims, method, path, key, idRaw string, req AssignmentReq) (DeliveryDTO, error) {
	return s.assign(ctx, c, method, path, key, idRaw, req, "reassign", "delivery:reassign")
}

// assign 分配配送DTO。
func (s *Service) assign(ctx context.Context, c *auth.Claims, method, path, key, idRaw string, req AssignmentReq, kind, perm string) (DeliveryDTO, error) {
	actor, e := adminID(c, perm)
	if e != nil {
		return DeliveryDTO{}, e
	}
	id, e := parseID(idRaw)
	if e != nil {
		return DeliveryDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid delivery id")
	}
	rider, e := parseID(req.RiderID)
	if e != nil {
		return DeliveryDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid rider id")
	}
	if s.dispatch != nil {
		result, err := s.dispatch.CommitAssignment(ctx, method, path, key, dispatch.CommitInput{
			Source: kind, DeliveryOrderID: id, RiderID: rider, ExpectedAssignmentVersion: req.ExpectedVersion,
			ActorType: "admin", ActorID: actor, ReasonCode: req.ReasonCode, Reason: req.Reason,
		})
		if err != nil {
			return DeliveryDTO{}, err
		}
		return DeliveryDTO{ID: result.DeliveryOrderID, OrderID: result.OrderID, RiderID: result.RiderID, Status: result.Status, AssignmentVersion: result.AssignmentVersion}, nil
	}
	var out DeliveryDTO
	e = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, e := s.idem.Start(ctx, tx, s.ids.Next(), "admin", actor, method, path, key, idempotency.ResourceRequestHash("delivery."+kind, id, req))
		if e != nil {
			return e
		}
		if !started {
			return cached(ctx, s.idem, tx, "admin", actor, path, key, &out)
		}
		var row Delivery
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, id).Error; errors.Is(e, gorm.ErrRecordNotFound) {
			return problem.NotFound("DELIVERY_NOT_FOUND", "delivery not found")
		} else if e != nil {
			return e
		}
		if row.AssignmentVersion != req.ExpectedVersion {
			return problem.Conflict("VERSION_CONFLICT", "delivery assignment version changed")
		}
		if kind == "manual" {
			if row.Status != "pending_assign" || row.RiderID != nil {
				return problem.Conflict("DELIVERY_INVALID_STATUS", "delivery is not assignable")
			}
		} else {
			if row.Status != "accepted" && row.Status != "pending_assign" {
				return problem.Conflict("DELIVERY_INVALID_STATUS", "delivery cannot be reassigned")
			}
		}
		var active int64
		if e := tx.Table("riders r").Joins("JOIN accounts a ON a.id=r.account_id").Where("r.id=? AND r.status='active' AND r.review_status='approved' AND a.status='active' AND (JSON_CONTAINS(r.service_scope, JSON_QUOTE(?), '$.shop_ids') OR JSON_CONTAINS(r.service_scope, JSON_ARRAY(?), '$.shop_ids'))", rider, idString(row.ShopID), row.ShopID).Count(&active).Error; e != nil {
			return e
		}
		if active != 1 {
			return problem.Conflict("RIDER_UNAVAILABLE", "rider is not active, approved, and in shop scope")
		}
		before := row.AssignmentVersion
		after := before + 1
		if row.RiderID != nil {
			if e := tx.Model(&Assignment{}).Where("delivery_order_id=? AND status='active'", id).Update("status", "superseded").Error; e != nil {
				return e
			}
		}
		assignment := Assignment{ID: s.ids.Next(), DeliveryOrderID: id, FromRiderID: row.RiderID, ToRiderID: rider, AssignmentType: kind, Status: "active", ReasonCode: &req.ReasonCode, Reason: &req.Reason, ActorType: "admin", ActorID: actor, VersionBefore: before, VersionAfter: after, RequestID: requestctx.RequestIDPtr(ctx)}
		if e := tx.Create(&assignment).Error; e != nil {
			return e
		}
		status := "accepted"
		if e := tx.Model(&Delivery{}).Where("id=? AND assignment_version=?", id, before).Updates(map[string]any{"rider_id": rider, "status": status, "assignment_version": after, "accepted_at": time.Now()}).Error; e != nil {
			return e
		}
		if e := tx.Table("orders").Where("id=?", row.OrderID).Update("delivery_status", "accepted").Error; e != nil {
			return e
		}
		if kind == "reassign" {
			if e := deliveryverification.InvalidateAndRegenerate(ctx, tx, s.cfg.CP1, s.ids, id); e != nil {
				return e
			}
		}
		eventType := "delivery.assigned"
		if kind == "reassign" {
			eventType = "delivery.reassigned"
		}
		if e := event(ctx, tx, s.ids.Next(), eventType, "delivery_order", id, map[string]any{"delivery_order_id": idString(id), "order_id": idString(row.OrderID), "rider_id": idString(rider), "from_rider_id": uintString(row.RiderID)}); e != nil {
			return e
		}
		if e := audit(ctx, tx, s.ids.Next(), actor, "delivery."+kind, id, map[string]any{"from_rider_id": uintString(row.RiderID), "to_rider_id": idString(rider), "reason_code": req.ReasonCode}); e != nil {
			return e
		}
		row.RiderID = &rider
		row.Status = status
		row.AssignmentVersion = after
		out = deliveryDTO(row)
		return s.idem.Succeed(ctx, tx, "admin", actor, path, key, out)
	})
	return out, e
}

// ForceComplete 强制执行Complete。
func (s *Service) ForceComplete(ctx context.Context, c *auth.Claims, method, path, key, idRaw string, req ForceCompleteReq) (DeliveryDTO, error) {
	actor, e := adminID(c, "delivery:force_complete")
	if e != nil {
		return DeliveryDTO{}, e
	}
	if !s.cfg.CP1.ForceActionEnabled {
		return DeliveryDTO{}, problem.Forbidden("FORCE_ACTION_DISABLED", "force actions are disabled")
	}
	id, e := parseID(idRaw)
	if e != nil {
		return DeliveryDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid delivery id")
	}
	var out DeliveryDTO
	e = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if ok, authErr := activeAdminHasPermission(ctx, tx, actor, "delivery:force_complete"); authErr != nil {
			return authErr
		} else if !ok {
			return problem.Forbidden("PERM_FORBIDDEN", "administrator is no longer active or authorized")
		}
		started, e := s.idem.Start(ctx, tx, s.ids.Next(), "admin", actor, method, path, key, idempotency.ResourceRequestHash("delivery.force_complete", id, req))
		if e != nil {
			return e
		}
		if !started {
			return cached(ctx, s.idem, tx, "admin", actor, path, key, &out)
		}
		var row Delivery
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, id).Error; e != nil {
			if errors.Is(e, gorm.ErrRecordNotFound) {
				return problem.NotFound("DELIVERY_NOT_FOUND", "delivery not found")
			}
			return e
		}
		if row.AssignmentVersion != req.ExpectedVersion {
			return problem.Conflict("VERSION_CONFLICT", "delivery version changed")
		}
		if row.Status != "delivering" {
			return problem.Conflict("DELIVERY_INVALID_STATUS", "only delivering orders can be force completed")
		}
		var orderRow Order
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&orderRow, row.OrderID).Error; e != nil {
			return e
		}
		now := time.Now()
		if e := tx.Model(&Delivery{}).Where("id=?", id).Updates(map[string]any{"status": "completed", "completed_at": now}).Error; e != nil {
			return e
		}
		if e := tx.Model(&Order{}).Where("id=?", row.OrderID).Updates(map[string]any{"status": "completed", "delivery_status": "completed", "completed_at": now, "version": gorm.Expr("version+1")}).Error; e != nil {
			return e
		}
		if s.incidents != nil {
			if e := s.incidents.ResolveActiveLocked(ctx, tx, id, "delivery", "force_completed"); e != nil {
				return e
			}
		}
		if s.returns != nil {
			active, guardErr := s.returns.HasActiveLocked(ctx, tx, id)
			if guardErr != nil {
				return guardErr
			}
			if active {
				return problem.Conflict("INVALID_RETURN_STATE", "delivery order has an active return and cannot be force completed")
			}
		}
		if e := tx.Model(&deliveryverification.Verification{}).
			Where(
				"delivery_order_id=? AND stage='delivery' AND status IN ?",
				id,
				[]string{"active", "locked"},
			).
			Updates(map[string]any{
				"status":               "overridden",
				"verified_at":          now,
				"verified_by_type":     "admin",
				"verified_by_id":       actor,
				"override_reason_code": req.ReasonCode,
				"override_reason":      req.Reason,
				"locked_until":         nil,
				"version":              gorm.Expr("version+1"),
			}).Error; e != nil {
			return e
		}
		if e := deliveryverification.Invalidate(ctx, tx, s.ids, id, "delivery_force_completed"); e != nil {
			return e
		}
		if e := event(ctx, tx, s.ids.Next(), "delivery.force_completed", "delivery_order", id, map[string]any{"delivery_order_id": idString(id), "order_id": idString(row.OrderID), "reason_code": req.ReasonCode}); e != nil {
			return e
		}
		if e := event(ctx, tx, s.ids.Next(), "order.completed", "order", row.OrderID, map[string]any{"order_id": idString(row.OrderID), "admin_override": true}); e != nil {
			return e
		}
		requestID := requestctx.RequestID(ctx)
		if e := auditTransition(
			ctx,
			tx,
			s.ids.Next(),
			actor,
			"delivery.force_complete",
			id,
			map[string]any{
				"status":        row.Status,
				"version":       row.AssignmentVersion,
				"order_status":  orderRow.Status,
				"order_version": orderRow.Version,
			},
			map[string]any{
				"actor_admin_id":       idString(actor),
				"permission":           "delivery:force_complete",
				"reason_code":          req.ReasonCode,
				"status":               "completed",
				"version":              row.AssignmentVersion,
				"order_status":         "completed",
				"order_version":        orderRow.Version + 1,
				"request_id":           requestID,
				"correlation_id":       requestID,
				"idempotency_key_hash": idempotency.KeyHash(key),
				"service_instance":     auditServiceInstance(s.cfg.App.InstanceID),
			},
		); e != nil {
			return e
		}
		row.Status = "completed"
		row.CompletedAt = &now
		out = deliveryDTO(row)
		return s.idem.Succeed(ctx, tx, "admin", actor, path, key, out)
	})
	return out, e
}

// Assignments 返回Assignments。
func (s *Service) Assignments(ctx context.Context, c *auth.Claims, idRaw string) ([]AssignmentDTO, error) {
	if _, e := adminID(c, "delivery_assignment:view"); e != nil {
		return nil, e
	}
	id, e := parseID(idRaw)
	if e != nil {
		return nil, problem.InvalidArgument("VALIDATION_FAILED", "invalid delivery id")
	}
	var rows []Assignment
	if e := s.db.WithContext(ctx).Where("delivery_order_id=?", id).Order("id DESC").Find(&rows).Error; e != nil {
		return nil, e
	}
	out := make([]AssignmentDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, assignmentDTO(r))
	}
	return out, nil
}

// activeAdminHasPermission 是高风险管理写入的事务权威检查。
// FOR UPDATE 会保持账户、管理员、角色、映射和权限状态稳定，
// 直到受保护的业务事务提交。
func activeAdminHasPermission(ctx context.Context, tx *gorm.DB, id uint64, permissionCode string) (bool, error) {
	var row struct {
		ID uint64
	}
	err := tx.WithContext(ctx).
		Table("admin_users au").
		Select("au.id").
		Joins("JOIN accounts a ON a.id = au.account_id").
		Joins("JOIN roles r ON r.id = au.role_id").
		Joins("JOIN role_permissions rp ON rp.role_id = r.id").
		Joins("JOIN permissions p ON p.id = rp.permission_id").
		Where(`au.id = ?
			AND au.status = 'active' AND au.deleted_at IS NULL
			AND a.account_type = 'admin' AND a.status = 'active' AND a.deleted_at IS NULL
			AND r.status = 'active' AND r.deleted_at IS NULL
			AND rp.deleted_at IS NULL
			AND p.code = ? AND p.status = 'active' AND p.deleted_at IS NULL`, id, permissionCode).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return err == nil, err
}

// adminID 从认证声明中解析并返回管理员 ID。
func adminID(c *auth.Claims, p string) (uint64, error) {
	if c == nil || c.AccountType != "admin" || !has(c.Permissions, p) {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
	}
	return parseID(c.AdminUserID)
}

// has 判断是否存在运营。
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

// uintString 将无符号整数转换为字符串。
func uintString(v *uint64) string {
	if v == nil {
		return ""
	}
	return idString(*v)
}

// str 返回字符串指针。
func str(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// ts 返回格式化时间字符串。
func ts(v *time.Time) string {
	if v == nil {
		return ""
	}
	return v.Format(time.RFC3339)
}

// deliveryDTO 返回配送DTO。
func deliveryDTO(r Delivery) DeliveryDTO {
	return DeliveryDTO{ID: idString(r.ID), OrderID: idString(r.OrderID), RiderID: uintString(r.RiderID), Status: r.Status, AssignmentVersion: r.AssignmentVersion, CompletedAt: ts(r.CompletedAt)}
}

// assignmentDTO 返回分配DTO。
func assignmentDTO(r Assignment) AssignmentDTO {
	return AssignmentDTO{ID: idString(r.ID), DeliveryOrderID: idString(r.DeliveryOrderID), FromRiderID: uintString(r.FromRiderID), ToRiderID: idString(r.ToRiderID), AssignmentType: r.AssignmentType, Status: r.Status, ReasonCode: str(r.ReasonCode), Reason: str(r.Reason), ActorID: idString(r.ActorID), VersionBefore: r.VersionBefore, VersionAfter: r.VersionAfter, CreatedAt: r.CreatedAt.Format(time.RFC3339)}
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

// event 返回事件。
func event(ctx context.Context, tx *gorm.DB, id uint64, eventType, aggregate string, aggregateID uint64, payload any) error {
	raw, _ := json.Marshal(payload)
	return tx.Create(&Outbox{ID: id, EventID: uuid.NewString(), EventType: eventType, AggregateType: aggregate, AggregateID: aggregateID, Payload: raw, Status: "pending", RequestID: requestctx.RequestIDPtr(ctx)}).Error
}

// audit 返回审计。
func audit(ctx context.Context, tx *gorm.DB, id, actor uint64, action string, resource uint64, after any) error {
	raw, _ := json.Marshal(after)
	return tx.Table("audit_logs").Create(map[string]any{"id": id, "actor_type": "admin", "actor_id": actor, "action": action, "resource_type": "delivery_order", "resource_id": resource, "after_data": datatypes.JSON(raw), "result": "success", "request_id": requestctx.RequestIDPtr(ctx)}).Error
}

// auditTransition 写入高风险状态迁移的完整前后事实。失败事务不会伪造
// 审计成功记录；调用方仍通过请求日志与 HTTP 指标观测回滚错误。
func auditTransition(
	ctx context.Context,
	tx *gorm.DB,
	id, actor uint64,
	action string,
	resource uint64,
	before, after map[string]any,
) error {
	beforeRaw, _ := json.Marshal(before)
	afterRaw, _ := json.Marshal(after)
	beforeStatus, _ := before["status"].(string)
	afterStatus, _ := after["status"].(string)
	version := uint64(0)
	switch value := after["version"].(type) {
	case uint:
		version = uint64(value)
	case uint64:
		version = value
	}
	values := map[string]any{
		"id":            id,
		"event_id":      uuid.NewString(),
		"actor_type":    "admin",
		"actor_id":      actor,
		"action":        action,
		"resource_type": "delivery_order",
		"resource_id":   resource,
		"before_data":   datatypes.JSON(beforeRaw),
		"after_data":    datatypes.JSON(afterRaw),
		"result":        "success",
		"before_status": beforeStatus,
		"after_status":  afterStatus,
		"version":       version,
		"request_id":    requestctx.RequestIDPtr(ctx),
		"ip_hash":       requestctx.IPHashPtr(ctx),
		"user_agent":    requestctx.UserAgentPtr(ctx),
	}
	if accountID := requestctx.AccountID(ctx); accountID != 0 {
		values["account_id"] = accountID
	}
	return tx.Table("audit_logs").Create(values).Error
}

func auditServiceInstance(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "unknown"
}
