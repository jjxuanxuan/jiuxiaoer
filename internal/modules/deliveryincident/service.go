package deliveryincident

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/evidencetoken"
	"jiuxiaoer-admin/backend-go/internal/pkg/evidenceview"
	"jiuxiaoer-admin/backend-go/internal/pkg/fixedwindow"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/securevalue"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type Service struct {
	cfg                config.Config
	repo               *Repository
	ids                *snowflake.Generator
	idem               *idempotency.Store
	now                func() time.Time
	metrics            *metricState
	limiter            *fixedwindow.Limiter
	views              *evidenceview.Signer
	returnOrchestrator ReturnOrchestrator
}

type ReturnOrchestrator interface {
	CreateApproveFromIncidentWithTx(context.Context, *gorm.DB, uint64, uint64, uint64, string) (uint64, error)
	ValidateIncidentResolutionWithTx(context.Context, *gorm.DB, uint64, string) error
	ReturnIDForIncident(context.Context, *gorm.DB, uint64) uint64
}

func NewService(cfg config.Config, db *gorm.DB, ids *snowflake.Generator, registry *metrics.Registry, redisClients ...*redis.Client) *Service {
	var redisClient *redis.Client
	if len(redisClients) > 0 {
		redisClient = redisClients[0]
	}
	views, _ := evidenceview.New(cfg.DeliveryIncident.EvidenceViewBaseURL, cfg.DeliveryIncident.EvidenceViewSecret, cfg.DeliveryIncident.EvidenceViewTTL)
	return &Service{cfg: cfg, repo: NewRepository(db), ids: ids, idem: idempotency.NewStore(db), now: time.Now,
		metrics: newMetricState(db, registry), limiter: fixedwindow.New(redisClient), views: views}
}

func (s *Service) WithReturnOrchestrator(orchestrator ReturnOrchestrator) *Service {
	s.returnOrchestrator = orchestrator
	return s
}

func (s *Service) Create(ctx context.Context, claims *auth.Claims, method, route, key, deliveryIDRaw string, req CreateReq) (out DTO, resultErr error) {
	startedAt := time.Now()
	defer func() { s.metrics.observe("report", req.Type, resultErr, time.Since(startedAt)) }()
	defer func() {
		s.auditFailure(ctx, claims, method+" "+route, "incident.report", "delivery_order", deliveryIDRaw, resultErr,
			map[string]any{"type": req.Type})
	}()
	if err := s.writeEnabled(); err != nil {
		return DTO{}, err
	}
	riderID, err := riderActor(claims, "delivery_incident:create")
	if err != nil {
		return DTO{}, err
	}
	if !allowedID(s.cfg.DeliveryIncident.RiderAllowlist, riderID) {
		return DTO{}, problem.Forbidden("PERM_FORBIDDEN", "rider is outside the incident rollout")
	}
	deliveryID, err := parseID(deliveryIDRaw)
	if err != nil {
		return DTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid delivery order id")
	}
	if err := validateIdempotencyKey(key); err != nil {
		return DTO{}, err
	}
	req.Description, err = cleanText(req.Description, true)
	if err != nil {
		return DTO{}, err
	}
	req.ReasonCode = strings.TrimSpace(req.ReasonCode)
	if err := validateCreateShape(req); err != nil {
		return DTO{}, err
	}

	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		claimed, err := s.idem.Start(ctx, tx, s.ids.Next(), "rider", riderID, method, route, key, idempotency.RequestHash(map[string]any{"delivery_order_id": deliveryID, "body": req}))
		if err != nil {
			return normalizeIdempotencyError(err)
		}
		if !claimed {
			return s.cached(ctx, tx, "rider", riderID, route, key, &out)
		}
		if err := s.checkWriteRate(ctx, "create", riderID); err != nil {
			return err
		}
		delivery, err := s.repo.LockDelivery(ctx, tx, deliveryID)
		if IsNotFound(err) {
			return problem.NotFound("DELIVERY_NOT_FOUND", "delivery order not found")
		}
		if err != nil {
			return err
		}
		if delivery.RiderID == nil || *delivery.RiderID != riderID {
			return problem.NotFound("DELIVERY_NOT_FOUND", "delivery order not found")
		}
		stage, err := stageFor(delivery.Status)
		if err != nil || !typeAllowedAtStage(req.Type, stage) {
			return problem.Conflict("DELIVERY_INCIDENT_INVALID_STAGE", "current delivery stage does not allow this incident type")
		}
		items, err := s.buildItems(ctx, tx, delivery.OrderID, req.Items)
		if err != nil {
			return err
		}
		if err := validateContactAttempts(req.Type, req.ContactAttempts, delivery.AcceptedAt, s.now().UTC()); err != nil {
			return err
		}
		evidence, err := s.buildEvidence(riderID, req.EvidenceTokens)
		if err != nil {
			return err
		}
		status := StatusOpen
		if req.Type == TypeAlcoholDamaged && len(evidence) == 0 {
			status = StatusEvidenceRequired
		}
		now := s.now().UTC()
		incidentID := s.ids.Next()
		location := summarizeLocation(req.Location, delivery.RecipientSnapshot, now)
		if location.distanceSuppressedReason != "" {
			s.metrics.incLocationDistanceSuppressed(location.distanceSuppressedReason)
		}
		row := Incident{
			ID: incidentID, IncidentNo: "DI" + idString(incidentID), DeliveryOrderID: delivery.ID,
			OrderID: delivery.OrderID, ShopID: delivery.ShopID, RiderID: riderID, Type: req.Type, Stage: stage,
			Status: status, Priority: priorityFor(req.Type), ReasonCode: optional(req.ReasonCode), Description: req.Description,
			DeliveryStatusSnapshot: delivery.Status, AssignmentVersionSnapshot: delivery.AssignmentVersion,
			ReportedAt: now, Version: 1, CreatedAt: now, UpdatedAt: now,
			DistanceToDestinationM: location.distance, LocationAccuracyM: location.accuracy, LocationCapturedAt: location.capturedAt,
		}
		if req.ContactAttempts != nil {
			first, last := req.ContactAttempts.FirstAt.UTC(), req.ContactAttempts.LastAt.UTC()
			row.ContactAttemptCount, row.FirstContactAt, row.LastContactAt = req.ContactAttempts.Count, &first, &last
		}
		if err := s.repo.CreateIncident(ctx, tx, &row); err != nil {
			if isDuplicate(err) {
				existing, findErr := s.repo.ActiveByType(ctx, tx, delivery.ID, req.Type)
				conflict := problem.Conflict("DELIVERY_INCIDENT_ACTIVE_EXISTS", "an active incident of this type already exists")
				if findErr == nil {
					conflict.Data = map[string]any{"incident_id": idString(existing.ID), "incident_no": existing.IncidentNo, "status": existing.Status}
				}
				return conflict
			}
			return err
		}
		for index := range items {
			items[index].ID, items[index].IncidentID, items[index].CreatedAt = s.ids.Next(), row.ID, now
		}
		for index := range evidence {
			evidence[index].ID, evidence[index].IncidentID, evidence[index].CreatedAt = s.ids.Next(), row.ID, now
		}
		if err := s.repo.CreateItems(ctx, tx, items); err != nil {
			return err
		}
		if err := s.repo.CreateEvidence(ctx, tx, evidence); err != nil {
			return evidenceWriteError(err)
		}
		if err := s.writeHistoryAuditEvent(ctx, tx, &row, "rider", &riderID, "reported", "", status, "", "", nil); err != nil {
			return err
		}
		aggregate := Aggregate{Incident: row, Items: items, Evidence: evidence}
		out = s.aggregateDTO(aggregate)
		return s.idem.Succeed(ctx, tx, "rider", riderID, route, key, out)
	})
	return out, err
}

