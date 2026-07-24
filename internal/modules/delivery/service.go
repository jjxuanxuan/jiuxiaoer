package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/deliveryverification"
	"jiuxiaoer-admin/backend-go/internal/modules/dispatch"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type Service struct {
	repo      *Repository
	idStore   *idempotency.Store
	idGen     *snowflake.Generator
	cp1       config.CP1Config
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

// WithCP1 设置CP 1并返回更新后的值。
func (s *Service) WithCP1(cfg config.CP1Config) *Service { s.cp1 = cfg; return s }

// WithDispatch 设置调度并返回更新后的值。
func (s *Service) WithDispatch(service *dispatch.Service) *Service { s.dispatch = service; return s }

func (s *Service) WithIncidentResolver(resolver IncidentResolver) *Service {
	s.incidents = resolver
	return s
}

// WithReturnGuard 防止正向完成流程与有效逆向物流事实竞争并越过它。
func (s *Service) WithReturnGuard(guard ReturnGuard) *Service {
	s.returns = guard
	return s
}

// NewService 负责骑手配送状态流转。
func NewService(db *gorm.DB, idGen *snowflake.Generator) *Service {
	return &Service{
		repo:    NewRepository(db),
		idStore: idempotency.NewStore(db),
		idGen:   idGen,
	}
}

// List 只暴露待接单任务以及已经分配给当前骑手的任务。
func (s *Service) List(ctx context.Context, claims *auth.Claims, query pagination.Query, status string) ([]any, string, error) {
	if query.OrderBy != "" || query.Filter != "" {
		return nil, "", problem.InvalidArgument("VALIDATION_INVALID_QUERY", "delivery list has a fixed sort and filter contract")
	}
	riderID, err := riderIDFromClaims(claims, "delivery:list", false)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.repo.List(ctx, riderID, status, query)
	if err != nil {
		return nil, "", err
	}
	nextPageToken := ""
	if len(rows) > query.PageSize {
		rows = rows[:query.PageSize]
		last := rows[len(rows)-1]
		nextPageToken = pagination.NextPageTokenWithCursor(query, last.CreatedAt.UTC().Format(time.RFC3339Nano), idString(last.ID))
	}
	items := make([]any, 0, len(rows))
	for _, row := range rows {
		if row.RiderID == nil {
			items = append(items, candidateDeliverySummaryDTO(row))
			continue
		}
		items = append(items, assignedDeliverySummaryDTO(row))
	}
	return items, nextPageToken, nil
}

func candidateDeliverySummaryDTO(row DeliveryOrder) CandidateDeliverySummaryDTO {
	distance := uint(0)
	if row.PickupDistanceM != nil {
		distance = *row.PickupDistanceM
	}
	return CandidateDeliverySummaryDTO{
		ViewType: "candidate", ID: idString(row.ID), OrderID: idString(row.OrderID), ShopID: idString(row.ShopID),
		ShopName: row.ShopName, DestinationDistrict: row.DestinationDistrict, ItemCount: row.ItemCount,
		PickupDistanceM: distance, GrabExpiresAt: optionalTimeString(row.GrabExpiresAt), AssignmentVersion: row.AssignmentVersion,
	}
}

func assignedDeliverySummaryDTO(row DeliveryOrder) AssignedDeliverySummaryDTO {
	pickup := deliveryPickupSnapshotDTO(jsonMap(row.PickupSnapshot))
	if pickup == nil {
		pickup = &DeliveryPickupSnapshotDTO{}
	}
	recipient := deliveryRecipientSnapshotDTO(jsonMap(row.RecipientSnapshot))
	if recipient == nil {
		recipient = &DeliveryRecipientSnapshotDTO{}
	}
	return AssignedDeliverySummaryDTO{
		ViewType: "assigned", ID: idString(row.ID), OrderID: idString(row.OrderID), ShopID: idString(row.ShopID),
		RiderID: riderIDString(row.RiderID), Status: row.Status, AssignmentVersion: row.AssignmentVersion,
		DispatchStatus: row.DispatchStatus, PickupReadyStatus: row.PickupReadyStatus,
		PickupSnapshot: *pickup, RecipientSnapshot: *recipient,
		PickupReadyAt: optionalTimeString(row.PickupReadyAt), AcceptedAt: optionalTimeString(row.AcceptedAt),
		PickedUpAt: optionalTimeString(row.PickedUpAt), StartedAt: optionalTimeString(row.StartedAt),
		CompletedAt: optionalTimeString(row.CompletedAt), CreatedAt: timeString(row.CreatedAt),
	}
}

// Accept 接受并处理配送订单DTO。
func (s *Service) Accept(ctx context.Context, claims *auth.Claims, method string, path string, key string, deliveryIDRaw string) (DeliveryOrderDTO, error) {
	return s.AcceptWithVersion(ctx, claims, method, path, key, deliveryIDRaw, 0)
}

// AcceptWithVersion 接受并处理With 版本。
func (s *Service) AcceptWithVersion(ctx context.Context, claims *auth.Claims, method string, path string, key string, deliveryIDRaw string, expectedVersion uint) (DeliveryOrderDTO, error) {
	if s.dispatch != nil {
		_, err := s.dispatch.Grab(ctx, claims, method, path, key, deliveryIDRaw, expectedVersion)
		if err != nil {
			return DeliveryOrderDTO{}, err
		}
		riderID, riderErr := riderIDFromClaims(claims, "delivery:accept", false)
		deliveryID, deliveryErr := parseID(deliveryIDRaw)
		if riderErr == nil && deliveryErr == nil {
			if row, reloadErr := s.repo.AssignedSummary(ctx, riderID, deliveryID); reloadErr == nil {
				return deliveryOrderDTO(row), nil
			}
		}
		return DeliveryOrderDTO{}, problem.Internal("assigned delivery reload failed")
	}
	return s.transition(ctx, claims, method, path, key, deliveryIDRaw, "", transitionSpec{
		Action:             "delivery_accept",
		Permission:         "delivery:accept",
		EventType:          "delivery.accepted",
		FromDeliveryStatus: "pending_assign",
		ToDeliveryStatus:   "accepted",
		OrderValues: func(now time.Time) map[string]any {
			return map[string]any{"delivery_status": "accepted"}
		},
		DeliveryValues: func(riderID uint64, now time.Time) map[string]any {
			return map[string]any{"rider_id": riderID, "status": "accepted", "accepted_at": &now}
		},
	})
}

// Pickup 返回Pickup。
func (s *Service) Pickup(ctx context.Context, claims *auth.Claims, method string, path string, key string, deliveryIDRaw string) (DeliveryOrderDTO, error) {
	return s.PickupWithCode(ctx, claims, method, path, key, deliveryIDRaw, "")
}

// PickupWithCode 使用取货码执行取货。
func (s *Service) PickupWithCode(ctx context.Context, claims *auth.Claims, method string, path string, key string, deliveryIDRaw string, code string) (DeliveryOrderDTO, error) {
	return s.transition(ctx, claims, method, path, key, deliveryIDRaw, code, transitionSpec{
		Action:             "delivery_pickup",
		Permission:         "delivery:pickup",
		AllowLegacyUpdate:  true,
		EventType:          "delivery.picked_up",
		FromDeliveryStatus: "accepted",
		ToDeliveryStatus:   "delivering",
		VerificationStage:  "pickup",
		OrderValues: func(now time.Time) map[string]any {
			return map[string]any{"status": "delivering", "delivery_status": "delivering"}
		},
		DeliveryValues: func(riderID uint64, now time.Time) map[string]any {
			return map[string]any{"status": "delivering", "picked_up_at": &now, "started_at": &now, "picked_up_verified_at": &now}
		},
	})
}

// Complete 返回Complete。
func (s *Service) Complete(ctx context.Context, claims *auth.Claims, method string, path string, key string, deliveryIDRaw string) (DeliveryOrderDTO, error) {
	return s.CompleteWithCode(ctx, claims, method, path, key, deliveryIDRaw, "")
}

// CompleteWithCode 使用送达码完成配送。
func (s *Service) CompleteWithCode(ctx context.Context, claims *auth.Claims, method string, path string, key string, deliveryIDRaw string, code string) (DeliveryOrderDTO, error) {
	return s.transition(ctx, claims, method, path, key, deliveryIDRaw, code, transitionSpec{
		Action:             "delivery_complete",
		Permission:         "delivery:complete",
		AllowLegacyUpdate:  true,
		EventType:          "delivery.completed",
		FromDeliveryStatus: "delivering",
		ToDeliveryStatus:   "completed",
		VerificationStage:  "delivery",
		OrderValues: func(now time.Time) map[string]any {
			return map[string]any{"status": "completed", "delivery_status": "completed", "completed_at": &now}
		},
		DeliveryValues: func(riderID uint64, now time.Time) map[string]any {
			return map[string]any{"status": "completed", "completed_at": &now, "completed_verified_at": &now}
		},
	})
}

// transitionSpec 描述一次合法配送状态变更，以及需要同步更新的订单字段。
type transitionSpec struct {
	Action             string
	Permission         string
	AllowLegacyUpdate  bool
	EventType          string
	FromDeliveryStatus string
	ToDeliveryStatus   string
	OrderValues        func(now time.Time) map[string]any
	DeliveryValues     func(riderID uint64, now time.Time) map[string]any
	VerificationStage  string
}

// transition 集中处理骑手归属、幂等、配送状态和订单状态更新。
// 只有 pending_assign 可以在未归属时被接单；后续每个动作都必须校验骑手归属。
func (s *Service) transition(ctx context.Context, claims *auth.Claims, method string, path string, key string, deliveryIDRaw string, code string, spec transitionSpec) (DeliveryOrderDTO, error) {
	riderID, err := riderIDFromClaims(claims, spec.Permission, spec.AllowLegacyUpdate)
	if err != nil {
		return DeliveryOrderDTO{}, err
	}
	deliveryID, err := parseID(deliveryIDRaw)
	if err != nil {
		return DeliveryOrderDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid delivery order id")
	}

	var resp DeliveryOrderDTO
	var rejection *problem.Details
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		requestValue := map[string]string{"delivery_order_id": deliveryIDRaw, "action": spec.Action}
		if spec.VerificationStage != "" {
			requestValue["code_hash"] = idempotency.RequestHash(code)
		}
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, riderID, method, path, key, idempotency.RequestHash(requestValue))
		if err != nil {
			return err
		}
		if !started {
			cached, err := s.idStore.CachedResponse(ctx, tx, claims.AccountType, riderID, path, key, &resp)
			if err != nil {
				return err
			}
			if cached {
				return nil
			}
			return problem.Conflict("IDEMPOTENCY_CONFLICT", "idempotency response missing")
		}

		deliveryRow, err := s.repo.LockDelivery(ctx, tx, deliveryID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("DELIVERY_NOT_FOUND", "delivery order not found")
		}
		if err != nil {
			return err
		}
		if spec.FromDeliveryStatus == "pending_assign" {
			// 骑手接单是并发竞争，锁内必须再次确认任务未分配。
			if deliveryRow.RiderID != nil || deliveryRow.Status != "pending_assign" {
				return problem.Conflict("DELIVERY_INVALID_STATUS", "delivery order is not assignable")
			}
			var eligible int64
			if err := tx.Table("riders r").Joins("JOIN accounts a ON a.id=r.account_id").Where("r.id=? AND r.status='active' AND r.review_status='approved' AND a.status='active' AND (JSON_CONTAINS(r.service_scope, JSON_QUOTE(?), '$.shop_ids') OR JSON_CONTAINS(r.service_scope, JSON_ARRAY(?), '$.shop_ids'))", riderID, idString(deliveryRow.ShopID), deliveryRow.ShopID).Count(&eligible).Error; err != nil {
				return err
			}
			if eligible != 1 {
				return problem.Forbidden("RIDER_OUT_OF_SCOPE", "delivery shop is outside rider service scope")
			}
		} else {
			// 接单后，只有已分配骑手可以继续修改配送任务。
			if deliveryRow.RiderID == nil || *deliveryRow.RiderID != riderID {
				return problem.NotFound("DELIVERY_NOT_FOUND", "delivery order not found")
			}
			if deliveryRow.Status != spec.FromDeliveryStatus {
				return problem.Conflict("DELIVERY_INVALID_STATUS", "delivery order status cannot transition")
			}
		}
		if spec.VerificationStage != "" {
			if spec.Action == "delivery_pickup" && deliveryRow.PickupReadyStatus != "ready" {
				return problem.Conflict("DELIVERY_PICKUP_NOT_READY", "store has not finished preparing this order")
			}
			accountID, _ := strconv.ParseUint(claims.AccountID, 10, 64)
			blocked, verifyErr := deliveryverification.VerifyLocked(ctx, tx, s.cp1, s.idGen, deliveryID, spec.VerificationStage, code, riderID, accountID, claims.SessionID)
			if verifyErr != nil {
				return verifyErr
			}
			if blocked != nil {
				rejection = blocked
				return s.idStore.Fail(ctx, tx, claims.AccountType, riderID, path, key)
			}
			if spec.Action == "delivery_pickup" {
				if err := deliveryverification.ActivateDelivery(ctx, tx, s.cp1, s.idGen, deliveryID); err != nil {
					return err
				}
			}
		}
		orderRow, err := s.repo.LockOrder(ctx, tx, deliveryRow.OrderID)
		if err != nil {
			return err
		}
		now := time.Now()
		deliveryValues := spec.DeliveryValues(riderID, now)
		if spec.Action == "delivery_accept" {
			beforeVersion := deliveryRow.AssignmentVersion
			if beforeVersion == 0 {
				beforeVersion = 1
			}
			deliveryValues["assignment_version"] = beforeVersion + 1
			if err := tx.WithContext(ctx).Table("delivery_assignments").Create(map[string]any{
				"id": s.idGen.Next(), "delivery_order_id": deliveryID, "to_rider_id": riderID,
				"assignment_type": "grab", "status": "active", "actor_type": "rider", "actor_id": riderID,
				"version_before": beforeVersion, "version_after": beforeVersion + 1, "request_id": requestctx.RequestIDPtr(ctx),
			}).Error; err != nil {
				return err
			}
			deliveryRow.AssignmentVersion = beforeVersion + 1
		}
		if err := s.repo.UpdateDelivery(ctx, tx, deliveryID, deliveryValues); err != nil {
			return err
		}
		if err := s.repo.UpdateOrder(ctx, tx, deliveryRow.OrderID, spec.OrderValues(now)); err != nil {
			return err
		}
		if s.incidents != nil {
			switch spec.Action {
			case "delivery_pickup":
				if err := s.incidents.ResolveActiveLocked(ctx, tx, deliveryID, "pickup", "pickup_resumed"); err != nil {
					return err
				}
			case "delivery_complete":
				if err := s.incidents.ResolveActiveLocked(ctx, tx, deliveryID, "delivery", "delivery_completed"); err != nil {
					return err
				}
			}
		}
		if spec.Action == "delivery_complete" && s.returns != nil {
			active, guardErr := s.returns.HasActiveLocked(ctx, tx, deliveryID)
			if guardErr != nil {
				return guardErr
			}
			if active {
				return problem.Conflict("INVALID_RETURN_STATE", "delivery order has an active return and cannot be completed")
			}
		}
		if spec.Action == "delivery_complete" {
			// 在 observe 模式下，无效或缺失验证码会被审计，但不阻止完成。
			// 终态边界仍须使任何保持有效的凭据永久不可用。
			if err := deliveryverification.Invalidate(ctx, tx, s.idGen, deliveryID, "delivery_completed"); err != nil {
				return err
			}
		}
		if err := s.repo.CreateOrderLog(ctx, tx, OrderLog{
			ID:         s.idGen.Next(),
			OrderID:    deliveryRow.OrderID,
			ActorType:  claims.AccountType,
			ActorID:    riderID,
			Action:     spec.Action,
			FromStatus: stringPtr(orderRow.Status),
			ToStatus:   stringPtr(orderStatusAfter(orderRow.Status, spec.ToDeliveryStatus)),
			RequestID:  requestctx.RequestIDPtr(ctx),
		}); err != nil {
			return err
		}
		if err := s.createAudit(ctx, tx, riderID, spec.Action, "delivery_order", deliveryID, deliveryRow, map[string]string{"status": spec.ToDeliveryStatus}); err != nil {
			return err
		}
		if err := s.createOutbox(ctx, tx, spec.EventType, "delivery_order", deliveryID, map[string]any{"delivery_order_id": idString(deliveryID), "order_id": idString(deliveryRow.OrderID)}); err != nil {
			return err
		}
		if spec.ToDeliveryStatus == "completed" {
			if err := s.createOutbox(ctx, tx, "order.completed", "order", deliveryRow.OrderID, map[string]any{"order_id": idString(deliveryRow.OrderID), "delivery_order_id": idString(deliveryID)}); err != nil {
				return err
			}
		}
		deliveryRow.RiderID = &riderID
		deliveryRow.Status = spec.ToDeliveryStatus
		switch spec.Action {
		case "delivery_accept":
			deliveryRow.AcceptedAt = &now
		case "delivery_pickup":
			deliveryRow.PickedUpAt = &now
			deliveryRow.PickedUpVerifiedAt = &now
			deliveryRow.StartedAt = &now
		case "delivery_complete":
			deliveryRow.CompletedAt = &now
			deliveryRow.CompletedVerifiedAt = &now
		}
		resp = deliveryOrderDTO(deliveryRow)
		return s.idStore.Succeed(ctx, tx, claims.AccountType, riderID, path, key, resp)
	})
	if err == nil && rejection != nil {
		return DeliveryOrderDTO{}, rejection
	}
	return resp, err
}

