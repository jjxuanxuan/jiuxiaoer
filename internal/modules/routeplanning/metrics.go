package routeplanning

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

type metricState struct {
	mu                    sync.RWMutex
	requests              map[string]uint64
	rateLimited           map[string]uint64
	providerCalls         map[string]uint64
	providerDuration      map[string]float64
	providerDurationCount map[string]uint64
	providerBuckets       map[string][]uint64
	cache                 map[string]uint64
	degraded              map[string]uint64
	inflight              map[string]int64
}

var providerDurationBounds = []float64{0.05, 0.1, 0.25, 0.5, 1, 1.5, 2, 5}

func newMetricState(registry *metrics.Registry) *metricState {
	state := &metricState{
		requests: map[string]uint64{}, rateLimited: map[string]uint64{}, providerCalls: map[string]uint64{},
		providerDuration: map[string]float64{}, providerDurationCount: map[string]uint64{}, cache: map[string]uint64{},
		providerBuckets: map[string][]uint64{}, degraded: map[string]uint64{}, inflight: map[string]int64{},
	}
	if registry != nil {
		registry.AddCollector(state.collect)
	}
	return state
}

func (m *metricState) incRequest(stage, result, source string) {
	m.mu.Lock()
	m.requests[metricKey(stage, result, source)]++
	m.mu.Unlock()
}

func (m *metricState) incRateLimited(scope string) {
	m.mu.Lock()
	m.rateLimited[scope]++
	m.mu.Unlock()
}

func (m *metricState) observeProvider(provider, mode string, err error, duration time.Duration) {
	provider, mode = sourceProvider(provider), sourceProvider(mode)
	result := "success"
	if err != nil {
		result = safeProvider(err)
	}
	key := metricKey(provider, mode)
	m.mu.Lock()
	m.providerCalls[metricKey(provider, mode, result)]++
	m.providerDuration[key] += duration.Seconds()
	m.providerDurationCount[key]++
	buckets := m.providerBuckets[key]
	if len(buckets) == 0 {
		buckets = make([]uint64, len(providerDurationBounds)+1)
	}
	seconds := duration.Seconds()
	for index, upperBound := range providerDurationBounds {
		if seconds <= upperBound {
			buckets[index]++
		}
	}
	buckets[len(buckets)-1]++
	m.providerBuckets[key] = buckets
	m.mu.Unlock()
}

func (m *metricState) incCache(result string) {
	m.mu.Lock()
	m.cache[result]++
	m.mu.Unlock()
}

func (m *metricState) incDegraded(reason string) {
	m.mu.Lock()
	m.degraded[reason]++
	m.mu.Unlock()
}

func (m *metricState) addInflight(provider string, delta int64) {
	m.mu.Lock()
	m.inflight[sourceProvider(provider)] += delta
	m.mu.Unlock()
}

func (m *metricState) collect() []metrics.Sample {
	m.mu.RLock()
	requests, limited, calls := clone(m.requests), clone(m.rateLimited), clone(m.providerCalls)
	durations, durationCounts := cloneFloat(m.providerDuration), clone(m.providerDurationCount)
	durationBuckets := cloneBuckets(m.providerBuckets)
	cache, degraded, inflight := clone(m.cache), clone(m.degraded), cloneInt(m.inflight)
	m.mu.RUnlock()
	samples := make([]metrics.Sample, 0, len(requests)+len(limited)+len(calls)+len(durations)*(len(providerDurationBounds)+3)+len(cache)+len(degraded)+len(inflight))
	for key, value := range requests {
		parts := splitMetricKey(key, 3)
		samples = append(samples, metrics.Sample{Name: "jxe_route_requests_total", Help: "Delivery route requests by stage, result, and source.", Type: "counter", Labels: map[string]string{"stage": parts[0], "result": parts[1], "source": parts[2]}, Value: float64(value)})
	}
	for scope, value := range limited {
		samples = append(samples, metrics.Sample{Name: "jxe_route_rate_limited_total", Help: "Delivery route rate limits by scope.", Type: "counter", Labels: map[string]string{"scope": scope}, Value: float64(value)})
	}
	for key, value := range calls {
		parts := splitMetricKey(key, 3)
		samples = append(samples, metrics.Sample{Name: "jxe_route_provider_calls_total", Help: "Map route provider calls by provider, mode, and result.", Type: "counter", Labels: map[string]string{"provider": parts[0], "mode": parts[1], "result": parts[2]}, Value: float64(value)})
	}
	for key, value := range durations {
		parts := splitMetricKey(key, 2)
		labels := map[string]string{"provider": parts[0], "mode": parts[1]}
		for index, count := range durationBuckets[key] {
			upperBound := "+Inf"
			if index < len(providerDurationBounds) {
				upperBound = fmt.Sprintf("%g", providerDurationBounds[index])
			}
			samples = append(samples, metrics.Sample{Name: "jxe_route_provider_duration_seconds_bucket", Family: "jxe_route_provider_duration_seconds", Help: "Map route provider call duration.", Type: "histogram", Labels: map[string]string{"provider": parts[0], "mode": parts[1], "le": upperBound}, Value: float64(count)})
		}
		samples = append(samples,
			metrics.Sample{Name: "jxe_route_provider_duration_seconds_sum", Family: "jxe_route_provider_duration_seconds", Help: "Map route provider call duration.", Type: "histogram", Labels: labels, Value: value},
			metrics.Sample{Name: "jxe_route_provider_duration_seconds_count", Family: "jxe_route_provider_duration_seconds", Labels: labels, Value: float64(durationCounts[key])},
		)
	}
	for result, value := range cache {
		samples = append(samples, metrics.Sample{Name: "jxe_route_cache_total", Help: "Delivery route cache operations by result.", Type: "counter", Labels: map[string]string{"result": result}, Value: float64(value)})
	}
	for reason, value := range degraded {
		samples = append(samples, metrics.Sample{Name: "jxe_route_degraded_total", Help: "Delivery route degraded responses by safe reason.", Type: "counter", Labels: map[string]string{"reason": reason}, Value: float64(value)})
	}
	for provider, value := range inflight {
		samples = append(samples, metrics.Sample{Name: "jxe_route_inflight", Help: "Current in-flight provider route calls.", Type: "gauge", Labels: map[string]string{"provider": provider}, Value: float64(value)})
	}
	return samples
}

func routeMetricResult(err error) string {
	if err == nil {
		return "success"
	}
	var details *problem.Details
	if errors.As(err, &details) {
		return details.ErrorCode
	}
	return "internal_error"
}

func metricKey(values ...string) string {
	key := ""
	for index, value := range values {
		if index > 0 {
			key += "\x00"
		}
		key += sourceProvider(value)
	}
	return key
}

func splitMetricKey(value string, count int) []string {
	result := make([]string, 0, count)
	start := 0
	for index := 0; index < len(value) && len(result) < count-1; index++ {
		if value[index] == 0 {
			result = append(result, value[start:index])
			start = index + 1
		}
	}
	result = append(result, value[start:])
	for len(result) < count {
		result = append(result, "unknown")
	}
	return result
}

func clone(input map[string]uint64) map[string]uint64 {
	output := make(map[string]uint64, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func cloneFloat(input map[string]float64) map[string]float64 {
	output := make(map[string]float64, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func cloneInt(input map[string]int64) map[string]int64 {
	output := make(map[string]int64, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func cloneBuckets(input map[string][]uint64) map[string][]uint64 {
	output := make(map[string][]uint64, len(input))
	for key, value := range input {
		output[key] = append([]uint64(nil), value...)
	}
	return output
}
