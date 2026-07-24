package dispatch

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/coordinate"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

const (
	heartbeatLocationAnomalyAction = "rider.location.anomaly"
	heartbeatRateLimitedAction     = "rider.heartbeat.rate_limited"
	heartbeatRejectedAction        = "rider.heartbeat.rejected"
)

// heartbeatAuditSpec 刻意只由服务端持有的常量构成。绝不能加入原始请求值：
// 心跳请求包含精确坐标和设备标识符，这些内容禁止写入 audit_logs。
type heartbeatAuditSpec struct {
	Action             string
	ErrorCode          string
	ReasonCode         string
	RequestStage       string
	RateLimitDimension string
}

// UpdateWorkStatus 更新Work 状态。
func (s *Service) UpdateWorkStatus(ctx context.Context, claims *auth.Claims, method, path, key string, req WorkStatusReq) (WorkStatusDTO, error) {
	riderID, err := riderActor(claims, "rider_work_status:update")
	if err != nil {
		return WorkStatusDTO{}, err
	}
	var out WorkStatusDTO
	changed := false
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), "rider", riderID, method, path, key, idempotency.RequestHash(req))
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
		if allowed, rateErr := s.allowFixedWindow(ctx, fmt.Sprintf("dispatch:rider:work-status-limit:%d", riderID), time.Minute, 10); rateErr == nil && !allowed {
			return rateLimited("work status updates are limited to ten requests per minute", time.Minute)
		}
		var rider riderRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=? AND deleted_at IS NULL", riderID).First(&rider).Error; err != nil {
			return problem.Conflict("RIDER_UNAVAILABLE", "rider is unavailable")
		}
		var accountStatus string
		if err := tx.Table("accounts").Select("status").Where("id=? AND deleted_at IS NULL", rider.AccountID).Scan(&accountStatus).Error; err != nil {
			return err
		}
		if rider.Status != "active" || rider.ReviewStatus != "approved" || accountStatus != "active" {
			return problem.Conflict("RIDER_UNAVAILABLE", "rider is not active and approved")
		}
		if rider.WorkStatusVersion != req.ExpectedVersion {
			return problem.Conflict("VERSION_CONFLICT", "rider work status version changed")
		}
		changed = rider.WorkStatus != req.Status
		if changed && s.redis != nil {
			if coolingDown, redisErr := s.redis.Exists(ctx, fmt.Sprintf("dispatch:rider:work-status-cooldown:%d", riderID)).Result(); redisErr == nil && coolingDown > 0 {
				return rateLimited("work status cannot be switched again within thirty seconds", 30*time.Second)
			}
		}
		now := time.Now()
		if err := tx.Model(&riderRow{}).Where("id=? AND work_status_version=?", riderID, rider.WorkStatusVersion).Updates(map[string]any{
			"work_status": req.Status, "work_status_version": gorm.Expr("work_status_version+1"),
		}).Error; err != nil {
			return err
		}
		var runtime RiderRuntimeState
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("rider_id=?", riderID).First(&runtime).Error
		if err != nil && !errorsIsNotFound(err) {
			return err
		}
		if errorsIsNotFound(err) {
			runtime = RiderRuntimeState{RiderID: riderID, WorkStatus: req.Status, Version: 1}
			if req.Status == "online" {
				runtime.OnlineSince = &now
			}
			if err := tx.Create(&runtime).Error; err != nil {
				return err
			}
		} else {
			values := map[string]any{"work_status": req.Status, "version": gorm.Expr("version+1")}
			if req.Status == "online" && runtime.WorkStatus != "online" {
				values["online_since"] = now
			}
			if err := tx.Model(&RiderRuntimeState{}).Where("rider_id=?", riderID).Updates(values).Error; err != nil {
				return err
			}
		}
		var active int64
		if err := tx.Table("delivery_orders").Where("rider_id=? AND status IN ? AND deleted_at IS NULL", riderID, []string{"accepted", "delivering"}).Count(&active).Error; err != nil {
			return err
		}
		out = WorkStatusDTO{RiderID: idString(riderID), Status: req.Status, Version: rider.WorkStatusVersion + 1, HasActiveDeliveries: active > 0}
		if err := s.createAudit(ctx, tx, "rider", riderID, "rider.work_status.update", "rider", riderID, map[string]any{"status": rider.WorkStatus}, out); err != nil {
			return err
		}
		eventPayload := map[string]any{"rider_id": idString(riderID), "status": req.Status, "version": out.Version}
		if req.ReasonCode != "" {
			eventPayload["reason_code"] = req.ReasonCode
		}
		if err := s.createEvent(ctx, tx, "rider.work_status.changed", "rider", riderID, eventPayload); err != nil {
			return err
		}
		return s.idem.Succeed(ctx, tx, "rider", riderID, path, key, out)
	})
	if err == nil {
		s.syncWorkStatusCache(ctx, riderID, req.Status)
		if changed && s.redis != nil {
			_ = s.redis.Set(ctx, fmt.Sprintf("dispatch:rider:work-status-cooldown:%d", riderID), req.Status, 30*time.Second).Err()
		}
	}
	return out, err
}

