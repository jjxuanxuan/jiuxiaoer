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
	cfg      config.CP1Config
	db       *gorm.DB
	ids      *snowflake.Generator
	idem     idempotencyStore
	crypt    string
	provider Provider
}

type idempotencyStore interface {
	Start(context.Context, *gorm.DB, uint64, string, uint64, string, string, string, string) (bool, error)
	Succeed(context.Context, *gorm.DB, string, uint64, string, string, any) error
	CachedResponse(context.Context, *gorm.DB, string, uint64, string, string, any) (bool, error)
}

// NewService 创建并初始化服务。
func NewService(cfg config.CP1Config, db *gorm.DB, ids *snowflake.Generator) *Service {
	return &Service{cfg: cfg, db: db, ids: ids, idem: idempotency.NewStore(db), crypt: cfg.DataEncryptionKey, provider: &UnavailableProvider{}}
}

func (s *Service) WithProvider(provider Provider) *Service {
	if provider != nil {
		s.provider = provider
	}
	return s
}

// CreateSettings 为获授权门店创建唯一允许的打印配置。
// 设备标识符在持久化前加密，且绝不会由读取 API 返回。
func (s *Service) CreateSettings(ctx context.Context, claims *auth.Claims, method, path, key string, req SettingCreateReq) (SettingDTO, error) {
	actorID, err := merchantActor(claims, "print_setting:update_shop")
	if err != nil {
		return SettingDTO{}, err
	}
	shopID, err := parseID(req.ShopID)
	if err != nil {
		return SettingDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid shop id")
	}
	if !merchantShopAllowed(claims, shopID) {
		return SettingDTO{}, problem.Forbidden("SHOP_SCOPE_FORBIDDEN", "shop is not authorized")
	}
	templateID, err := parseID(req.TemplateID)
	if err != nil || req.Provider != s.cfg.PrintProvider || !validPrintEvents(req.AutoPrintEvents) {
		return SettingDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid provider, template, or auto_print_events")
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
		var template Template
		if err := tx.Where("id=? AND status='published'", templateID).First(&template).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.InvalidArgument("PRINT_TEMPLATE_INVALID", "published print template not found")
		} else if err != nil {
			return err
		}
		if !validReceiptTemplate(template) {
			return problem.InvalidArgument("PRINT_TEMPLATE_INVALID", "print template is not compatible with receipt.v1")
		}
		ciphertext, err := securevalue.Seal(s.crypt, req.DeviceID)
		if err != nil {
			return err
		}
		events, _ := json.Marshal(req.AutoPrintEvents)
		row := Setting{
			ID: s.ids.Next(), ShopID: shopID, Provider: req.Provider,
			DeviceIDCiphertext: ciphertext, DeviceIDMask: mask(req.DeviceID), DeviceStatus: "unknown",
			TemplateID: template.ID, Copies: req.Copies, AutoPrintEvents: datatypes.JSON(events),
			Enabled: req.Enabled, Version: 1, CreatedBy: actorID, UpdatedBy: actorID,
		}
		created := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if created.Error != nil {
			return created.Error
		}
		if created.RowsAffected != 1 {
			return problem.Conflict("PRINT_SETTING_EXISTS", "print setting already exists for this shop")
		}
		result = settingDTO(row)
		if err := audit(ctx, tx, s.ids.Next(), "merchant", actorID, "print_setting.create", "print_setting", row.ID, nil, result); err != nil {
			return err
		}
		return s.idem.Succeed(ctx, tx, "merchant", actorID, path, key, result)
	})
	return result, err
}