// createOutbox 创建发件箱事件。
func (s *Service) createOutbox(ctx context.Context, tx *gorm.DB, eventType string, aggregateType string, aggregateID uint64, payload any) error {
	return s.repo.CreateOutbox(ctx, tx, OutboxEvent{
		ID:            s.idGen.Next(),
		EventID:       uuid.NewString(),
		EventType:     eventType,
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		Payload:       jsonData(payload),
		Status:        "pending",
		RetryCount:    0,
		RequestID:     requestctx.RequestIDPtr(ctx),
	})
}

// createAudit 记录骑手侧履约动作，便于后续运营复核。
func (s *Service) createAudit(ctx context.Context, tx *gorm.DB, actorID uint64, action string, resourceType string, resourceID uint64, before any, after any) error {
	return s.repo.CreateAuditLog(ctx, tx, AuditLog{
		ID:           s.idGen.Next(),
		ActorType:    "rider",
		ActorID:      actorID,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		BeforeData:   jsonData(before),
		AfterData:    jsonData(after),
		Result:       "success",
		RequestID:    requestctx.RequestIDPtr(ctx),
		IPHash:       requestctx.IPHashPtr(ctx),
		UserAgent:    requestctx.UserAgentPtr(ctx),
	})
}

// AuditFailure 在业务事务之外持久化被拒绝的履约操作。
// 这样既能追踪权限、所有权、状态和版本失败，
// 又不会意外提交部分配送状态变更。
func (s *Service) AuditFailure(ctx context.Context, claims *auth.Claims, action, deliveryIDRaw string, cause error) error {
	actorID := uint64(0)
	if claims != nil {
		actorID, _ = strconv.ParseUint(claims.RiderID, 10, 64)
	}
	deliveryID, _ := strconv.ParseUint(deliveryIDRaw, 10, 64)
	detail := problem.FromError(cause)
	return s.repo.CreateAuditLog(ctx, s.repo.DB(), AuditLog{
		ID: s.idGen.Next(), ActorType: "rider", ActorID: actorID, Action: action,
		ResourceType: "delivery_order", ResourceID: deliveryID,
		AfterData: jsonData(map[string]any{"error_code": detail.ErrorCode, "status": detail.Status}),
		Result:    "failed", RequestID: requestctx.RequestIDPtr(ctx), IPHash: requestctx.IPHashPtr(ctx), UserAgent: requestctx.UserAgentPtr(ctx),
	})
}

