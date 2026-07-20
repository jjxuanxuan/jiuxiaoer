package realtime

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestTicketIsOpaqueOneTimeAndSessionBound 验证Ticket Is Opaque One 时间 And 会话 Bound的预期行为。
func TestTicketIsOpaqueOneTimeAndSessionBound(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cfg := realtimeTestConfig()
	service := NewService(cfg, nil, client, snowflake.New(1), nil)
	claims := riderClaims("101", time.Now().Add(time.Hour))
	key := "session:rider:" + claims.AccountID + ":" + claims.SessionID
	if err := client.HSet(context.Background(), key, "access_jti", claims.ID).Err(); err != nil {
		t.Fatal(err)
	}

	response, err := service.IssueTicket(context.Background(), claims, TicketRequest{
		DeviceID: "device-raw-secret", Platform: "weapp", ClientVersion: "1.0.0", ProtocolVersion: 1,
	}, "127.0.0.1")
	if err != nil {
		t.Fatalf("issue ticket: %v", err)
	}
	if !strings.HasPrefix(response.Ticket, "rtk_") || response.WSPath != "/api/v1/realtime/ws" || response.HeartbeatIntervalSeconds != 25 || response.MaxResumeItems != 100 {
		t.Fatalf("unexpected ticket response: %+v", response)
	}
	for _, key := range mini.Keys() {
		if strings.Contains(key, response.Ticket) || strings.Contains(key, "device-raw-secret") {
			t.Fatalf("raw credential leaked into Redis key %q", key)
		}
	}

	var successes atomic.Int32
	var wait sync.WaitGroup
	for range 20 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if info, consumeErr := service.ConsumeTicket(context.Background(), response.Ticket); consumeErr == nil && info.RiderID == 101 {
				successes.Add(1)
			}
		}()
	}
	wait.Wait()
	if successes.Load() != 1 {
		t.Fatalf("ticket must be consumed exactly once, successes=%d", successes.Load())
	}
}

// TestAcknowledgementAuthorizationIdempotencyAndResumeCloseFilter 验证Acknowledgement Authorization 幂等 And Resume Close 筛选条件的预期行为。
func TestAcknowledgementAuthorizationIdempotencyAndResumeCloseFilter(t *testing.T) {
	db := realtimeSQLite(t)
	cfg := realtimeTestConfig()
	service := NewService(cfg, db, nil, snowflake.New(2), nil)
	now := time.Now().UTC()
	rows := []Delivery{
		{ID: 1001, SourceEventID: "source-1", SourceEventType: "dispatch.offer.created", ClientEventType: "dispatch.offer.opened", RecipientType: recipientRider, RecipientID: 101, AggregateType: "dispatch_offer", AggregateID: 501, PayloadSnapshot: []byte(`{"offer_id":"501"}`), OccurredAt: now, ExpiresAt: now.Add(time.Hour), RelayStatus: relayRelayed, NextRelayAt: now},
		{ID: 1002, SourceEventID: "source-2", SourceEventType: "dispatch.offer.expired", ClientEventType: "dispatch.offer.closed", RecipientType: recipientRider, RecipientID: 101, AggregateType: "dispatch_offer", AggregateID: 501, PayloadSnapshot: []byte(`{"offer_id":"501","reason_code":"expired"}`), OccurredAt: now, ExpiresAt: now.Add(time.Hour), RelayStatus: relayRelayed, NextRelayAt: now},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	owner := TicketInfo{RiderID: 101, DeviceHash: hashString("device-a"), Platform: "weapp", ClientVersion: "1.0.0"}
	other := owner
	other.RiderID = 202
	frame := ClientFrame{DeliveryID: "1001", Outcome: "displayed"}
	if err := service.Acknowledge(context.Background(), other, frame); err == nil || problem.FromError(err).ErrorCode != "REALTIME_DELIVERY_FORBIDDEN" {
		t.Fatalf("cross-rider ACK must fail without disclosure, got %v", err)
	}
	if err := service.Acknowledge(context.Background(), owner, frame); err != nil {
		t.Fatal(err)
	}
	if err := service.Acknowledge(context.Background(), owner, frame); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&Acknowledgement{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("duplicate ACK must be idempotent, count=%d err=%v", count, err)
	}
	if err := service.Acknowledge(context.Background(), owner, ClientFrame{DeliveryID: "1002", Outcome: "closed"}); err != nil {
		t.Fatal(err)
	}
	resumed, _, err := service.Resume(context.Background(), owner, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range resumed {
		if row.ID == 1002 {
			t.Fatal("a close event ACKed by this device must not be replayed")
		}
	}
}

// realtimeTestConfig 返回实时消息 Test 配置。
func realtimeTestConfig() config.Config {
	cfg := config.Load()
	cfg.App.InstanceID = "realtime-test"
	cfg.Realtime.Enabled = true
	cfg.Realtime.RelayEnabled = true
	cfg.Realtime.TicketTTL = time.Minute
	cfg.Realtime.HandshakeTimeout = time.Second
	cfg.Realtime.HeartbeatInterval = 25 * time.Second
	cfg.Realtime.PongTimeout = time.Minute
	cfg.Realtime.SessionCheckInterval = time.Minute
	cfg.Realtime.ShutdownDrainTimeout = 10 * time.Millisecond
	cfg.Realtime.RelayInterval = time.Millisecond
	cfg.Realtime.RelayBatchSize = 100
	cfg.Realtime.ResumeLimit = 100
	cfg.Realtime.SendQueueSize = 32
	cfg.Realtime.MaxFrameBytes = 8192
	cfg.Realtime.MaxConnectionsPerRider = 3
	cfg.Realtime.TicketRiderRatePerMinute = 10
	cfg.Realtime.TicketIPRatePerMinute = 30
	cfg.Realtime.HandshakeIPRatePerMinute = 60
	cfg.Realtime.ACKRatePerMinute = 120
	cfg.Realtime.ResumeRatePerMinute = 6
	return cfg
}

// riderClaims 返回骑手认证声明。
func riderClaims(riderID string, expiresAt time.Time) *auth.Claims {
	return &auth.Claims{
		TokenType: "access", SessionID: "session-1", AccountType: "rider", AccountID: "9001", RiderID: riderID,
		RegisteredClaims: jwt.RegisteredClaims{ID: "access-jti-1", ExpiresAt: jwt.NewNumericDate(expiresAt)},
	}
}

// realtimeSQLite 返回实时消息 SQ Lite。
func realtimeSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Delivery{}, &Acknowledgement{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS uk_test_ack ON realtime_acknowledgements(realtime_delivery_id,device_hash,ack_type)").Error; err != nil {
		t.Fatal(err)
	}
	return db
}
