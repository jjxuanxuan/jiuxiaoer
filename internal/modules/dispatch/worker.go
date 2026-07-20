package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Worker struct {
	service *Service
	owner   string
	log     *slog.Logger
}

type scoredCandidate struct {
	candidate Candidate
	last      *time.Time
}

// NewWorker 创建并初始化工作器。
func NewWorker(service *Service, owner string, log *slog.Logger) *Worker {
	return &Worker{service: service, owner: owner, log: log}
}

// Run 运行当前实例的核心处理流程。
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.service.cfg.Dispatch.WorkerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.RunOnce(ctx); err != nil && w.log != nil {
				w.log.Error("dispatch sweep failed", slog.Any("error", err))
			}
		}
	}
}

// RunOnce 运行Once处理流程。
func (w *Worker) RunOnce(ctx context.Context) error {
	for i := 0; i < w.service.cfg.Dispatch.WorkerBatchSize; i++ {
		jobID, token, ok, err := w.claim(ctx)
		if err != nil || !ok {
			return err
		}
		jobCtx, cancel := context.WithTimeout(ctx, w.service.cfg.Dispatch.JobTimeout)
		err = w.service.processJob(jobCtx, jobID, token)
		cancel()
		if err != nil {
			w.service.recordJobFailure(ctx, jobID, token, err)
		}
	}
	return nil
}

// claim 认领uint 64。
func (w *Worker) claim(ctx context.Context) (uint64, string, bool, error) {
	token := w.owner + ":" + uuid.NewString()
	var row Job
	err := w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status IN ? AND next_action_at<=? AND (locked_until IS NULL OR locked_until<=?)", []string{"pending", "scoring", "offering", "grab_open"}, now, now).
			Order("next_action_at,id").First(&row).Error
		if err != nil {
			return err
		}
		until := now.Add(w.service.cfg.Dispatch.LeaseDuration)
		return tx.Model(&Job{}).Where("id=?", row.ID).Updates(map[string]any{"locked_by": token, "locked_until": until}).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, "", false, nil
	}
	return row.ID, token, row.ID != 0, err
}

// ProcessJobID 返回Process 任务ID。
func (s *Service) ProcessJobID(ctx context.Context, jobID uint64) error {
	return s.processJob(ctx, jobID, "")
}

