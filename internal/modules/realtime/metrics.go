package realtime

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
)

type metricState struct {
	mu            sync.RWMutex
	connections   int64
	tickets       map[string]uint64
	materialized  map[string]uint64
	relays        map[string]uint64
	sends         map[string]uint64
	acks          map[string]uint64
	sessionCloses map[string]uint64
	lagSum        map[string]float64
	lagCount      map[string]uint64
	lagBuckets    map[string][]uint64
	slowCloses    uint64
	instanceID    string
}

// newMetricState 创建并初始化指标状态。
func newMetricState(registry *metrics.Registry, instanceID string) *metricState {
	state := &metricState{
		tickets: make(map[string]uint64), materialized: make(map[string]uint64),
		relays: make(map[string]uint64), sends: make(map[string]uint64), acks: make(map[string]uint64),
		sessionCloses: make(map[string]uint64), lagSum: make(map[string]float64), lagCount: make(map[string]uint64),
		lagBuckets: make(map[string][]uint64),
		instanceID: instanceID,
	}
	if registry != nil {
		registry.AddCollector(state.collect)
	}
	return state
}

// incPair 递增配对指标计数。
func (m *metricState) incPair(target map[string]uint64, first, second string) {
	m.inc(target, first+"\x00"+second)
}

// inc 递增实时消息指标计数。
func (m *metricState) inc(target map[string]uint64, key string) {
	m.add(target, key, 1)
}

// add 添加实时消息。
func (m *metricState) add(target map[string]uint64, key string, value uint64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	target[key] += value
	m.mu.Unlock()
}

// connection 处理连接相关逻辑。
func (m *metricState) connection(delta int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.connections += delta
	m.mu.Unlock()
}

// slowClose 记录缓慢关闭。
func (m *metricState) slowClose() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.slowCloses++
	m.mu.Unlock()
}

// sessionClose 处理会话 Close相关逻辑。
func (m *metricState) sessionClose(reason string) {
	if m == nil {
		return
	}
	m.inc(m.sessionCloses, reason)
}

// observeLag 记录Lag观测指标。
func (m *metricState) observeLag(eventType string, lag time.Duration) {
	if m == nil || eventType == "" {
		return
	}
	seconds := lag.Seconds()
	if seconds < 0 {
		seconds = 0
	}
	m.mu.Lock()
	m.lagSum[eventType] += seconds
	m.lagCount[eventType]++
	buckets := m.lagBuckets[eventType]
	if buckets == nil {
		buckets = make([]uint64, len(realtimeLagBounds)+1)
	}
	for index, bound := range realtimeLagBounds {
		if seconds <= bound {
			buckets[index]++
		}
	}
	buckets[len(buckets)-1]++
	m.lagBuckets[eventType] = buckets
	m.mu.Unlock()
}

var realtimeLagBounds = []float64{0.1, 0.25, 0.5, 1, 3, 10}

// collect 收集Sample列表。
func (m *metricState) collect() []metrics.Sample {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := []metrics.Sample{
		{Name: "jxe_realtime_connections", Help: "Active realtime WebSocket connections.", Type: "gauge", Labels: map[string]string{"instance": m.instanceID, "state": "active"}, Value: float64(m.connections)},
		{Name: "jxe_realtime_slow_consumer_close_total", Help: "Realtime connections closed because their send queue was full.", Type: "counter", Labels: map[string]string{"instance": m.instanceID}, Value: float64(m.slowCloses)},
	}
	appendMap := func(name, help, label string, values map[string]uint64) {
		for key, value := range values {
			result = append(result, metrics.Sample{Name: name, Help: help, Type: "counter", Labels: map[string]string{label: key}, Value: float64(value)})
		}
	}
	appendPair := func(name, help, firstLabel, secondLabel string, values map[string]uint64) {
		for key, value := range values {
			parts := strings.SplitN(key, "\x00", 2)
			if len(parts) != 2 {
				continue
			}
			result = append(result, metrics.Sample{Name: name, Help: help, Type: "counter", Labels: map[string]string{firstLabel: parts[0], secondLabel: parts[1]}, Value: float64(value)})
		}
	}
	appendMap("jxe_realtime_ticket_total", "Realtime ticket issue and consume results.", "result", m.tickets)
	appendPair("jxe_realtime_delivery_materialized_total", "Materialized realtime delivery rows.", "event_type", "result", m.materialized)
	appendMap("jxe_realtime_relay_total", "Realtime Redis relay results.", "result", m.relays)
	appendPair("jxe_realtime_socket_send_total", "Realtime WebSocket send results.", "event_type", "result", m.sends)
	appendPair("jxe_realtime_ack_total", "Realtime acknowledgement outcomes.", "outcome", "result", m.acks)
	appendMap("jxe_realtime_session_close_total", "Realtime session closes by stable reason.", "reason", m.sessionCloses)
	const lagMetric = "jxe_realtime_delivery_lag_seconds"
	for eventType, sum := range m.lagSum {
		count := m.lagCount[eventType]
		if count > 0 {
			for index, value := range m.lagBuckets[eventType] {
				upperBound := "+Inf"
				if index < len(realtimeLagBounds) {
					upperBound = strconv.FormatFloat(realtimeLagBounds[index], 'f', -1, 64)
				}
				result = append(result, metrics.Sample{Name: lagMetric + "_bucket", Family: lagMetric, Help: "Domain event to socket write lag.", Type: "histogram", Labels: map[string]string{"event_type": eventType, "le": upperBound}, Value: float64(value)})
			}
			result = append(result,
				metrics.Sample{Name: lagMetric + "_sum", Family: lagMetric, Help: "Domain event to socket write lag.", Type: "histogram", Labels: map[string]string{"event_type": eventType}, Value: sum},
				metrics.Sample{Name: lagMetric + "_count", Family: lagMetric, Help: "Domain event to socket write lag.", Type: "histogram", Labels: map[string]string{"event_type": eventType}, Value: float64(count)},
			)
		}
	}
	return result
}

// registerDatabaseMetrics 注册Database 指标。
func registerDatabaseMetrics(db *gorm.DB, registry *metrics.Registry, instanceID string) {
	if db == nil || registry == nil {
		return
	}
	registry.AddCollector(func() []metrics.Sample {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		now := time.Now().UTC()
		var backlog int64
		_ = db.WithContext(ctx).Model(&Delivery{}).Where("relay_status=? AND expires_at>?", relayPending, now).Count(&backlog).Error
		result := []metrics.Sample{{Name: "jxe_realtime_relay_backlog", Help: "Pending realtime Redis wakeups.", Type: "gauge", Labels: map[string]string{"instance": instanceID}, Value: float64(backlog)}}
		type unackedRow struct {
			EventType string
			Count     int64
		}
		var rows []unackedRow
		_ = db.WithContext(ctx).Table("realtime_deliveries d").
			Select("d.client_event_type AS event_type,COUNT(*) AS count").
			Joins("LEFT JOIN realtime_acknowledgements a ON a.realtime_delivery_id=d.id").
			Where("d.expires_at>? AND a.id IS NULL", now).Group("d.client_event_type").Scan(&rows).Error
		for _, row := range rows {
			result = append(result, metrics.Sample{Name: "jxe_realtime_unacked_active", Help: "Unacknowledged non-expired realtime deliveries.", Type: "gauge", Labels: map[string]string{"event_type": row.EventType}, Value: float64(row.Count)})
		}
		return result
	})
}
