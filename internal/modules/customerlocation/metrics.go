package customerlocation

import (
	"strings"
	"sync"

	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
)

type lbsMetrics struct {
	mu           sync.Mutex
	resolve      map[string]uint64
	provider     map[string]uint64
	cache        map[string]uint64
	read         map[string]uint64
	hintMismatch uint64
}

func (m *lbsMetrics) incHintMismatch() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.hintMismatch++
	m.mu.Unlock()
}

func newLBSMetrics(registry *metrics.Registry) *lbsMetrics {
	m := &lbsMetrics{resolve: map[string]uint64{}, provider: map[string]uint64{}, cache: map[string]uint64{}, read: map[string]uint64{}}
	if registry != nil {
		registry.AddCollector(m.samples)
	}
	return m
}

// ObserveReadComparison records only bounded classifications and never raw
// coordinates, addresses, sessions, or context identifiers.
func (s *Service) ObserveReadComparison(endpoint, legacyCity, legacyShop string, value LocationContext) {
	if s == nil || s.metrics == nil {
		return
	}
	result := "no_legacy"
	if legacyCity != "" || legacyShop != "" {
		result = "match"
		if legacyCity != "" && legacyCity != value.Location.CityCode {
			result = "mismatch"
		}
		if legacyShop != "" && (value.ServiceShop == nil || legacyShop != value.ServiceShop.ID) {
			result = "mismatch"
		}
	}
	s.metrics.mu.Lock()
	s.metrics.read[endpoint+"\x00"+result]++
	s.metrics.mu.Unlock()
}

func (m *lbsMetrics) incResolve(source, result, level string, degraded bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.resolve[source+"\x00"+result+"\x00"+level+"\x00"+boolLabel(degraded)]++
	m.mu.Unlock()
}

func (m *lbsMetrics) incProvider(operation, result, kind string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.provider[operation+"\x00"+result+"\x00"+kind]++
	m.mu.Unlock()
}

func (m *lbsMetrics) incCache(cache, result string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.cache[cache+"\x00"+result]++
	m.mu.Unlock()
}

func (m *lbsMetrics) samples() []metrics.Sample {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]metrics.Sample, 0, len(m.resolve)+len(m.provider)+len(m.cache)+len(m.read))
	for key, value := range m.resolve {
		parts := strings.Split(key, "\x00")
		result = append(result, metrics.Sample{Name: "jxe_c_lbs_resolve_total", Help: "Customer LBS resolve results.", Type: "counter", Labels: map[string]string{"source": parts[0], "result": parts[1], "location_level": parts[2], "degraded": parts[3]}, Value: float64(value)})
	}
	for key, value := range m.provider {
		parts := strings.Split(key, "\x00")
		result = append(result, metrics.Sample{Name: "jxe_c_lbs_provider_total", Help: "Customer LBS provider calls.", Type: "counter", Labels: map[string]string{"provider": "amap", "operation": parts[0], "result": parts[1], "error_kind": parts[2]}, Value: float64(value)})
	}
	for key, value := range m.cache {
		parts := strings.Split(key, "\x00")
		result = append(result, metrics.Sample{Name: "jxe_c_lbs_cache_total", Help: "Customer LBS cache results.", Type: "counter", Labels: map[string]string{"cache": parts[0], "result": parts[1]}, Value: float64(value)})
	}
	for key, value := range m.read {
		parts := strings.Split(key, "\x00")
		result = append(result, metrics.Sample{Name: "jxe_c_lbs_read_comparison_total", Help: "Observe-mode comparison of location-context and legacy public reads.", Type: "counter", Labels: map[string]string{"endpoint": parts[0], "result": parts[1]}, Value: float64(value)})
	}
	if m.hintMismatch > 0 {
		result = append(result, metrics.Sample{Name: "jxe_c_lbs_city_hint_mismatch_total", Help: "Device city hints that differed from the authoritative reverse-geocode mapping.", Type: "counter", Value: float64(m.hintMismatch)})
	}
	return result
}

func boolLabel(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