// TestSettings 使用从幂等键派生的稳定服务商请求 ID 执行同步提交，
// 因此服务商侧重试不会产生第二次实体测试打印。
func (s *Service) TestSettings(ctx context.Context, claims *auth.Claims, method, path, key, idRaw string) (TestPrintDTO, error) {
	actorID, err := merchantActor(claims, "print_setting:test_shop")
	if err != nil {
		return TestPrintDTO{}, err
	}
	id, err := parseID(idRaw)
	if err != nil {
		return TestPrintDTO{}, problem.NotFound("PRINT_SETTING_NOT_FOUND", "print setting not found")
	}
	if _, unavailable := s.provider.(*UnavailableProvider); unavailable {
		return TestPrintDTO{}, problem.New(503, "PRINT_PROVIDER_UNAVAILABLE", "Service Unavailable", "print provider is not configured")
	}
	requestHash := idempotency.RequestHash(map[string]string{"setting_id": idRaw})
	var result TestPrintDTO
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), "merchant", actorID, method, path, key, requestHash)
		if err != nil {
			return err
		}
		if !started {
			return cached(ctx, s.idem, tx, "merchant", actorID, path, key, &result)
		}
		var setting Setting
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&setting, id).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("PRINT_SETTING_NOT_FOUND", "print setting not found")
		} else if err != nil {
			return err
		}
		if !merchantShopAllowed(claims, setting.ShopID) {
			return problem.NotFound("PRINT_SETTING_NOT_FOUND", "print setting not found")
		}
		if setting.Provider != s.cfg.PrintProvider {
			return problem.Conflict("PRINT_PROVIDER_MISMATCH", "print setting provider is not active")
		}
		var template Template
		if err := tx.Where("id=? AND status='published'", setting.TemplateID).First(&template).Error; err != nil {
			return problem.Conflict("PRINT_TEMPLATE_INVALID", "published print template not found")
		}
		deviceID, err := securevalue.Open(s.crypt, setting.DeviceIDCiphertext)
		if err != nil {
			return problem.New(503, "PRINT_DEVICE_DECRYPT_FAILED", "Service Unavailable", "print device configuration is unavailable")
		}
		payload, _ := json.Marshal(map[string]any{
			"schema_version": "print.test.v1", "title": "酒小二打印测试页",
			"shop_id": idString(setting.ShopID), "printed_at": time.Now().UTC().Format(time.RFC3339),
			"paper_width_mm": template.PaperWidthMM,
		})
		taskID := s.ids.Next()
		taskNo := fmt.Sprintf("PT%d", taskID)
		var testSequence uint
		if err := tx.Model(&Task{}).Where("shop_id=? AND order_id=0 AND event_type='test_print' AND template_version=?", setting.ShopID, template.Version).
			Select("COALESCE(MAX(reprint_seq),0)").Scan(&testSequence).Error; err != nil {
			return err
		}
		providerRequestID := testProviderRequestID(actorID, id, key)
		startedAt := time.Now()
		providerResult, callErr := s.provider.Submit(ctx, PrintRequest{TaskNo: taskNo, ProviderRequestID: providerRequestID, DeviceID: deviceID, Copies: 1, Payload: payload})
		if callErr != nil {
			return problem.New(503, "PRINT_PROVIDER_UNAVAILABLE", "Service Unavailable", "test print submission failed")
		}
		if providerResult.ProviderRequestID != "" {
			providerRequestID = providerResult.ProviderRequestID
		}
		providerStatus := providerResult.Status
		if providerStatus == "" {
			providerStatus = "submitted"
		}
		taskStatus := "querying"
		var confirmedAt *time.Time
		if providerStatus == "succeeded" {
			taskStatus = "succeeded"
			confirmedAt = &startedAt
		}
		row := Task{
			ID: taskID, TaskNo: taskNo, EventID: uuid.NewString(), OrderID: 0, ShopID: setting.ShopID,
			EventType: "test_print", TemplateID: template.ID, TemplateVersion: template.Version,
			RenderPayload: datatypes.JSON(payload), PayloadSchemaVersion: "print.test.v1",
			ReprintSeq: testSequence + 1,
			Provider:   setting.Provider, ProviderRequestID: &providerRequestID, ProviderStatus: &providerStatus,
			Status: taskStatus, Attempts: 1, SubmittedAt: &startedAt, ConfirmedAt: confirmedAt,
		}
		if taskStatus == "succeeded" {
			row.SucceededAt = &startedAt
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		if err := tx.Create(&Attempt{
			ID: s.ids.Next(), PrintTaskID: taskID, AttemptNo: 1, Operation: "submit",
			ProviderRequestID: &providerRequestID, RequestHash: securevalue.Digest(string(payload)),
			Result: taskStatus, ProviderStatus: &providerStatus,
			DurationMS: uint(time.Since(startedAt).Milliseconds()), StartedAt: startedAt, FinishedAt: time.Now(),
			RequestID: requestctx.RequestIDPtr(ctx),
		}).Error; err != nil {
			return err
		}
		if err := tx.Model(&Setting{}).Where("id=?", id).Updates(map[string]any{"device_status": "online", "last_health_at": startedAt, "last_health_error_code": nil}).Error; err != nil {
			return err
		}
		result = TestPrintDTO{TaskID: idString(taskID), ProviderRequestID: providerRequestID, Status: taskStatus, SubmittedAt: startedAt.Format(time.RFC3339)}
		if err := audit(ctx, tx, s.ids.Next(), "merchant", actorID, "print_setting.test", "print_setting", id, nil, map[string]any{"task_id": result.TaskID, "status": result.Status}); err != nil {
			return err
		}
		return s.idem.Succeed(ctx, tx, "merchant", actorID, path, key, result)
	})
	return result, err
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
	if !hasSettingPatch(req) {
		return SettingDTO{}, problem.InvalidArgument("PRINT_SETTING_NO_CHANGES", "at least one setting field is required")
	}
	var templateID uint64
	if req.TemplateID != nil {
		templateID, err = parseID(*req.TemplateID)
		if err != nil {
			return SettingDTO{}, problem.InvalidArgument("PRINT_TEMPLATE_INVALID", "invalid template id")
		}
	}
	if req.AutoPrintEvents != nil && !validPrintEvents(*req.AutoPrintEvents) {
		return SettingDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid auto_print_events")
	}
	if req.Provider != nil && *req.Provider != s.cfg.PrintProvider {
		return SettingDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid print provider")
	}
	requestHash := idempotency.RequestHash(struct {
		SettingID string          `json:"setting_id"`
		Request   SettingPatchReq `json:"request"`
	}{SettingID: idRaw, Request: req})
	var result SettingDTO
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), "merchant", actorID, method, path, key, requestHash)
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
			return problem.NotFound("PRINT_SETTING_NOT_FOUND", "print setting not found")
		}
		if row.Version != req.Version {
			return problem.Conflict("VERSION_CONFLICT", "print setting version changed")
		}
		updates := map[string]any{"updated_by": actorID, "version": gorm.Expr("version + 1")}
		if req.Enabled != nil {
			updates["enabled"] = *req.Enabled
			row.Enabled = *req.Enabled
		}
		if req.Provider != nil {
			updates["provider"] = *req.Provider
			row.Provider = *req.Provider
		}
		if req.DeviceID != nil {
			ciphertext, sealErr := securevalue.Seal(s.crypt, *req.DeviceID)
			if sealErr != nil {
				return sealErr
			}
			updates["device_id_ciphertext"] = ciphertext
			updates["device_id_mask"] = mask(*req.DeviceID)
			updates["device_status"] = "unknown"
			updates["last_health_at"] = nil
			updates["last_health_error_code"] = nil
			row.DeviceIDCiphertext = ciphertext
			row.DeviceIDMask = mask(*req.DeviceID)
			row.DeviceStatus = "unknown"
			row.LastHealthAt = nil
			row.LastHealthErrorCode = nil
		}
		if req.TemplateID != nil {
			var template Template
			if err := tx.Where("id=? AND status='published'", templateID).First(&template).Error; errors.Is(err, gorm.ErrRecordNotFound) {
				return problem.InvalidArgument("PRINT_TEMPLATE_INVALID", "published print template not found")
			} else if err != nil {
				return err
			}
			if !validReceiptTemplate(template) {
				return problem.InvalidArgument("PRINT_TEMPLATE_INVALID", "print template is not compatible with receipt.v1")
			}
			updates["template_id"] = templateID
			row.TemplateID = templateID
		}
		if req.Copies != nil {
			updates["copies"] = *req.Copies
			row.Copies = *req.Copies
		}
		if req.AutoPrintEvents != nil {
			events, _ := json.Marshal(*req.AutoPrintEvents)
			updates["auto_print_events"] = datatypes.JSON(events)
			row.AutoPrintEvents = datatypes.JSON(events)
		}
		updated := tx.Model(&Setting{}).Where("id = ? AND version = ?", id, req.Version).Updates(updates)
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return problem.Conflict("VERSION_CONFLICT", "print setting version changed")
		}
		row.UpdatedBy, row.Version = actorID, row.Version+1
		if err := audit(ctx, tx, s.ids.Next(), "merchant", actorID, "print_setting.update", "print_setting", id, map[string]any{"version": req.Version}, settingDTO(row)); err != nil {
			return err
		}
		result = settingDTO(row)
		return s.idem.Succeed(ctx, tx, "merchant", actorID, path, key, result)
	})
	return result, err
}

