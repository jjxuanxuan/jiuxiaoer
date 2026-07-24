package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type Service struct {
	db   *gorm.DB
	ids  *snowflake.Generator
	idem *idempotency.Store
}

// NewService 创建并初始化服务。
func NewService(db *gorm.DB, ids *snowflake.Generator) *Service {
	return &Service{db: db, ids: ids, idem: idempotency.NewStore(db)}
}

// ListMessages 查询Messages列表。
func (s *Service) ListMessages(ctx context.Context, c *auth.Claims, q pagination.Query) ([]MessageDTO, string, error) {
	customer, e := customerID(c)
	if e != nil {
		return nil, "", e
	}
	var rows []Message
	e = s.db.WithContext(ctx).Where("customer_id=? AND archived_at IS NULL", customer).Order("id DESC").Offset(q.Offset).Limit(q.PageSize + 1).Find(&rows).Error
	if e != nil {
		return nil, "", e
	}
	next := ""
	if len(rows) > q.PageSize {
		rows = rows[:q.PageSize]
		next = pagination.NextPageToken(q)
	}
	out := make([]MessageDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, messageDTO(r))
	}
	return out, next, nil
}

// Read 读取消息DTO。
func (s *Service) Read(ctx context.Context, c *auth.Claims, idRaw string) (MessageDTO, error) {
	customer, e := customerID(c)
	if e != nil {
		return MessageDTO{}, e
	}
	id, e := parseID(idRaw)
	if e != nil {
		return MessageDTO{}, problem.NotFound("MESSAGE_NOT_FOUND", "message not found")
	}
	now := time.Now()
	result := s.db.WithContext(ctx).Model(&Message{}).Where("id=? AND customer_id=?", id, customer).Update("read_at", gorm.Expr("COALESCE(read_at, ?)", now))
	if result.Error != nil {
		return MessageDTO{}, result.Error
	}
	if result.RowsAffected == 0 {
		return MessageDTO{}, problem.NotFound("MESSAGE_NOT_FOUND", "message not found")
	}
	var row Message
	if e := s.db.WithContext(ctx).Where("id=? AND customer_id=?", id, customer).First(&row).Error; e != nil {
		return MessageDTO{}, e
	}
	return messageDTO(row), nil
}

// ReadAll 读取All。
func (s *Service) ReadAll(ctx context.Context, c *auth.Claims, method, path, key string) (map[string]int64, error) {
	customer, e := customerID(c)
	if e != nil {
		return nil, e
	}
	var out map[string]int64
	e = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, e := s.idem.Start(ctx, tx, s.ids.Next(), "customer", customer, method, path, key, idempotency.RequestHash(map[string]any{"customer_id": customer}))
		if e != nil {
			return e
		}
		if !started {
			return cached(ctx, s.idem, tx, "customer", customer, path, key, &out)
		}
		res := tx.Model(&Message{}).Where("customer_id=? AND read_at IS NULL", customer).Update("read_at", time.Now())
		if res.Error != nil {
			return res.Error
		}
		out = map[string]int64{"updated": res.RowsAffected}
		return s.idem.Succeed(ctx, tx, "customer", customer, path, key, out)
	})
	return out, e
}

// ListDeliveries 查询Deliveries列表。
func (s *Service) ListDeliveries(ctx context.Context, c *auth.Claims, q pagination.Query, status string) ([]DeliveryDTO, string, error) {
	if _, e := adminID(c, "notification:list_all"); e != nil {
		return nil, "", e
	}
	db := s.db.WithContext(ctx).Model(&Delivery{})
	if status != "" {
		db = db.Where("status=?", status)
	}
	var rows []Delivery
	if e := db.Order("id DESC").Offset(q.Offset).Limit(q.PageSize + 1).Find(&rows).Error; e != nil {
		return nil, "", e
	}
	next := ""
	if len(rows) > q.PageSize {
		rows = rows[:q.PageSize]
		next = pagination.NextPageToken(q)
	}
	out := make([]DeliveryDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, deliveryDTO(r))
	}
	return out, next, nil
}

// Retry 重试配送DTO。
func (s *Service) Retry(ctx context.Context, c *auth.Claims, method, path, key, idRaw string, req RetryReq) (DeliveryDTO, error) {
	actor, e := adminID(c, "notification:retry")
	if e != nil {
		return DeliveryDTO{}, e
	}
	id, e := parseID(idRaw)
	if e != nil {
		return DeliveryDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid delivery id")
	}
	var out DeliveryDTO
	e = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, e := s.idem.Start(ctx, tx, s.ids.Next(), "admin", actor, method, path, key, idempotency.ResourceRequestHash("notification.retry", id, req))
		if e != nil {
			return e
		}
		if !started {
			return cached(ctx, s.idem, tx, "admin", actor, path, key, &out)
		}
		var row Delivery
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, id).Error; e != nil {
			return problem.NotFound("NOTIFICATION_NOT_FOUND", "delivery not found")
		}
		if row.Status != "dead" && row.Status != "retry_wait" {
			return problem.Conflict("NOTIFICATION_INVALID_STATUS", "delivery is not retryable")
		}
		if e := tx.Model(&Delivery{}).Where("id=?", id).Updates(map[string]any{"status": "pending", "next_retry_at": nil, "locked_by": nil, "locked_until": nil, "last_error_code": nil}).Error; e != nil {
			return e
		}
		row.Status = "pending"
		row.LastErrorCode = nil
		out = deliveryDTO(row)
		if e := audit(ctx, tx, s.ids.Next(), actor, "notification.retry", id, map[string]any{"reason": req.Reason}); e != nil {
			return e
		}
		return s.idem.Succeed(ctx, tx, "admin", actor, path, key, out)
	})
	return out, e
}

// ListTemplates 查询Templates列表。
func (s *Service) ListTemplates(ctx context.Context, c *auth.Claims, q pagination.Query) ([]TemplateDTO, string, error) {
	if _, e := adminID(c, "notification_template:list"); e != nil {
		return nil, "", e
	}
	var rows []Template
	if e := s.db.WithContext(ctx).Order("id DESC").Offset(q.Offset).Limit(q.PageSize + 1).Find(&rows).Error; e != nil {
		return nil, "", e
	}
	next := ""
	if len(rows) > q.PageSize {
		rows = rows[:q.PageSize]
		next = pagination.NextPageToken(q)
	}
	out := make([]TemplateDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, templateDTO(r))
	}
	return out, next, nil
}

// CreateTemplate 创建通知模板。
func (s *Service) CreateTemplate(ctx context.Context, c *auth.Claims, method, path, key string, req TemplateReq) (TemplateDTO, error) {
	actor, e := adminID(c, "notification_template:create")
	if e != nil {
		return TemplateDTO{}, e
	}
	if e = validateTemplate(req); e != nil {
		return TemplateDTO{}, e
	}
	if req.Status == "published" {
		if _, e = adminID(c, "notification_template:publish"); e != nil {
			return TemplateDTO{}, e
		}
	}
	var out TemplateDTO
	e = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, e := s.idem.Start(ctx, tx, s.ids.Next(), "admin", actor, method, path, key, idempotency.RequestHash(req))
		if e != nil {
			return e
		}
		if !started {
			return cached(ctx, s.idem, tx, "admin", actor, path, key, &out)
		}
		raw, _ := json.Marshal(req.AllowedFields)
		id := s.ids.Next()
		if req.Status == "published" {
			if e := s.retirePublished(ctx, tx, actor, req.EventType, req.Channel, 0); e != nil {
				return e
			}
		}
		row := Template{ID: id, TemplateCode: req.TemplateCode, EventType: req.EventType, Channel: req.Channel, ProviderTemplateID: nullString(req.ProviderTemplateID), Version: req.Version, TitleTemplate: req.TitleTemplate, BodyTemplate: req.BodyTemplate, AllowedFields: raw, Status: req.Status, CreatedBy: actor}
		if req.Status == "published" {
			row.PublishedBy = &actor
		}
		if e := tx.Create(&row).Error; e != nil {
			return e
		}
		out = templateDTO(row)
		if e := audit(ctx, tx, s.ids.Next(), actor, "notification_template.create", id, out); e != nil {
			return e
		}
		return s.idem.Succeed(ctx, tx, "admin", actor, path, key, out)
	})
	return out, e
}

// UpdateTemplate 更新Template。
func (s *Service) UpdateTemplate(ctx context.Context, c *auth.Claims, method, path, key, idRaw string, req TemplateReq) (TemplateDTO, error) {
	actor, e := adminID(c, "notification_template:create")
	if e != nil {
		return TemplateDTO{}, e
	}
	if e = validateTemplate(req); e != nil {
		return TemplateDTO{}, e
	}
	if req.Status == "published" {
		if _, e = adminID(c, "notification_template:publish"); e != nil {
			return TemplateDTO{}, e
		}
	}
	id, e := parseID(idRaw)
	if e != nil {
		return TemplateDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid template id")
	}
	var out TemplateDTO
	e = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, e := s.idem.Start(ctx, tx, s.ids.Next(), "admin", actor, method, path, key, idempotency.ResourceRequestHash("notification_template.update", id, req))
		if e != nil {
			return e
		}
		if !started {
			return cached(ctx, s.idem, tx, "admin", actor, path, key, &out)
		}
		var row Template
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, id).Error; e != nil {
			return problem.NotFound("NOTIFICATION_TEMPLATE_NOT_FOUND", "template not found")
		}
		if row.Status != "draft" {
			return problem.Conflict("NOTIFICATION_TEMPLATE_IMMUTABLE", "published or retired template versions are immutable")
		}
		fields, _ := json.Marshal(req.AllowedFields)
		if req.Status == "published" {
			if e := s.retirePublished(ctx, tx, actor, req.EventType, req.Channel, id); e != nil {
				return e
			}
		}
		updates := map[string]any{"template_code": req.TemplateCode, "event_type": req.EventType, "channel": req.Channel, "provider_template_id": nullString(req.ProviderTemplateID), "version": req.Version, "title_template": req.TitleTemplate, "body_template": req.BodyTemplate, "allowed_fields": datatypes.JSON(fields), "status": req.Status}
		if req.Status == "published" {
			updates["published_by"] = actor
		}
		if e := tx.Model(&Template{}).Where("id=?", id).Updates(updates).Error; e != nil {
			return e
		}
		row.TemplateCode = req.TemplateCode
		row.EventType = req.EventType
		row.Channel = req.Channel
		row.ProviderTemplateID = nullString(req.ProviderTemplateID)
		row.Version = req.Version
		row.TitleTemplate = req.TitleTemplate
		row.BodyTemplate = req.BodyTemplate
		row.AllowedFields = fields
		row.Status = req.Status
		out = templateDTO(row)
		if e := audit(ctx, tx, s.ids.Next(), actor, "notification_template.update", id, out); e != nil {
			return e
		}
		return s.idem.Succeed(ctx, tx, "admin", actor, path, key, out)
	})
	return out, e
}