// riderIDFromClaims 从认证声明中返回骑手 ID。
func riderIDFromClaims(claims *auth.Claims, permission string, allowLegacyUpdate bool) (uint64, error) {
	if claims == nil || claims.AccountType != "rider" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "rider account required")
	}
	allowed := permission == ""
	for _, candidate := range claims.Permissions {
		if candidate == permission || (allowLegacyUpdate && candidate == "delivery:update_status") {
			allowed = true
			break
		}
	}
	if !allowed {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "rider permission required")
	}
	return parseID(claims.RiderID)
}

// deliveryOrderDTO 返回配送订单DTO。
func deliveryOrderDTO(row DeliveryOrder) DeliveryOrderDTO {
	viewType := "candidate"
	if row.RiderID != nil {
		viewType = "assigned"
	}
	distance := uint(0)
	if row.PickupDistanceM != nil {
		distance = *row.PickupDistanceM
	}
	pickup := DeliveryPickupSnapshotDTO{}
	if value := deliveryPickupSnapshotDTO(jsonMap(row.PickupSnapshot)); value != nil {
		pickup = *value
	}
	recipient := DeliveryRecipientSnapshotDTO{}
	if value := deliveryRecipientSnapshotDTO(jsonMap(row.RecipientSnapshot)); value != nil {
		recipient = *value
	}
	return DeliveryOrderDTO{
		ViewType:            viewType,
		ID:                  idString(row.ID),
		OrderID:             idString(row.OrderID),
		ShopID:              idString(row.ShopID),
		RiderID:             riderIDString(row.RiderID),
		Status:              row.Status,
		AssignmentVersion:   row.AssignmentVersion,
		DispatchStatus:      row.DispatchStatus,
		PickupReadyStatus:   row.PickupReadyStatus,
		PickupReadyAt:       optionalTimeString(row.PickupReadyAt),
		PickupSnapshot:      pickup,
		RecipientSnapshot:   recipient,
		CreatedAt:           timeString(row.CreatedAt),
		AcceptedAt:          optionalTimeString(row.AcceptedAt),
		PickedUpAt:          optionalTimeString(row.PickedUpAt),
		PickedUpVerifiedAt:  optionalTimeString(row.PickedUpVerifiedAt),
		StartedAt:           optionalTimeString(row.StartedAt),
		CompletedAt:         optionalTimeString(row.CompletedAt),
		CompletedVerifiedAt: optionalTimeString(row.CompletedVerifiedAt),
		ShopName:            row.ShopName, DestinationDistrict: row.DestinationDistrict,
		ItemCount: row.ItemCount, PickupDistanceM: distance, GrabExpiresAt: optionalTimeString(row.GrabExpiresAt),
	}
}

