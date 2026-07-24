package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
)

// ListOffers 查询Offers列表。
func (s *Service) ListOffers(ctx context.Context, claims *auth.Claims, query pagination.Query) ([]OfferDTO, string, error) {
	riderID, err := riderActor(claims, "delivery_offer:list")
	if err != nil {
		return nil, "", err
	}
	type offerView struct {
		OfferID             uint64
		DeliveryOrderID     uint64
		AssignmentVersion   uint
		ShopID              uint64
		ShopName            string
		DestinationDistrict string
		DistanceM           *uint
		ItemCount           int
		ExpiresAt           time.Time
		Version             uint
		PickupReadyStatus   string
	}
	var rows []offerView
	err = s.db.WithContext(ctx).Table("dispatch_offers o").Select(`
		o.id AS offer_id,o.delivery_order_id,d.assignment_version,d.shop_id,s.name AS shop_name,
		COALESCE(JSON_UNQUOTE(JSON_EXTRACT(d.recipient_snapshot,'$.district')),'') AS destination_district,
		c.distance_m,(SELECT COALESCE(SUM(quantity),0) FROM order_items oi WHERE oi.order_id=d.order_id AND oi.deleted_at IS NULL) AS item_count,
		o.expires_at,o.version,d.pickup_ready_status`).
		Joins("JOIN delivery_orders d ON d.id=o.delivery_order_id AND d.deleted_at IS NULL").
		Joins("JOIN shops s ON s.id=d.shop_id AND s.deleted_at IS NULL").
		Joins("LEFT JOIN dispatch_candidates c ON c.id=o.candidate_id").
		Where("o.rider_id=? AND o.status='pending' AND o.expires_at>?", riderID, time.Now()).
		Order("o.expires_at,o.id").Offset(query.Offset).Limit(query.PageSize + 1).Scan(&rows).Error
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		next = pagination.NextPageToken(query)
		rows = rows[:query.PageSize]
	}
	items := make([]OfferDTO, 0, len(rows))
	for _, row := range rows {
		distance := uint(0)
		if row.DistanceM != nil {
			distance = *row.DistanceM
		}
		items = append(items, OfferDTO{
			ID: idString(row.OfferID), DeliveryOrderID: idString(row.DeliveryOrderID), AssignmentVersion: row.AssignmentVersion,
			ShopID: idString(row.ShopID), ShopName: row.ShopName, DestinationDistrict: row.DestinationDistrict,
			DistanceM: distance, ItemCount: row.ItemCount, ExpiresAt: row.ExpiresAt.Format(time.RFC3339Nano),
			Version: row.Version, SoundKey: "new_delivery_offer", PickupReadyStatus: row.PickupReadyStatus,
		})
	}
	return items, next, nil
}

// AcceptOffer 接受并处理Offer。
func (s *Service) AcceptOffer(ctx context.Context, claims *auth.Claims, method, path, key, offerRaw string, req OfferActionReq) (AssignmentResult, error) {
	riderID, err := riderActor(claims, "delivery_offer:accept")
	if err != nil {
		return AssignmentResult{}, err
	}
	offerID, err := parseID(offerRaw)
	if err != nil {
		return AssignmentResult{}, problem.NotFound("DISPATCH_OFFER_NOT_FOUND", "dispatch offer not found")
	}
	var offer Offer
	if err := s.db.WithContext(ctx).Where("id=? AND rider_id=?", offerID, riderID).First(&offer).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return AssignmentResult{}, problem.NotFound("DISPATCH_OFFER_NOT_FOUND", "dispatch offer not found")
	} else if err != nil {
		return AssignmentResult{}, err
	}
	return s.CommitAssignment(ctx, method, path, key, CommitInput{
		Source: "auto_offer", DeliveryOrderID: offer.DeliveryOrderID, RiderID: riderID, OfferID: offerID,
		ExpectedOfferVersion: req.ExpectedOfferVersion, ExpectedAssignmentVersion: req.ExpectedAssignmentVersion,
		ActorType: "rider", ActorID: riderID,
	})
}