func (s *Service) RiderList(ctx context.Context, claims *auth.Claims, deliveryIDRaw string, query pagination.Query, filters ListFilters) (out []DTO, next string, resultErr error) {
	defer func() {
		s.auditFailure(ctx, claims, "GET /api/v1/delivery/orders/:id/incidents", "incident.list", "delivery_order", deliveryIDRaw, resultErr, nil)
	}()
	if err := s.readEnabled(); err != nil {
		return nil, "", err
	}
	riderID, err := riderActor(claims, "delivery_incident:view_own")
	if err != nil {
		return nil, "", err
	}
	if !allowedID(s.cfg.DeliveryIncident.RiderAllowlist, riderID) {
		return nil, "", problem.Forbidden("PERM_FORBIDDEN", "rider is outside the incident rollout")
	}
	deliveryID, err := parseID(deliveryIDRaw)
	if err != nil {
		return nil, "", problem.InvalidArgument("VALIDATION_FAILED", "invalid delivery order id")
	}
	rows, err := s.repo.RiderList(ctx, riderID, deliveryID, query, filters)
	if err != nil {
		return nil, "", err
	}
	return pageRows(rows, query)
}

func (s *Service) RiderDetail(ctx context.Context, claims *auth.Claims, incidentIDRaw string) (out DTO, resultErr error) {
	const route = "GET /api/v1/delivery/incidents/:id"
	defer func() {
		s.auditFailure(ctx, claims, route, "incident.detail_view", "delivery_incident", incidentIDRaw, resultErr, nil)
	}()
	if err := s.readEnabled(); err != nil {
		return DTO{}, err
	}
	riderID, err := riderActor(claims, "delivery_incident:view_own")
	if err != nil {
		return DTO{}, err
	}
	if !allowedID(s.cfg.DeliveryIncident.RiderAllowlist, riderID) {
		return DTO{}, problem.Forbidden("PERM_FORBIDDEN", "rider is outside the incident rollout")
	}
	id, err := parseID(incidentIDRaw)
	if err != nil {
		return DTO{}, incidentNotFound()
	}
	aggregate, err := s.repo.RiderAggregate(ctx, id, riderID)
	if IsNotFound(err) {
		return DTO{}, incidentNotFound()
	}
	if err != nil {
		return DTO{}, err
	}
	out = s.aggregateDTO(aggregate)
	if err := s.writeAccessAudit(ctx, "rider", riderID, "incident.detail_view", "delivery_incident", id, "success", map[string]any{"route": route}); err != nil {
		return DTO{}, err
	}
	return out, nil
}

func (s *Service) AddEvidence(ctx context.Context, claims *auth.Claims, method, route, key, incidentIDRaw string, req AddEvidenceReq) (out DTO, resultErr error) {
	startedAt := time.Now()
	defer func() { s.metrics.observe("evidence_add", "unknown", resultErr, time.Since(startedAt)) }()
	defer func() {
		s.auditFailure(ctx, claims, method+" "+route, "incident.evidence_add", "delivery_incident", incidentIDRaw, resultErr,
			map[string]any{"evidence_count": len(req.EvidenceTokens)})
	}()
	if err := s.writeEnabled(); err != nil {
		return DTO{}, err
	}
	riderID, err := riderActor(claims, "delivery_incident:evidence_add")
	if err != nil {
		return DTO{}, err
	}
	if !allowedID(s.cfg.DeliveryIncident.RiderAllowlist, riderID) {
		return DTO{}, problem.Forbidden("PERM_FORBIDDEN", "rider is outside the incident rollout")
	}
	id, err := parseID(incidentIDRaw)
	if err != nil {
		return DTO{}, incidentNotFound()
	}
	if err := validateIdempotencyKey(key); err != nil {
		return DTO{}, err
	}
	if req.ExpectedVersion == 0 {
		return DTO{}, problem.InvalidArgument("VALIDATION_FAILED", "expected_version must be positive")
	}
	if len(req.EvidenceTokens) == 0 || len(req.EvidenceTokens) > 9 {
		return DTO{}, evidenceInvalid("between 1 and 9 evidence tokens are required")
	}
	if duplicateStrings(req.EvidenceTokens) {
		return DTO{}, evidenceInvalid("duplicate evidence token")
	}
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		claimed, err := s.idem.Start(ctx, tx, s.ids.Next(), "rider", riderID, method, route, key, idempotency.RequestHash(map[string]any{"incident_id": id, "body": req}))
		if err != nil {
			return normalizeIdempotencyError(err)
		}
		if !claimed {
			return s.cached(ctx, tx, "rider", riderID, route, key, &out)
		}
		ref, err := s.repo.IncidentRef(ctx, tx, id)
		if IsNotFound(err) {
			return incidentNotFound()
		}
		if err != nil {
			return err
		}
		delivery, err := s.repo.LockDelivery(ctx, tx, ref.DeliveryOrderID)
		if err != nil {
			return incidentNotFound()
		}
		row, err := s.repo.LockIncident(ctx, tx, id)
		if err != nil {
			return incidentNotFound()
		}
		if row.RiderID != riderID || delivery.RiderID == nil || *delivery.RiderID != riderID || delivery.AssignmentVersion != row.AssignmentVersionSnapshot {
			return incidentNotFound()
		}
		if !isActive(row.Status) {
			return statusConflict("terminal incidents cannot receive evidence")
		}
		if row.Version != req.ExpectedVersion {
			return versionConflict()
		}
		if err := s.checkWriteRate(ctx, "evidence", riderID); err != nil {
			return err
		}
		existingCount, err := s.repo.EvidenceCount(ctx, tx, id)
		if err != nil {
			return err
		}
		if existingCount+int64(len(req.EvidenceTokens)) > 9 {
			return evidenceInvalid("an incident can contain at most 9 evidence files")
		}
		evidence, err := s.buildEvidence(riderID, req.EvidenceTokens)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		for index := range evidence {
			evidence[index].ID, evidence[index].IncidentID, evidence[index].CreatedAt = s.ids.Next(), row.ID, now
		}
		if err := s.repo.CreateEvidence(ctx, tx, evidence); err != nil {
			return evidenceWriteError(err)
		}
		target := row.Status
		if row.Status == StatusEvidenceRequired {
			target = StatusOpen
		}
		updated, err := s.repo.UpdateIncidentVersioned(ctx, tx, row.ID, row.Version, map[string]any{"status": target, "updated_at": now})
		if err != nil {
			return err
		}
		if !updated {
			return versionConflict()
		}
		from := row.Status
		row.Status, row.Version, row.UpdatedAt = target, row.Version+1, now
		if err := s.writeHistoryAuditEvent(ctx, tx, &row, "rider", &riderID, "evidence_added", from, target, "", "", map[string]any{"evidence_count": len(evidence)}); err != nil {
			return err
		}
		aggregate, err := s.repo.Aggregate(ctx, tx, row.ID)
		if err != nil {
			return err
		}
		out = s.aggregateDTO(aggregate)
		return s.idem.Succeed(ctx, tx, "rider", riderID, route, key, out)
	})
	return out, err
}

func (s *Service) StoreList(ctx context.Context, claims *auth.Claims, query pagination.Query, filters ListFilters) (out []DTO, next string, resultErr error) {
	defer func() {
		s.auditFailure(ctx, claims, "GET /api/v1/store/delivery-incidents", "incident.list", "delivery_incident", "", resultErr, nil)
	}()
	if err := s.readEnabled(); err != nil {
		return nil, "", err
	}
	_, shops, err := storeActor(claims, "delivery_incident:view_shop")
	if err != nil {
		return nil, "", err
	}
	shops = rolloutShops(shops, s.cfg.DeliveryIncident.ShopAllowlist)
	if len(shops) == 0 {
		return nil, "", problem.Forbidden("PERM_FORBIDDEN", "no authorized rollout shops")
	}
	if filters.ReportedFrom == nil {
		from := s.now().UTC().Add(-90 * 24 * time.Hour)
		filters.ReportedFrom = &from
	}
	rows, err := s.repo.StoreList(ctx, shops, query, filters)
	if err != nil {
		return nil, "", err
	}
	return pageRows(rows, query)
}

