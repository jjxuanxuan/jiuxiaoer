package deliveryreturn

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/aftersale"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/fixedwindow"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type Service struct {
	cfg        config.Config
	repo       *Repository
	afterSales *aftersale.Service
	ids        *snowflake.Generator
	idem       *idempotency.Store
	limiter    *fixedwindow.Limiter
	now        func() time.Time
}

func NewService(cfg config.Config, db *gorm.DB, redisClient *redis.Client, ids *snowflake.Generator) *Service {
	return &Service{
		cfg: cfg, repo: NewRepository(db), ids: ids, idem: idempotency.NewStore(db),
		limiter: fixedwindow.New(redisClient), now: time.Now,
	}
}

func (s *Service) WithAfterSale(service *aftersale.Service) *Service {
	s.afterSales = service
	return s
}

// Create records only a rider-reported reverse-logistics request. It never
// creates an after-sale or refund and never changes the order's financial state.
func (s *Service) Create(ctx context.Context, claims *auth.Claims, method, route, key, deliveryIDRaw string, req CreateReq) (out DTO, resultErr error) {
	defer func() {
		s.auditFailure(ctx, claims, method+" "+route, "delivery_return.request_denied", "delivery_order", deliveryIDRaw, resultErr)
	}()
	if !s.cfg.DeliveryReturn.Enabled || !s.cfg.DeliveryReturn.RiderWriteEnabled {
		return DTO{}, problem.New(http.StatusServiceUnavailable, "DELIVERY_RETURN_DISABLED", "Service Unavailable", "delivery return rider writes are disabled")
	}
	riderID, err := requireRider(claims, "delivery_return:create")
	if err != nil {
		return DTO{}, err
	}
	if !allowlisted(s.cfg.DeliveryReturn.RiderAllowlist, riderID) {
		return DTO{}, problem.Forbidden("RETURN_FORBIDDEN", "rider is outside the delivery return rollout")
	}
	deliveryID, err := parseID(deliveryIDRaw)
	if err != nil {
		return DTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid delivery order id")
	}
	if err := validateIdempotencyKey(key); err != nil {
		return DTO{}, err
	}
	req.ReasonCode = strings.TrimSpace(req.ReasonCode)
	if !validReason(req.ReasonCode) {
		return DTO{}, problem.InvalidArgument("INVALID_REASON_CODE", "invalid delivery return reason")
	}
	req.Note, err = cleanNote(req.Note)
	if err != nil {
		return DTO{}, err
	}
	if req.ReasonCode == ReasonOther && req.Note == "" {
		return DTO{}, problem.InvalidArgument("VALIDATION_FAILED", "other reason requires a note")
	}
	incidentID, err := optionalID(req.IncidentID)
	if err != nil {
		return DTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid incident id")
	}
	if req.ReasonCode == ReasonDamagedInTransit && incidentID == nil {
		return DTO{}, problem.InvalidArgument("VALIDATION_FAILED", "damaged delivery return requires an incident")
	}
	if req.ExpectedDeliveryVersion == 0 {
		return DTO{}, problem.InvalidArgument("VALIDATION_FAILED", "expected_delivery_version must be positive")
	}
	requestHash := idempotency.RequestHash(map[string]any{"delivery_order_id": deliveryID, "body": req})
	if s.repo.DB() != nil {
		replayed, replayErr := s.idem.ReplayCompleted(ctx, s.repo.DB(), "rider", riderID, route, key, requestHash, &out)
		if replayErr != nil {
			return DTO{}, normalizeIdempotency(replayErr)
		}
		if replayed {
			return out, nil
		}
	}

	// High-risk writes are fail-closed when the shared Redis limiter is not
	// authoritative. A process-local fallback cannot enforce a cluster limit.
	rate := s.limiter.Allow(ctx, "rate:delivery_return:create:rider:"+idString(riderID), 10*time.Minute, int64(s.cfg.DeliveryReturn.RiderRatePer10Minutes))
	if rate.Degraded {
		return DTO{}, problem.New(http.StatusServiceUnavailable, "DELIVERY_RETURN_DEPENDENCY_UNAVAILABLE", "Service Unavailable", "delivery return rate limiter is unavailable")
	}
	if !rate.Allowed {
		err := problem.TooManyRequests("RATE_LIMITED", "delivery return request rate exceeded")
		err.Data = map[string]any{"retry_after_seconds": int(rate.RetryAfter.Seconds())}
		return DTO{}, err
	}

	resultErr = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), "rider", riderID, method, route, key, requestHash)
		if err != nil {
			return normalizeIdempotency(err)
		}
		if !started {
			return s.cached(ctx, tx, riderID, route, key, &out)
		}
		delivery, err := s.repo.LockDelivery(ctx, tx, deliveryID)
		if IsNotFound(err) {
			return problem.NotFound("DELIVERY_NOT_FOUND", "delivery order not found")
		}
		if err != nil {
			return err
		}
		if delivery.RiderID == nil || *delivery.RiderID != riderID {
			return problem.Forbidden("RETURN_FORBIDDEN", "delivery order is not assigned to this rider")
		}
		if delivery.Status != "delivering" || delivery.PickedUpAt == nil {
			return problem.Conflict("INVALID_RETURN_STATE", "only a picked-up delivering order can be returned")
		}
		if delivery.AssignmentVersion != req.ExpectedDeliveryVersion {
			return problem.Conflict("VERSION_CONFLICT", "delivery assignment version changed")
		}
		if incidentID != nil {
			incident, findErr := s.repo.Incident(ctx, tx, *incidentID)
			if IsNotFound(findErr) || (findErr == nil && incident.DeliveryOrderID != delivery.ID) {
				return problem.InvalidArgument("VALIDATION_FAILED", "incident does not belong to this delivery")
			}
			if findErr != nil {
				return findErr
			}
			if req.ReasonCode == ReasonDamagedInTransit {
				count, countErr := s.repo.IncidentEvidenceCount(ctx, tx, incident.ID)
				if countErr != nil {
					return countErr
				}
				if count == 0 {
					return problem.Conflict("RETURN_EVIDENCE_REQUIRED", "damaged delivery return requires clean incident evidence")
				}
			}
		}
		existing, err := s.repo.ActiveByDelivery(ctx, tx, delivery.ID, true)
		if err == nil {
			aggregate := Aggregate{Return: existing}
			out = s.dto(aggregate, "rider")
			out.Deduplicated = true
			return s.idem.Succeed(ctx, tx, "rider", riderID, route, key, out)
		}
		if !IsNotFound(err) {
			return err
		}

		now := s.now().UTC()
		returnID := s.ids.Next()
		activeDeliveryID := delivery.ID
		row := Return{
			ID: returnID, ReturnNo: "DR" + idString(returnID), DeliveryOrderID: delivery.ID,
			ActiveDeliveryOrderID: &activeDeliveryID, OrderID: delivery.OrderID, ShopID: delivery.ShopID,
			RiderID: riderID, IncidentID: incidentID, ReasonCode: req.ReasonCode, Status: StatusRequested,
			InitiatorType: "rider", InitiatorID: riderID, RequestNote: optionalString(req.Note),
			RequestedAt: now, Version: 1, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.repo.CreateReturn(ctx, tx, &row); err != nil {
			if isDuplicate(err) {
				existing, findErr := s.repo.ActiveByDelivery(ctx, tx, delivery.ID, false)
				conflict := problem.Conflict("RETURN_ALREADY_ACTIVE", "delivery order already has an active return")
				if findErr == nil {
					conflict.Data = map[string]any{"existing_return_id": idString(existing.ID)}
				}
				return conflict
			}
			return err
		}
		if err := s.writeFacts(ctx, tx, row, "rider", &riderID, "request", "", StatusRequested, key); err != nil {
			return err
		}
		out = s.dto(Aggregate{Return: row}, "rider")
		return s.idem.Succeed(ctx, tx, "rider", riderID, route, key, out)
	})
	return out, resultErr
}