// RejectOffer 拒绝Offer。
func (s *Service) RejectOffer(ctx context.Context, claims *auth.Claims, method, path, key, offerRaw string, req OfferRejectReq) (map[string]any, error) {
	riderID, err := riderActor(claims, "delivery_offer:reject")
	if err != nil {
		return nil, err
	}
	offerID, err := parseID(offerRaw)
	if err != nil {
		return nil, problem.NotFound("DISPATCH_OFFER_NOT_FOUND", "dispatch offer not found")
	}
	var out map[string]any
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), "rider", riderID, method, path, key, idempotency.RequestHash(map[string]any{"offer_id": offerID, "request": req}))
		if err != nil {
			return err
		}
		if !started {
			cached, err := s.idem.CachedResponse(ctx, tx, "rider", riderID, path, key, &out)
			if err != nil || cached {
				return err
			}
			return problem.Conflict("IDEMPOTENCY_CONFLICT", "idempotency response missing")
		}
		if allowed, rateErr := s.allowFixedWindow(ctx, idempotencyRateKey("dispatch:rider:offer-actions", riderID), time.Minute, 30); rateErr == nil && !allowed {
			return rateLimited("offer actions are limited to thirty requests per minute", time.Minute)
		}
		var probe Offer
		if err := tx.Select("id,job_id").Where("id=? AND rider_id=?", offerID, riderID).First(&probe).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("DISPATCH_OFFER_NOT_FOUND", "dispatch offer not found")
		} else if err != nil {
			return err
		}
		var job Job
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&job, probe.JobID).Error; err != nil {
			return problem.Conflict("DISPATCH_OFFER_NOT_ACTIVE", "dispatch offer is not active")
		}
		var offer Offer
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=? AND rider_id=?", offerID, riderID).First(&offer).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("DISPATCH_OFFER_NOT_FOUND", "dispatch offer not found")
		} else if err != nil {
			return err
		}
		if offer.Status != "pending" {
			return problem.Conflict("DISPATCH_OFFER_NOT_ACTIVE", "dispatch offer is not active")
		}
		if job.Status != "offering" {
			return problem.Conflict("DISPATCH_OFFER_NOT_ACTIVE", "dispatch offer is not active")
		}
		if offer.Version != req.ExpectedOfferVersion {
			return problem.Conflict("VERSION_CONFLICT", "dispatch offer version changed")
		}
		now := time.Now()
		if !offer.ExpiresAt.After(now) {
			return problem.Conflict("DISPATCH_OFFER_EXPIRED", "dispatch offer has expired")
		}
		if err := tx.Model(&Offer{}).Where("id=? AND status='pending' AND version=?", offer.ID, offer.Version).Updates(map[string]any{"status": "rejected", "responded_at": now, "rejection_reason_code": req.ReasonCode, "rejection_remark": req.Remark, "request_id": requestctx.RequestIDPtr(ctx), "version": gorm.Expr("version+1")}).Error; err != nil {
			return err
		}
		if err := tx.Model(&Job{}).Where("id=? AND status='offering'", offer.JobID).Updates(map[string]any{"next_action_at": now, "locked_by": nil, "locked_until": nil, "version": gorm.Expr("version+1")}).Error; err != nil {
			return err
		}
		if err := s.createEvent(ctx, tx, "dispatch.offer.rejected", "dispatch_offer", offer.ID, map[string]any{"offer_id": idString(offer.ID), "delivery_order_id": idString(offer.DeliveryOrderID), "rider_id": idString(riderID), "reason_code": req.ReasonCode}); err != nil {
			return err
		}
		if err := s.createAudit(ctx, tx, "rider", riderID, "dispatch.offer.reject", "dispatch_offer", offer.ID, offer, map[string]any{"status": "rejected", "reason_code": req.ReasonCode, "version": offer.Version + 1}); err != nil {
			return err
		}
		out = map[string]any{"offer_id": idString(offer.ID), "status": "rejected", "version": offer.Version + 1}
		return s.idem.Succeed(ctx, tx, "rider", riderID, path, key, out)
	})
	return out, err
}