func hasSettingPatch(req SettingPatchReq) bool {
	return req.Enabled != nil || req.Provider != nil || req.DeviceID != nil || req.TemplateID != nil || req.Copies != nil || req.AutoPrintEvents != nil
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
	if query.OrderBy != "" || query.Filter != "" {
		return nil, "", problem.InvalidArgument("VALIDATION_INVALID_QUERY", "print task list has a fixed sort and filter contract")
	}
	db := s.db.WithContext(ctx).Model(&Task{})
	// nil 专用于明确授权的管理员全局视图。非 nil 空切片表示商户当前没有
	// 获授权门店，绝不能降级为无范围查询。
	if shopIDs != nil && len(shopIDs) == 0 {
		return []TaskDTO{}, "", nil
	}
	if shopIDs != nil {
		db = db.Where("shop_id IN ?", shopIDs)
	}
	if status != "" {
		db = db.Where("status = ?", status)
	}
	db, err := pagination.ApplyTimeIDCursor(db, query, "created_at", "id", "desc")
	if err != nil {
		return nil, "", err
	}
	var rows []Task
	if err := pagination.OffsetDB(db, query).Order("created_at DESC, id DESC").Limit(query.PageSize + 1).Find(&rows).Error; err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		rows = rows[:query.PageSize]
		last := rows[len(rows)-1]
		next = pagination.NextPageTokenWithCursor(query, last.CreatedAt.Format(time.RFC3339Nano), idString(last.ID))
	}
	paperWidths, err := s.templatePaperWidths(ctx, s.db, rows)
	if err != nil {
		return nil, "", err
	}
	result := make([]TaskDTO, 0, len(rows))
	for _, row := range rows {
		result = append(result, taskDTO(row, paperWidths[row.TemplateID]))
	}
	return result, next, nil
}