func (s *Service) AuditInvalidRequest(ctx context.Context, claims *auth.Claims, method, route, deliveryIDRaw string) {
	s.auditFailure(ctx, claims, method+" "+route, "delivery_return.request_denied", "delivery_order", deliveryIDRaw,
		problem.InvalidArgument("VALIDATION_FAILED", "request validation failed"))
}

// RiderDetail remains available when write switches are disabled so emergency
// rollback never hides an already-active reverse-logistics fact.
func (s *Service) RiderDetail(ctx context.Context, claims *auth.Claims, idRaw string) (DTO, error) {
	riderID, err := requireRider(claims, "delivery_return:view_own")
	if err != nil {
		return DTO{}, err
	}
	id, err := parseID(idRaw)
	if err != nil {
		return DTO{}, returnNotFound()
	}
	aggregate, err := s.repo.RiderAggregate(ctx, id, riderID)
	if IsNotFound(err) {
		return DTO{}, returnNotFound()
	}
	if err != nil {
		return DTO{}, err
	}
	return s.dto(aggregate, "rider"), nil
}

// HasActiveLocked is used by normal and force-complete paths after they have
// locked the delivery row. The guard applies even when new writes are disabled.
func (s *Service) HasActiveLocked(ctx context.Context, tx *gorm.DB, deliveryID uint64) (bool, error) {
	return s.repo.HasActiveLocked(ctx, tx, deliveryID)
}