// Grab 返回Grab。
func (s *Service) Grab(ctx context.Context, claims *auth.Claims, method, path, key, deliveryRaw string, expectedVersion uint) (AssignmentResult, error) {
	riderID, err := riderActor(claims, "delivery:accept")
	if err != nil {
		return AssignmentResult{}, err
	}
	deliveryID, err := parseID(deliveryRaw)
	if err != nil {
		return AssignmentResult{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid delivery order id")
	}
	return s.CommitAssignment(ctx, method, path, key, CommitInput{Source: "grab", DeliveryOrderID: deliveryID, RiderID: riderID, ExpectedAssignmentVersion: expectedVersion, ActorType: "rider", ActorID: riderID})
}

// ListJobs 查询Jobs列表。
func (s *Service) ListJobs(ctx context.Context, claims *auth.Claims, query pagination.Query) ([]JobDTO, string, error) {
	if _, err := adminActor(claims, "dispatch_job:list"); err != nil {
		return nil, "", err
	}
	db := s.db.WithContext(ctx)
	var err error
	db, err = pagination.ApplyFilter(db, query.Filter, map[string]string{"status": "status", "mode": "mode", "shop_id": "shop_id", "policy_version": "policy_version", "created_at": "created_at", "next_action_at": "next_action_at"})
	if err != nil {
		return nil, "", err
	}
	db, err = pagination.ApplyOrder(db, query.OrderBy, map[string]string{"created_at": "created_at", "next_action_at": "next_action_at", "assigned_at": "assigned_at", "id": "id"}, "created_at DESC,id DESC")
	if err != nil {
		return nil, "", err
	}
	var rows []Job
	if err := db.Offset(query.Offset).Limit(query.PageSize + 1).Find(&rows).Error; err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		next = pagination.NextPageToken(query)
		rows = rows[:query.PageSize]
	}
	items := make([]JobDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, jobDTO(row))
	}
	return items, next, nil
}

// JobDetail 返回任务 Detail。
func (s *Service) JobDetail(ctx context.Context, claims *auth.Claims, idRaw string) (JobDetailDTO, error) {
	if _, err := adminActor(claims, "dispatch_job:view"); err != nil {
		return JobDetailDTO{}, err
	}
	id, err := parseID(idRaw)
	if err != nil {
		return JobDetailDTO{}, problem.NotFound("DISPATCH_JOB_NOT_FOUND", "dispatch job not found")
	}
	var job Job
	if err := s.db.WithContext(ctx).First(&job, id).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return JobDetailDTO{}, problem.NotFound("DISPATCH_JOB_NOT_FOUND", "dispatch job not found")
	} else if err != nil {
		return JobDetailDTO{}, err
	}
	var candidates []Candidate
	var offers []Offer
	var assignments []Assignment
	if err := s.db.WithContext(ctx).Where("job_id=?", id).Order("rank_no").Find(&candidates).Error; err != nil {
		return JobDetailDTO{}, err
	}
	if err := s.db.WithContext(ctx).Where("job_id=?", id).Order("created_at").Find(&offers).Error; err != nil {
		return JobDetailDTO{}, err
	}
	if err := s.db.WithContext(ctx).Where("dispatch_job_id=?", id).Order("created_at").Find(&assignments).Error; err != nil {
		return JobDetailDTO{}, err
	}
	candidateDTOs := make([]CandidateDTO, 0, len(candidates))
	for _, candidate := range candidates {
		candidateDTOs = append(candidateDTOs, candidateDTO(candidate))
	}
	offerDTOs := make([]OfferTimelineDTO, 0, len(offers))
	for _, offer := range offers {
		offerDTOs = append(offerDTOs, offerTimelineDTO(offer))
	}
	assignmentDTOs := make([]AssignmentTimelineDTO, 0, len(assignments))
	for _, assignment := range assignments {
		assignmentDTOs = append(assignmentDTOs, assignmentTimelineDTO(assignment))
	}
	return JobDetailDTO{Job: jobDTO(job), Candidates: candidateDTOs, Offers: offerDTOs, Assignments: assignmentDTOs}, nil
}

