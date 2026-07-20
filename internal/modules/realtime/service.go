package realtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

const (
	ticketKeyPrefix = "realtime:ticket:"
	wakeupChannel   = "realtime:wakeup:v1"
)

var consumeTicketScript = redis.NewScript(`
local value = redis.call('GET', KEYS[1])
if not value then return false end
redis.call('DEL', KEYS[1])
return value
`)

var rateLimitScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
return count
`)

type Service struct {
	cfg     config.Config
	db      *gorm.DB
	redis   *redis.Client
	ids     *snowflake.Generator
	metrics *metricState
}

// NewService 创建并初始化服务。
func NewService(cfg config.Config, db *gorm.DB, redisClient *redis.Client, ids *snowflake.Generator, metrics *metricState) *Service {
	if metrics == nil {
		metrics = newMetricState(nil, cfg.App.InstanceID)
	}
	return &Service{cfg: cfg, db: db, redis: redisClient, ids: ids, metrics: metrics}
}

// IssueTicket 返回Issue Ticket。
func (s *Service) IssueTicket(ctx context.Context, claims *auth.Claims, req TicketRequest, clientIP string) (TicketResponse, error) {
	if !s.cfg.Realtime.Enabled {
		return TicketResponse{}, realtimeDisabled()
	}
	if s.redis == nil {
		return TicketResponse{}, realtimeUnavailable("realtime dependencies are unavailable")
	}
	if err := req.Validate(); err != nil {
		return TicketResponse{}, problem.InvalidArgument("REALTIME_TICKET_REQUEST_INVALID", err.Error())
	}
	if claims == nil || claims.AccountType != "rider" || claims.RiderID == "" || claims.TokenType != "access" {
		return TicketResponse{}, problem.Forbidden("REALTIME_RIDER_REQUIRED", "a rider access token is required")
	}
	riderID, err := strconv.ParseUint(claims.RiderID, 10, 64)
	if err != nil || riderID == 0 || claims.ExpiresAt == nil || claims.ExpiresAt.Time.Before(time.Now()) {
		return TicketResponse{}, problem.Unauthorized("AUTH_TOKEN_INVALID", "access token is invalid")
	}
	if !s.canaryAllowed(claims.RiderID) {
		return TicketResponse{}, problem.Forbidden("REALTIME_RIDER_NOT_ENABLED", "rider is not enabled for realtime")
	}
	if err := s.checkRate(ctx, "ticket:rider:"+claims.RiderID, int64(s.cfg.Realtime.TicketRiderRatePerMinute)); err != nil {
		return TicketResponse{}, err
	}
	if clientIP != "" {
		if err := s.checkRate(ctx, "ticket:ip:"+hashString(clientIP), int64(s.cfg.Realtime.TicketIPRatePerMinute)); err != nil {
			return TicketResponse{}, err
		}
	}

	raw, err := randomTicket()
	if err != nil {
		return TicketResponse{}, err
	}
	now := time.Now().UTC()
	expiresAt := now.Add(s.cfg.Realtime.TicketTTL)
	if claims.ExpiresAt.Time.Before(expiresAt) {
		expiresAt = claims.ExpiresAt.Time.UTC()
	}
	info := TicketInfo{
		RiderID: riderID, AccountType: claims.AccountType, AccountID: claims.AccountID,
		SessionID: claims.SessionID, AccessJTI: claims.ID, AccessExpiresAt: claims.ExpiresAt.Time.UTC(),
		DeviceHash: hashString(req.DeviceID), Platform: req.Platform, ClientVersion: req.ClientVersion,
		ProtocolVersion: req.ProtocolVersion,
	}
	valid, err := s.SessionValid(ctx, info)
	if err != nil {
		return TicketResponse{}, err
	}
	if !valid {
		return TicketResponse{}, problem.Unauthorized("REALTIME_SESSION_REVOKED", "session revoked")
	}
	encoded, err := json.Marshal(info)
	if err != nil {
		return TicketResponse{}, err
	}
	ttl := time.Until(expiresAt)
	if ttl <= 0 {
		return TicketResponse{}, problem.Unauthorized("AUTH_TOKEN_EXPIRED", "access token expired")
	}
	if err := s.redis.Set(ctx, ticketKey(raw), encoded, ttl).Err(); err != nil {
		s.metrics.inc(s.metrics.tickets, "error")
		return TicketResponse{}, realtimeUnavailable("realtime Redis is unavailable")
	}
	s.metrics.inc(s.metrics.tickets, "issued")
	return TicketResponse{
		Ticket: raw, ExpiresAt: expiresAt, WSPath: "/api/v1/realtime/ws",
		HeartbeatIntervalSeconds: int(s.cfg.Realtime.HeartbeatInterval.Seconds()),
		MaxResumeItems:           s.cfg.Realtime.ResumeLimit, ProtocolVersion: ProtocolVersion,
	}, nil
}

// ConsumeTicket 消费并处理Ticket。
func (s *Service) ConsumeTicket(ctx context.Context, raw string) (TicketInfo, error) {
	invalid := problem.Unauthorized("REALTIME_TICKET_EXPIRED", "ticket expired or invalid")
	if !s.cfg.Realtime.Enabled {
		return TicketInfo{}, realtimeDisabled()
	}
	if s.redis == nil {
		return TicketInfo{}, realtimeUnavailable("realtime Redis is unavailable")
	}
	if len(raw) < 20 || len(raw) > 256 {
		return TicketInfo{}, invalid
	}
	value, err := consumeTicketScript.Run(ctx, s.redis, []string{ticketKey(raw)}).Text()
	if errors.Is(err, redis.Nil) || value == "" {
		s.metrics.inc(s.metrics.tickets, "invalid")
		return TicketInfo{}, invalid
	}
	if err != nil {
		s.metrics.inc(s.metrics.tickets, "error")
		return TicketInfo{}, realtimeUnavailable("realtime Redis is unavailable")
	}
	var info TicketInfo
	if json.Unmarshal([]byte(value), &info) != nil || info.RiderID == 0 || info.ProtocolVersion != ProtocolVersion || info.AccessExpiresAt.Before(time.Now()) {
		s.metrics.inc(s.metrics.tickets, "invalid")
		return TicketInfo{}, invalid
	}
	valid, err := s.SessionValid(ctx, info)
	if err != nil {
		return TicketInfo{}, realtimeUnavailable("realtime Redis is unavailable")
	}
	if !valid {
		s.metrics.inc(s.metrics.tickets, "revoked")
		return TicketInfo{}, problem.Unauthorized("REALTIME_SESSION_REVOKED", "session revoked")
	}
	s.metrics.inc(s.metrics.tickets, "consumed")
	return info, nil
}

// SessionValid 返回会话有效。
func (s *Service) SessionValid(ctx context.Context, info TicketInfo) (bool, error) {
	if s.redis == nil {
		return false, realtimeUnavailable("realtime Redis is unavailable")
	}
	if info.AccessExpiresAt.Before(time.Now()) {
		return false, nil
	}
	key := "session:" + info.AccountType + ":" + info.AccountID + ":" + info.SessionID
	stored, err := s.redis.HGet(ctx, key, "access_jti").Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, realtimeUnavailable("realtime Redis is unavailable")
	}
	return stored == info.AccessJTI, nil
}

// Resume 返回Resume。
func (s *Service) Resume(ctx context.Context, info TicketInfo, afterID uint64) ([]Delivery, bool, error) {
	if s.db == nil {
		return nil, false, realtimeUnavailable("realtime database is unavailable")
	}
	limit := s.cfg.Realtime.ResumeLimit
	var rows []Delivery
	err := s.db.WithContext(ctx).
		Where("recipient_type=? AND recipient_id=? AND id>? AND expires_at>?", recipientRider, info.RiderID, afterID, time.Now()).
		Where("NOT (client_event_type IN ? AND EXISTS (SELECT 1 FROM realtime_acknowledgements a WHERE a.realtime_delivery_id=realtime_deliveries.id AND a.device_hash=? AND a.ack_type='closed'))", []string{"dispatch.offer.closed", "dispatch.grab.closed"}, info.DeviceHash).
		Order("id").Limit(limit + 1).Find(&rows).Error
	if err != nil {
		return nil, false, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	return rows, hasMore, nil
}

// LoadDelivery 加载配送。
func (s *Service) LoadDelivery(ctx context.Context, riderID, deliveryID uint64) (Delivery, error) {
	var row Delivery
	if s.db == nil {
		return row, realtimeUnavailable("realtime database is unavailable")
	}
	err := s.db.WithContext(ctx).Where("id=? AND recipient_type=? AND recipient_id=? AND expires_at>?", deliveryID, recipientRider, riderID, time.Now()).First(&row).Error
	return row, err
}

// Acknowledge 返回Acknowledge。
func (s *Service) Acknowledge(ctx context.Context, info TicketInfo, frame ClientFrame) error {
	if !allowedAckTypes[frame.Outcome] {
		return problem.InvalidArgument("REALTIME_ACK_OUTCOME_INVALID", "ack outcome is unsupported")
	}
	deliveryID, err := parseID(frame.DeliveryID)
	if err != nil {
		return problem.InvalidArgument("REALTIME_DELIVERY_ID_INVALID", "delivery_id is invalid")
	}
	if s.db == nil || s.ids == nil {
		return realtimeUnavailable("realtime database is unavailable")
	}
	var count int64
	if err := s.db.WithContext(ctx).Model(&Delivery{}).Where("id=? AND recipient_type=? AND recipient_id=?", deliveryID, recipientRider, info.RiderID).Count(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return problem.Forbidden("REALTIME_DELIVERY_FORBIDDEN", "delivery unavailable")
	}
	var errorCode *string
	if frame.ErrorCode != "" {
		value := frame.ErrorCode
		if len(value) > 64 {
			value = value[:64]
		}
		errorCode = &value
	}
	row := Acknowledgement{
		ID: s.ids.Next(), RealtimeDeliveryID: deliveryID, RiderID: info.RiderID, DeviceHash: info.DeviceHash,
		AckType: frame.Outcome, ClientOccurredAt: frame.ClientAt, ReceivedAt: time.Now().UTC(),
		ErrorCode: errorCode, ClientVersion: info.ClientVersion, Platform: info.Platform,
	}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		s.metrics.incPair(s.metrics.acks, frame.Outcome, "error")
		return err
	}
	s.metrics.incPair(s.metrics.acks, frame.Outcome, "accepted")
	return nil
}

// PublishWakeup 发布Wakeup。
func (s *Service) PublishWakeup(ctx context.Context, row Delivery) error {
	return s.PublishWakeups(ctx, []Delivery{row})
}

// PublishWakeups 发布Wakeups。
func (s *Service) PublishWakeups(ctx context.Context, rows []Delivery) error {
	if len(rows) == 0 {
		return nil
	}
	if s.redis == nil {
		return realtimeUnavailable("realtime Redis is unavailable")
	}
	payloads := make([][]byte, 0, len(rows))
	ids := make([]uint64, 0, len(rows))
	for _, row := range rows {
		payload, err := json.Marshal(Wakeup{DeliveryID: row.ID, RiderID: row.RecipientID, ExpiresAt: row.ExpiresAt})
		if err != nil {
			return err
		}
		payloads = append(payloads, payload)
		ids = append(ids, row.ID)
	}
	_, err := s.redis.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for _, payload := range payloads {
			pipe.Publish(ctx, wakeupChannel, payload)
		}
		return nil
	})
	if err != nil {
		s.metrics.inc(s.metrics.relays, "error")
		return err
	}
	now := time.Now().UTC()
	if s.db != nil {
		if err := s.db.WithContext(ctx).Model(&Delivery{}).Where("id IN ? AND relay_status IN ?", ids, []string{relayPending, relayRelayed}).Updates(map[string]any{"relay_status": relayRelayed, "relayed_at": now, "last_error_code": nil}).Error; err != nil {
			s.metrics.inc(s.metrics.relays, "mark_error")
			return err
		}
	}
	s.metrics.add(s.metrics.relays, "published", uint64(len(rows)))
	return nil
}

// CheckUpgradeRate 检查Upgrade 速率是否满足要求。
func (s *Service) CheckUpgradeRate(ctx context.Context, clientIP string) error {
	if s.redis == nil {
		return realtimeUnavailable("realtime Redis is unavailable")
	}
	if strings.TrimSpace(clientIP) == "" {
		return nil
	}
	return s.checkRate(ctx, "handshake:ip:"+hashString(clientIP), int64(s.cfg.Realtime.HandshakeIPRatePerMinute))
}

// checkRate 检查速率是否满足要求。
func (s *Service) checkRate(ctx context.Context, dimension string, maximum int64) error {
	key := "ratelimit:realtime:" + dimension + ":" + time.Now().UTC().Format("200601021504")
	count, err := rateLimitScript.Run(ctx, s.redis, []string{key}, time.Minute.Milliseconds()).Int64()
	if err != nil {
		return realtimeUnavailable("realtime Redis is unavailable")
	}
	if count > maximum {
		detail := problem.TooManyRequests("REALTIME_RATE_LIMITED", "rate limited")
		detail.Data = map[string]any{"retry_after_seconds": 60}
		return detail
	}
	return nil
}

// canaryAllowed 判断canary 允许状态。
func (s *Service) canaryAllowed(riderID string) bool {
	if len(s.cfg.Realtime.CanaryRiderIDs) == 0 {
		return true
	}
	for _, allowed := range s.cfg.Realtime.CanaryRiderIDs {
		if riderID == allowed {
			return true
		}
	}
	return false
}

// randomTicket 返回random Ticket。
func randomTicket() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return "rtk_" + base64.RawURLEncoding.EncodeToString(buffer), nil
}

// ticketKey 返回ticket 密钥。
func ticketKey(raw string) string { return ticketKeyPrefix + hashString(raw) }

// hashString 计算字符串的哈希值。
func hashString(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// realtimeUnavailable 返回实时消息 Unavailable。
func realtimeUnavailable(detail string) error {
	return problem.New(503, "REALTIME_DEPENDENCY_UNAVAILABLE", "Service Unavailable", detail)
}

// realtimeDisabled 返回实时消息 Disabled。
func realtimeDisabled() error {
	return problem.New(503, "REALTIME_DISABLED", "Service Unavailable", "realtime service is disabled")
}

// idFromPayload 返回ID From 载荷。
func idFromPayload(payload map[string]any, key string) (uint64, error) {
	value, ok := payload[key]
	if !ok {
		return 0, fmt.Errorf("%s is missing", key)
	}
	return parseID(fmt.Sprint(value))
}

// optionalIDFromPayload 返回optional ID From 载荷。
func optionalIDFromPayload(payload map[string]any, key string) uint64 {
	value, ok := payload[key]
	if !ok || value == nil || strings.TrimSpace(fmt.Sprint(value)) == "" {
		return 0
	}
	id, _ := strconv.ParseUint(fmt.Sprint(value), 10, 64)
	return id
}
