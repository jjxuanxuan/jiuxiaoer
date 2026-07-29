package dispatch

import (
	"context"
	"errors"
	"sort"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/modules/deliveryverification"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
)

type CommitInput struct {
	Source                    string
	DeliveryOrderID           uint64
	RiderID                   uint64
	OfferID                   uint64
	ExpectedOfferVersion      uint
	ExpectedAssignmentVersion uint
	ActorType                 string
	ActorID                   uint64
	ReasonCode                string
	Reason                    string
}

// CommitAssignment 是自动报价、公共抓取,
// 手动分配和拾取前重新分配
func (s *Service) CommitAssignment(ctx context.Context, method, path, key string, input CommitInput) (AssignmentResult, error) {
	var out AssignmentResult
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), input.ActorType, input.ActorID, method, path, key, idempotency.RequestHash(input))
		if err != nil {
			return err
		}
		if !started {
			cached, err := s.idem.CachedResponse(ctx, tx, input.ActorType, input.ActorID, path, key, &out)
			if err != nil {
				return err
			}
			if cached {
				return nil
			}
			return problem.Conflict("IDEMPOTENCY_CONFLICT", "idempotency response missing")
		}
		switch input.Source {
		case "auto_offer":
			if allowed, rateErr := s.allowFixedWindow(ctx, idempotencyRateKey("dispatch:rider:offer-actions", input.ActorID), time.Minute, 30); rateErr == nil && !allowed {
				return rateLimited("offer actions are limited to thirty requests per minute", time.Minute)
			}
		case "grab":
			if allowed, rateErr := s.allowFixedWindow(ctx, idempotencyRateKey("dispatch:rider:grab-actions", input.ActorID), time.Minute, 30); rateErr == nil && !allowed {
				return rateLimited("grab attempts are limited to thirty requests per minute", time.Minute)
			}
			if allowed, rateErr := s.allowFixedWindow(ctx, idempotencyRateKey("dispatch:delivery:grab-actions", input.DeliveryOrderID), time.Second, 100); rateErr == nil && !allowed {
				return rateLimited("this delivery order is receiving too many grab attempts", time.Second)
			}
		case "manual", "reassign":
			if allowed, rateErr := s.allowFixedWindow(ctx, idempotencyRateKey("dispatch:admin:assignment-actions", input.ActorID), time.Minute, 30); rateErr == nil && !allowed {
				return rateLimited("manual assignment actions are limited to thirty requests per minute", time.Minute)
			}
		}

		// 在获取锁之前阅读旧所有者只是为了建立稳定
		// 骑手锁定命令。在 FOR UPDATE 锁定后重新检查交付.
		var ownerProbe struct{ RiderID *uint64 }
		if err := tx.Table("delivery_orders").Select("rider_id").Where("id=?", input.DeliveryOrderID).Scan(&ownerProbe).Error; err != nil {
			return err
		}
		riderIDs := []uint64{input.RiderID}
		if input.Source == "reassign" && ownerProbe.RiderID != nil && *ownerProbe.RiderID != input.RiderID {
			riderIDs = append(riderIDs, *ownerProbe.RiderID)
		}
		sort.Slice(riderIDs, func(i, j int) bool { return riderIDs[i] < riderIDs[j] })
		for _, riderID := range riderIDs {
			var locked riderRow
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=? AND deleted_at IS NULL", riderID).First(&locked).Error; errors.Is(err, gorm.ErrRecordNotFound) {
				return problem.Conflict("RIDER_UNAVAILABLE", "rider is unavailable")
			} else if err != nil {
				return err
			}
		}

		var delivery DeliveryOrder
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=? AND deleted_at IS NULL", input.DeliveryOrderID).First(&delivery).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("DELIVERY_NOT_FOUND", "delivery order not found")
		} else if err != nil {
			return err
		}
		if input.ExpectedAssignmentVersion != 0 && delivery.AssignmentVersion != input.ExpectedAssignmentVersion {
			return problem.Conflict("VERSION_CONFLICT", "delivery assignment version changed")
		}
		var settlementOrder domainOrder
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND deleted_at IS NULL", delivery.OrderID).
			First(&settlementOrder).Error; err != nil {
			return err
		}

		var job Job
		jobFound := false
		jobQuery := tx.Clauses(clause.Locking{Strength: "UPDATE"})
		if delivery.CurrentDispatchJobID != nil {
			jobQuery = jobQuery.Where("id=?", *delivery.CurrentDispatchJobID)
		} else {
			jobQuery = jobQuery.Where("delivery_order_id=?", delivery.ID).Order("dispatch_seq DESC")
		}
		if err := jobQuery.First(&job).Error; err == nil {
			jobFound = true
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		var offer Offer
		var scoreSnapshot any
		now := time.Now()
		switch input.Source {
		case "auto_offer":
			if !jobFound || job.Status != "offering" {
				return problem.Conflict("DISPATCH_OFFER_NOT_ACTIVE", "dispatch offer is not active")
			}
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=? AND job_id=? AND rider_id=?", input.OfferID, job.ID, input.RiderID).First(&offer).Error; errors.Is(err, gorm.ErrRecordNotFound) {
				return problem.NotFound("DISPATCH_OFFER_NOT_FOUND", "dispatch offer not found")
			} else if err != nil {
				return err
			}
			if offer.Status != "pending" {
				return problem.Conflict("DISPATCH_OFFER_NOT_ACTIVE", "dispatch offer is not active")
			}
			if input.ExpectedOfferVersion != 0 && offer.Version != input.ExpectedOfferVersion {
				return problem.Conflict("VERSION_CONFLICT", "dispatch offer version changed")
			}
			if !offer.ExpiresAt.After(now) {
				return problem.Conflict("DISPATCH_OFFER_EXPIRED", "dispatch offer has expired")
			}
			var candidate Candidate
			if err := tx.Where("id=?", offer.CandidateID).First(&candidate).Error; err == nil {
				scoreSnapshot = candidate
			}
		case "grab":
			if !jobFound || job.Status != "grab_open" || job.GrabExpiresAt == nil || !job.GrabExpiresAt.After(now) {
				return problem.Conflict("DISPATCH_GRAB_NOT_OPEN", "dispatch grab window is not open")
			}
		case "manual":
			if delivery.Status != "pending_assign" || delivery.RiderID != nil {
				return problem.Conflict("DELIVERY_ALREADY_ASSIGNED", "delivery order has already been assigned")
			}
		case "reassign":
			if delivery.RiderID == nil || delivery.PickedUpAt != nil || delivery.Status != "accepted" {
				return problem.Conflict("DELIVERY_INVALID_STATUS", "delivery cannot be reassigned after pickup")
			}
		default:
			return problem.InvalidArgument("VALIDATION_FAILED", "unsupported assignment source")
		}

		if input.Source != "reassign" && (delivery.Status != "pending_assign" || delivery.RiderID != nil) {
			return problem.Conflict("DELIVERY_ALREADY_ASSIGNED", "delivery order has already been assigned")
		}
		if err := s.recheckRider(ctx, tx, input.RiderID, delivery, job, input.Source, now); err != nil {
			return err
		}
		if err := s.applyAssignmentSettlement(
			ctx,
			tx,
			delivery,
			settlementOrder,
			now,
			input.ActorType,
			input.ActorID,
		); err != nil {
			return err
		}

		before := delivery.AssignmentVersion
		if before == 0 {
			before = 1
		}
		after := before + 1
		if input.Source == "reassign" {
			if err := tx.Model(&Assignment{}).Where("delivery_order_id=? AND status='active'", delivery.ID).Update("status", "superseded").Error; err != nil {
				return err
			}
		}
		assignment := Assignment{
			ID: s.ids.Next(), DeliveryOrderID: delivery.ID, ToRiderID: input.RiderID,
			AssignmentType: input.Source, Status: "active", ActorType: input.ActorType, ActorID: input.ActorID,
			VersionBefore: before, VersionAfter: after, RequestID: requestctx.RequestIDPtr(ctx),
		}
		if delivery.RiderID != nil {
			assignment.FromRiderID = delivery.RiderID
		}
		if jobFound {
			assignment.DispatchJobID = &job.ID
		}
		if offer.ID != 0 {
			assignment.OfferID = &offer.ID
		}
		if input.ReasonCode != "" {
			assignment.ReasonCode = &input.ReasonCode
		}
		if input.Reason != "" {
			assignment.Reason = &input.Reason
		}
		if scoreSnapshot != nil {
			assignment.ScoreSnapshot = jsonData(scoreSnapshot)
		}
		if err := tx.Create(&assignment).Error; err != nil {
			if isDuplicate(err) {
				return problem.Conflict("DELIVERY_ALREADY_ASSIGNED", "delivery order has already been assigned")
			}
			return err
		}
		updates := map[string]any{
			"rider_id": input.RiderID, "status": "accepted", "dispatch_status": "assigned",
			"assignment_version": after, "accepted_at": now,
		}
		result := tx.Model(&DeliveryOrder{}).Where("id=? AND assignment_version=?", delivery.ID, delivery.AssignmentVersion).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return problem.Conflict("VERSION_CONFLICT", "delivery assignment version changed")
		}
		if err := tx.Table("orders").Where("id=?", delivery.OrderID).Update("delivery_status", "accepted").Error; err != nil {
			return err
		}
		if jobFound {
			if err := tx.Model(&Job{}).Where("id=?", job.ID).Updates(map[string]any{
				"status": "assigned", "assigned_at": now, "assigned_rider_id": input.RiderID,
				"locked_by": nil, "locked_until": nil, "version": gorm.Expr("version+1"),
			}).Error; err != nil {
				return err
			}
			if err := tx.Model(&Offer{}).Where("job_id=? AND status='pending' AND id<>?", job.ID, offer.ID).Updates(map[string]any{"status": "cancelled", "responded_at": now, "version": gorm.Expr("version+1")}).Error; err != nil {
				return err
			}
		}
		if offer.ID != 0 {
			if err := tx.Model(&Offer{}).Where("id=? AND status='pending'", offer.ID).Updates(map[string]any{"status": "accepted", "responded_at": now, "version": gorm.Expr("version+1")}).Error; err != nil {
				return err
			}
			if err := s.createAudit(ctx, tx, input.ActorType, input.ActorID, "dispatch.offer.accept", "dispatch_offer", offer.ID, offer, map[string]any{"status": "accepted", "version": offer.Version + 1}); err != nil {
				return err
			}
		}
		if err := tx.Model(&RiderRuntimeState{}).Where("rider_id=?", input.RiderID).Updates(map[string]any{"last_assigned_at": now, "version": gorm.Expr("version+1")}).Error; err != nil {
			return err
		}
		if input.Source == "reassign" && delivery.PickupReadyStatus == "ready" {
			if err := deliveryverification.InvalidateAndRegenerate(ctx, tx, s.cfg.CP1, s.ids, delivery.ID); err != nil {
				return err
			}
		}
		eventType := "delivery.assigned"
		if input.Source == "reassign" {
			eventType = "delivery.reassigned"
		}
		payload := map[string]any{
			"order_id": idString(delivery.OrderID), "delivery_order_id": idString(delivery.ID),
			"rider_id": idString(input.RiderID), "assignment_type": input.Source,
		}
		if jobFound {
			payload["dispatch_job_id"] = idString(job.ID)
		}
		if offer.ID != 0 {
			payload["offer_id"] = idString(offer.ID)
		}
		if delivery.RiderID != nil {
			payload["from_rider_id"] = idString(*delivery.RiderID)
		}
		if err := s.createEvent(ctx, tx, eventType, "delivery_order", delivery.ID, payload); err != nil {
			return err
		}
		auditBefore := map[string]any{
			"delivery_order_id": idString(delivery.ID), "status": delivery.Status,
			"rider_id": optionalID(delivery.RiderID), "assignment_version": delivery.AssignmentVersion,
			"dispatch_status": delivery.DispatchStatus, "pickup_ready_status": delivery.PickupReadyStatus,
		}
		if err := s.createAudit(ctx, tx, input.ActorType, input.ActorID, "delivery."+input.Source, "delivery_order", delivery.ID, auditBefore, payload); err != nil {
			return err
		}
		if err := tx.Table("order_logs").Create(map[string]any{
			"id": s.ids.Next(), "order_id": delivery.OrderID, "actor_type": input.ActorType,
			"actor_id": input.ActorID, "action": "delivery_assign", "request_id": requestctx.RequestIDPtr(ctx),
		}).Error; err != nil {
			return err
		}
		out = AssignmentResult{
			DeliveryOrderID: idString(delivery.ID), OrderID: idString(delivery.OrderID), ShopID: idString(delivery.ShopID),
			RiderID: idString(input.RiderID), Status: "accepted", DispatchStatus: "assigned",
			AssignmentVersion: after, PickupReadyStatus: delivery.PickupReadyStatus,
			PickupReadyAt: timeValue(delivery.PickupReadyAt), AcceptedAt: now.Format(time.RFC3339Nano),
		}
		return s.idem.Succeed(ctx, tx, input.ActorType, input.ActorID, path, key, out)
	})
	return out, err
}