func (s *Service) writeFacts(ctx context.Context, tx *gorm.DB, row Return, actorType string, actorID *uint64, action, from, to, key string) error {
	now := s.now().UTC()
	history := History{
		ID: s.ids.Next(), DeliveryReturnID: row.ID, FromStatus: optionalString(from), ToStatus: optionalString(to),
		Action: action, ActorType: actorType, ActorID: actorID, RequestID: requestctx.RequestIDPtr(ctx),
		IdempotencyKey: optionalString(key), MetadataJSON: jsonData(map[string]any{"reason_code": row.ReasonCode}), CreatedAt: now,
	}
	if err := s.repo.CreateHistory(ctx, tx, history); err != nil {
		return err
	}
	actor := uint64(0)
	if actorID != nil {
		actor = *actorID
	}
	if err := s.repo.CreateAudit(ctx, tx, AuditLog{
		ID: s.ids.Next(), ActorType: actorType, ActorID: actor, Action: "delivery_return." + action,
		ResourceType: "delivery_return", ResourceID: row.ID, BeforeData: jsonData(map[string]any{"status": from}),
		AfterData: jsonData(map[string]any{"status": to, "reason_code": row.ReasonCode, "delivery_order_id": idString(row.DeliveryOrderID)}),
		Result:    "success", RequestID: requestctx.RequestIDPtr(ctx), IP: requestctx.IPPtr(ctx), UserAgent: requestctx.UserAgentPtr(ctx),
	}); err != nil {
		return err
	}
	payload := map[string]any{
		"delivery_return_id": idString(row.ID), "return_no": row.ReturnNo,
		"delivery_order_id": idString(row.DeliveryOrderID), "order_id": idString(row.OrderID),
		"shop_id": idString(row.ShopID), "rider_id": idString(row.RiderID), "reason_code": row.ReasonCode,
		"from_status": from, "to_status": to,
	}
	return s.repo.CreateOutbox(ctx, tx, OutboxEvent{
		ID: s.ids.Next(), EventID: uuid.NewString(), EventType: "delivery.return_" + eventSuffix(action),
		AggregateType: "delivery_return", AggregateID: row.ID, Payload: jsonData(payload), Status: "pending",
		RequestID: requestctx.RequestIDPtr(ctx),
	})
}