// processJob 返回process 任务。
func (s *Service) processJob(ctx context.Context, jobID uint64, leaseToken string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Match CommitAssignment's delivery -> job lock order. A non-locking
		// probe is safe because delivery_order_id is immutable.
		var probe Job
		if err := tx.Select("id,delivery_order_id").First(&probe, jobID).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		var delivery DeliveryOrder
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&delivery, probe.DeliveryOrderID).Error; err != nil {
			return err
		}
		var job Job
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&job, jobID).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		if leaseToken != "" && (job.LockedBy == nil || *job.LockedBy != leaseToken) {
			return nil
		}
		if job.Status == "assigned" || job.Status == "cancelled" || job.Status == "manual_required" {
			return s.releaseLease(tx, job.ID, leaseToken)
		}
		now := time.Now()
		if leaseToken == "" && job.NextActionAt.After(now) {
			return nil
		}
		if delivery.Status != "pending_assign" || delivery.RiderID != nil {
			return s.cancelJob(ctx, tx, job, delivery, "DELIVERY_NOT_ASSIGNABLE")
		}
		snapshot := decodeSnapshot(job)
		if !s.cfg.Dispatch.Enabled {
			snapshot.Mode = "manual"
		} else if s.cfg.Dispatch.ModeOverride != "" {
			snapshot.Mode = s.cfg.Dispatch.ModeOverride
		}
		switch job.Status {
		case "pending", "scoring":
			if job.FirstStartedAt == nil {
				job.FirstStartedAt = &now
			}
			if err := tx.Model(&Job{}).Where("id=?", job.ID).Updates(map[string]any{"status": "scoring", "first_started_at": job.FirstStartedAt, "version": gorm.Expr("version+1")}).Error; err != nil {
				return err
			}
			job.Status = "scoring"
			if snapshot.Mode == "manual" {
				return s.openManual(ctx, tx, &job, &delivery, "MODE_MANUAL")
			}
			if snapshot.Mode == "grab" {
				if err := s.scoreCandidates(ctx, tx, job, snapshot); err != nil {
					return err
				}
				return s.openGrab(ctx, tx, &job, &delivery, snapshot)
			}
			if err := s.scoreCandidates(ctx, tx, job, snapshot); err != nil {
				return err
			}
			return s.createNextOfferOrFallback(ctx, tx, &job, &delivery, snapshot)
		case "offering":
			var offer Offer
			err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("job_id=? AND status='pending'", job.ID).First(&offer).Error
			if err == nil && offer.ExpiresAt.After(now) {
				return tx.Model(&Job{}).Where("id=?", job.ID).Updates(map[string]any{"next_action_at": offer.ExpiresAt, "locked_by": nil, "locked_until": nil}).Error
			}
			if err == nil {
				if err := tx.Model(&Offer{}).Where("id=? AND status='pending'", offer.ID).Updates(map[string]any{"status": "expired", "responded_at": now, "version": gorm.Expr("version+1")}).Error; err != nil {
					return err
				}
				if err := s.createEvent(ctx, tx, "dispatch.offer.expired", "dispatch_offer", offer.ID, map[string]any{
					"offer_id": idString(offer.ID), "delivery_order_id": idString(offer.DeliveryOrderID),
					"rider_id": idString(offer.RiderID), "reason_code": "EXPIRED",
				}); err != nil {
					return err
				}
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			return s.createNextOfferOrFallback(ctx, tx, &job, &delivery, snapshot)
		case "grab_open":
			if job.GrabExpiresAt != nil && job.GrabExpiresAt.After(now) {
				return tx.Model(&Job{}).Where("id=?", job.ID).Updates(map[string]any{"next_action_at": *job.GrabExpiresAt, "locked_by": nil, "locked_until": nil}).Error
			}
			return s.openManual(ctx, tx, &job, &delivery, "GRAB_EXPIRED")
		}
		return nil
	})
}

