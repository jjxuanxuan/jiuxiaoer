package printjob

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
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/securevalue"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type Service struct {
	cfg   config.CP1Config
	db    *gorm.DB
	ids   *snowflake.Generator
	idem  *idempotency.Store
	crypt string
}

// NewService 创建并初始化服务。
func NewService(cfg config.CP1Config, db *gorm.DB, ids *snowflake.Generator) *Service {
	return &Service{cfg: cfg, db: db, ids: ids, idem: idempotency.NewStore(db), crypt: cfg.DataEncryptionKey}
}

// GetSettings 获取Settings。
func (s *Service) GetSettings(ctx context.Context, claims *auth.Claims, shopIDRaw string) (SettingDTO, error) {
	if _, err := merchantActor(claims, "print_setting:view_shop"); err != nil {
		return SettingDTO{}, err
	}
	shopID, err := parseID(shopIDRaw)
	if err != nil || !merchantShopAllowed(claims, shopID) {
		return SettingDTO{}, problem.NotFound("PRINT_SETTING_NOT_FOUND", "print setting not found")
	}
	var row Setting
	if err := s.db.WithContext(ctx).Where("shop_id = ?", shopID).First(&row).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return SettingDTO{}, problem.NotFound("PRINT_SETTING_NOT_FOUND", "print setting not found")
	} else if err != nil {
		return SettingDTO{}, err
	}
	return settingDTO(row), nil
}

// PatchSettings 返回Patch Settings。
func (s *Service) PatchSettings(ctx context.Context, claims *auth.Claims, method, path, key, idRaw string, req SettingPatchReq) (SettingDTO, error) {
	actorID, err := merchantActor(claims, "print_setting:update_shop")
	if err != nil {
		return SettingDTO{}, err
	}
	id, err := parseID(idRaw)
	if err != nil {
		return SettingDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid print setting id")
	}
	templateID, err := parseID(req.TemplateID)
	if err != nil || !validPrintEvents(req.AutoPrintEvents) || req.Provider != s.cfg.PrintProvider {
		return SettingDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid template or auto_print_events")
	}
	var result SettingDTO
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), "merchant", actorID, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		if !started {
			return cached(ctx, s.idem, tx, "merchant", actorID, path, key, &result)
		}
		var row Setting
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).First(&row).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("PRINT_SETTING_NOT_FOUND", "print setting not found")
		} else if err != nil {
			return err
		}
		if !merchantShopAllowed(claims, row.ShopID) {
			return problem.Forbidden("PERM_FORBIDDEN", "shop is not authorized")
		}
		if row.Version != req.Version {
			return problem.Conflict("VERSION_CONFLICT", "print setting version changed")
		}
		ciphertext, err := securevalue.Seal(s.crypt, req.DeviceID)
		if err != nil {
			return err
		}
		events, _ := json.Marshal(req.AutoPrintEvents)
		updates := map[string]any{"enabled": req.Enabled, "provider": req.Provider, "device_id_ciphertext": ciphertext, "device_id_mask": mask(req.DeviceID), "template_id": templateID, "copies": req.Copies, "auto_print_events": datatypes.JSON(events), "updated_by": actorID, "version": gorm.Expr("version + 1")}
		if err := tx.Model(&Setting{}).Where("id = ? AND version = ?", id, req.Version).Updates(updates).Error; err != nil {
			return err
		}
		row.Enabled, row.Provider, row.DeviceIDMask, row.TemplateID, row.Copies, row.AutoPrintEvents, row.UpdatedBy, row.Version = req.Enabled, req.Provider, mask(req.DeviceID), templateID, req.Copies, events, actorID, row.Version+1
		if err := audit(ctx, tx, s.ids.Next(), "merchant", actorID, "print_setting.update", "print_setting", id, map[string]any{"version": req.Version}, settingDTO(row)); err != nil {
			return err
		}
		result = settingDTO(row)
		return s.idem.Succeed(ctx, tx, "merchant", actorID, path, key, result)
	})
	return result, err
}

// ListStoreTasks 查询门店 Tasks列表。
func (s *Service) ListStoreTasks(ctx context.Context, claims *auth.Claims, query pagination.Query, status string) ([]TaskDTO, string, error) {
	if _, err := merchantActor(claims, "print_task:list_shop"); err != nil {
		return nil, "", err
	}
	shopIDs, err := claimShopIDs(claims)
	if err != nil {
		return nil, "", err
	}
	return s.listTasks(ctx, query, status, shopIDs)
}

