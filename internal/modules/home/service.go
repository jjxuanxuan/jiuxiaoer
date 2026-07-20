package home

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/customerlocation"
	"jiuxiaoer-admin/backend-go/internal/modules/servicearea"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

var homeCityPattern = regexp.MustCompile(`^[0-9]{6}$`)
var allowedSlotTypes = map[string]bool{"popup": true, "banner": true, "category_block": true, "product_block": true, "campaign_block": true}

type Service struct {
	cfg       config.ServiceAreaConfig
	repo      *Repository
	redis     *goredis.Client
	resolver  *servicearea.Service
	idStore   *idempotency.Store
	idGen     *snowflake.Generator
	lbsMode   string
	locations *customerlocation.Service
}

func (s *Service) WithLocationContexts(mode string, locations *customerlocation.Service) *Service {
	s.lbsMode, s.locations = mode, locations
	return s
}

// NewService 创建并初始化服务。
func NewService(cfg config.ServiceAreaConfig, db *gorm.DB, redisClient *goredis.Client, resolver *servicearea.Service, idGen *snowflake.Generator) *Service {
	return &Service{cfg: cfg, repo: NewRepository(db), redis: redisClient, resolver: resolver, idStore: idempotency.NewStore(db), idGen: idGen}
}

// Public 返回公开数据。
func (s *Service) Public(ctx context.Context, cityCode string, lat, lng *float64, contextID string, actor customerlocation.Actor) (PublicDTO, error) {
	if s.lbsMode == "enforce" {
		if contextID == "" || s.locations == nil {
			return PublicDTO{}, problem.New(422, "LOCATION_CONTEXT_REQUIRED", "Unprocessable Entity", "X-Location-Context is required")
		}
		if cityCode != "" || lat != nil || lng != nil {
			return PublicDTO{}, problem.New(409, "LOCATION_CONTEXT_CONFLICT", "Conflict", "legacy location query conflicts with location context")
		}
		location, err := s.locations.GetContext(ctx, actor, contextID)
		if err != nil {
			return PublicDTO{}, err
		}
		return s.publicFromLocation(ctx, location)
	}
	if s.lbsMode == "observe" && contextID != "" && s.locations != nil {
		if location, err := s.locations.GetContext(ctx, actor, contextID); err == nil {
			s.locations.ObserveReadComparison("home", cityCode, "", location)
			return s.publicFromLocation(ctx, location)
		}
	}
	if !homeCityPattern.MatchString(cityCode) {
		return PublicDTO{}, problem.InvalidArgument("VALIDATION_INVALID_QUERY", "valid city_code is required")
	}
	base, err := s.publicBase(ctx, cityCode)
	if err != nil {
		return PublicDTO{}, err
	}
	base.Serviceability = ServiceabilityDTO{Serviceable: false, ReasonCode: "LOCATION_REQUIRED"}
	if lat == nil && lng == nil {
		return base, nil
	}
	if lat == nil || lng == nil || s.resolver == nil {
		return PublicDTO{}, problem.New(422, "LOCATION_REQUIRED", "Unprocessable Entity", "lat and lng are required together")
	}
	resolved, err := s.resolver.Resolve(ctx, servicearea.ResolveInput{CityCode: cityCode, Latitude: *lat, Longitude: *lng})
	if err != nil {
		var details *problem.Details
		if errors.As(err, &details) {
			base.Serviceability.ReasonCode = details.ErrorCode
			return base, nil
		}
		return PublicDTO{}, err
	}
	base.Serviceability = ServiceabilityDTO{Serviceable: true}
	base.ServiceShop = resolved.ServiceShop
	base.DeliveryPromise = resolved.ServiceShop.DeliveryPromise
	return base, nil
}

func (s *Service) publicFromLocation(ctx context.Context, location customerlocation.LocationContext) (PublicDTO, error) {
	base, err := s.publicBase(ctx, location.Location.CityCode)
	if err != nil {
		return PublicDTO{}, err
	}
	base.Serviceability = ServiceabilityDTO{Serviceable: location.Serviceability.Serviceable, ReasonCode: location.Serviceability.ReasonCode}
	if location.LocationLevel != "city" && location.ServiceShop != nil {
		base.ServiceShop, base.DeliveryPromise = location.ServiceShop, location.DeliveryPromise
	}
	return base, nil
}

// publicBase 返回公开数据 Base。
func (s *Service) publicBase(ctx context.Context, cityCode string) (PublicDTO, error) {
	version := "0:0"
	if s.redis != nil {
		cityVersion, _ := s.redis.Get(ctx, "home_version:"+cityCode).Result()
		if cityVersion == "" {
			cityVersion = "0"
		}
		globalVersion, _ := s.redis.Get(ctx, "home_version:global").Result()
		if globalVersion == "" {
			globalVersion = "0"
		}
		version = cityVersion + ":" + globalVersion
	}
	key := "home:v1:" + cityCode + ":" + version
	if s.redis != nil {
		var cached PublicDTO
		if raw, err := s.redis.Get(ctx, key).Bytes(); err == nil && json.Unmarshal(raw, &cached) == nil {
			return cached, nil
		}
	}
	slots, err := s.repo.PublicSlots(ctx, cityCode, time.Now().UTC())
	if err != nil {
		return PublicDTO{}, err
	}
	categories, err := s.repo.Categories(ctx)
	if err != nil {
		return PublicDTO{}, err
	}
	result := PublicDTO{CityCode: cityCode, Slots: slotDTOs(slots), Categories: categoryDTOs(categories), ProductBlocks: []map[string]any{}}
	for _, slot := range result.Slots {
		if slot.SlotType == "product_block" {
			result.ProductBlocks = append(result.ProductBlocks, slot.Payload)
		}
	}
	if s.redis != nil {
		if raw, err := json.Marshal(result); err == nil {
			_ = s.redis.Set(ctx, key, raw, s.cfg.HomeCacheTTL).Err()
		}
	}
	return result, nil
}

// List 查询时段 DTO列表列表。
func (s *Service) List(ctx context.Context, claims *auth.Claims, cityCode, slotType, status string, query pagination.Query) ([]SlotDTO, string, error) {
	if _, err := requirePermission(claims, "home_slot:list"); err != nil {
		return nil, "", err
	}
	rows, err := s.repo.List(ctx, cityCode, slotType, status, query)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		next = pagination.NextPageToken(query)
		rows = rows[:query.PageSize]
	}
	return slotDTOs(rows), next, nil
}

// Create 创建时段DTO。
func (s *Service) Create(ctx context.Context, claims *auth.Claims, method, path, key string, req SlotWriteReq) (SlotDTO, error) {
	adminID, err := requirePermission(claims, "home_slot:create")
	if err != nil {
		return SlotDTO{}, err
	}
	start, end, err := validateWrite(req, false)
	if err != nil {
		return SlotDTO{}, err
	}
	row := Slot{ID: s.idGen.Next(), CityCode: optional(req.CityCode), SlotType: req.SlotType, SlotKey: req.SlotKey, Title: req.Title, PayloadJSON: jsonData(req.Payload), StartAt: start, EndAt: end, Status: "draft", SortOrder: req.SortOrder, Version: 1, CreatedBy: adminID, UpdatedBy: adminID}
	var out SlotDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, adminID, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		if !started {
			return cached(ctx, s.idStore, tx, claims.AccountType, adminID, path, key, &out)
		}
		if err := s.repo.Create(ctx, tx, &row); err != nil {
			return err
		}
		if err := s.events(ctx, tx, adminID, "home_slot.create", row.ID, nil, row); err != nil {
			return err
		}
		out = slotDTO(row)
		return s.idStore.Succeed(ctx, tx, claims.AccountType, adminID, path, key, out)
	})
	if err == nil {
		s.bump(ctx, req.CityCode)
	}
	return out, err
}

// Update 更新时段DTO。
func (s *Service) Update(ctx context.Context, claims *auth.Claims, method, path, key, idRaw string, req SlotWriteReq) (SlotDTO, error) {
	adminID, err := requirePermission(claims, "home_slot:update")
	if err != nil {
		return SlotDTO{}, err
	}
	id, err := parseID(idRaw)
	if err != nil {
		return SlotDTO{}, problem.NotFound("HOME_SLOT_NOT_FOUND", "home slot not found")
	}
	if req.Version == 0 {
		return SlotDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "version is required")
	}
	start, end, err := validateWrite(req, false)
	if err != nil {
		return SlotDTO{}, err
	}
	var out SlotDTO
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		request := struct {
			ID   uint64       `json:"id"`
			Body SlotWriteReq `json:"body"`
		}{id, req}
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, adminID, method, path, key, idempotency.RequestHash(request))
		if err != nil {
			return err
		}
		if !started {
			return cached(ctx, s.idStore, tx, claims.AccountType, adminID, path, key, &out)
		}
		before, err := s.repo.Lock(ctx, tx, id)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("HOME_SLOT_NOT_FOUND", "home slot not found")
		}
		if err != nil {
			return err
		}
		ok, err := s.repo.Update(ctx, tx, id, req.Version, map[string]any{"city_code": optional(req.CityCode), "slot_type": req.SlotType, "slot_key": req.SlotKey, "title": req.Title, "payload_json": jsonData(req.Payload), "start_at": start, "end_at": end, "sort_order": req.SortOrder, "updated_by": adminID})
		if err != nil {
			return err
		}
		if !ok {
			return problem.Conflict("HOME_SLOT_VERSION_CONFLICT", "home slot was changed")
		}
		after, err := s.repo.Lock(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := s.events(ctx, tx, adminID, "home_slot.update", id, before, after); err != nil {
			return err
		}
		out = slotDTO(after)
		return s.idStore.Succeed(ctx, tx, claims.AccountType, adminID, path, key, out)
	})
	if err == nil {
		s.bump(ctx, req.CityCode)
	}
	return out, err
}

// SetStatus 设置状态。
func (s *Service) SetStatus(ctx context.Context, claims *auth.Claims, method, path, key, idRaw string, req SlotStatusReq) (SlotDTO, error) {
	adminID, err := requirePermission(claims, "home_slot:publish")
	if err != nil {
		return SlotDTO{}, err
	}
	id, err := parseID(idRaw)
	if err != nil {
		return SlotDTO{}, problem.NotFound("HOME_SLOT_NOT_FOUND", "home slot not found")
	}
	var out SlotDTO
	var city string
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		request := struct {
			ID   uint64        `json:"id"`
			Body SlotStatusReq `json:"body"`
		}{id, req}
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, adminID, method, path, key, idempotency.RequestHash(request))
		if err != nil {
			return err
		}
		if !started {
			return cached(ctx, s.idStore, tx, claims.AccountType, adminID, path, key, &out)
		}
		before, err := s.repo.Lock(ctx, tx, id)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("HOME_SLOT_NOT_FOUND", "home slot not found")
		}
		if err != nil {
			return err
		}
		city = value(before.CityCode)
		if err := validateTransition(before.Status, req.Status); err != nil {
			return err
		}
		if req.Status == "published" {
			if err := s.validatePublish(ctx, tx, before); err != nil {
				return err
			}
		}
		ok, err := s.repo.Update(ctx, tx, id, req.Version, map[string]any{"status": req.Status, "updated_by": adminID})
		if err != nil {
			return err
		}
		if !ok {
			return problem.Conflict("HOME_SLOT_VERSION_CONFLICT", "home slot was changed")
		}
		after, err := s.repo.Lock(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := s.events(ctx, tx, adminID, "home_slot.status", id, before, after); err != nil {
			return err
		}
		out = slotDTO(after)
		return s.idStore.Succeed(ctx, tx, claims.AccountType, adminID, path, key, out)
	})
	if err == nil {
		s.bump(ctx, city)
	}
	return out, err
}

// validatePublish 校验Publish是否合法。
func (s *Service) validatePublish(ctx context.Context, tx *gorm.DB, row Slot) error {
	if row.EndAt != nil && row.StartAt != nil && !row.EndAt.After(*row.StartAt) {
		return problem.New(422, "HOME_SLOT_INVALID_PAYLOAD", "Unprocessable Entity", "invalid effective time window")
	}
	var payload map[string]any
	if json.Unmarshal(row.PayloadJSON, &payload) != nil {
		return problem.New(422, "HOME_SLOT_INVALID_PAYLOAD", "Unprocessable Entity", "payload must be an object")
	}
	for field, table := range map[string]string{"product_ids": "products", "category_ids": "categories"} {
		ids, err := payloadIDs(payload[field])
		if err != nil {
			return problem.New(422, "HOME_SLOT_INVALID_PAYLOAD", "Unprocessable Entity", field+" must contain string ids")
		}
		ok, err := s.repo.ReferencesExist(ctx, tx, table, ids)
		if err != nil {
			return err
		}
		if !ok {
			return problem.New(422, "HOME_SLOT_INVALID_PAYLOAD", "Unprocessable Entity", field+" contains missing references")
		}
	}
	return nil
}

// events 返回events。
func (s *Service) events(ctx context.Context, tx *gorm.DB, adminID uint64, action string, id uint64, before, after any) error {
	if err := s.repo.CreateAudit(ctx, tx, AuditLog{ID: s.idGen.Next(), ActorType: "admin", ActorID: adminID, Action: action, ResourceType: "home_slot", ResourceID: id, BeforeData: jsonData(before), AfterData: jsonData(after), Result: "success"}); err != nil {
		return err
	}
	if err := s.repo.CreateOutbox(ctx, tx, OutboxEvent{ID: s.idGen.Next(), EventID: uuid.NewString(), EventType: "home_slot.changed", AggregateType: "home_slot", AggregateID: id, Payload: jsonData(map[string]any{"home_slot_id": strconv.FormatUint(id, 10)}), Status: "pending"}); err != nil {
		return err
	}
	if err := s.repo.CreateOutbox(ctx, tx, OutboxEvent{ID: s.idGen.Next(), EventID: uuid.NewString(), EventType: "cache.invalidate", AggregateType: "home_slot", AggregateID: id, Payload: jsonData(map[string]any{"patterns": []string{"home:*"}, "reason": "home_slot_changed"}), Status: "pending"}); err != nil {
		return err
	}
	return nil
}

// bump 递增首页。
func (s *Service) bump(ctx context.Context, city string) {
	if s.redis == nil {
		return
	}
	if city == "" {
		_ = s.redis.Incr(ctx, "home_version:global").Err()
		return
	}
	_ = s.redis.Incr(ctx, "home_version:"+city).Err()
}

// requirePermission 校验并确保权限满足要求。
func requirePermission(claims *auth.Claims, permission string) (uint64, error) {
	if claims == nil || claims.AccountType != "admin" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "admin account required")
	}
	allowed := false
	for _, p := range claims.Permissions {
		if p == permission {
			allowed = true
			break
		}
	}
	if !allowed {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
	}
	id, err := parseID(claims.AdminUserID)
	if err != nil {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "invalid admin identity")
	}
	return id, nil
}

// validateWrite 校验写入是否合法。
func validateWrite(req SlotWriteReq, version bool) (*time.Time, *time.Time, error) {
	if req.CityCode != "" && !homeCityPattern.MatchString(req.CityCode) {
		return nil, nil, problem.InvalidArgument("VALIDATION_FAILED", "invalid city_code")
	}
	if !allowedSlotTypes[req.SlotType] || req.Payload == nil {
		return nil, nil, problem.New(422, "HOME_SLOT_INVALID_PAYLOAD", "Unprocessable Entity", "unsupported slot_type or payload")
	}
	if err := validatePayload(req.SlotType, req.Payload); err != nil {
		return nil, nil, err
	}
	start, err := parseTime(req.StartAt)
	if err != nil {
		return nil, nil, problem.InvalidArgument("VALIDATION_FAILED", "invalid start_at")
	}
	end, err := parseTime(req.EndAt)
	if err != nil {
		return nil, nil, problem.InvalidArgument("VALIDATION_FAILED", "invalid end_at")
	}
	if start != nil && end != nil && !end.After(*start) {
		return nil, nil, problem.New(422, "HOME_SLOT_INVALID_PAYLOAD", "Unprocessable Entity", "end_at must be after start_at")
	}
	return start, end, nil
}