// Heartbeat 返回Heartbeat。
func (s *Service) Heartbeat(ctx context.Context, claims *auth.Claims, req HeartbeatReq, clientIP string) (HeartbeatDTO, error) {
	riderID, err := riderActor(claims, "rider_location:update")
	if err != nil {
		s.createHeartbeatFailureAudit(ctx, claims, heartbeatAuditSpec{
			Action: heartbeatRejectedAction, ErrorCode: "PERM_FORBIDDEN",
			ReasonCode: heartbeatAuthorizationReason(claims), RequestStage: "authorization",
		})
		return HeartbeatDTO{}, err
	}
	if reasonCode := heartbeatLocationInputReason(req); reasonCode != "" {
		requestErr := problem.InvalidArgument("VALIDATION_FAILED", "valid latitude, longitude, and accuracy_m are required")
		s.createHeartbeatFailureAudit(ctx, claims, heartbeatAuditSpec{
			Action: heartbeatLocationAnomalyAction, ErrorCode: requestErr.ErrorCode,
			ReasonCode: reasonCode, RequestStage: "location_validation",
		})
		return HeartbeatDTO{}, requestErr
	}
	latitude, longitude, normalizeErr := coordinate.Normalize(*req.Latitude, *req.Longitude, req.CoordinateSystem)
	if normalizeErr != nil {
		requestErr := problem.InvalidArgument("COORDINATE_INVALID", "coordinate is invalid")
		s.createHeartbeatFailureAudit(ctx, claims, heartbeatAuditSpec{
			Action: heartbeatLocationAnomalyAction, ErrorCode: requestErr.ErrorCode,
			ReasonCode: "coordinate_normalization_failed", RequestStage: "location_validation",
		})
		return HeartbeatDTO{}, requestErr
	}
	req.Latitude, req.Longitude, req.CoordinateSystem = &latitude, &longitude, coordinate.GCJ02
	capturedAt, err := time.Parse(time.RFC3339Nano, req.CapturedAt)
	if err != nil {
		requestErr := problem.InvalidArgument("VALIDATION_FAILED", "captured_at must be RFC3339")
		s.createHeartbeatFailureAudit(ctx, claims, heartbeatAuditSpec{
			Action: heartbeatLocationAnomalyAction, ErrorCode: requestErr.ErrorCode,
			ReasonCode: "captured_at_invalid", RequestStage: "location_validation",
		})
		return HeartbeatDTO{}, requestErr
	}
	now := time.Now()
	if capturedAt.Before(now.Add(-5 * time.Minute)) {
		requestErr := problem.InvalidArgument("VALIDATION_FAILED", "captured_at differs from server time by more than five minutes")
		s.createHeartbeatFailureAudit(ctx, claims, heartbeatAuditSpec{
			Action: heartbeatLocationAnomalyAction, ErrorCode: requestErr.ErrorCode,
			ReasonCode: "captured_at_too_old", RequestStage: "location_validation",
		})
		return HeartbeatDTO{}, requestErr
	}
	if capturedAt.After(now.Add(5 * time.Minute)) {
		requestErr := problem.InvalidArgument("VALIDATION_FAILED", "captured_at differs from server time by more than five minutes")
		s.createHeartbeatFailureAudit(ctx, claims, heartbeatAuditSpec{
			Action: heartbeatLocationAnomalyAction, ErrorCode: requestErr.ErrorCode,
			ReasonCode: "captured_at_in_future", RequestStage: "location_validation",
		})
		return HeartbeatDTO{}, requestErr
	}
	deviceHash := fmt.Sprintf("%x", sha256.Sum256([]byte(req.DeviceID)))
	cachedSequence := uint64(0)
	cachedSameDevice := false
	redisAvailable := false
	if s.redis != nil {
		values, redisErr := s.redis.HMGet(ctx, fmt.Sprintf("dispatch:rider:presence:%d", riderID), "sequence", "device_id_hash").Result()
		if redisErr == nil {
			redisAvailable = true
			if len(values) == 2 {
				cachedSequence, _ = strconv.ParseUint(fmt.Sprint(values[0]), 10, 64)
				cachedSameDevice = fmt.Sprint(values[1]) == deviceHash
			}
		}
		if !(cachedSameDevice && cachedSequence >= req.Sequence) {
			if allowed, rateErr := s.allowFixedWindow(ctx, fmt.Sprintf("dispatch:rider:heartbeat-limit:%d", riderID), 15*time.Second, 5); rateErr == nil && !allowed {
				requestErr := rateLimited("heartbeat rate exceeds the allowed burst", 15*time.Second)
				s.createHeartbeatFailureAudit(ctx, claims, heartbeatAuditSpec{
					Action: heartbeatRateLimitedAction, ErrorCode: requestErr.ErrorCode,
					ReasonCode: "rider_burst_limit", RequestStage: "abuse_protection", RateLimitDimension: "rider",
				})
				return HeartbeatDTO{}, requestErr
			}
			if allowed, rateErr := s.allowFixedWindow(ctx, "dispatch:device:heartbeat-limit:"+deviceHash, 15*time.Second, 5); rateErr == nil && !allowed {
				requestErr := rateLimited("device heartbeat rate exceeds the allowed burst", 15*time.Second)
				s.createHeartbeatFailureAudit(ctx, claims, heartbeatAuditSpec{
					Action: heartbeatRateLimitedAction, ErrorCode: requestErr.ErrorCode,
					ReasonCode: "device_burst_limit", RequestStage: "abuse_protection", RateLimitDimension: "device",
				})
				return HeartbeatDTO{}, requestErr
			}
			if clientIP != "" {
				ipHash := fmt.Sprintf("%x", sha256.Sum256([]byte(clientIP)))
				if allowed, rateErr := s.allowFixedWindow(ctx, "dispatch:ip:heartbeat-limit:"+ipHash, time.Second, 200); rateErr == nil && !allowed {
					requestErr := rateLimited("heartbeat source rate exceeds the allowed limit", time.Second)
					s.createHeartbeatFailureAudit(ctx, claims, heartbeatAuditSpec{
						Action: heartbeatRateLimitedAction, ErrorCode: requestErr.ErrorCode,
						ReasonCode: "ip_source_limit", RequestStage: "abuse_protection", RateLimitDimension: "ip",
					})
					return HeartbeatDTO{}, requestErr
				}
			}
		}
	}
	accepted := req.Sequence
	workStatus := "offline"
	persisted := false
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rider riderRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=? AND deleted_at IS NULL", riderID).First(&rider).Error; err != nil {
			return problem.Conflict("RIDER_UNAVAILABLE", "rider is unavailable")
		}
		var accountStatus string
		if err := tx.Table("accounts").Select("status").Where("id=? AND deleted_at IS NULL", rider.AccountID).Scan(&accountStatus).Error; err != nil {
			return err
		}
		if rider.Status != "active" || rider.ReviewStatus != "approved" || accountStatus != "active" {
			return problem.Conflict("RIDER_UNAVAILABLE", "rider is unavailable")
		}
		workStatus = rider.WorkStatus
		var runtime RiderRuntimeState
		runtimeExists := true
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("rider_id=?", riderID).First(&runtime).Error
		if errorsIsNotFound(err) {
			runtime = RiderRuntimeState{WorkStatus: workStatus, Version: 1}
			runtimeExists = false
		} else if err != nil {
			return err
		}
		latestSequence := heartbeatSequence(runtime.DeviceIDHash, runtime.LastSequence, deviceHash, cachedSameDevice, cachedSequence)
		if latestSequence >= req.Sequence {
			accepted = latestSequence
			return nil
		}
		shouldPersist := !redisAvailable || runtime.HeartbeatAt == nil || now.Sub(*runtime.HeartbeatAt) >= s.cfg.Dispatch.HeartbeatPersistInterval || runtime.DeviceIDHash == nil || *runtime.DeviceIDHash != deviceHash
		if !shouldPersist {
			return nil
		}
		values := map[string]any{
			"work_status": workStatus, "latitude": *req.Latitude, "longitude": *req.Longitude, "coordinate_system": coordinate.GCJ02,
			"accuracy_m": *req.AccuracyM, "captured_at": capturedAt, "heartbeat_at": now,
			"device_id_hash": deviceHash, "last_sequence": req.Sequence, "version": gorm.Expr("version+1"),
		}
		if !runtimeExists {
			runtime = RiderRuntimeState{RiderID: riderID, WorkStatus: workStatus, Latitude: req.Latitude, Longitude: req.Longitude, CoordinateSystem: coordinate.GCJ02, AccuracyM: req.AccuracyM, CapturedAt: &capturedAt, HeartbeatAt: &now, DeviceIDHash: &deviceHash, LastSequence: req.Sequence, Version: 1}
			if err := tx.Create(&runtime).Error; err != nil {
				return err
			}
			persisted = true
			return nil
		}
		if err := tx.Model(&RiderRuntimeState{}).Where("rider_id=?", riderID).Updates(values).Error; err != nil {
			return err
		}
		persisted = true
		return nil
	})
	if err != nil {
		details := problem.FromError(err)
		reasonCode := "persistence_failed"
		if details.ErrorCode == "RIDER_UNAVAILABLE" {
			reasonCode = "rider_unavailable"
		}
		s.createHeartbeatFailureAudit(ctx, claims, heartbeatAuditSpec{
			Action: heartbeatRejectedAction, ErrorCode: details.ErrorCode,
			ReasonCode: reasonCode, RequestStage: "eligibility",
		})
		return HeartbeatDTO{}, err
	}
	ttl := 2 * time.Duration(defaultPolicySnapshot().HeartbeatFreshSeconds) * time.Second
	if accepted == req.Sequence {
		s.syncHeartbeatCache(ctx, riderID, workStatus, req, capturedAt, ttl)
	}
	return HeartbeatDTO{ServerTime: now.Format(time.RFC3339Nano), AcceptedSequence: accepted, PresenceExpiresAt: now.Add(ttl).Format(time.RFC3339Nano), Persisted: persisted, CoordinateSystem: coordinate.GCJ02}, nil
}