// ListAdminTasks 查询管理端 Tasks列表。
func (s *Service) ListAdminTasks(ctx context.Context, claims *auth.Claims, query pagination.Query, status string) ([]TaskDTO, string, error) {
	if _, err := adminActor(claims, "print_task:list_all"); err != nil {
		return nil, "", err
	}
	return s.listTasks(ctx, query, status, nil)
}

// listTasks 查询Tasks列表。
func (s *Service) listTasks(ctx context.Context, query pagination.Query, status string, shopIDs []uint64) ([]TaskDTO, string, error) {
	db := s.db.WithContext(ctx).Model(&Task{})
	if len(shopIDs) > 0 {
		db = db.Where("shop_id IN ?", shopIDs)
	}
	if status != "" {
		db = db.Where("status = ?", status)
	}
	var rows []Task
	if err := db.Order("id DESC").Offset(query.Offset).Limit(query.PageSize + 1).Find(&rows).Error; err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		rows = rows[:query.PageSize]
		next = pagination.NextPageToken(query)
	}
	result := make([]TaskDTO, 0, len(rows))
	for _, row := range rows {
		result = append(result, taskDTO(row))
	}
	return result, next, nil
}

// GetTask 获取任务。
func (s *Service) GetTask(ctx context.Context, claims *auth.Claims, idRaw string) (TaskDTO, error) {
	id, err := parseID(idRaw)
	if err != nil {
		return TaskDTO{}, problem.NotFound("PRINT_TASK_NOT_FOUND", "print task not found")
	}
	var row Task
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		return TaskDTO{}, problem.NotFound("PRINT_TASK_NOT_FOUND", "print task not found")
	}
	if !merchantShopAllowed(claims, row.ShopID) {
		return TaskDTO{}, problem.NotFound("PRINT_TASK_NOT_FOUND", "print task not found")
	}
	return taskDTO(row), nil
}

// Reprint 返回Reprint。
func (s *Service) Reprint(ctx context.Context, claims *auth.Claims, method, path, key, idRaw string, req ReprintReq) (TaskDTO, error) {
	actorID, err := merchantActor(claims, "print_task:reprint_shop")
	if err != nil {
		return TaskDTO{}, err
	}
	return s.cloneTask(ctx, claims, actorID, "merchant", method, path, key, idRaw, req.Reason)
}

// Retry 重试任务DTO。
func (s *Service) Retry(ctx context.Context, claims *auth.Claims, method, path, key, idRaw string, req RetryReq) (TaskDTO, error) {
	actorID, err := adminActor(claims, "print_task:retry_all")
	if err != nil {
		return TaskDTO{}, err
	}
	id, err := parseID(idRaw)
	if err != nil {
		return TaskDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid print task id")
	}
	var result TaskDTO
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), "admin", actorID, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		if !started {
			return cached(ctx, s.idem, tx, "admin", actorID, path, key, &result)
		}
		var row Task
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, id).Error; err != nil {
			return problem.NotFound("PRINT_TASK_NOT_FOUND", "print task not found")
		}
		if row.Status != "dead" && row.Status != "retry_wait" {
			return problem.Conflict("PRINT_TASK_INVALID_STATUS", "task is not retryable")
		}
		if err := tx.Model(&Task{}).Where("id = ?", id).Updates(map[string]any{"status": "pending", "next_retry_at": nil, "locked_by": nil, "locked_until": nil, "last_error_code": nil, "last_error_safe": nil}).Error; err != nil {
			return err
		}
		if err := enqueueWakeup(ctx, tx, s.ids, "print.task.retry_requested", row); err != nil {
			return err
		}
		row.Status = "pending"
		row.NextRetryAt = nil
		row.LastErrorCode = nil
		if err := audit(ctx, tx, s.ids.Next(), "admin", actorID, "print_task.retry", "print_task", id, nil, map[string]any{"reason": req.Reason}); err != nil {
			return err
		}
		result = taskDTO(row)
		return s.idem.Succeed(ctx, tx, "admin", actorID, path, key, result)
	})
	return result, err
}

// cloneTask 克隆任务。
func (s *Service) cloneTask(ctx context.Context, claims *auth.Claims, actorID uint64, actorType, method, path, key, idRaw, reason string) (TaskDTO, error) {
	id, err := parseID(idRaw)
	if err != nil {
		return TaskDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid print task id")
	}
	var result TaskDTO
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), actorType, actorID, method, path, key, idempotency.RequestHash(map[string]string{"task": idRaw, "reason": reason}))
		if err != nil {
			return err
		}
		if !started {
			return cached(ctx, s.idem, tx, actorType, actorID, path, key, &result)
		}
		var source Task
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&source, id).Error; err != nil {
			return problem.NotFound("PRINT_TASK_NOT_FOUND", "print task not found")
		}
		if actorType == "merchant" && !merchantShopAllowed(claims, source.ShopID) {
			return problem.Forbidden("PERM_FORBIDDEN", "shop is not authorized")
		}
		var maxSeq uint
		if err := tx.Model(&Task{}).Where("shop_id=? AND order_id=? AND event_type=? AND template_version=?", source.ShopID, source.OrderID, source.EventType, source.TemplateVersion).Select("COALESCE(MAX(reprint_seq),0)").Scan(&maxSeq).Error; err != nil {
			return err
		}
		row := source
		row.ID = s.ids.Next()
		row.TaskNo = fmt.Sprintf("PT%d", row.ID)
		row.EventID = uuid.NewString()
		row.ReprintSeq = maxSeq + 1
		row.ProviderRequestID = nil
		row.Status = "pending"
		row.Attempts = 0
		row.NextRetryAt = nil
		row.LockedBy = nil
		row.LockedUntil = nil
		row.LastErrorCode = nil
		row.LastErrorSafe = nil
		row.SucceededAt = nil
		row.CreatedAt = time.Time{}
		row.UpdatedAt = time.Time{}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		if err := enqueueWakeup(ctx, tx, s.ids, "print.task.ready", row); err != nil {
			return err
		}
		if err := audit(ctx, tx, s.ids.Next(), actorType, actorID, "print_task.reprint", "print_task", row.ID, nil, map[string]any{"source_task_id": idRaw, "reason": reason}); err != nil {
			return err
		}
		result = taskDTO(row)
		return s.idem.Succeed(ctx, tx, actorType, actorID, path, key, result)
	})
	return result, err
}

// EnqueueAuto 返回Enqueue Auto。
// EnqueueAuto is called inside the order transaction. It creates a unique
// task only when an enabled shop setting subscribes to the event.
func EnqueueAuto(ctx context.Context, tx *gorm.DB, ids *snowflake.Generator, shopID, orderID uint64, eventID, eventType string, payload any) error {
	var setting Setting
	if err := tx.WithContext(ctx).Where("shop_id = ? AND enabled = 1", shopID).First(&setting).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	var events []string
	_ = json.Unmarshal(setting.AutoPrintEvents, &events)
	if !contains(events, eventType) {
		return nil
	}
	raw, _ := json.Marshal(payload)
	id := ids.Next()
	row := Task{ID: id, TaskNo: fmt.Sprintf("PT%d", id), EventID: eventID, OrderID: orderID, ShopID: shopID, EventType: eventType, TemplateID: setting.TemplateID, TemplateVersion: "v1", RenderPayload: raw, Provider: setting.Provider, Status: "pending"}
	result := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if result.Error != nil || result.RowsAffected == 0 {
		return result.Error
	}
	return enqueueWakeup(ctx, tx, ids, "print.task.ready", row)
}

// enqueueWakeup 返回enqueue Wakeup。
func enqueueWakeup(ctx context.Context, tx *gorm.DB, ids *snowflake.Generator, eventType string, task Task) error {
	payload, err := json.Marshal(map[string]any{"print_task_id": idString(task.ID), "shop_id": idString(task.ShopID), "task_event_type": task.EventType})
	if err != nil {
		return err
	}
	return tx.WithContext(ctx).Table("outbox_events").Create(map[string]any{
		"id": ids.Next(), "event_id": uuid.NewString(), "event_type": eventType,
		"aggregate_type": "print_task", "aggregate_id": task.ID, "payload": datatypes.JSON(payload),
		"status": "pending", "request_id": requestctx.RequestIDPtr(ctx),
	}).Error
}

