package deliveryincident

import (
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

type metricState struct {
	mu                 sync.RWMutex
	requests           map[string]uint64
	durations          map[string]float64
	durationN          map[string]uint64
	samples            map[string][]float64
	ackSum             map[string]float64
	ackN               map[string]uint64
	ackSample          map[string][]float64
	rateLimited        map[string]uint64
	rateDegraded       map[string]uint64
	locationSuppressed map[string]uint64
	db                 *gorm.DB
}

func newMetricState(db *gorm.DB, registry *metrics.Registry) *metricState {
	state := &metricState{requests: map[string]uint64{}, durations: map[string]float64{}, durationN: map[string]uint64{}, samples: map[string][]float64{},
		ackSum: map[string]float64{}, ackN: map[string]uint64{}, ackSample: map[string][]float64{}, rateLimited: map[string]uint64{}, rateDegraded: map[string]uint64{},
		locationSuppressed: map[string]uint64{}, db: db}
	if registry != nil {
		registry.AddCollector(state.collect)
	}
	return state
}

func (m *metricState) incRateLimited(scope string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.rateLimited[scope]++
	m.mu.Unlock()
}

func (m *metricState) incRateLimiterDegraded(scope string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.rateDegraded[scope]++
	m.mu.Unlock()
}

func (m *metricState) incLocationDistanceSuppressed(reason string) {
	if m == nil || strings.TrimSpace(reason) == "" {
		return
	}
	m.mu.Lock()
	m.locationSuppressed[reason]++
	m.mu.Unlock()
}

func (m *metricState) observeAcknowledge(incidentType string, duration time.Duration) {
	if m == nil || duration < 0 {
		return
	}
	m.mu.Lock()
	m.ackSum[incidentType] += duration.Seconds()
	m.ackN[incidentType]++
	m.ackSample[incidentType] = append(m.ackSample[incidentType], duration.Seconds())
	if len(m.ackSample[incidentType]) > 2048 {
		m.ackSample[incidentType] = append([]float64(nil), m.ackSample[incidentType][1024:]...)
	}
	m.mu.Unlock()
}

func (m *metricState) observe(action, incidentType string, err error, duration time.Duration) {
	if m == nil {
		return
	}
	result, code := "success", ""
	if err != nil {
		result, code = "error", "INTERNAL_ERROR"
		var details *problem.Details
		if errors.As(err, &details) {
			code = details.ErrorCode
		}
	}
	if strings.TrimSpace(incidentType) == "" {
		incidentType = "unknown"
	}
	key := action + "\x00" + incidentType + "\x00" + result + "\x00" + code
	m.mu.Lock()
	m.requests[key]++
	m.durations[action] += duration.Seconds()
	m.durationN[action]++
	m.samples[action] = append(m.samples[action], duration.Seconds())
	if len(m.samples[action]) > 2048 {
		m.samples[action] = append([]float64(nil), m.samples[action][1024:]...)
	}
	m.mu.Unlock()
}

func (m *metricState) collect() []metrics.Sample {
	m.mu.RLock()
	requests := make(map[string]uint64, len(m.requests))
	for key, value := range m.requests {
		requests[key] = value
	}
	durations := make(map[string]float64, len(m.durations))
	counts := make(map[string]uint64, len(m.durationN))
	durationSamples := make(map[string][]float64, len(m.samples))
	ackSums := make(map[string]float64, len(m.ackSum))
	ackCounts := make(map[string]uint64, len(m.ackN))
	ackSamples := make(map[string][]float64, len(m.ackSample))
	rateLimited := make(map[string]uint64, len(m.rateLimited))
	rateDegraded := make(map[string]uint64, len(m.rateDegraded))
	locationSuppressed := make(map[string]uint64, len(m.locationSuppressed))
	for key, value := range m.durations {
		durations[key] = value
	}
	for key, value := range m.durationN {
		counts[key] = value
	}
	for key, values := range m.samples {
		durationSamples[key] = append([]float64(nil), values...)
	}
	for key, value := range m.ackSum {
		ackSums[key] = value
	}
	for key, value := range m.ackN {
		ackCounts[key] = value
	}
	for key, values := range m.ackSample {
		ackSamples[key] = append([]float64(nil), values...)
	}
	for key, value := range m.rateLimited {
		rateLimited[key] = value
	}
	for key, value := range m.rateDegraded {
		rateDegraded[key] = value
	}
	for key, value := range m.locationSuppressed {
		locationSuppressed[key] = value
	}
	m.mu.RUnlock()

	samples := make([]metrics.Sample, 0, len(requests)+len(durations)*2+len(rateLimited)+len(rateDegraded)+len(locationSuppressed)+10)
	for key, value := range requests {
		parts := strings.Split(key, "\x00")
		if len(parts) != 4 {
			continue
		}
		samples = append(samples, metrics.Sample{Name: "jxe_delivery_incident_requests_total", Help: "Delivery incident requests by action, type and result.", Type: "counter",
			Labels: map[string]string{"action": parts[0], "type": parts[1], "result": parts[2], "code": parts[3]}, Value: float64(value)})
	}
	for action, value := range durations {
		labels := map[string]string{"action": action}
		samples = append(samples,
			metrics.Sample{Name: "jxe_delivery_incident_request_duration_seconds_sum", Family: "jxe_delivery_incident_request_duration_seconds", Help: "Delivery incident request duration.", Type: "summary", Labels: labels, Value: value},
			metrics.Sample{Name: "jxe_delivery_incident_request_duration_seconds_count", Family: "jxe_delivery_incident_request_duration_seconds", Labels: labels, Value: float64(counts[action])},
		)
		values := durationSamples[action]
		if len(values) > 0 {
			sort.Float64s(values)
			index := int(math.Ceil(float64(len(values))*0.99)) - 1
			if index < 0 {
				index = 0
			}
			samples = append(samples, metrics.Sample{Name: "jxe_delivery_incident_request_duration_seconds", Family: "jxe_delivery_incident_request_duration_seconds",
				Labels: map[string]string{"action": action, "quantile": "0.99"}, Value: values[index]})
		}
	}
	for incidentType, value := range ackSums {
		labels := map[string]string{"type": incidentType}
		samples = append(samples,
			metrics.Sample{Name: "jxe_delivery_incident_ack_latency_seconds_sum", Family: "jxe_delivery_incident_ack_latency_seconds", Help: "Delivery incident acknowledgement latency.", Type: "summary", Labels: labels, Value: value},
			metrics.Sample{Name: "jxe_delivery_incident_ack_latency_seconds_count", Family: "jxe_delivery_incident_ack_latency_seconds", Labels: labels, Value: float64(ackCounts[incidentType])},
		)
		values := ackSamples[incidentType]
		if len(values) > 0 {
			sort.Float64s(values)
			index := int(math.Ceil(float64(len(values))*0.99)) - 1
			if index < 0 {
				index = 0
			}
			samples = append(samples, metrics.Sample{Name: "jxe_delivery_incident_ack_latency_seconds", Family: "jxe_delivery_incident_ack_latency_seconds",
				Labels: map[string]string{"type": incidentType, "quantile": "0.99"}, Value: values[index]})
		}
	}
	for scope, value := range rateLimited {
		samples = append(samples, metrics.Sample{Name: "jxe_delivery_incident_rate_limited_total", Help: "Delivery incident writes rejected by the fixed-window limiter.", Type: "counter",
			Labels: map[string]string{"scope": scope}, Value: float64(value)})
	}
	for scope, value := range rateDegraded {
		samples = append(samples, metrics.Sample{Name: "jxe_delivery_incident_rate_limiter_degraded_total", Help: "Delivery incident rate checks served by the process-local fallback.", Type: "counter",
			Labels: map[string]string{"scope": scope}, Value: float64(value)})
	}
	for reason, value := range locationSuppressed {
		samples = append(samples, metrics.Sample{Name: "jxe_delivery_incident_location_distance_suppressed_total", Help: "Delivery incident locations whose distance was intentionally not persisted.", Type: "counter",
			Labels: map[string]string{"reason": reason}, Value: float64(value)})
	}
	if m.db == nil {
		return samples
	}
	type activeCount struct {
		Status string
		Type   string
		Stage  string
		Count  int64
	}
	var active []activeCount
	if m.db.Table("delivery_incidents").Select("status,type,stage,COUNT(*) count").Where("status IN ?", activeStatuses).Group("status,type,stage").Scan(&active).Error == nil {
		for _, row := range active {
			samples = append(samples, metrics.Sample{Name: "jxe_delivery_incident_active_total", Help: "Active delivery incidents by status, type and stage.", Type: "gauge",
				Labels: map[string]string{"status": row.Status, "type": row.Type, "stage": row.Stage}, Value: float64(row.Count)})
		}
	}
	var stale int64
	if m.db.Table("delivery_incidents i").Joins("JOIN delivery_orders d ON d.id=i.delivery_order_id").
		Where("i.status IN ? AND ((i.stage='pickup' AND d.status IN ('delivering','completed','cancelled')) OR (i.stage='delivery' AND d.status IN ('completed','cancelled')))", activeStatuses).
		Count(&stale).Error == nil {
		samples = append(samples, metrics.Sample{Name: "jxe_delivery_incident_stale_after_natural_close_total", Help: "Active delivery incidents left after a natural delivery close.", Type: "gauge", Value: float64(stale)})
	}
	duplicateQuery := m.db.Table("delivery_incidents").Select("delivery_order_id,type").
		Where("status IN ?", activeStatuses).Group("delivery_order_id,type").Having("COUNT(*) > 1")
	var duplicateGroups int64
	if m.db.Table("(?) AS duplicate_groups", duplicateQuery).Count(&duplicateGroups).Error == nil {
		samples = append(samples, metrics.Sample{Name: "jxe_delivery_incident_duplicate_active", Help: "Duplicate active incident groups that violate the single-active invariant.", Type: "gauge", Value: float64(duplicateGroups)})
	}
	var inconsistent int64
	if m.db.Raw(`SELECT COUNT(*) FROM delivery_incidents i
		WHERE NOT EXISTS (SELECT 1 FROM delivery_incident_history h WHERE h.incident_id=i.id)
		   OR (SELECT COUNT(*) FROM delivery_incident_history h WHERE h.incident_id=i.id) <>
		      (SELECT COUNT(*) FROM outbox_events o WHERE o.aggregate_type='delivery_incident' AND o.aggregate_id=i.id AND o.event_type LIKE 'delivery.incident.%')
		   OR (i.type='alcohol_damaged' AND i.status IN ('open','acknowledged') AND
		      NOT EXISTS (SELECT 1 FROM delivery_incident_evidence e WHERE e.incident_id=i.id AND e.scan_status='clean'))`).Scan(&inconsistent).Error == nil {
		samples = append(samples, metrics.Sample{Name: "jxe_delivery_incident_inconsistent_facts", Help: "Delivery incidents whose history, outbox, or evidence invariant is inconsistent.", Type: "gauge", Value: float64(inconsistent)})
	}
	var openTotal, unacknowledged int64
	openErr := m.db.Table("delivery_incidents").Where("status='open'").Count(&openTotal).Error
	unackErr := m.db.Table("delivery_incidents").Where("status='open' AND reported_at<?", time.Now().UTC().Add(-10*time.Minute)).Count(&unacknowledged).Error
	if openErr == nil && unackErr == nil {
		ratio := float64(0)
		if openTotal > 0 {
			ratio = float64(unacknowledged) / float64(openTotal)
		}
		samples = append(samples,
			metrics.Sample{Name: "jxe_delivery_incident_unacknowledged_over_10m", Help: "Open delivery incidents older than ten minutes.", Type: "gauge", Value: float64(unacknowledged)},
			metrics.Sample{Name: "jxe_delivery_incident_unacknowledged_ratio", Help: "Ratio of open delivery incidents older than ten minutes.", Type: "gauge", Value: ratio},
		)
	}
	var customerDeliveries, customerInbox int64
	deliveryErr := m.db.Table("notification_deliveries").Where("event_type LIKE 'delivery.incident.%' AND recipient_type='customer'").Count(&customerDeliveries).Error
	inboxErr := m.db.Table("message_inboxes").Where("type LIKE 'delivery.incident.%'").Count(&customerInbox).Error
	if deliveryErr == nil && inboxErr == nil {
		samples = append(samples, metrics.Sample{Name: "jxe_delivery_incident_customer_notifications", Help: "Customer notifications or inbox messages incorrectly created for internal delivery incidents.", Type: "gauge", Value: float64(customerDeliveries + customerInbox)})
	}
	return samples
}