func heartbeatLocationInputReason(req HeartbeatReq) string {
	if req.Latitude == nil || req.Longitude == nil || req.AccuracyM == nil {
		return "location_fields_missing"
	}
	if math.IsNaN(*req.Latitude) || math.IsInf(*req.Latitude, 0) || *req.Latitude < -90 || *req.Latitude > 90 {
		return "latitude_out_of_range"
	}
	if math.IsNaN(*req.Longitude) || math.IsInf(*req.Longitude, 0) || *req.Longitude < -180 || *req.Longitude > 180 {
		return "longitude_out_of_range"
	}
	if math.IsNaN(*req.AccuracyM) || math.IsInf(*req.AccuracyM, 0) || *req.AccuracyM < 0 || *req.AccuracyM > 1000 {
		return "accuracy_out_of_range"
	}
	return ""
}

func heartbeatAuthorizationReason(claims *auth.Claims) string {
	if claims == nil || claims.AccountType != "rider" {
		return "rider_account_required"
	}
	for _, permission := range claims.Permissions {
		if permission == "rider_location:update" {
			if _, err := parseID(claims.RiderID); err != nil {
				return "rider_identity_invalid"
			}
			return "authorization_rejected"
		}
	}
	return "location_permission_missing"
}

// heartbeatSequence 返回心跳序列号。
func heartbeatSequence(runtimeDeviceHash *string, runtimeSequence uint64, currentDeviceHash string, cachedSameDevice bool, cachedSequence uint64) uint64 {
	latest := uint64(0)
	if runtimeDeviceHash != nil && *runtimeDeviceHash == currentDeviceHash {
		latest = runtimeSequence
	}
	if cachedSameDevice && cachedSequence > latest {
		latest = cachedSequence
	}
	return latest
}