// settingDTO 返回setting DTO。
func settingDTO(row Setting) SettingDTO {
	var events []string
	_ = json.Unmarshal(row.AutoPrintEvents, &events)
	return SettingDTO{ID: idString(row.ID), ShopID: idString(row.ShopID), Provider: row.Provider, DeviceIDMask: row.DeviceIDMask, TemplateID: idString(row.TemplateID), Copies: row.Copies, AutoPrintEvents: events, Enabled: row.Enabled, Version: row.Version}
}

// taskDTO 返回任务DTO。
func taskDTO(row Task) TaskDTO {
	return TaskDTO{ID: idString(row.ID), TaskNo: row.TaskNo, OrderID: idString(row.OrderID), ShopID: idString(row.ShopID), EventType: row.EventType, TemplateID: idString(row.TemplateID), TemplateVersion: row.TemplateVersion, ReprintSeq: row.ReprintSeq, Provider: row.Provider, ProviderRequestID: str(row.ProviderRequestID), Status: row.Status, Attempts: row.Attempts, NextRetryAt: ts(row.NextRetryAt), LastErrorCode: str(row.LastErrorCode), SucceededAt: ts(row.SucceededAt), CreatedAt: row.CreatedAt.Format(time.RFC3339)}
}

// validPrintEvents 判断有效打印 Events。
func validPrintEvents(values []string) bool {
	if len(values) == 0 {
		return false
	}
	for _, v := range values {
		if v != "order_accepted" && v != "order_prepared" {
			return false
		}
	}
	return true
}

// merchantActor 返回商户 Actor。
func merchantActor(c *auth.Claims, perm string) (uint64, error) {
	if c == nil || c.AccountType != "merchant" || !has(c.Permissions, perm) {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "merchant permission required")
	}
	return parseID(c.MerchantUserID)
}

// adminActor 返回管理端 Actor。
func adminActor(c *auth.Claims, perm string) (uint64, error) {
	if c == nil || c.AccountType != "admin" || !has(c.Permissions, perm) {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "admin permission required")
	}
	return parseID(c.AdminUserID)
}

// merchantShopAllowed 判断商户门店允许状态。
func merchantShopAllowed(c *auth.Claims, id uint64) bool {
	if c == nil || c.AccountType != "merchant" {
		return false
	}
	for _, v := range c.AuthorizedShopIDs {
		if v == idString(id) {
			return true
		}
	}
	return false
}

// claimShopIDs 认领门店 I Ds。
func claimShopIDs(c *auth.Claims) ([]uint64, error) {
	result := make([]uint64, 0, len(c.AuthorizedShopIDs))
	for _, v := range c.AuthorizedShopIDs {
		id, e := parseID(v)
		if e != nil {
			return nil, e
		}
		result = append(result, id)
	}
	return result, nil
}

// has 判断是否存在printjob。
func has(v []string, w string) bool {
	for _, x := range v {
		if x == w {
			return true
		}
	}
	return false
}

// contains 判断contains。
func contains(v []string, w string) bool { return has(v, w) }

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

// str 返回str。
func str(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// ts 返回ts。
func ts(v *time.Time) string {
	if v == nil {
		return ""
	}
	return v.Format(time.RFC3339)
}

// mask 对字符串进行脱敏。
func mask(v string) string {
	r := []rune(strings.TrimSpace(v))
	if len(r) <= 4 {
		return "****"
	}
	return string(r[:2]) + "***" + string(r[len(r)-2:])
}

// cached 返回缓存。
func cached(ctx context.Context, store *idempotency.Store, tx *gorm.DB, actorType string, actorID uint64, path, key string, target any) error {
	ok, e := store.CachedResponse(ctx, tx, actorType, actorID, path, key, target)
	if e != nil {
		return e
	}
	if !ok {
		return problem.Conflict("IDEMPOTENCY_CONFLICT", "idempotency response missing")
	}
	return nil
}

// audit 返回审计。
func audit(ctx context.Context, tx *gorm.DB, id uint64, actorType string, actorID uint64, action, resource string, resourceID uint64, before, after any) error {
	b, _ := json.Marshal(before)
	a, _ := json.Marshal(after)
	return tx.WithContext(ctx).Table("audit_logs").Create(map[string]any{"id": id, "actor_type": actorType, "actor_id": actorID, "action": action, "resource_type": resource, "resource_id": resourceID, "before_data": datatypes.JSON(b), "after_data": datatypes.JSON(a), "result": "success", "request_id": requestctx.RequestIDPtr(ctx)}).Error
}