// validatePayload 校验载荷是否合法。
func validatePayload(slotType string, payload map[string]any) error {
	common := map[string]bool{"subtitle": true, "image_url": true, "target_path": true, "content": true}
	allowed := map[string]map[string]bool{
		"popup":          common,
		"banner":         common,
		"category_block": {"subtitle": true, "category_ids": true},
		"product_block":  {"subtitle": true, "product_ids": true},
		"campaign_block": common,
	}[slotType]
	for key, raw := range payload {
		if !allowed[key] {
			return problem.New(422, "HOME_SLOT_INVALID_PAYLOAD", "Unprocessable Entity", "payload contains unsupported field: "+key)
		}
		switch key {
		case "image_url":
			value, ok := raw.(string)
			if !ok || !strings.HasPrefix(value, "https://") {
				return problem.New(422, "HOME_SLOT_INVALID_PAYLOAD", "Unprocessable Entity", "image_url must use https")
			}
		case "target_path":
			value, ok := raw.(string)
			if !ok || !strings.HasPrefix(value, "/pages/") || strings.Contains(value, "://") {
				return problem.New(422, "HOME_SLOT_INVALID_PAYLOAD", "Unprocessable Entity", "target_path must be an internal mini-program path")
			}
		case "product_ids", "category_ids":
			if _, err := payloadIDs(raw); err != nil {
				return problem.New(422, "HOME_SLOT_INVALID_PAYLOAD", "Unprocessable Entity", key+" must contain string ids")
			}
		default:
			if _, ok := raw.(string); !ok {
				return problem.New(422, "HOME_SLOT_INVALID_PAYLOAD", "Unprocessable Entity", key+" must be a string")
			}
		}
	}
	return nil
}

// validateTransition 校验状态流转是否合法。
func validateTransition(from, to string) error {
	valid := from == "draft" && to == "published" || from == "published" && (to == "disabled" || to == "draft") || from == "disabled" && to == "draft" || from == to
	if !valid {
		return problem.Conflict("HOME_SLOT_STATUS_CONFLICT", "invalid home slot status transition")
	}
	return nil
}

// parseTime 解析时间。
func parseTime(raw string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	v, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// parseID 解析并校验字符串形式的 ID。
func parseID(raw string) (uint64, error) {
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

// optional 返回optional。
func optional(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// value 返回值。
func value(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// jsonData 将输入值序列化为 JSON 数据。
func jsonData(v any) datatypes.JSON { raw, _ := json.Marshal(v); return datatypes.JSON(raw) }

// cached 返回缓存。
func cached(ctx context.Context, store *idempotency.Store, tx *gorm.DB, actorType string, actorID uint64, path, key string, out any) error {
	ok, err := store.CachedResponse(ctx, tx, actorType, actorID, path, key, out)
	if err != nil {
		return err
	}
	if !ok {
		return problem.Conflict("IDEMPOTENCY_IN_PROGRESS", "request is still processing")
	}
	return nil
}

// payloadIDs 返回载荷 I Ds。
func payloadIDs(v any) ([]uint64, error) {
	if v == nil {
		return nil, nil
	}
	items, ok := v.([]any)
	if !ok {
		return nil, errors.New("not array")
	}
	ids := make([]uint64, 0, len(items))
	seen := map[uint64]bool{}
	for _, item := range items {
		raw, ok := item.(string)
		if !ok {
			return nil, errors.New("not string")
		}
		id, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || id == 0 {
			return nil, errors.New("invalid id")
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	return ids, nil
}

// slotDTOs 返回时段 DT Os。
func slotDTOs(rows []Slot) []SlotDTO {
	out := make([]SlotDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, slotDTO(row))
	}
	return out
}

// slotDTO 返回时段DTO。
func slotDTO(row Slot) SlotDTO {
	var payload map[string]any
	_ = json.Unmarshal(row.PayloadJSON, &payload)
	out := SlotDTO{ID: strconv.FormatUint(row.ID, 10), CityCode: value(row.CityCode), SlotType: row.SlotType, SlotKey: row.SlotKey, Title: row.Title, Payload: payload, Status: row.Status, SortOrder: row.SortOrder, Version: row.Version}
	if row.StartAt != nil {
		out.StartAt = row.StartAt.Format(time.RFC3339)
	}
	if row.EndAt != nil {
		out.EndAt = row.EndAt.Format(time.RFC3339)
	}
	if !row.UpdatedAt.IsZero() {
		out.UpdatedAt = row.UpdatedAt.Format(time.RFC3339)
	}
	return out
}

// categoryDTOs 返回分类 DT Os。
func categoryDTOs(rows []Category) []CategoryDTO {
	out := make([]CategoryDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, CategoryDTO{ID: strconv.FormatUint(row.ID, 10), Name: row.Name, SortOrder: row.SortOrder})
	}
	return out
}