// GetTask 获取任务。
func (s *Service) GetTask(ctx context.Context, claims *auth.Claims, idRaw string) (TaskDTO, error) {
	if _, err := merchantActor(claims, "print_task:list_shop"); err != nil {
		return TaskDTO{}, err
	}
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
	paperWidth, err := s.templatePaperWidth(ctx, s.db, row.TemplateID)
	if err != nil {
		return TaskDTO{}, err
	}
	return taskDTO(row, paperWidth), nil
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
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), "admin", actorID, method, path, key, idempotency.ResourceRequestHash("print_task.retry", id, req))
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
		paperWidth, widthErr := s.templatePaperWidth(ctx, tx, row.TemplateID)
		if widthErr != nil {
			return widthErr
		}
		result = taskDTO(row, paperWidth)
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
		if source.Status == "processing" || source.Status == "querying" {
			return problem.Conflict("PRINT_TASK_INVALID_STATUS", "an in-flight task cannot be reprinted")
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
		row.SourceTaskID = &source.ID
		row.ProviderRequestID = nil
		row.ProviderStatus = nil
		row.SubmittedAt = nil
		row.ConfirmedAt = nil
		row.CallbackDeadlineAt = nil
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
		paperWidth, widthErr := s.templatePaperWidth(ctx, tx, row.TemplateID)
		if widthErr != nil {
			return widthErr
		}
		result = taskDTO(row, paperWidth)
		return s.idem.Succeed(ctx, tx, actorType, actorID, path, key, result)
	})
	return result, err
}