// retirePublished 停用已发布模板。
func (s *Service) retirePublished(ctx context.Context, tx *gorm.DB, actor uint64, eventType, channel string, exceptID uint64) error {
	var active []Template
	query := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("event_type=? AND channel=? AND status='published'", eventType, channel)
	if exceptID != 0 {
		query = query.Where("id<>?", exceptID)
	}
	if e := query.Find(&active).Error; e != nil {
		return e
	}
	if len(active) == 0 {
		return nil
	}
	ids := make([]uint64, 0, len(active))
	for _, row := range active {
		ids = append(ids, row.ID)
	}
	if e := tx.Model(&Template{}).Where("id IN ?", ids).Update("status", "retired").Error; e != nil {
		return e
	}
	for _, id := range ids {
		if e := audit(ctx, tx, s.ids.Next(), actor, "notification_template.auto_retire", id, map[string]any{"replacement_event_type": eventType, "channel": channel}); e != nil {
			return e
		}
	}
	return nil
}

// validateTemplate 校验Template是否合法。
func validateTemplate(req TemplateReq) error {
	allowed := map[string]bool{}
	for _, f := range req.AllowedFields {
		allowed[f] = true
	}
	for _, text := range []string{req.TitleTemplate, req.BodyTemplate} {
		for {
			start := strings.Index(text, "{{")
			if start < 0 {
				break
			}
			end := strings.Index(text[start+2:], "}}")
			if end < 0 {
				return problem.InvalidArgument("VALIDATION_FAILED", "unclosed template variable")
			}
			name := strings.TrimSpace(text[start+2 : start+2+end])
			if !allowed[name] {
				return problem.InvalidArgument("TEMPLATE_FIELD_FORBIDDEN", "template uses a field outside allowed_fields")
			}
			text = text[start+2+end+2:]
		}
	}
	return nil
}

// customerID 返回用户ID。
func customerID(c *auth.Claims) (uint64, error) {
	if c == nil || c.AccountType != "customer" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "customer account required")
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

// has 判断是否存在通知。
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

// nullString 返回可空字符串值。
func nullString(v string) *string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return &v
}

// messageDTO 返回消息DTO。
func messageDTO(r Message) MessageDTO {
	return MessageDTO{ID: idString(r.ID), Type: r.Type, Title: r.Title, Summary: r.Summary, TargetType: str(r.TargetType), TargetID: func() string {
		if r.TargetID == nil {
			return ""
		}
		return idString(*r.TargetID)
	}(), ReadAt: ts(r.ReadAt), CreatedAt: r.CreatedAt.Format(time.RFC3339)}
}

// deliveryDTO 返回配送DTO。
func deliveryDTO(r Delivery) DeliveryDTO {
	return DeliveryDTO{ID: idString(r.ID), DeliveryNo: r.DeliveryNo, EventID: r.EventID, EventType: r.EventType, RecipientType: r.RecipientType, RecipientID: idString(r.RecipientID), Channel: r.Channel, TemplateID: idString(r.TemplateID), TemplateVersion: r.TemplateVersion, Status: r.Status, Attempts: r.Attempts, LastErrorCode: str(r.LastErrorCode), SentAt: ts(r.SentAt), CreatedAt: r.CreatedAt.Format(time.RFC3339)}
}

// templateDTO 返回通知模板 DTO。
func templateDTO(r Template) TemplateDTO {
	var f []string
	_ = json.Unmarshal(r.AllowedFields, &f)
	return TemplateDTO{ID: idString(r.ID), TemplateCode: r.TemplateCode, EventType: r.EventType, Channel: r.Channel, ProviderTemplateID: str(r.ProviderTemplateID), Version: r.Version, TitleTemplate: r.TitleTemplate, BodyTemplate: r.BodyTemplate, AllowedFields: f, Status: r.Status}
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
	return tx.Table("audit_logs").Create(map[string]any{"id": id, "actor_type": "admin", "actor_id": actor, "action": action, "resource_type": "notification", "resource_id": resource, "after_data": datatypes.JSON(raw), "result": "success", "request_id": requestctx.RequestIDPtr(ctx)}).Error
}
