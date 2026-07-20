package realtime

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestRelayWorkerRelaysLiveAndExpiresStaleRows 验证转发工作器 Relays Live And 过期时间 Stale Rows的预期行为。
func TestRelayWorkerRelaysLiveAndExpiresStaleRows(t *testing.T) {
	db := realtimeSQLite(t)
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cfg := realtimeTestConfig()
	service := NewService(cfg, db, client, snowflake.New(3), nil)
	worker := NewRelayWorker(cfg.Realtime, db, service, "test", nil)
	now := time.Now().UTC()
	rows := []Delivery{
		{ID: 2001, SourceEventID: "relay-live", SourceEventType: "dispatch.offer.created", ClientEventType: "dispatch.offer.opened", RecipientType: recipientRider, RecipientID: 101, AggregateType: "dispatch_offer", AggregateID: 501, PayloadSnapshot: []byte(`{"offer_id":"501"}`), OccurredAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), RelayStatus: relayPending, NextRelayAt: now.Add(-time.Second)},
		{ID: 2002, SourceEventID: "relay-expired", SourceEventType: "dispatch.offer.created", ClientEventType: "dispatch.offer.opened", RecipientType: recipientRider, RecipientID: 101, AggregateType: "dispatch_offer", AggregateID: 502, PayloadSnapshot: []byte(`{"offer_id":"502"}`), OccurredAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Second), RelayStatus: relayPending, NextRelayAt: now.Add(-time.Second)},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var live, expired Delivery
	if err := db.First(&live, 2001).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&expired, 2002).Error; err != nil {
		t.Fatal(err)
	}
	if live.RelayStatus != relayRelayed || expired.RelayStatus != relayExpired {
		t.Fatalf("unexpected relay states live=%s expired=%s", live.RelayStatus, expired.RelayStatus)
	}
}