// RetryJob 重试任务。
func (s *Service) RetryJob(ctx context.Context, claims *auth.Claims, method, path, key, idRaw string, req JobRetryReq) (JobDTO, error) {
	actor, err := adminActor(claims, "dispatch_job:retry")
	if err != nil {
		return JobDTO{}, err
	}
	id, err := parseID(idRaw)
	if err != nil {
		return JobDTO{}, problem.NotFound("DISPATCH_JOB_NOT_FOUND", "dispatch job not found")
	}
	var out JobDTO
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), "admin", actor, method, path, key, idempotency.RequestHash(map[string]any{"job_id": id, "request": req}))
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
		if allowed, rateErr := s.allowFixedWindow(ctx, idempotencyRateKey("dispatch:admin:job-retries", actor), time.Minute, 30); rateErr == nil && !allowed {
			return rateLimited("job retries are limited to thirty requests per minute", time.Minute)
		}
		if allowed, rateErr := s.allowFixedWindow(ctx, idempotencyRateKey("dispatch:job:retry", id), time.Minute, 1); rateErr == nil && !allowed {
			return rateLimited("this dispatch job can only be retried once per minute", time.Minute)
		}
		var probe Job
		if err := tx.Select("id,delivery_order_id").First(&probe, id).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("DISPATCH_JOB_NOT_FOUND", "dispatch job not found")
		} else if err != nil {
			return err
		}
		var delivery DeliveryOrder
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&delivery, probe.DeliveryOrderID).Error; err != nil {
			return err
		}
		var old Job
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&old, id).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("DISPATCH_JOB_NOT_FOUND", "dispatch job not found")
		} else if err != nil {
			return err
		}
		if old.Version != req.ExpectedVersion || old.Status != "manual_required" {
			return problem.Conflict("DISPATCH_JOB_NOT_ACTIONABLE", "dispatch job is not retryable")
		}
		if delivery.Status != "pending_assign" || delivery.RiderID != nil {
			return problem.Conflict("DELIVERY_ALREADY_ASSIGNED", "delivery order has already been assigned")
		}
		now := time.Now()
		if err := tx.Model(&Job{}).Where("id=? AND version=?", old.ID, old.Version).Updates(map[string]any{"status": "cancelled", "status_reason_code": "RETRY_SUPERSEDED", "status_reason_safe": req.Reason, "version": gorm.Expr("version+1")}).Error; err != nil {
			return err
		}
		newID := s.ids.Next()
		job := Job{ID: newID, JobNo: fmt.Sprintf("DJ%d", newID), DeliveryOrderID: old.DeliveryOrderID, OrderID: old.OrderID, ShopID: old.ShopID, DispatchSeq: old.DispatchSeq + 1, PolicyID: old.PolicyID, PolicyVersion: old.PolicyVersion, PolicySnapshot: old.PolicySnapshot, Mode: old.Mode, Status: "pending", NextActionAt: now, Version: 1}
		if err := tx.Create(&job).Error; err != nil {
			return err
		}
		if err := tx.Model(&DeliveryOrder{}).Where("id=?", delivery.ID).Updates(map[string]any{"dispatch_status": "pending", "current_dispatch_job_id": job.ID}).Error; err != nil {
			return err
		}
		if err := s.createEvent(ctx, tx, "dispatch.job.retry_requested", "dispatch_job", job.ID, map[string]any{"dispatch_job_id": idString(job.ID), "delivery_order_id": idString(delivery.ID), "reason_code": req.ReasonCode}); err != nil {
			return err
		}
		if err := s.createEvent(ctx, tx, "dispatch.job.ready", "dispatch_job", job.ID, map[string]any{"dispatch_job_id": idString(job.ID), "delivery_order_id": idString(delivery.ID)}); err != nil {
			return err
		}
		if err := s.createAudit(ctx, tx, "admin", actor, "dispatch.job.retry", "dispatch_job", old.ID, old, map[string]any{"new_job_id": idString(job.ID), "reason_code": req.ReasonCode}); err != nil {
			return err
		}
		out = jobDTO(job)
		return s.idem.Succeed(ctx, tx, "admin", actor, path, key, out)
	})
	return out, err
}