// scoreCandidates 返回score Candidates。
func (s *Service) scoreCandidates(ctx context.Context, tx *gorm.DB, job Job, policy PolicySnapshot) error {
	var existing int64
	if err := tx.Model(&Candidate{}).Where("job_id=?", job.ID).Count(&existing).Error; err != nil {
		return err
	}
	if existing > 0 {
		return nil
	}
	var shop shopRow
	if err := tx.Where("id=? AND deleted_at IS NULL", job.ShopID).First(&shop).Error; err != nil {
		return err
	}
	preselected := make([]uint64, 0)
	if s.redis != nil && shop.Latitude != nil && shop.Longitude != nil && shop.CityCode != nil {
		members, err := s.redis.GeoSearch(ctx, "dispatch:riders:geo:"+*shop.CityCode, &redis.GeoSearchQuery{
			Longitude: *shop.Longitude, Latitude: *shop.Latitude, Radius: float64(policy.MaxPickupDistanceM), RadiusUnit: "m", Sort: "ASC", Count: int(policy.CandidateLimit),
		}).Result()
		if err == nil {
			for _, member := range members {
				if id, err := strconv.ParseUint(member, 10, 64); err == nil {
					preselected = append(preselected, id)
				}
			}
		}
	}
	type sourceRow struct {
		RiderID         uint64
		RiderStatus     string
		ReviewStatus    string
		AccountStatus   string
		WorkStatus      string
		Latitude        *float64
		Longitude       *float64
		AccuracyM       *float64
		CapturedAt      *time.Time
		HeartbeatAt     *time.Time
		LastAssignedAt  *time.Time
		MaxActiveOrders *uint8
	}
	baseQuery := func() *gorm.DB {
		return tx.Table("rider_service_shops rss").Select(`
			r.id AS rider_id,r.status AS rider_status,r.review_status,a.status AS account_status,
			rrs.work_status,rrs.latitude,rrs.longitude,rrs.accuracy_m,rrs.captured_at,rrs.heartbeat_at,
			rrs.last_assigned_at,rrs.max_active_orders`).
			Joins("JOIN riders r ON r.id=rss.rider_id AND r.deleted_at IS NULL").
			Joins("JOIN accounts a ON a.id=r.account_id AND a.deleted_at IS NULL").
			Joins("LEFT JOIN rider_runtime_states rrs ON rrs.rider_id=r.id").
			Where("rss.shop_id=? AND rss.status='active'", job.ShopID).
			Order("r.id").Limit(int(policy.CandidateLimit))
	}
	var sources []sourceRow
	if len(preselected) > 0 {
		if err := baseQuery().Where("r.id IN ?", preselected).Scan(&sources).Error; err != nil {
			return err
		}
		// GEO is an accelerator, never the source of truth. Always inspect a
		// bounded DB fallback set as well so a partially rebuilt or stale Redis
		// index cannot suppress every otherwise eligible rider.
		var fallback []sourceRow
		if err := baseQuery().Where("r.id NOT IN ?", preselected).Scan(&fallback).Error; err != nil {
			return err
		}
		sources = append(sources, fallback...)
	} else if err := baseQuery().Scan(&sources).Error; err != nil {
		return err
	}
	now := time.Now()
	scores := make([]scoredCandidate, 0, len(sources))
	for _, row := range sources {
		exclusions := make([]string, 0)
		if row.RiderStatus != "active" || row.ReviewStatus != "approved" || row.AccountStatus != "active" {
			exclusions = append(exclusions, "RIDER_INACTIVE")
		}
		if row.WorkStatus != "online" {
			exclusions = append(exclusions, "RIDER_OFFLINE")
		}
		heartbeatAge, locationAge := ageSeconds(now, row.HeartbeatAt), ageSeconds(now, row.CapturedAt)
		if heartbeatAge == nil || *heartbeatAge > policy.HeartbeatFreshSeconds {
			exclusions = append(exclusions, "HEARTBEAT_STALE")
		}
		if locationAge == nil || *locationAge > policy.LocationFreshSeconds || row.Latitude == nil || row.Longitude == nil {
			exclusions = append(exclusions, "LOCATION_STALE")
		}
		if row.AccuracyM == nil || *row.AccuracyM > float64(policy.MaxLocationAccuracyM) {
			exclusions = append(exclusions, "LOCATION_INACCURATE")
		}
		maxActive := policy.MaxActiveOrdersDefault
		if row.MaxActiveOrders != nil && *row.MaxActiveOrders > 0 {
			maxActive = *row.MaxActiveOrders
		}
		var active int64
		if err := tx.Table("delivery_orders").Where("rider_id=? AND status IN ? AND deleted_at IS NULL", row.RiderID, []string{"accepted", "delivering"}).Count(&active).Error; err != nil {
			return err
		}
		if active >= int64(maxActive) {
			exclusions = append(exclusions, "AT_CAPACITY")
		}
		var distance *uint
		if shop.Latitude != nil && shop.Longitude != nil && row.Latitude != nil && row.Longitude != nil {
			value := uint(math.Round(haversineMeters(*row.Latitude, *row.Longitude, *shop.Latitude, *shop.Longitude)))
			distance = &value
			if value > policy.MaxPickupDistanceM {
				exclusions = append(exclusions, "TOO_FAR")
			}
		} else {
			exclusions = append(exclusions, "DISTANCE_UNKNOWN")
		}
		distanceScore := scoreDistance(distance, policy.MaxPickupDistanceM)
		loadScore := clamp(100*(1-float64(active)/float64(maxActive)), 0, 100)
		idleSeconds := float64(policy.IdleFullScoreSeconds)
		if row.LastAssignedAt != nil {
			idleSeconds = now.Sub(*row.LastAssignedAt).Seconds()
		}
		idleScore := clamp(idleSeconds/float64(policy.IdleFullScoreSeconds)*100, 0, 100)
		freshnessScore := float64(0)
		if locationAge != nil {
			freshnessScore = clamp(100*(1-float64(*locationAge)/float64(policy.LocationFreshSeconds)), 0, 100)
		}
		var rejected int64
		cooldownStart := now.Add(-time.Duration(policy.RejectionCooldownSeconds) * time.Second)
		if policy.RejectionCooldownSeconds > 0 {
			_ = tx.Model(&Offer{}).Where("rider_id=? AND status='rejected' AND updated_at>=?", row.RiderID, cooldownStart).Count(&rejected).Error
		}
		penalty := math.Min(15, float64(rejected)*5)
		final := clamp(distanceScore*policy.ScoreWeights.Distance+loadScore*policy.ScoreWeights.Load+idleScore*policy.ScoreWeights.Idle+freshnessScore*policy.ScoreWeights.Freshness-penalty, 0, 100)
		exclusionJSON, _ := json.Marshal(exclusions)
		input := jsonData(map[string]any{"distance_m": distance, "active_orders": active, "max_active_orders": maxActive, "heartbeat_age_seconds": heartbeatAge, "location_age_seconds": locationAge})
		candidate := Candidate{
			ID: s.ids.Next(), JobID: job.ID, RiderID: row.RiderID, Eligible: len(exclusions) == 0,
			ExclusionCodes: exclusionJSON, DistanceM: distance, ActiveOrders: uint8(active), MaxActiveOrders: maxActive,
			HeartbeatAgeSeconds: heartbeatAge, LocationAgeSeconds: locationAge,
			DistanceScore: floatPtr(round4(distanceScore)), LoadScore: floatPtr(round4(loadScore)), IdleScore: floatPtr(round4(idleScore)),
			FreshnessScore: floatPtr(round4(freshnessScore)), RejectionPenalty: floatPtr(round4(penalty)), FinalScore: floatPtr(round4(final)),
			ScoreVersion: policy.ScoreVersion, InputSnapshot: input,
		}
		scores = append(scores, scoredCandidate{candidate: candidate, last: row.LastAssignedAt})
	}
	sortScoredCandidates(scores)
	if len(scores) > int(policy.CandidateLimit) {
		scores = scores[:policy.CandidateLimit]
	}
	for i := range scores {
		scores[i].candidate.RankNo = uint(i + 1)
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&scores[i].candidate).Error; err != nil {
			return err
		}
	}
	return nil
}

// sortScoredCandidates 处理sort Scored Candidates相关逻辑。
func sortScoredCandidates(scores []scoredCandidate) {
	sort.SliceStable(scores, func(i, j int) bool {
		a, b := scores[i], scores[j]
		if a.candidate.Eligible != b.candidate.Eligible {
			return a.candidate.Eligible
		}
		if value(a.candidate.FinalScore) != value(b.candidate.FinalScore) {
			return value(a.candidate.FinalScore) > value(b.candidate.FinalScore)
		}
		if (a.last == nil) != (b.last == nil) {
			return a.last == nil
		}
		if a.last != nil && !a.last.Equal(*b.last) {
			return a.last.Before(*b.last)
		}
		return a.candidate.RiderID < b.candidate.RiderID
	})
}

// createNextOfferOrFallback 创建Next Offer Or 降级。
func (s *Service) createNextOfferOrFallback(ctx context.Context, tx *gorm.DB, job *Job, delivery *DeliveryOrder, policy PolicySnapshot) error {
	maxRounds := uint(policy.AutoRounds)
	if uint(policy.OfferCandidateLimit) < maxRounds {
		maxRounds = uint(policy.OfferCandidateLimit)
	}
	if uint(job.RoundNo) < maxRounds {
		var candidate Candidate
		err := tx.Where("job_id=? AND eligible=1 AND rank_no>?", job.ID, job.CandidateCursor).Order("rank_no").First(&candidate).Error
		if err == nil {
			now := time.Now()
			expires := now.Add(time.Duration(policy.OfferTTLSeconds) * time.Second)
			round := job.RoundNo + 1
			offerID := s.ids.Next()
			offer := Offer{ID: offerID, OfferNo: fmt.Sprintf("DO%d", offerID), JobID: job.ID, DeliveryOrderID: delivery.ID, RiderID: candidate.RiderID, RoundNo: round, CandidateID: candidate.ID, Status: "pending", ExpiresAt: expires, Version: 1}
			if err := tx.Create(&offer).Error; err != nil {
				return err
			}
			if err := tx.Model(&Job{}).Where("id=?", job.ID).Updates(map[string]any{
				"status": "offering", "round_no": round, "candidate_cursor": candidate.RankNo,
				"next_action_at": expires, "locked_by": nil, "locked_until": nil, "version": gorm.Expr("version+1"),
			}).Error; err != nil {
				return err
			}
			if err := tx.Model(&DeliveryOrder{}).Where("id=?", delivery.ID).Update("dispatch_status", "offering").Error; err != nil {
				return err
			}
			return s.createEvent(ctx, tx, "dispatch.offer.created", "dispatch_offer", offer.ID, map[string]any{
				"offer_id": idString(offer.ID), "delivery_order_id": idString(delivery.ID), "rider_id": idString(candidate.RiderID),
				"expires_at": expires.Format(time.RFC3339Nano), "sound_key": "new_delivery_offer",
			})
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
	}
	if policy.Mode == "hybrid" || policy.Mode == "grab" {
		return s.openGrab(ctx, tx, job, delivery, policy)
	}
	return s.openManual(ctx, tx, job, delivery, "AUTO_EXHAUSTED")
}

// openGrab 解密并返回Grab。
func (s *Service) openGrab(ctx context.Context, tx *gorm.DB, job *Job, delivery *DeliveryOrder, policy PolicySnapshot) error {
	now := time.Now()
	expires := now.Add(time.Duration(policy.GrabTTLSeconds) * time.Second)
	if err := tx.Model(&Job{}).Where("id=?", job.ID).Updates(map[string]any{"status": "grab_open", "grab_opened_at": now, "grab_expires_at": expires, "next_action_at": expires, "locked_by": nil, "locked_until": nil, "version": gorm.Expr("version+1")}).Error; err != nil {
		return err
	}
	if err := tx.Model(&DeliveryOrder{}).Where("id=?", delivery.ID).Update("dispatch_status", "grab_open").Error; err != nil {
		return err
	}
	return s.createEvent(ctx, tx, "dispatch.grab.opened", "dispatch_job", job.ID, map[string]any{"dispatch_job_id": idString(job.ID), "delivery_order_id": idString(delivery.ID), "shop_id": idString(delivery.ShopID), "expires_at": expires.Format(time.RFC3339Nano)})
}

// openManual 解密并返回Manual。
func (s *Service) openManual(ctx context.Context, tx *gorm.DB, job *Job, delivery *DeliveryOrder, reason string) error {
	next := time.Now().Add(365 * 24 * time.Hour)
	if err := tx.Model(&Job{}).Where("id=?", job.ID).Updates(map[string]any{"status": "manual_required", "status_reason_code": reason, "next_action_at": next, "locked_by": nil, "locked_until": nil, "version": gorm.Expr("version+1")}).Error; err != nil {
		return err
	}
	if err := tx.Model(&DeliveryOrder{}).Where("id=?", delivery.ID).Update("dispatch_status", "manual_required").Error; err != nil {
		return err
	}
	return s.createEvent(ctx, tx, "dispatch.manual_required", "dispatch_job", job.ID, map[string]any{"dispatch_job_id": idString(job.ID), "delivery_order_id": idString(delivery.ID), "shop_id": idString(delivery.ShopID), "reason_code": reason})
}

// cancelJob 取消任务。
func (s *Service) cancelJob(ctx context.Context, tx *gorm.DB, job Job, delivery DeliveryOrder, reason string) error {
	now := time.Now()
	if err := tx.Model(&Offer{}).Where("job_id=? AND status='pending'", job.ID).Updates(map[string]any{"status": "cancelled", "responded_at": now, "version": gorm.Expr("version+1")}).Error; err != nil {
		return err
	}
	return tx.Model(&Job{}).Where("id=?", job.ID).Updates(map[string]any{"status": "cancelled", "status_reason_code": reason, "locked_by": nil, "locked_until": nil, "version": gorm.Expr("version+1")}).Error
}

// releaseLease 释放租约。
func (s *Service) releaseLease(tx *gorm.DB, jobID uint64, token string) error {
	query := tx.Model(&Job{}).Where("id=?", jobID)
	if token != "" {
		query = query.Where("locked_by=?", token)
	}
	return query.Updates(map[string]any{"locked_by": nil, "locked_until": nil}).Error
}

// recordJobFailure 处理记录任务 Failure相关逻辑。
func (s *Service) recordJobFailure(ctx context.Context, jobID uint64, token string, cause error) {
	_ = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job Job
		query := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=?", jobID)
		if token != "" {
			query = query.Where("locked_by=?", token)
		}
		if err := query.First(&job).Error; err != nil {
			return err
		}
		attempts := job.Attempts + 1
		if int(attempts) >= s.cfg.Dispatch.MaxAttempts {
			var delivery DeliveryOrder
			if err := tx.First(&delivery, job.DeliveryOrderID).Error; err != nil {
				return err
			}
			return s.openManual(ctx, tx, &job, &delivery, "WORKER_MAX_ATTEMPTS")
		}
		backoff := time.Duration(math.Min(30, math.Pow(2, float64(attempts-1)))) * time.Second
		return tx.Model(&Job{}).Where("id=?", job.ID).Updates(map[string]any{"attempts": attempts, "last_error_code": "DISPATCH_PROCESS_FAILED", "last_error_safe": safeError(cause), "next_action_at": time.Now().Add(backoff), "locked_by": nil, "locked_until": nil}).Error
	})
}