// deliveryDTOFromAssignment 根据分配记录构造配送 DTO。
func deliveryDTOFromAssignment(row dispatch.AssignmentResult) DeliveryOrderDTO {
	return DeliveryOrderDTO{
		ViewType: "assigned", ID: row.DeliveryOrderID, OrderID: row.OrderID, ShopID: row.ShopID,
		RiderID: row.RiderID, Status: row.Status, DispatchStatus: row.DispatchStatus,
		AssignmentVersion: row.AssignmentVersion, PickupReadyStatus: row.PickupReadyStatus,
		PickupReadyAt: row.PickupReadyAt, AcceptedAt: row.AcceptedAt,
	}
}

// orderStatusAfter 返回订单状态售后。
func orderStatusAfter(current string, deliveryStatus string) string {
	switch deliveryStatus {
	case "delivering":
		return "delivering"
	case "completed":
		return "completed"
	default:
		return current
	}
}

// parseID 解析并校验字符串形式的 ID。
func parseID(raw string) (uint64, error) {
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, problem.InvalidArgument("VALIDATION_FAILED", "invalid id")
	}
	return id, nil
}

// idString 将数字 ID 转换为字符串。
func idString(id uint64) string {
	return strconv.FormatUint(id, 10)
}

// riderIDString 返回骑手ID字符串。
func riderIDString(id *uint64) string {
	if id == nil {
		return ""
	}
	return idString(*id)
}

// stringPtr 将非空字符串转换为字符串指针。
func stringPtr(value string) *string {
	return &value
}

// timeString 将可选时间转换为字符串。
func timeString(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format(time.RFC3339)
}

// optionalTimeString 返回可选时间字符串。
func optionalTimeString(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.Format(time.RFC3339)
}

// jsonMap 返回JSON Map。
func jsonMap(data datatypes.JSON) map[string]any {
	if len(data) == 0 {
		return nil
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil
	}
	return result
}

// jsonData 将输入值序列化为 JSON 数据。
func jsonData(value any) datatypes.JSON {
	if value == nil {
		return nil
	}
	payload, _ := json.Marshal(value)
	return datatypes.JSON(payload)
}