func (s *Service) StoreDetail(ctx context.Context, claims *auth.Claims, incidentIDRaw string) (out DTO, resultErr error) {
	const route = "GET /api/v1/store/delivery-incidents/:id"
	defer func() {
		s.auditFailure(ctx, claims, route, "incident.detail_view", "delivery_incident", incidentIDRaw, resultErr, nil)
	}()
	if err := s.readEnabled(); err != nil {
		return DTO{}, err
	}
	merchantID, shops, err := storeActor(claims, "delivery_incident:view_shop")
	if err != nil {
		return DTO{}, err
	}
	shops = rolloutShops(shops, s.cfg.DeliveryIncident.ShopAllowlist)
	id, err := parseID(incidentIDRaw)
	if err != nil || len(shops) == 0 {
		return DTO{}, incidentNotFound()
	}
	aggregate, err := s.repo.StoreAggregate(ctx, id, shops)
	if IsNotFound(err) {
		return DTO{}, incidentNotFound()
	}
	if err != nil {
		return DTO{}, err
	}
	out = s.aggregateDTO(aggregate)
	if err := s.writeAccessAudit(ctx, "merchant", merchantID, "incident.detail_view", "delivery_incident", id, "success", map[string]any{"route": route}); err != nil {
		return DTO{}, err
	}
	return out, nil
}

func (s *Service) AdminList(ctx context.Context, claims *auth.Claims, query pagination.Query, filters ListFilters) (out []DTO, next string, resultErr error) {
	defer func() {
		s.auditFailure(ctx, claims, "GET /api/v1/admin/delivery-incidents", "incident.list", "delivery_incident", "", resultErr, nil)
	}()
	if _, err := adminActor(claims, "delivery_incident:list_all"); err != nil {
		return nil, "", err
	}
	rows, err := s.repo.AdminList(ctx, query, filters)
	if err != nil {
		return nil, "", err
	}
	return pageRows(rows, query)
}

func (s *Service) AdminDetail(ctx context.Context, claims *auth.Claims, incidentIDRaw string) (out DTO, resultErr error) {
	const route = "GET /api/v1/admin/delivery-incidents/:id"
	defer func() {
		s.auditFailure(ctx, claims, route, "incident.detail_view", "delivery_incident", incidentIDRaw, resultErr, nil)
	}()
	adminID, err := adminActor(claims, "delivery_incident:view_all")
	if err != nil {
		return DTO{}, err
	}
	id, err := parseID(incidentIDRaw)
	if err != nil {
		return DTO{}, incidentNotFound()
	}
	aggregate, err := s.repo.Aggregate(ctx, s.repo.DB(), id)
	if IsNotFound(err) {
		return DTO{}, incidentNotFound()
	}
	if err != nil {
		return DTO{}, err
	}
	out = s.aggregateDTO(aggregate)
	if err := s.writeAccessAudit(ctx, "admin", adminID, "incident.detail_view", "delivery_incident", id, "success", map[string]any{"route": route}); err != nil {
		return DTO{}, err
	}
	return out, nil
}

func (s *Service) RiderEvidenceView(ctx context.Context, claims *auth.Claims, incidentIDRaw, evidenceIDRaw string) (out EvidenceViewDTO, resultErr error) {
	const route = "GET /api/v1/delivery/incidents/:id/evidence/:evidence_id/view"
	defer func() {
		s.auditFailure(ctx, claims, route, "incident.evidence_view", "delivery_incident", incidentIDRaw, resultErr, nil)
	}()
	if err := s.readEnabled(); err != nil {
		return EvidenceViewDTO{}, err
	}
	riderID, err := riderActor(claims, "delivery_incident:view_own")
	if err != nil {
		return EvidenceViewDTO{}, err
	}
	if !allowedID(s.cfg.DeliveryIncident.RiderAllowlist, riderID) {
		return EvidenceViewDTO{}, problem.Forbidden("PERM_FORBIDDEN", "rider is outside the incident rollout")
	}
	incidentID, evidenceID, err := parseEvidenceViewIDs(incidentIDRaw, evidenceIDRaw)
	if err != nil {
		return EvidenceViewDTO{}, incidentNotFound()
	}
	aggregate, err := s.repo.RiderAggregate(ctx, incidentID, riderID)
	if IsNotFound(err) {
		return EvidenceViewDTO{}, incidentNotFound()
	}
	if err != nil {
		return EvidenceViewDTO{}, err
	}
	return s.signEvidenceView(ctx, aggregate, evidenceID, "rider", riderID)
}

func (s *Service) StoreEvidenceView(ctx context.Context, claims *auth.Claims, incidentIDRaw, evidenceIDRaw string) (out EvidenceViewDTO, resultErr error) {
	const route = "GET /api/v1/store/delivery-incidents/:id/evidence/:evidence_id/view"
	defer func() {
		s.auditFailure(ctx, claims, route, "incident.evidence_view", "delivery_incident", incidentIDRaw, resultErr, nil)
	}()
	if err := s.readEnabled(); err != nil {
		return EvidenceViewDTO{}, err
	}
	merchantID, shops, err := storeActor(claims, "delivery_incident:view_shop")
	if err != nil {
		return EvidenceViewDTO{}, err
	}
	shops = rolloutShops(shops, s.cfg.DeliveryIncident.ShopAllowlist)
	incidentID, evidenceID, err := parseEvidenceViewIDs(incidentIDRaw, evidenceIDRaw)
	if err != nil || len(shops) == 0 {
		return EvidenceViewDTO{}, incidentNotFound()
	}
	aggregate, err := s.repo.StoreAggregate(ctx, incidentID, shops)
	if IsNotFound(err) {
		return EvidenceViewDTO{}, incidentNotFound()
	}
	if err != nil {
		return EvidenceViewDTO{}, err
	}
	return s.signEvidenceView(ctx, aggregate, evidenceID, "merchant", merchantID)
}

func (s *Service) AdminEvidenceView(ctx context.Context, claims *auth.Claims, incidentIDRaw, evidenceIDRaw string) (out EvidenceViewDTO, resultErr error) {
	const route = "GET /api/v1/admin/delivery-incidents/:id/evidence/:evidence_id/view"
	defer func() {
		s.auditFailure(ctx, claims, route, "incident.evidence_view", "delivery_incident", incidentIDRaw, resultErr, nil)
	}()
	adminID, err := adminActor(claims, "delivery_incident:view_all")
	if err != nil {
		return EvidenceViewDTO{}, err
	}
	incidentID, evidenceID, err := parseEvidenceViewIDs(incidentIDRaw, evidenceIDRaw)
	if err != nil {
		return EvidenceViewDTO{}, incidentNotFound()
	}
	aggregate, err := s.repo.Aggregate(ctx, s.repo.DB(), incidentID)
	if IsNotFound(err) {
		return EvidenceViewDTO{}, incidentNotFound()
	}
	if err != nil {
		return EvidenceViewDTO{}, err
	}
	return s.signEvidenceView(ctx, aggregate, evidenceID, "admin", adminID)
}

func parseEvidenceViewIDs(incidentIDRaw, evidenceIDRaw string) (uint64, uint64, error) {
	incidentID, err := parseID(incidentIDRaw)
	if err != nil {
		return 0, 0, err
	}
	evidenceID, err := parseID(evidenceIDRaw)
	if err != nil {
		return 0, 0, err
	}
	return incidentID, evidenceID, nil
}

