package realtime

import "testing"

// TestHubEnforcesPerRiderConnectionLimit 验证消息中心限制每个骑手连接限制的预期行为。
func TestHubEnforcesPerRiderConnectionLimit(t *testing.T) {
	cfg := realtimeTestConfig().Realtime
	cfg.MaxConnectionsPerRider = 2
	metrics := newMetricState(nil, "test")
	hub := NewHub(cfg, nil, nil, metrics, nil)
	first := &connection{id: "one", info: TicketInfo{RiderID: 101}}
	second := &connection{id: "two", info: TicketInfo{RiderID: 101}}
	third := &connection{id: "three", info: TicketInfo{RiderID: 101}}
	if err := hub.register(first); err != nil {
		t.Fatal(err)
	}
	if err := hub.register(second); err != nil {
		t.Fatal(err)
	}
	if err := hub.register(third); err == nil {
		t.Fatal("expected connection limit")
	}
	hub.unregister(first)
	if err := hub.register(third); err != nil {
		t.Fatalf("slot was not released: %v", err)
	}
}

// TestRealtimeMetricsDoNotUseSensitiveHighCardinalityLabels 验证实时消息指标 Do 不使用敏感信息高基数标签的预期行为。
func TestRealtimeMetricsDoNotUseSensitiveHighCardinalityLabels(t *testing.T) {
	metrics := newMetricState(nil, "instance-test")
	metrics.inc(metrics.tickets, "issued")
	metrics.incPair(metrics.acks, "sound_played", "accepted")
	for _, sample := range metrics.collect() {
		for label := range sample.Labels {
			if label == "rider_id" || label == "device_id" || label == "device_hash" || label == "ticket" {
				t.Fatalf("sensitive/high-cardinality metric label %q in %s", label, sample.Name)
			}
		}
	}
}