func (s *Service) dto(aggregate Aggregate, role string) DTO {
	row := aggregate.Return
	out := DTO{
		ID: idString(row.ID), ReturnNo: row.ReturnNo, OrderID: idString(row.OrderID), DeliveryOrderID: idString(row.DeliveryOrderID),
		ShopID: idString(row.ShopID), RiderID: idString(row.RiderID), IncidentID: optionalIDString(row.IncidentID),
		AfterSaleID: optionalIDString(row.AfterSaleID), ReasonCode: row.ReasonCode, Status: row.Status,
		InitiatorType: row.InitiatorType, LogisticsStatus: logisticsStatus(row.Status), RefundStatus: aggregateRefundStatus(aggregate),
		InventoryStatus: inventoryStatus(row.Status), Version: row.Version, AllowedActions: s.allowedActions(row, role),
		RequestedAt: timeString(row.RequestedAt), ApprovedAt: optionalTimeString(row.ApprovedAt), ArrivedAt: optionalTimeString(row.ArrivedAt),
		ReceivedAt: optionalTimeString(row.ReceivedAt), ClosedAt: optionalTimeString(row.ClosedAt),
		ReceiptDeadlineAt: optionalTimeString(row.ReceiptDeadlineAt), CreatedAt: timeString(row.CreatedAt), UpdatedAt: timeString(row.UpdatedAt),
		History: make([]HistoryDTO, 0, len(aggregate.History)),
		Items:   make([]ItemDTO, 0, len(aggregate.Items)),
	}
	for _, item := range aggregate.Items {
		disposition := ""
		if item.Disposition != nil {
			disposition = *item.Disposition
		}
		out.Items = append(out.Items, ItemDTO{
			AfterSaleItemID: idString(item.AfterSaleItemID), OrderItemID: idString(item.OrderItemID),
			ShopProductID: idString(item.ShopProductID), ProductID: idString(item.ProductID),
			ExpectedQuantity: item.ExpectedQuantity, ReceivedQuantity: item.ReceivedQuantity,
			Disposition: disposition, PolicyCode: item.PolicyCode, PolicyVersion: item.PolicyVersion,
			AvailableBefore: item.AvailableBefore, AvailableAfter: item.AvailableAfter,
			Note: optionalStringValue(item.Note),
		})
	}
	for _, history := range aggregate.History {
		metadata := map[string]any{}
		_ = json.Unmarshal(history.MetadataJSON, &metadata)
		out.History = append(out.History, HistoryDTO{
			ID: idString(history.ID), FromStatus: optionalStringValue(history.FromStatus), ToStatus: optionalStringValue(history.ToStatus),
			Action: history.Action, ActorType: history.ActorType, ActorID: optionalIDString(history.ActorID), Metadata: metadata,
			CreatedAt: timeString(history.CreatedAt),
		})
	}
	return out
}

func (s *Service) allowedActions(row Return, role string) []string {
	actions := []string{}
	if role == "rider" && row.Status == StatusReturning && s.cfg.DeliveryReturn.Enabled && s.cfg.DeliveryReturn.RiderWriteEnabled && allowlisted(s.cfg.DeliveryReturn.RiderAllowlist, row.RiderID) {
		actions = append(actions, "arrive")
	}
	if role == "admin" && row.Status == StatusRequested && s.cfg.DeliveryReturn.Enabled && s.cfg.DeliveryReturn.ApprovalEnabled {
		actions = append(actions, "approve", "cancel")
	}
	if role == "store" && (row.Status == StatusArrived || row.Status == StatusException) && s.cfg.DeliveryReturn.Enabled && s.cfg.DeliveryReturn.ReceiptEnabled && allowlisted(s.cfg.DeliveryReturn.ShopAllowlist, row.ShopID) {
		actions = append(actions, "receive")
	}
	return actions
}

func logisticsStatus(status string) string {
	switch status {
	case StatusRequested, StatusReturning, StatusArrived, StatusReceived:
		return status
	case StatusClosed:
		return StatusReceived
	default:
		return StatusException
	}
}

func aggregateRefundStatus(aggregate Aggregate) string {
	row := aggregate.Return
	if row.AfterSaleID == nil {
		return "not_authorized"
	}
	if aggregate.RefundStatus == "" {
		if row.Status == StatusClosed {
			return "succeeded"
		}
		return "processing"
	}
	switch aggregate.RefundStatus {
	case "succeeded":
		return "succeeded"
	case "failed", "exception":
		return "failed"
	default:
		return "processing"
	}
}

func inventoryStatus(status string) string {
	switch status {
	case StatusRequested:
		return "not_applicable"
	case StatusReturning, StatusArrived:
		return "pending_receipt"
	case StatusReceived, StatusClosed:
		return "disposed"
	default:
		return "exception"
	}
}

func requireRider(claims *auth.Claims, permission string) (uint64, error) {
	if claims == nil || claims.AccountType != "rider" || !hasPermission(claims.Permissions, permission) {
		return 0, problem.Forbidden("RETURN_FORBIDDEN", "rider permission denied")
	}
	id, err := parseID(claims.RiderID)
	if err != nil {
		return 0, problem.Forbidden("RETURN_FORBIDDEN", "invalid rider identity")
	}
	return id, nil
}

func hasPermission(values []string, expected string) bool {
	for _, value := range values {
		if value == expected || value == "*" {
			return true
		}
	}
	return false
}

func allowlisted(values []string, id uint64) bool {
	want := idString(id)
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "*" || strings.EqualFold(value, "all") || value == want {
			return true
		}
	}
	return false
}