// EnqueueAuto 返回Enqueue Auto。
// EnqueueAuto 在订单事务内调用。只有已启用的门店设置订阅该事件时，
// 才创建唯一任务。
func EnqueueAuto(ctx context.Context, tx *gorm.DB, ids *snowflake.Generator, shopID, orderID uint64, eventID, eventType string, payload any) error {
	_ = payload // 不信任事件载荷，不将其作为可打印的业务快照
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
	var template Template
	if err := tx.WithContext(ctx).Where("id=? AND status='published'", setting.TemplateID).First(&template).Error; err != nil {
		return err
	}
	if !validReceiptTemplate(template) {
		return problem.InvalidArgument("PRINT_TEMPLATE_INVALID", "print template is not compatible with receipt.v1")
	}
	raw, err := renderReceiptV1(tx.WithContext(ctx), orderID, shopID, template)
	if err != nil {
		return err
	}
	id := ids.Next()
	row := Task{ID: id, TaskNo: fmt.Sprintf("PT%d", id), EventID: eventID, OrderID: orderID, ShopID: shopID, EventType: eventType, TemplateID: setting.TemplateID, TemplateVersion: template.Version, RenderPayload: raw, PayloadSchemaVersion: template.PayloadSchemaVersion, Provider: setting.Provider, Status: "pending"}
	result := tx.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if result.Error != nil || result.RowsAffected == 0 {
		return result.Error
	}
	if err := enqueueWakeup(ctx, tx, ids, "print.task.ready", row); err != nil {
		return err
	}
	return audit(ctx, tx, ids.Next(), "system", 0, "print_task.enqueued", "print_task", row.ID, nil, map[string]any{
		"shop_id": shopID, "order_id": orderID, "event_type": eventType, "status": row.Status,
	})
}

// enqueueWakeup 写入任务唤醒事件。
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

// settingDTO 返回打印设置 DTO。
func settingDTO(row Setting) SettingDTO {
	var events []string
	_ = json.Unmarshal(row.AutoPrintEvents, &events)
	return SettingDTO{ID: idString(row.ID), ShopID: idString(row.ShopID), Provider: row.Provider, DeviceIDMask: row.DeviceIDMask, TemplateID: idString(row.TemplateID), Copies: row.Copies, AutoPrintEvents: events, Enabled: row.Enabled, Version: row.Version, DeviceStatus: row.DeviceStatus, LastHealthAt: ts(row.LastHealthAt), LastHealthErrorCode: str(row.LastHealthErrorCode)}
}

// taskDTO 返回封闭且不敏感的任务投影。RenderPayload 保留在服务端；
// 商户只会收到汇总计数及其 SHA-256 摘要。
func taskDTO(row Task, paperWidth uint16) TaskDTO {
	sourceTaskID := ""
	if row.SourceTaskID != nil {
		sourceTaskID = idString(*row.SourceTaskID)
	}
	return TaskDTO{
		ID: idString(row.ID), TaskNo: row.TaskNo, OrderID: idString(row.OrderID), ShopID: idString(row.ShopID),
		EventType: row.EventType, TemplateID: idString(row.TemplateID), TemplateVersion: row.TemplateVersion,
		PayloadSchemaVersion: row.PayloadSchemaVersion, SourceTaskID: sourceTaskID, ReprintSeq: row.ReprintSeq,
		Provider: row.Provider, ProviderRequestID: str(row.ProviderRequestID), ProviderStatus: str(row.ProviderStatus),
		Status: row.Status, Attempts: row.Attempts, NextRetryAt: ts(row.NextRetryAt), LastErrorCode: str(row.LastErrorCode),
		SucceededAt: ts(row.SucceededAt), RenderSummary: printRenderSummary(row, paperWidth), CreatedAt: row.CreatedAt.Format(time.RFC3339),
	}
}

func printRenderSummary(row Task, paperWidth uint16) *PrintRenderSummaryDTO {
	if row.PayloadSchemaVersion != "receipt.v1" || (paperWidth != 58 && paperWidth != 80) || len(row.RenderPayload) == 0 {
		return nil
	}
	var payload struct {
		Items []struct {
			Quantity int `json:"quantity"`
		} `json:"items"`
		Amounts struct {
			Payable int64 `json:"payable"`
		} `json:"amounts"`
	}
	if err := json.Unmarshal(row.RenderPayload, &payload); err != nil || len(payload.Items) == 0 || payload.Amounts.Payable < 0 {
		return nil
	}
	totalQuantity := 0
	for _, item := range payload.Items {
		if item.Quantity <= 0 {
			return nil
		}
		totalQuantity += item.Quantity
	}
	return &PrintRenderSummaryDTO{
		ItemKindCount: len(payload.Items), TotalQuantity: totalQuantity, PayableAmount: payload.Amounts.Payable,
		PaperWidthMM: paperWidth, ContentHash: securevalue.Digest(string(row.RenderPayload)),
	}
}