// allowFixedWindow 执行固定窗口限流。
func (s *Service) allowFixedWindow(ctx context.Context, key string, window time.Duration, limit int64) (bool, error) {
	if s.redis == nil {
		return true, nil
	}
	const script = `
local current = redis.call('INCR', KEYS[1])
if current == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return current`
	count, err := s.redis.Eval(ctx, script, []string{key}, window.Milliseconds()).Int64()
	if err != nil {
		return true, err
	}
	return count <= limit, nil
}

// rateLimited 返回速率 Limited。
func rateLimited(detail string, retryAfter time.Duration) *problem.Details {
	err := problem.TooManyRequests("RATE_LIMITED", detail)
	seconds := int(retryAfter.Round(time.Second) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	err.Data = map[string]any{"retry_after_seconds": seconds}
	return err
}

// syncWorkStatusCache 同步工作状态缓存。
func (s *Service) syncWorkStatusCache(ctx context.Context, riderID uint64, status string) {
	if s.redis == nil {
		return
	}
	presenceKey := fmt.Sprintf("dispatch:rider:presence:%d", riderID)
	if status == "offline" {
		_ = s.redis.Del(ctx, presenceKey).Err()
		for _, city := range s.riderCities(ctx, riderID) {
			_ = s.redis.ZRem(ctx, "dispatch:riders:geo:"+city, idString(riderID)).Err()
		}
		return
	}
	_ = s.redis.HSet(ctx, presenceKey, "work_status", status, "version", time.Now().UnixNano()).Err()
	_ = s.redis.Expire(ctx, presenceKey, 2*time.Minute).Err()
}

// syncHeartbeatCache 同步心跳缓存。
func (s *Service) syncHeartbeatCache(ctx context.Context, riderID uint64, status string, req HeartbeatReq, capturedAt time.Time, ttl time.Duration) {
	if s.redis == nil {
		return
	}
	presenceKey := fmt.Sprintf("dispatch:rider:presence:%d", riderID)
	_ = s.redis.HSet(ctx, presenceKey,
		"work_status", status, "sequence", req.Sequence, "captured_at", capturedAt.UnixMilli(),
		"latitude", *req.Latitude, "longitude", *req.Longitude, "accuracy_m", *req.AccuracyM,
		"device_id_hash", fmt.Sprintf("%x", sha256.Sum256([]byte(req.DeviceID))),
	).Err()
	_ = s.redis.Expire(ctx, presenceKey, ttl).Err()
	for _, city := range s.riderCities(ctx, riderID) {
		key := "dispatch:riders:geo:" + city
		if status == "online" {
			_ = s.redis.GeoAdd(ctx, key, &redis.GeoLocation{Name: idString(riderID), Longitude: *req.Longitude, Latitude: *req.Latitude}).Err()
		} else {
			_ = s.redis.ZRem(ctx, key, idString(riderID)).Err()
		}
	}
}

// riderCities 返回骑手 Cities。
func (s *Service) riderCities(ctx context.Context, riderID uint64) []string {
	var cities []string
	_ = s.db.WithContext(ctx).Table("rider_service_shops rss").
		Select("DISTINCT COALESCE(s.city_code,'unknown')").
		Joins("JOIN shops s ON s.id=rss.shop_id AND s.deleted_at IS NULL").
		Where("rss.rider_id=? AND rss.status='active'", riderID).Scan(&cities).Error
	return cities
}

// riderActor 返回骑手审计主体。
func riderActor(claims *auth.Claims, permission string) (uint64, error) {
	if claims == nil || claims.AccountType != "rider" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "rider account required")
	}
	allowed := false
	for _, item := range claims.Permissions {
		if item == permission {
			allowed = true
			break
		}
	}
	if !allowed {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "rider permission required")
	}
	id, err := parseID(claims.RiderID)
	if err != nil {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "invalid rider identity")
	}
	return id, nil
}

// errorsIsNotFound 判断错误是否表示记录不存在。
func errorsIsNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }
