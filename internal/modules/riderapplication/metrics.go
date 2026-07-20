package riderapplication

import (
	"errors"
	"sync"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

type metricState struct {
	mu             sync.RWMutex
	db             *gorm.DB
	submissions    map[string]uint64
	reviews        map[string]uint64
	conflicts      map[string]uint64
	rateLimited    map[string]uint64
	openSecondsSum float64
	openCount      uint64
}

// newMetricState 创建并初始化指标状态。
func newMetricState(db *gorm.DB, registry *metrics.Registry) *metricState {
	state := &metricState{
		db: db, submissions: map[string]uint64{}, reviews: map[string]uint64{},
		conflicts: map[string]uint64{}, rateLimited: map[string]uint64{},
	}
	if registry != nil {
		registry.AddCollector(state.collect)
	}
	return state
}

// incSubmission 递增Submission指标计数。
func (m *metricState) incSubmission(result string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.submissions[result]++
	m.mu.Unlock()
}

// incReview 递增Review指标计数。
func (m *metricState) incReview(decision, result string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.reviews[decision+"\x00"+result]++
	m.mu.Unlock()
}

// incConflict 递增冲突指标计数。
func (m *metricState) incConflict(action string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.conflicts[action]++
	m.mu.Unlock()
}

// incRateLimited 递增速率 Limited指标计数。
func (m *metricState) incRateLimited(scope string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.rateLimited[scope]++
	m.mu.Unlock()
}

// observeOpen 记录打开观测指标。
func (m *metricState) observeOpen(duration time.Duration) {
	if m == nil || duration < 0 {
		return
	}
	m.mu.Lock()
	m.openSecondsSum += duration.Seconds()
	m.openCount++
	m.mu.Unlock()
}

// collect 收集Sample列表。
func (m *metricState) collect() []metrics.Sample {
	m.mu.RLock()
	submissions := copyCounts(m.submissions)
	reviews := copyCounts(m.reviews)
	conflicts := copyCounts(m.conflicts)
	rateLimited := copyCounts(m.rateLimited)
	openSum, openCount := m.openSecondsSum, m.openCount
	m.mu.RUnlock()
	samples := make([]metrics.Sample, 0, len(submissions)+len(reviews)+len(conflicts)+len(rateLimited)+6)
	for result, value := range submissions {
		samples = append(samples, metrics.Sample{Name: "jxe_rider_application_submissions_total", Help: "Rider application submissions by result.", Type: "counter", Labels: map[string]string{"result": result}, Value: float64(value)})
	}
	for key, value := range reviews {
		parts := splitMetricKey(key)
		samples = append(samples, metrics.Sample{Name: "jxe_rider_application_reviews_total", Help: "Rider application reviews by decision and result.", Type: "counter", Labels: map[string]string{"decision": parts[0], "result": parts[1]}, Value: float64(value)})
	}
	for action, value := range conflicts {
		samples = append(samples, metrics.Sample{Name: "jxe_rider_application_state_conflicts_total", Help: "Rider application state and version conflicts.", Type: "counter", Labels: map[string]string{"action": action}, Value: float64(value)})
	}
	for scope, value := range rateLimited {
		samples = append(samples, metrics.Sample{Name: "jxe_rider_application_rate_limited_total", Help: "Rider application rate limits by safe scope.", Type: "counter", Labels: map[string]string{"scope": scope}, Value: float64(value)})
	}
	samples = append(samples,
		metrics.Sample{Name: "jxe_rider_application_open_duration_seconds_sum", Family: "jxe_rider_application_open_duration_seconds", Help: "Time from first application creation to rider opening.", Type: "summary", Value: openSum},
		metrics.Sample{Name: "jxe_rider_application_open_duration_seconds_count", Family: "jxe_rider_application_open_duration_seconds", Value: float64(openCount)},
	)
	if m.db != nil {
		type statusCount struct {
			Status string
			Count  int64
		}
		var rows []statusCount
		if err := m.db.Table("rider_applications").Select("status, COUNT(*) AS count").Where("status IN ?", []string{StatusSubmitted, StatusRejected}).Group("status").Scan(&rows).Error; err == nil {
			for _, row := range rows {
				samples = append(samples, metrics.Sample{Name: "jxe_rider_application_pending", Help: "Pending rider applications by actionable status.", Type: "gauge", Labels: map[string]string{"status": row.Status}, Value: float64(row.Count)})
			}
		}
	}
	return samples
}

// copyCounts 复制Counts。
func copyCounts(source map[string]uint64) map[string]uint64 {
	target := make(map[string]uint64, len(source))
	for key, value := range source {
		target[key] = value
	}
	return target
}

// splitMetricKey 返回split 指标密钥。
func splitMetricKey(value string) [2]string {
	for index := 0; index < len(value); index++ {
		if value[index] == 0 {
			return [2]string{value[:index], value[index+1:]}
		}
	}
	return [2]string{value, "unknown"}
}

// metricResult 返回指标结果。
func metricResult(err error) string {
	if err == nil {
		return "success"
	}
	var details *problem.Details
	if errors.As(err, &details) {
		return details.ErrorCode
	}
	return "internal_error"
}