// safeError 返回safe 错误。
func safeError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if len(message) > 255 {
		message = message[:255]
	}
	return message
}

// ageSeconds 返回age Seconds。
func ageSeconds(now time.Time, value *time.Time) *uint {
	if value == nil {
		return nil
	}
	seconds := now.Sub(*value).Seconds()
	if seconds < 0 {
		seconds = 0
	}
	result := uint(seconds)
	return &result
}

// haversineMeters 返回haversine Meters。
func haversineMeters(lat1, lon1, lat2, lon2 float64) float64 {
	const radius = 6371000.0
	toRad := math.Pi / 180
	dLat, dLon := (lat2-lat1)*toRad, (lon2-lon1)*toRad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*toRad)*math.Cos(lat2*toRad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return radius * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// scoreDistance 返回score Distance。
func scoreDistance(distance *uint, max uint) float64 {
	if distance == nil || max == 0 {
		return 0
	}
	return clamp(100*(1-float64(*distance)/float64(max)), 0, 100)
}

// clamp 返回clamp。
func clamp(value, minimum, maximum float64) float64 {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

// round4 返回round 4。
func round4(value float64) float64 { return math.Round(value*10000) / 10000 }

// floatPtr 返回float Ptr。
func floatPtr(value float64) *float64 { return &value }

// value 返回值。
func value(pointer *float64) float64 {
	if pointer == nil {
		return 0
	}
	return *pointer
}