func (s *Service) templatePaperWidths(ctx context.Context, db *gorm.DB, tasks []Task) (map[uint64]uint16, error) {
	ids := make([]uint64, 0, len(tasks))
	seen := make(map[uint64]struct{}, len(tasks))
	for _, task := range tasks {
		if _, ok := seen[task.TemplateID]; ok {
			continue
		}
		seen[task.TemplateID] = struct{}{}
		ids = append(ids, task.TemplateID)
	}
	result := make(map[uint64]uint16, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	var templates []Template
	if err := db.WithContext(ctx).Select("id", "paper_width_mm").Where("id IN ?", ids).Find(&templates).Error; err != nil {
		return nil, err
	}
	for _, template := range templates {
		result[template.ID] = template.PaperWidthMM
	}
	return result, nil
}

func (s *Service) templatePaperWidth(ctx context.Context, db *gorm.DB, templateID uint64) (uint16, error) {
	var template Template
	if err := db.WithContext(ctx).Select("id", "paper_width_mm").First(&template, templateID).Error; err != nil {
		return 0, err
	}
	return template.PaperWidthMM, nil
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

func validReceiptTemplate(template Template) bool {
	return template.TemplateCode == "store_receipt" && template.PayloadSchemaVersion == "receipt.v1"
}

// merchantActor 返回商户审计主体。
func merchantActor(c *auth.Claims, perm string) (uint64, error) {
	if c == nil || c.AccountType != "merchant" || !has(c.Permissions, perm) {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "merchant permission required")
	}
	return parseID(c.MerchantUserID)
}

// adminActor 返回管理端审计主体。
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

// contains 判断集合是否包含指定字符串。
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

// mask 对字符串进行脱敏。
func mask(v string) string {
	r := []rune(strings.TrimSpace(v))
	if len(r) <= 4 {
		return "****"
	}
	return string(r[:2]) + "***" + string(r[len(r)-2:])
}

// cached 返回缓存。
func cached(ctx context.Context, store idempotencyStore, tx *gorm.DB, actorType string, actorID uint64, path, key string, target any) error {
	ok, e := store.CachedResponse(ctx, tx, actorType, actorID, path, key, target)
	if e != nil {
		return e
	}
	if !ok {
		return problem.Conflict("IDEMPOTENCY_CONFLICT", "idempotency response missing")
	}
	return nil
}

// testProviderRequestID 对一次测试打印操作保持稳定；
// 即使客户端复用幂等键，不同主体和设置之间仍保持唯一。
func testProviderRequestID(actorID, settingID uint64, key string) string {
	scope := fmt.Sprintf("%d:%d:%s", actorID, settingID, key)
	return "print-test-" + securevalue.Digest(scope)[:32]
}

// audit 返回审计。
func audit(ctx context.Context, tx *gorm.DB, id uint64, actorType string, actorID uint64, action, resource string, resourceID uint64, before, after any) error {
	return auditWithResult(ctx, tx, id, actorType, actorID, action, resource, resourceID, before, after, "success")
}

// auditWithResult 持久化有界业务结果。绝不能向此辅助函数传入
// 服务商载荷或原始错误消息。
func auditWithResult(ctx context.Context, tx *gorm.DB, id uint64, actorType string, actorID uint64, action, resource string, resourceID uint64, before, after any, result string) error {
	b, _ := json.Marshal(before)
	a, _ := json.Marshal(after)
	return tx.WithContext(ctx).Table("audit_logs").Create(map[string]any{"id": id, "actor_type": actorType, "actor_id": actorID, "action": action, "resource_type": resource, "resource_id": resourceID, "before_data": datatypes.JSON(b), "after_data": datatypes.JSON(a), "result": result, "request_id": requestctx.RequestIDPtr(ctx)}).Error
}