func validateIdempotencyKey(key string) error {
	if len(key) < 8 || len(key) > 128 {
		return problem.InvalidArgument("IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must contain 8 to 128 visible ASCII characters")
	}
	for index := range len(key) {
		if key[index] < 33 || key[index] > 126 {
			return problem.InvalidArgument("IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must contain 8 to 128 visible ASCII characters")
		}
	}
	return nil
}

func normalizeIdempotency(err error) error {
	var details *problem.Details
	if errors.As(err, &details) && details.ErrorCode == "IDEMPOTENCY_CONFLICT" && strings.Contains(details.Detail, "different request") {
		return problem.Conflict("IDEMPOTENCY_KEY_REUSED", "same idempotency key used with a different request")
	}
	return err
}

func (s *Service) cached(ctx context.Context, tx *gorm.DB, riderID uint64, route, key string, out *DTO) error {
	ok, err := s.idem.CachedResponse(ctx, tx, "rider", riderID, route, key, out)
	if err != nil {
		return err
	}
	if !ok {
		return problem.Conflict("IDEMPOTENCY_IN_PROGRESS", "request is still processing")
	}
	return nil
}

func cleanNote(value string) (string, error) {
	value = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value))
	if utf8.RuneCountInString(value) > 500 {
		return "", problem.InvalidArgument("VALIDATION_FAILED", "note must not exceed 500 characters")
	}
	return value, nil
}

func parseID(value string) (uint64, error) {
	id, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	if err != nil || id == 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

func optionalID(value string) (*uint64, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	id, err := parseID(value)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func optionalString(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	value = strings.TrimSpace(value)
	return &value
}

func optionalStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func optionalIDString(value *uint64) string {
	if value == nil {
		return ""
	}
	return idString(*value)
}

func idString(value uint64) string { return strconv.FormatUint(value, 10) }

func timeString(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func optionalTimeString(value *time.Time) string {
	if value == nil {
		return ""
	}
	return timeString(*value)
}

func returnNotFound() error {
	return problem.NotFound("DELIVERY_RETURN_NOT_FOUND", "delivery return not found")
}

func jsonData(value any) datatypes.JSON {
	payload, _ := json.Marshal(value)
	return payload
}

func eventSuffix(action string) string {
	switch action {
	case "request":
		return "requested"
	case "approve":
		return "approved"
	case "arrive", "receive", "close", "cancel":
		return map[string]string{"arrive": "arrived", "receive": "received", "close": "closed", "cancel": "cancelled"}[action]
	case "manual_review":
		return "disputed"
	case "refund_exception":
		return "exception"
	case "sla_reminder":
		return "sla_reminder"
	case "sla_breach":
		return "sla_breached"
	}
	return action
}

func isDuplicate(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "duplicate") || strings.Contains(message, "unique constraint")
}

func (s *Service) auditFailure(ctx context.Context, claims *auth.Claims, route, action, resourceType, resourceIDRaw string, requestErr error) {
	if requestErr == nil || s == nil || s.repo == nil || s.repo.DB() == nil || s.ids == nil {
		return
	}
	actorID := uint64(0)
	actorType := "unknown"
	if claims != nil {
		actorType = claims.AccountType
		switch claims.AccountType {
		case "rider":
			actorID, _ = parseID(claims.RiderID)
		case "admin":
			actorID, _ = parseID(claims.AdminUserID)
		case "merchant":
			actorID, _ = parseID(claims.MerchantUserID)
		case "customer":
			actorID, _ = parseID(claims.CustomerID)
		}
	}
	resourceID, _ := parseID(resourceIDRaw)
	details := problem.FromError(requestErr)
	result := "error"
	if details.Status == http.StatusForbidden || details.Status == http.StatusNotFound || details.Status == http.StatusTooManyRequests {
		result = "denied"
	} else if details.Status == http.StatusConflict {
		result = "conflict"
	} else if details.Status >= 400 && details.Status < 500 {
		result = "invalid"
	}
	_ = s.repo.CreateAudit(ctx, s.repo.DB(), AuditLog{
		ID: s.ids.Next(), ActorType: actorType, ActorID: actorID, Action: action, ResourceType: resourceType,
		ResourceID: resourceID, AfterData: jsonData(map[string]any{"route": strings.TrimSpace(route), "error_code": details.ErrorCode, "reason": details.Detail}),
		Result: result, RequestID: requestctx.RequestIDPtr(ctx), IP: requestctx.IPPtr(ctx), UserAgent: requestctx.UserAgentPtr(ctx),
	})
}
