package search

import (
	"strings"
	"sync"
	"time"

	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
)

type searchMetrics struct {
	mu          sync.Mutex
	events      map[string]uint64
	history     map[string]uint64
	hot         map[string]uint64
	hotDuration map[string]float64
	hotCount    map[string]uint64
	cleanup     map[string]uint64
	deleted     map[string]uint64
	rateLimited map[string]uint64
}

func newSearchMetrics(registry *metrics.Registry) *searchMetrics {
	value := &searchMetrics{
		events:      make(map[string]uint64),
		history:     make(map[string]uint64),
		hot:         make(map[string]uint64),
		hotDuration: make(map[string]float64),
		hotCount:    make(map[string]uint64),
		cleanup:     make(map[string]uint64),
		deleted:     make(map[string]uint64),
		rateLimited: make(map[string]uint64),
	}
	if registry != nil {
		registry.AddCollector(value.collect)
	}
	return value
}

func (m *searchMetrics) incEvent(source, result string, counted bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.events[source+"\x00"+result+"\x00"+boolLabel(counted)]++
	m.mu.Unlock()
}

func (m *searchMetrics) incHistory(operation, result string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.history[operation+"\x00"+result]++
	m.mu.Unlock()
}

func (m *searchMetrics) incHot(scopeType, cacheResult string, degraded bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.hot[scopeType+"\x00"+cacheResult+"\x00"+boolLabel(degraded)]++
	m.mu.Unlock()
}

func (m *searchMetrics) observeHotRefresh(scopeType, result string, duration time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	key := scopeType + "\x00" + result
	m.hotDuration[key] += duration.Seconds()
	m.hotCount[key]++
	m.mu.Unlock()
}

func (m *searchMetrics) incRateLimited(actorType string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.rateLimited[actorType]++
	m.mu.Unlock()
}

func (m *searchMetrics) addCleanup(table, result string, deleted int64) {
	if m == nil {
		return
	}
	if deleted < 0 {
		deleted = 0
	}
	m.mu.Lock()
	m.cleanup[table+"\x00"+result]++
	m.deleted[table] += uint64(deleted)
	m.mu.Unlock()
}

func (m *searchMetrics) collect() []metrics.Sample {
	m.mu.Lock()
	defer m.mu.Unlock()
	samples := make([]metrics.Sample, 0, len(m.events)+len(m.history)+len(m.hot)+len(m.hotDuration)*2+len(m.cleanup)+len(m.deleted)+len(m.rateLimited))
	for key, value := range m.events {
		labels := strings.Split(key, "\x00")
		samples = append(samples, metrics.Sample{Name: "jxe_search_events_total", Help: "Processed customer search events.", Type: "counter", Labels: map[string]string{"source": labels[0], "result": labels[1], "counted_for_hot": labels[2]}, Value: float64(value)})
	}
	for key, value := range m.history {
		labels := strings.Split(key, "\x00")
		samples = append(samples, metrics.Sample{Name: "jxe_search_history_operations_total", Help: "Customer search history operations.", Type: "counter", Labels: map[string]string{"operation": labels[0], "result": labels[1]}, Value: float64(value)})
	}
	for key, value := range m.hot {
		labels := strings.Split(key, "\x00")
		samples = append(samples, metrics.Sample{Name: "jxe_search_hot_reads_total", Help: "Hot search ranking reads.", Type: "counter", Labels: map[string]string{"scope_type": labels[0], "cache_result": labels[1], "degraded": labels[2]}, Value: float64(value)})
	}
	for key, value := range m.hotDuration {
		labels := strings.Split(key, "\x00")
		metricLabels := map[string]string{"scope_type": labels[0], "result": labels[1]}
		samples = append(samples,
			metrics.Sample{Name: "jxe_search_hot_refresh_duration_seconds_sum", Family: "jxe_search_hot_refresh_duration_seconds", Help: "MySQL hot ranking refresh duration.", Type: "summary", Labels: metricLabels, Value: value},
			metrics.Sample{Name: "jxe_search_hot_refresh_duration_seconds_count", Family: "jxe_search_hot_refresh_duration_seconds", Labels: metricLabels, Value: float64(m.hotCount[key])},
		)
	}
	for key, value := range m.cleanup {
		labels := strings.Split(key, "\x00")
		samples = append(samples, metrics.Sample{Name: "jxe_search_cleanup_total", Help: "Search retention cleanup batches.", Type: "counter", Labels: map[string]string{"table": labels[0], "result": labels[1]}, Value: float64(value)})
	}
	for table, value := range m.deleted {
		samples = append(samples, metrics.Sample{Name: "jxe_search_cleanup_deleted_total", Help: "Expired search rows deleted by retention cleanup.", Type: "counter", Labels: map[string]string{"table": table}, Value: float64(value)})
	}
	for actorType, value := range m.rateLimited {
		samples = append(samples, metrics.Sample{Name: "jxe_search_rate_limited_total", Help: "Search events rejected by rate limiting.", Type: "counter", Labels: map[string]string{"actor_type": actorType}, Value: float64(value)})
	}
	return samples
}

func boolLabel(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