// jobDTO 返回任务DTO。
func jobDTO(row Job) JobDTO {
	assigned := ""
	if row.AssignedRiderID != nil {
		assigned = idString(*row.AssignedRiderID)
	}
	return JobDTO{
		ID: idString(row.ID), JobNo: row.JobNo, DeliveryOrderID: idString(row.DeliveryOrderID), OrderID: idString(row.OrderID), ShopID: idString(row.ShopID),
		DispatchSeq: row.DispatchSeq, PolicyVersion: row.PolicyVersion, Mode: row.Mode, Status: row.Status,
		RoundNo: row.RoundNo, CandidateCursor: row.CandidateCursor, NextActionAt: row.NextActionAt.Format(time.RFC3339Nano),
		GrabExpiresAt: timeValue(row.GrabExpiresAt), AssignedRiderID: assigned, Version: row.Version,
		CreatedAt: row.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: row.UpdatedAt.Format(time.RFC3339Nano),
	}
}

// candidateDTO 返回候选骑手 DTO。
func candidateDTO(row Candidate) CandidateDTO {
	exclusions := make([]string, 0)
	_ = json.Unmarshal(row.ExclusionCodes, &exclusions)
	return CandidateDTO{
		ID: idString(row.ID), RiderID: idString(row.RiderID), RankNo: row.RankNo,
		Eligible: row.Eligible, ExclusionCodes: exclusions, DistanceM: row.DistanceM,
		ActiveOrders: row.ActiveOrders, MaxActiveOrders: row.MaxActiveOrders,
		HeartbeatAgeSeconds: row.HeartbeatAgeSeconds, LocationAgeSeconds: row.LocationAgeSeconds,
		DistanceScore: row.DistanceScore, LoadScore: row.LoadScore, IdleScore: row.IdleScore,
		FreshnessScore: row.FreshnessScore, RejectionPenalty: row.RejectionPenalty,
		FinalScore: row.FinalScore, ScoreVersion: row.ScoreVersion, CreatedAt: row.CreatedAt,
	}
}

// offerTimelineDTO 返回派单时间线 DTO。
func offerTimelineDTO(row Offer) OfferTimelineDTO {
	return OfferTimelineDTO{
		ID: idString(row.ID), DeliveryOrderID: idString(row.DeliveryOrderID), RiderID: idString(row.RiderID),
		RoundNo: row.RoundNo, CandidateID: idString(row.CandidateID), Status: row.Status,
		ExpiresAt: row.ExpiresAt.Format(time.RFC3339Nano), RespondedAt: timeValue(row.RespondedAt), Version: row.Version,
	}
}

// assignmentTimelineDTO 返回分配时间线 DTO。
func assignmentTimelineDTO(row Assignment) AssignmentTimelineDTO {
	return AssignmentTimelineDTO{
		ID: idString(row.ID), DeliveryOrderID: idString(row.DeliveryOrderID), DispatchJobID: idValue(row.DispatchJobID),
		OfferID: idValue(row.OfferID), FromRiderID: idValue(row.FromRiderID), ToRiderID: idString(row.ToRiderID),
		AssignmentType: row.AssignmentType, Status: row.Status, ReasonCode: stringValue(row.ReasonCode),
		Reason: stringValue(row.Reason), ActorType: row.ActorType, ActorID: idString(row.ActorID),
		VersionBefore: row.VersionBefore, VersionAfter: row.VersionAfter, CreatedAt: row.CreatedAt.Format(time.RFC3339Nano),
	}
}

// idValue 返回ID值。
func idValue(value *uint64) string {
	if value == nil {
		return ""
	}
	return idString(*value)
}