// idempotencyRateKey 返回幂等速率密钥。
func idempotencyRateKey(prefix string, id uint64) string {
	return prefix + ":" + idString(id)
}

// recheckRider 重新检查骑手资格。
func (s *Service) recheckRider(ctx context.Context, tx *gorm.DB, riderID uint64, delivery DeliveryOrder, job Job, source string, now time.Time) error {
	var eligibility struct {
		Status          string
		ReviewStatus    string
		AccountStatus   string
		ServiceStatus   *string
		WorkStatus      *string
		HeartbeatAt     *time.Time
		CapturedAt      *time.Time
		Latitude        *float64
		Longitude       *float64
		AccuracyM       *float64
		MaxActiveOrders *uint8
	}
	err := tx.WithContext(ctx).Table("riders r").
		Select("r.status,r.review_status,a.status AS account_status,rss.status AS service_status,rrs.work_status,rrs.heartbeat_at,rrs.captured_at,rrs.latitude,rrs.longitude,rrs.accuracy_m,rrs.max_active_orders").
		Joins("JOIN accounts a ON a.id=r.account_id AND a.deleted_at IS NULL").
		Joins("LEFT JOIN rider_service_shops rss ON rss.rider_id=r.id AND rss.shop_id=?", delivery.ShopID).
		Joins("LEFT JOIN rider_runtime_states rrs ON rrs.rider_id=r.id").
		Where("r.id=? AND r.deleted_at IS NULL", riderID).Scan(&eligibility).Error
	if err != nil {
		return err
	}
	if eligibility.Status != "active" || eligibility.ReviewStatus != "approved" || eligibility.AccountStatus != "active" {
		return problem.Conflict("RIDER_UNAVAILABLE", "rider is unavailable")
	}
	if eligibility.ServiceStatus == nil || *eligibility.ServiceStatus != "active" {
		return problem.Forbidden("RIDER_OUT_OF_SCOPE", "rider does not serve this shop")
	}
	snapshot := decodeSnapshot(job)
	if source == "auto_offer" || source == "grab" {
		if eligibility.WorkStatus == nil || *eligibility.WorkStatus != "online" || eligibility.HeartbeatAt == nil || eligibility.CapturedAt == nil ||
			now.Sub(*eligibility.HeartbeatAt) > time.Duration(snapshot.HeartbeatFreshSeconds)*time.Second ||
			now.Sub(*eligibility.CapturedAt) > time.Duration(snapshot.LocationFreshSeconds)*time.Second ||
			eligibility.AccuracyM == nil || *eligibility.AccuracyM > float64(snapshot.MaxLocationAccuracyM) {
			return problem.Conflict("RIDER_UNAVAILABLE", "rider heartbeat or location is stale")
		}
		var shop shopRow
		if err := tx.Select("id,latitude,longitude").Where("id=? AND deleted_at IS NULL", delivery.ShopID).First(&shop).Error; err != nil {
			return err
		}
		if shop.Latitude == nil || shop.Longitude == nil || eligibility.Latitude == nil || eligibility.Longitude == nil ||
			haversineMeters(*eligibility.Latitude, *eligibility.Longitude, *shop.Latitude, *shop.Longitude) > float64(snapshot.MaxPickupDistanceM) {
			return problem.Conflict("RIDER_UNAVAILABLE", "rider is outside the pickup distance")
		}
	}
	maxActive := snapshot.MaxActiveOrdersDefault
	if eligibility.MaxActiveOrders != nil && *eligibility.MaxActiveOrders > 0 {
		maxActive = *eligibility.MaxActiveOrders
	}
	var active int64
	if err := tx.Table("delivery_orders").Where("rider_id=? AND status IN ? AND id<>? AND deleted_at IS NULL", riderID, []string{"accepted", "delivering"}, delivery.ID).Count(&active).Error; err != nil {
		return err
	}
	if active >= int64(maxActive) {
		return problem.Conflict("RIDER_AT_CAPACITY", "rider has reached active delivery capacity")
	}
	return nil
}