func (s *Service) signEvidenceView(ctx context.Context, aggregate Aggregate, evidenceID uint64, actorType string, actorID uint64) (EvidenceViewDTO, error) {
	if s.views == nil || !s.views.Available() {
		return EvidenceViewDTO{}, problem.New(http.StatusServiceUnavailable, "DELIVERY_INCIDENT_EVIDENCE_VIEW_UNAVAILABLE", "Service Unavailable", "temporary evidence viewing is unavailable")
	}
	var selected *Evidence
	for index := range aggregate.Evidence {
		if aggregate.Evidence[index].ID == evidenceID {
			selected = &aggregate.Evidence[index]
			break
		}
	}
	if selected == nil {
		return EvidenceViewDTO{}, incidentNotFound()
	}
	if selected.ScanStatus != "clean" {
		return EvidenceViewDTO{}, problem.Conflict("DELIVERY_INCIDENT_EVIDENCE_VIEW_UNAVAILABLE", "evidence is not available for viewing")
	}
	result, err := s.views.Sign(evidenceview.Input{EvidenceID: selected.ID, IncidentID: aggregate.Incident.ID, ObjectKey: selected.ObjectKey,
		MimeType: selected.MimeType, SHA256: selected.SHA256, ActorType: actorType, ActorID: actorID})
	if err != nil {
		return EvidenceViewDTO{}, problem.New(http.StatusServiceUnavailable, "DELIVERY_INCIDENT_EVIDENCE_VIEW_UNAVAILABLE", "Service Unavailable", "temporary evidence viewing is unavailable")
	}
	if err := s.writeAccessAudit(ctx, actorType, actorID, "incident.evidence_view", "delivery_incident_evidence", selected.ID, "success",
		map[string]any{"incident_id": idString(aggregate.Incident.ID), "expires_at": result.ExpiresAt.UTC().Format(time.RFC3339)}); err != nil {
		return EvidenceViewDTO{}, err
	}
	return EvidenceViewDTO{URL: result.URL, ExpiresAt: result.ExpiresAt.UTC().Format(time.RFC3339)}, nil
}

func (s *Service) Acknowledge(ctx context.Context, claims *auth.Claims, method, route, key, incidentIDRaw string, req AcknowledgeReq) (DTO, error) {
	note, err := cleanText(req.Note, false)
	if err != nil {
		return DTO{}, err
	}
	return s.adminTransition(ctx, claims, method, route, key, incidentIDRaw, req.ExpectedVersion, "delivery_incident:acknowledge", "acknowledged", "", note)
}

func (s *Service) Resolve(ctx context.Context, claims *auth.Claims, method, route, key, incidentIDRaw string, req ResolveReq) (DTO, error) {
	if req.ResolutionCode != "issue_cleared_resume" && req.ResolutionCode != "return_required" && req.ResolutionCode != "returned_to_store" && req.ResolutionCode != "refund_followup" && req.ResolutionCode != "other" {
		return DTO{}, problem.InvalidArgument("VALIDATION_FAILED", "unsupported resolution_code")
	}
	if req.ResolutionCode == "return_required" && (claims == nil || !hasPermission(claims.Permissions, "delivery_return:approve")) {
		return DTO{}, problem.Forbidden("PERM_FORBIDDEN", "delivery return approval permission denied")
	}
	note, err := cleanText(req.ResolutionNote, true)
	if err != nil {
		return DTO{}, err
	}
	return s.adminTransition(ctx, claims, method, route, key, incidentIDRaw, req.ExpectedVersion, "delivery_incident:resolve", "resolved", req.ResolutionCode, note)
}

func (s *Service) Reject(ctx context.Context, claims *auth.Claims, method, route, key, incidentIDRaw string, req RejectReq) (DTO, error) {
	reason, err := cleanText(req.Reason, true)
	if err != nil {
		return DTO{}, err
	}
	if strings.TrimSpace(req.ReasonCode) == "" || len(strings.TrimSpace(req.ReasonCode)) > 64 {
		return DTO{}, problem.InvalidArgument("VALIDATION_FAILED", "reason_code is required")
	}
	return s.adminTransition(ctx, claims, method, route, key, incidentIDRaw, req.ExpectedVersion, "delivery_incident:reject", "rejected", strings.TrimSpace(req.ReasonCode), reason)
}

func (s *Service) adminTransition(ctx context.Context, claims *auth.Claims, method, route, key, incidentIDRaw string, version uint, permission, action, reasonCode, remark string) (out DTO, resultErr error) {
	startedAt := time.Now()
	defer func() { s.metrics.observe(action, "unknown", resultErr, time.Since(startedAt)) }()
	defer func() {
		data := map[string]any{"expected_version": version}
		if reasonCode != "" {
			data["reason_code"] = reasonCode
		}
		s.auditFailure(ctx, claims, method+" "+route, "incident."+auditAction(action, "admin"), "delivery_incident", incidentIDRaw, resultErr, data)
	}()
	if err := s.writeEnabled(); err != nil {
		return DTO{}, err
	}
	adminID, err := adminActor(claims, permission)
	if err != nil {
		return DTO{}, err
	}
	id, err := parseID(incidentIDRaw)
	if err != nil {
		return DTO{}, incidentNotFound()
	}
	if err := validateIdempotencyKey(key); err != nil {
		return DTO{}, err
	}
	if version == 0 {
		return DTO{}, problem.InvalidArgument("VALIDATION_FAILED", "expected_version must be positive")
	}
	var deliveryReturnID uint64
	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		requestHash := idempotency.RequestHash(map[string]any{"incident_id": id, "version": version, "action": action, "reason_code": reasonCode, "remark": remark})
		claimed, err := s.idem.Start(ctx, tx, s.ids.Next(), "admin", adminID, method, route, key, requestHash)
		if err != nil {
			return normalizeIdempotencyError(err)
		}
		if !claimed {
			return s.cached(ctx, tx, "admin", adminID, route, key, &out)
		}
		ref, err := s.repo.IncidentRef(ctx, tx, id)
		if IsNotFound(err) {
			return incidentNotFound()
		}
		if err != nil {
			return err
		}
		if _, err := s.repo.LockDelivery(ctx, tx, ref.DeliveryOrderID); err != nil {
			return incidentNotFound()
		}
		if action == "resolved" && reasonCode == "return_required" {
			if s.returnOrchestrator == nil {
				return problem.New(http.StatusServiceUnavailable, "DELIVERY_RETURN_DEPENDENCY_UNAVAILABLE", "Service Unavailable", "delivery return orchestrator is unavailable")
			}
			deliveryReturnID, err = s.returnOrchestrator.CreateApproveFromIncidentWithTx(ctx, tx, id, ref.DeliveryOrderID, adminID, remark)
			if err != nil {
				return err
			}
		}
		row, err := s.repo.LockIncident(ctx, tx, id)
		if err != nil {
			return incidentNotFound()
		}
		if row.Version != version {
			return versionConflict()
		}
		if err := validateTransition(row.Status, action); err != nil {
			return err
		}
		if action == "resolved" && s.returnOrchestrator != nil {
			if err := s.returnOrchestrator.ValidateIncidentResolutionWithTx(ctx, tx, id, reasonCode); err != nil {
				return err
			}
		}
		now := s.now().UTC()
		values := map[string]any{"updated_at": now}
		target := row.Status
		switch action {
		case "acknowledged":
			target = StatusAcknowledged
			values["status"], values["acknowledged_by"], values["acknowledged_at"] = target, adminID, now
		case "resolved":
			target = StatusResolved
			values["status"], values["resolved_by"], values["resolved_at"] = target, adminID, now
			values["resolution_code"], values["resolution_note"] = reasonCode, remark
		case "rejected":
			target = StatusRejected
			values["status"], values["rejected_by"], values["rejected_at"] = target, adminID, now
			values["rejection_code"], values["rejection_reason"] = reasonCode, remark
		}
		updated, err := s.repo.UpdateIncidentVersioned(ctx, tx, row.ID, row.Version, values)
		if err != nil {
			return err
		}
		if !updated {
			return versionConflict()
		}
		from := row.Status
		row.Status, row.Version, row.UpdatedAt = target, row.Version+1, now
		switch action {
		case "acknowledged":
			row.AcknowledgedBy, row.AcknowledgedAt = &adminID, &now
		case "resolved":
			row.ResolvedBy, row.ResolvedAt, row.ResolutionCode, row.ResolutionNote = &adminID, &now, optional(reasonCode), optional(remark)
		case "rejected":
			row.RejectedBy, row.RejectedAt, row.RejectionCode, row.RejectionReason = &adminID, &now, optional(reasonCode), optional(remark)
		}
		if err := s.writeHistoryAuditEvent(ctx, tx, &row, "admin", &adminID, action, from, target, reasonCode, remark, nil); err != nil {
			return err
		}
		aggregate, err := s.repo.Aggregate(ctx, tx, row.ID)
		if err != nil {
			return err
		}
		out = s.aggregateDTO(aggregate)
		if deliveryReturnID == 0 && s.returnOrchestrator != nil {
			deliveryReturnID = s.returnOrchestrator.ReturnIDForIncident(ctx, tx, id)
		}
		if deliveryReturnID != 0 {
			out.DeliveryReturnID = idString(deliveryReturnID)
		}
		return s.idem.Succeed(ctx, tx, "admin", adminID, route, key, out)
	})
	if err == nil && action == "acknowledged" {
		reportedAt, reportedErr := time.Parse(time.RFC3339, out.ReportedAt)
		acknowledgedAt, acknowledgedErr := time.Parse(time.RFC3339, out.AcknowledgedAt)
		if reportedErr == nil && acknowledgedErr == nil {
			s.metrics.observeAcknowledge(out.Type, acknowledgedAt.Sub(reportedAt))
		}
	}
	return out, err
}

// ResolveActiveLocked is called from an existing transaction after its
// delivery row has been locked. stage="" resolves all active incidents.
func (s *Service) ResolveActiveLocked(ctx context.Context, tx *gorm.DB, deliveryID uint64, stage, resolutionCode string) error {
	if !s.cfg.DeliveryIncident.Enabled || !s.cfg.DeliveryIncident.AutoResolveEnabled || tx == nil {
		return nil
	}
	rows, err := s.repo.ActiveForUpdate(ctx, tx, deliveryID, stage)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	for index := range rows {
		row := &rows[index]
		from := row.Status
		updated, err := s.repo.UpdateIncidentVersioned(ctx, tx, row.ID, row.Version, map[string]any{
			"status": StatusResolved, "resolved_at": now, "resolved_by": nil,
			"resolution_code": resolutionCode, "resolution_note": nil, "updated_at": now,
		})
		if err != nil {
			return err
		}
		if !updated {
			return versionConflict()
		}
		row.Status, row.Version, row.UpdatedAt, row.ResolvedAt = StatusResolved, row.Version+1, now, &now
		row.ResolutionCode = optional(resolutionCode)
		if err := s.writeHistoryAuditEvent(ctx, tx, row, "system", nil, "resolved", from, StatusResolved, resolutionCode, "", map[string]any{"natural_close": true}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) writeHistoryAuditEvent(ctx context.Context, tx *gorm.DB, row *Incident, actorType string, actorID *uint64, action, fromStatus, toStatus, reasonCode, remark string, extra map[string]any) error {
	requestID := requestctx.RequestID(ctx)
	if requestID == "" {
		requestID = "system"
	}
	history := History{ID: s.ids.Next(), IncidentID: row.ID, ActorType: actorType, ActorID: actorID, Action: action,
		ToStatus: toStatus, ReasonCode: optional(reasonCode), Remark: optional(remark), RequestID: requestID, CreatedAt: s.now().UTC()}
	if fromStatus != "" {
		history.FromStatus = optional(fromStatus)
	}
	if err := s.repo.CreateHistory(ctx, tx, history); err != nil {
		return err
	}
	auditActorID := uint64(0)
	if actorID != nil {
		auditActorID = *actorID
	}
	after := map[string]any{"type": row.Type, "stage": row.Stage, "status": toStatus, "version": row.Version}
	for key, value := range extra {
		after[key] = value
	}
	if reasonCode != "" {
		after["reason_code"] = reasonCode
	}
	if err := s.repo.CreateAudit(ctx, tx, AuditLog{ID: s.ids.Next(), ActorType: actorType, ActorID: auditActorID,
		Action: "incident." + auditAction(action, actorType), ResourceType: "delivery_incident", ResourceID: row.ID,
		BeforeData: jsonData(map[string]any{"status": fromStatus}), AfterData: jsonData(after), Result: "success",
		RequestID: requestctx.RequestIDPtr(ctx), IPHash: requestctx.IPHashPtr(ctx), UserAgent: requestctx.UserAgentPtr(ctx)}); err != nil {
		return err
	}
	payload := map[string]any{
		"incident_id": idString(row.ID), "incident_no": row.IncidentNo, "delivery_order_id": idString(row.DeliveryOrderID),
		"order_id": idString(row.OrderID), "shop_id": idString(row.ShopID), "type": row.Type, "stage": row.Stage,
		"from_status": fromStatus, "to_status": toStatus, "actor_type": actorType,
	}
	return s.repo.CreateOutbox(ctx, tx, OutboxEvent{ID: s.ids.Next(), EventID: uuid.NewString(), EventType: "delivery.incident." + eventAction(action),
		AggregateType: "delivery_incident", AggregateID: row.ID, Payload: jsonData(payload), Status: "pending", RequestID: requestctx.RequestIDPtr(ctx)})
}

func (s *Service) writeAccessAudit(ctx context.Context, actorType string, actorID uint64, action, resourceType string, resourceID uint64, result string, data map[string]any) error {
	return s.repo.CreateAudit(ctx, s.repo.DB(), AuditLog{ID: s.ids.Next(), ActorType: actorType, ActorID: actorID,
		Action: action, ResourceType: resourceType, ResourceID: resourceID, AfterData: jsonData(data), Result: result,
		RequestID: requestctx.RequestIDPtr(ctx), IPHash: requestctx.IPHashPtr(ctx), UserAgent: requestctx.UserAgentPtr(ctx)})
}

func (s *Service) AuditInvalidRequest(ctx context.Context, claims *auth.Claims, method, route, action, resourceType, resourceIDRaw string) {
	s.auditFailure(ctx, claims, strings.TrimSpace(method+" "+route), action, resourceType, resourceIDRaw,
		problem.InvalidArgument("VALIDATION_FAILED", "request validation failed"), nil)
}

// auditFailure runs after a rejected request's business transaction has rolled
// back. It is deliberately best-effort so audit storage trouble never changes
// a stable business error into an unrelated response or opens the write path.
func (s *Service) auditFailure(ctx context.Context, claims *auth.Claims, route, action, resourceType, resourceIDRaw string, requestErr error, data map[string]any) {
	if requestErr == nil || s == nil || s.repo == nil || s.repo.DB() == nil || s.ids == nil {
		return
	}
	result, errorCode := auditFailureResult(action, requestErr)
	actorType, actorID := coarseAuditActor(claims)
	resourceID, _ := parseID(resourceIDRaw)
	safe := map[string]any{"route": strings.TrimSpace(route), "error_code": errorCode}
	for key, value := range data {
		if value != nil && value != "" {
			safe[key] = value
		}
	}
	_ = s.writeAccessAudit(ctx, actorType, actorID, action, resourceType, resourceID, result, safe)
}

func auditFailureResult(action string, err error) (string, string) {
	var details *problem.Details
	if !errors.As(err, &details) {
		return "error", "INTERNAL_ERROR"
	}
	if action == "incident.evidence_add" && (details.ErrorCode == "DELIVERY_INCIDENT_EVIDENCE_INVALID" || details.ErrorCode == "DELIVERY_INCIDENT_EVIDENCE_SCAN_PENDING") {
		return "token_invalid", details.ErrorCode
	}
	switch details.Status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests:
		return "denied", details.ErrorCode
	case http.StatusConflict:
		return "conflict", details.ErrorCode
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusTooEarly:
		return "invalid", details.ErrorCode
	default:
		return "error", details.ErrorCode
	}
}

func coarseAuditActor(claims *auth.Claims) (string, uint64) {
	if claims == nil {
		return "unknown", 0
	}
	switch claims.AccountType {
	case "rider":
		id, _ := parseID(claims.RiderID)
		return "rider", id
	case "merchant":
		id, _ := parseID(claims.MerchantUserID)
		return "merchant", id
	case "admin":
		id, _ := parseID(claims.AdminUserID)
		return "admin", id
	default:
		return "unknown", 0
	}
}

func (s *Service) buildItems(ctx context.Context, tx *gorm.DB, orderID uint64, inputs []ItemInput) ([]Item, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	ids := make([]uint64, 0, len(inputs))
	quantities := make(map[uint64]uint, len(inputs))
	for _, input := range inputs {
		if input.OrderItemID == 0 || input.Quantity == 0 || quantities[input.OrderItemID] != 0 {
			return nil, itemInvalid("duplicate or invalid order item")
		}
		ids = append(ids, input.OrderItemID)
		quantities[input.OrderItemID] = input.Quantity
	}
	rows, err := s.repo.OrderItems(ctx, tx, orderID, ids)
	if err != nil {
		return nil, err
	}
	if len(rows) != len(inputs) {
		return nil, itemInvalid("order item does not belong to this order")
	}
	items := make([]Item, 0, len(rows))
	for _, row := range rows {
		quantity := quantities[row.ID]
		if quantity == 0 || quantity > uint(row.Quantity) {
			return nil, itemInvalid("incident quantity exceeds the order item quantity")
		}
		shopProductID, productID := row.ShopProductID, row.ProductID
		items = append(items, Item{OrderItemID: row.ID, ShopProductID: &shopProductID, ProductID: &productID, Quantity: quantity, ItemSnapshot: itemSnapshot(row.ProductSnapshot)})
	}
	return items, nil
}

func (s *Service) buildEvidence(riderID uint64, tokens []string) ([]Evidence, error) {
	if duplicateStrings(tokens) {
		return nil, evidenceInvalid("duplicate evidence token")
	}
	rows := make([]Evidence, 0, len(tokens))
	for _, raw := range tokens {
		if len(raw) < 20 || len(raw) > 8192 {
			return nil, evidenceInvalid("invalid or expired evidence token")
		}
		meta, err := evidencetoken.Verify(raw, evidencetoken.Policy{
			Secret: s.cfg.AfterSale.EvidenceTokenSecret, Issuer: "jxe-upload", Audience: "delivery-incident",
			Subject: "rider:" + idString(riderID), Purpose: "delivery_incident_evidence",
			AllowedMedia: map[string]evidencetoken.MediaRule{
				"image/jpeg": {MaxBytes: 20 << 20}, "image/png": {MaxBytes: 20 << 20}, "image/heic": {MaxBytes: 20 << 20},
			}, AllowedScanStatus: map[string]bool{"clean": true},
			ObjectKeyPrefixes: []string{"rider/" + idString(riderID) + "/", "riders/" + idString(riderID) + "/"},
			ClockSkew:         30 * time.Second, Now: s.now,
		})
		if errors.Is(err, evidencetoken.ErrScanPending) {
			pending := problem.New(http.StatusTooEarly, "DELIVERY_INCIDENT_EVIDENCE_SCAN_PENDING", "Too Early", "evidence security scan is pending")
			pending.Data = map[string]any{"retry_after_seconds": 10}
			return nil, pending
		}
		if err != nil {
			return nil, evidenceInvalid("invalid or expired evidence token")
		}
		rows = append(rows, Evidence{TokenID: meta.TokenID, ObjectKey: meta.ObjectKey, MimeType: meta.MimeType, SizeBytes: meta.SizeBytes, SHA256: meta.SHA256, ScanStatus: "clean"})
	}
	return rows, nil
}

func validateCreateShape(req CreateReq) error {
	if req.Type != TypeOutOfStock && req.Type != TypeAlcoholDamaged && req.Type != TypeCustomerRefused && req.Type != TypeCustomerUnreachable {
		return problem.InvalidArgument("VALIDATION_FAILED", "unsupported incident type")
	}
	if len(req.Items) > 50 {
		return itemInvalid("an incident can contain at most 50 order items")
	}
	if len(req.EvidenceTokens) > 9 {
		return evidenceInvalid("an incident can contain at most 9 evidence files")
	}
	if (req.Type == TypeOutOfStock || req.Type == TypeAlcoholDamaged) && len(req.Items) == 0 {
		return itemInvalid("this incident type requires at least one order item")
	}
	if len(req.ReasonCode) > 64 {
		return problem.InvalidArgument("VALIDATION_FAILED", "reason_code must not exceed 64 characters")
	}
	if req.Type == TypeCustomerRefused && strings.TrimSpace(req.ReasonCode) == "" {
		return problem.InvalidArgument("VALIDATION_FAILED", "customer refusal requires reason_code")
	}
	if duplicateStrings(req.EvidenceTokens) {
		return evidenceInvalid("duplicate evidence token")
	}
	if req.Location != nil && (req.Location.Longitude < -180 || req.Location.Longitude > 180 || req.Location.Latitude < -90 || req.Location.Latitude > 90 || req.Location.AccuracyM < 0 || req.Location.AccuracyM > 10000 || req.Location.CapturedAt.IsZero()) {
		return problem.InvalidArgument("VALIDATION_FAILED", "location is invalid")
	}
	return nil
}

func validateContactAttempts(incidentType string, attempts *ContactAttemptsInput, acceptedAt *time.Time, now time.Time) error {
	if incidentType != TypeCustomerUnreachable {
		return nil
	}
	if attempts == nil || attempts.Count < 2 || attempts.FirstAt.IsZero() || attempts.LastAt.IsZero() || attempts.LastAt.Sub(attempts.FirstAt) < 3*time.Minute {
		return contactInvalid("at least two contact attempts spanning three minutes are required")
	}
	if attempts.FirstAt.After(now.Add(5*time.Minute)) || attempts.LastAt.After(now.Add(5*time.Minute)) || attempts.FirstAt.Before(now.Add(-24*time.Hour)) {
		return contactInvalid("contact attempt times are outside the allowed window")
	}
	if acceptedAt != nil && attempts.FirstAt.Before(*acceptedAt) {
		return contactInvalid("contact attempts cannot predate delivery acceptance")
	}
	return nil
}

func validateTransition(status, action string) error {
	switch action {
	case "acknowledged":
		if status == StatusOpen {
			return nil
		}
	case "resolved":
		if status == StatusOpen || status == StatusAcknowledged {
			return nil
		}
	case "rejected":
		if isActive(status) {
			return nil
		}
	}
	return statusConflict("incident status does not allow this transition")
}

func (s *Service) writeEnabled() error {
	if !s.cfg.DeliveryIncident.Enabled {
		return problem.New(http.StatusServiceUnavailable, "DELIVERY_INCIDENT_DISABLED", "Service Unavailable", "delivery incident writes are disabled")
	}
	return nil
}

func (s *Service) readEnabled() error {
	if !s.cfg.DeliveryIncident.Enabled {
		return problem.New(http.StatusServiceUnavailable, "DELIVERY_INCIDENT_DISABLED", "Service Unavailable", "delivery incidents are disabled")
	}
	return nil
}

func stageFor(status string) (string, error) {
	switch status {
	case "accepted":
		return StagePickup, nil
	case "delivering":
		return StageDelivery, nil
	default:
		return "", errors.New("terminal or unsupported delivery status")
	}
}

func typeAllowedAtStage(incidentType, stage string) bool {
	if stage == StagePickup {
		return incidentType == TypeOutOfStock || incidentType == TypeAlcoholDamaged
	}
	return stage == StageDelivery && (incidentType == TypeAlcoholDamaged || incidentType == TypeCustomerRefused || incidentType == TypeCustomerUnreachable)
}

func priorityFor(incidentType string) string {
	if incidentType == TypeAlcoholDamaged {
		return "urgent"
	}
	return "high"
}

func isActive(status string) bool {
	return status == StatusEvidenceRequired || status == StatusOpen || status == StatusAcknowledged
}

func cleanText(value string, required bool) (string, error) {
	value = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value))
	length := utf8.RuneCountInString(value)
	if (required && length == 0) || length > 1000 {
		return "", problem.InvalidArgument("VALIDATION_FAILED", "text must contain between 1 and 1000 characters")
	}
	return value, nil
}

func validateIdempotencyKey(key string) error {
	if len(key) < 8 || len(key) > 64 {
		return problem.InvalidArgument("IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must contain 8 to 64 visible ASCII characters")
	}
	for index := 0; index < len(key); index++ {
		if key[index] < 33 || key[index] > 126 {
			return problem.InvalidArgument("IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must contain 8 to 64 visible ASCII characters")
		}
	}
	return nil
}

func riderActor(claims *auth.Claims, permission string) (uint64, error) {
	if claims == nil || claims.AccountType != "rider" || !hasPermission(claims.Permissions, permission) {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "rider permission denied")
	}
	id, err := parseID(claims.RiderID)
	if err != nil {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "invalid rider identity")
	}
	return id, nil
}

func adminActor(claims *auth.Claims, permission string) (uint64, error) {
	if claims == nil || claims.AccountType != "admin" || !hasPermission(claims.Permissions, permission) {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "admin permission denied")
	}
	id, err := parseID(claims.AdminUserID)
	if err != nil {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "invalid admin identity")
	}
	return id, nil
}

func storeActor(claims *auth.Claims, permission string) (uint64, []uint64, error) {
	if claims == nil || claims.AccountType != "merchant" || !hasPermission(claims.Permissions, permission) {
		return 0, nil, problem.Forbidden("PERM_FORBIDDEN", "merchant permission denied")
	}
	actor, err := parseID(claims.MerchantUserID)
	if err != nil {
		return 0, nil, problem.Forbidden("PERM_FORBIDDEN", "invalid merchant identity")
	}
	shops := make([]uint64, 0, len(claims.AuthorizedShopIDs))
	for _, raw := range claims.AuthorizedShopIDs {
		id, err := parseID(raw)
		if err != nil {
			return 0, nil, problem.Forbidden("PERM_FORBIDDEN", "invalid shop scope")
		}
		shops = append(shops, id)
	}
	if len(shops) == 0 {
		return 0, nil, problem.Forbidden("PERM_FORBIDDEN", "no authorized shops")
	}
	return actor, shops, nil
}

func hasPermission(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func allowedID(values []string, id uint64) bool {
	if len(values) == 0 {
		return false
	}
	if containsFullRollout(values) {
		return true
	}
	want := idString(id)
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsFullRollout(values []string) bool {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "*" || strings.EqualFold(value, "all") {
			return true
		}
	}
	return false
}

func rolloutShops(authorized []uint64, rollout []string) []uint64 {
	if len(rollout) == 0 {
		return nil
	}
	out := make([]uint64, 0, len(authorized))
	for _, id := range authorized {
		if allowedID(rollout, id) {
			out = append(out, id)
		}
	}
	return out
}

func pageRows(rows []Incident, query pagination.Query) ([]DTO, string, error) {
	next := ""
	if len(rows) > query.PageSize {
		next = pagination.NextPageToken(query)
		rows = rows[:query.PageSize]
	}
	out := make([]DTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, aggregateDTO(Aggregate{Incident: row}, false))
	}
	return out, next, nil
}

func aggregateDTO(value Aggregate, evidenceViewAvailable bool) DTO {
	r := value.Incident
	out := DTO{ID: idString(r.ID), IncidentNo: r.IncidentNo, DeliveryOrderID: idString(r.DeliveryOrderID), OrderID: idString(r.OrderID),
		ShopID: idString(r.ShopID), RiderID: idString(r.RiderID), Type: r.Type, Stage: r.Stage, Status: r.Status, Priority: r.Priority,
		Description: r.Description, DeliveryStatusSnapshot: r.DeliveryStatusSnapshot, AssignmentVersionSnapshot: r.AssignmentVersionSnapshot,
		ContactAttemptCount: r.ContactAttemptCount, DistanceToDestinationM: r.DistanceToDestinationM, LocationAccuracyM: r.LocationAccuracyM,
		Version: r.Version, ReportedAt: timeString(r.ReportedAt), CreatedAt: timeString(r.CreatedAt), UpdatedAt: timeString(r.UpdatedAt)}
	out.ReasonCode = valueOf(r.ReasonCode)
	out.FirstContactAt, out.LastContactAt, out.LocationCapturedAt = timePtrString(r.FirstContactAt), timePtrString(r.LastContactAt), timePtrString(r.LocationCapturedAt)
	out.AcknowledgedBy, out.AcknowledgedAt = idPtrString(r.AcknowledgedBy), timePtrString(r.AcknowledgedAt)
	out.ResolvedBy, out.ResolvedAt, out.ResolutionCode, out.ResolutionNote = idPtrString(r.ResolvedBy), timePtrString(r.ResolvedAt), valueOf(r.ResolutionCode), valueOf(r.ResolutionNote)
	out.RejectedBy, out.RejectedAt, out.RejectionCode, out.RejectionReason = idPtrString(r.RejectedBy), timePtrString(r.RejectedAt), valueOf(r.RejectionCode), valueOf(r.RejectionReason)
	for _, item := range value.Items {
		out.Items = append(out.Items, ItemDTO{ID: idString(item.ID), OrderItemID: idString(item.OrderItemID), Quantity: item.Quantity, ItemSnapshot: jsonMap(item.ItemSnapshot)})
	}
	for _, evidence := range value.Evidence {
		suffix := evidence.SHA256
		if len(suffix) > 8 {
			suffix = suffix[len(suffix)-8:]
		}
		out.Evidence = append(out.Evidence, EvidenceDTO{ID: idString(evidence.ID), MimeType: evidence.MimeType, SizeBytes: evidence.SizeBytes,
			SHA256Suffix: suffix, ScanStatus: evidence.ScanStatus, ViewAvailable: evidenceViewAvailable && evidence.ScanStatus == "clean", CreatedAt: timeString(evidence.CreatedAt)})
	}
	for _, history := range value.History {
		out.History = append(out.History, HistoryDTO{ID: idString(history.ID), ActorType: history.ActorType, ActorID: idPtrString(history.ActorID),
			Action: history.Action, FromStatus: valueOf(history.FromStatus), ToStatus: history.ToStatus, ReasonCode: valueOf(history.ReasonCode),
			Remark: valueOf(history.Remark), CreatedAt: timeString(history.CreatedAt)})
	}
	return out
}

func (s *Service) aggregateDTO(value Aggregate) DTO {
	available := s.views != nil && s.views.Available()
	return aggregateDTO(value, available)
}

type locationSummary struct {
	distance                 *uint
	accuracy                 *float64
	capturedAt               *time.Time
	distanceSuppressedReason string
}

func summarizeLocation(input *LocationInput, recipient datatypes.JSON, now time.Time) locationSummary {
	if input == nil || input.CapturedAt.After(now.Add(5*time.Minute)) || input.CapturedAt.Before(now.Add(-24*time.Hour)) {
		return locationSummary{}
	}
	accuracy, captured := input.AccuracyM, input.CapturedAt.UTC()
	summary := locationSummary{accuracy: &accuracy, capturedAt: &captured}
	if input.AccuracyM > 1000 {
		summary.distanceSuppressedReason = "accuracy_gt_1000m"
		return summary
	}
	var destination struct {
		Latitude  *float64 `json:"latitude"`
		Longitude *float64 `json:"longitude"`
	}
	if len(recipient) == 0 || json.Unmarshal(recipient, &destination) != nil || destination.Latitude == nil || destination.Longitude == nil {
		return summary
	}
	distance := uint(math.Round(haversineM(input.Latitude, input.Longitude, *destination.Latitude, *destination.Longitude)))
	summary.distance = &distance
	return summary
}

func haversineM(lat1, lng1, lat2, lng2 float64) float64 {
	const radius = 6371000.0
	toRad := math.Pi / 180
	dLat, dLng := (lat2-lat1)*toRad, (lng2-lng1)*toRad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*toRad)*math.Cos(lat2*toRad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return radius * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

func itemSnapshot(raw datatypes.JSON) datatypes.JSON {
	var source map[string]any
	_ = json.Unmarshal(raw, &source)
	result := map[string]any{"schema_version": 1}
	for _, key := range []string{"name", "product_name", "spec", "unit"} {
		if value, ok := source[key]; ok {
			result[key] = value
		}
	}
	return jsonData(result)
}

func (s *Service) cached(ctx context.Context, tx *gorm.DB, actorType string, actorID uint64, route, key string, target any) error {
	ok, err := s.idem.CachedResponse(ctx, tx, actorType, actorID, route, key, target)
	if err != nil {
		return err
	}
	if !ok {
		return problem.Conflict("IDEMPOTENCY_IN_PROGRESS", "idempotency request is still processing")
	}
	return nil
}

func normalizeIdempotencyError(err error) error {
	var details *problem.Details
	if errors.As(err, &details) && details.ErrorCode == "IDEMPOTENCY_CONFLICT" {
		copy := *details
		copy.ErrorCode = "IDEMPOTENCY_KEY_REUSED"
		copy.Type = "https://api.jiuxiaoer.com/problems/IDEMPOTENCY_KEY_REUSED"
		return &copy
	}
	return err
}

func itemInvalid(detail string) *problem.Details {
	return problem.New(http.StatusUnprocessableEntity, "DELIVERY_INCIDENT_ITEM_INVALID", "Unprocessable Entity", detail)
}

func evidenceInvalid(detail string) *problem.Details {
	return problem.New(http.StatusUnprocessableEntity, "DELIVERY_INCIDENT_EVIDENCE_INVALID", "Unprocessable Entity", detail)
}

func contactInvalid(detail string) *problem.Details {
	return problem.New(http.StatusUnprocessableEntity, "DELIVERY_INCIDENT_CONTACT_ATTEMPTS_INVALID", "Unprocessable Entity", detail)
}

func statusConflict(detail string) *problem.Details {
	return problem.Conflict("DELIVERY_INCIDENT_STATUS_CONFLICT", detail)
}

func versionConflict() *problem.Details {
	return problem.Conflict("DELIVERY_INCIDENT_VERSION_CONFLICT", "incident version changed")
}

func incidentNotFound() *problem.Details {
	return problem.NotFound("DELIVERY_INCIDENT_NOT_FOUND", "delivery incident not found")
}

func (s *Service) checkWriteRate(ctx context.Context, action string, riderID uint64) error {
	actorLimit, ipLimit, detail := s.cfg.DeliveryIncident.CreateRatePerHour, s.cfg.DeliveryIncident.CreateIPRatePerHour, "incident create rate limit exceeded"
	if action == "evidence" {
		actorLimit, ipLimit, detail = s.cfg.DeliveryIncident.EvidenceRatePerHour, s.cfg.DeliveryIncident.EvidenceIPRatePerHour, "incident evidence rate limit exceeded"
	}
	actorKey := "rate:delivery_incident:" + action + ":rider:" + idString(riderID)
	actorResult := s.limiter.Allow(ctx, actorKey, time.Hour, int64(actorLimit))
	if actorResult.Degraded {
		s.metrics.incRateLimiterDegraded(action + "_rider")
	}
	if !actorResult.Allowed {
		s.metrics.incRateLimited(action + "_rider")
		return rateLimited(detail, actorResult.RetryAfter)
	}
	if ip := strings.TrimSpace(requestctx.IP(ctx)); ip != "" {
		ipHash := securevalue.HMAC(s.cfg.JWT.AccessSecret, "delivery_incident_rate_ip", ip)
		ipResult := s.limiter.Allow(ctx, "rate:delivery_incident:"+action+":ip:"+ipHash, time.Hour, int64(ipLimit))
		if ipResult.Degraded {
			s.metrics.incRateLimiterDegraded(action + "_ip")
		}
		if !ipResult.Allowed {
			s.metrics.incRateLimited(action + "_ip")
			return rateLimited(detail, ipResult.RetryAfter)
		}
	}
	return nil
}

func rateLimited(detail string, retryAfter time.Duration) *problem.Details {
	err := problem.TooManyRequests("DELIVERY_INCIDENT_RATE_LIMITED", detail)
	seconds := int(math.Ceil(retryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	err.Data = map[string]any{"retry_after_seconds": seconds}
	return err
}

func evidenceWriteError(err error) error {
	if isDuplicate(err) {
		return evidenceInvalid("evidence token has already been consumed")
	}
	return err
}

func isDuplicate(err error) bool {
	var mysqlErr *mysqlDriver.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 || strings.Contains(strings.ToLower(errString(err)), "unique constraint") || strings.Contains(strings.ToLower(errString(err)), "duplicate entry")
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func duplicateStrings(values []string) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if seen[value] {
			return true
		}
		seen[value] = true
	}
	return false
}

func eventAction(action string) string {
	if action == "reported" || action == "evidence_added" || action == "acknowledged" || action == "resolved" || action == "rejected" {
		return action
	}
	return "updated"
}

func auditAction(action, actorType string) string {
	switch action {
	case "reported":
		return "report"
	case "evidence_added":
		return "evidence_add"
	case "acknowledged":
		return "acknowledge"
	case "resolved":
		if actorType == "system" {
			return "auto_resolve"
		}
		return "resolve"
	case "rejected":
		return "reject"
	}
	return action
}

func jsonData(value any) datatypes.JSON {
	payload, _ := json.Marshal(value)
	return datatypes.JSON(payload)
}

func jsonMap(value datatypes.JSON) map[string]any {
	var result map[string]any
	_ = json.Unmarshal(value, &result)
	return result
}

func parseID(value string) (uint64, error) {
	id, err := strconv.ParseUint(value, 10, 64)
	if err != nil || id == 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

func idString(value uint64) string { return strconv.FormatUint(value, 10) }

func idPtrString(value *uint64) string {
	if value == nil {
		return ""
	}
	return idString(*value)
}

func optional(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	copy := strings.TrimSpace(value)
	return &copy
}

func valueOf(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func timeString(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func timePtrString(value *time.Time) string {
	if value == nil {
		return ""
	}
	return timeString(*value)
}
