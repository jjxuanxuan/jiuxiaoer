package metrics

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

type HTTPKey struct {
	Method    string
	Route     string
	Status    int
	ErrorCode string
}

type HTTPValue struct {
	Count       uint64
	DurationSum float64
}

// Registry 刻意保持轻量且无依赖，以 Prometheus 文本格式公开核心 L0
// RED 指标和工作进程指标。
type Registry struct {
	mu           sync.RWMutex
	http         map[HTTPKey]HTTPValue
	outbox       map[string]uint64
	orderExpiry  map[string]uint64
	payment      map[string]uint64
	serviceArea  map[string]uint64
	mqConsume    map[string]uint64
	mqRetry      map[string]uint64
	mqDead       map[string]uint64
	mqUnrouted   map[string]uint64
	startedAt    time.Time
	instanceID   string
	metricsToken string
	collectors   []func() []Sample
}

type Sample struct {
	Name   string
	Family string
	Help   string
	Type   string
	Labels map[string]string
	Value  float64
}

// New 创建并初始化注册表。
func New(instanceID, token string) *Registry {
	return &Registry{
		http:         make(map[HTTPKey]HTTPValue),
		outbox:       make(map[string]uint64),
		orderExpiry:  make(map[string]uint64),
		payment:      make(map[string]uint64),
		serviceArea:  make(map[string]uint64),
		mqConsume:    make(map[string]uint64),
		mqRetry:      make(map[string]uint64),
		mqDead:       make(map[string]uint64),
		mqUnrouted:   make(map[string]uint64),
		startedAt:    time.Now(),
		instanceID:   instanceID,
		metricsToken: token,
	}
}

// ObserveHTTP 记录HTTP观测指标。
func (r *Registry) ObserveHTTP(method, route string, status int, errorCode string, duration time.Duration) {
	if r == nil {
		return
	}
	key := HTTPKey{Method: method, Route: route, Status: status, ErrorCode: errorCode}
	r.mu.Lock()
	value := r.http[key]
	value.Count++
	value.DurationSum += duration.Seconds()
	r.http[key] = value
	r.mu.Unlock()
}

// IncOutbox 递增发件箱事件指标计数。
func (r *Registry) IncOutbox(result string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.outbox[result]++
	r.mu.Unlock()
}

// IncOrderExpiry 递增订单过期指标计数。
func (r *Registry) IncOrderExpiry(result string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.orderExpiry[result]++
	r.mu.Unlock()
}

// IncPayment 递增支付指标计数。
func (r *Registry) IncPayment(provider string, result string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.payment[provider+"\x00"+result]++
	r.mu.Unlock()
}

// IncServiceArea 递增服务 Area指标计数。
func (r *Registry) IncServiceArea(result string, cache string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.serviceArea[result+"\x00"+cache]++
	r.mu.Unlock()
}

// IncMQConsume 递增消息队列 Consume指标计数。
func (r *Registry) IncMQConsume(consumer, result string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.mqConsume[consumer+"\x00"+result]++
	r.mu.Unlock()
}

// IncMQRetry 递增消息队列重试指标计数。
func (r *Registry) IncMQRetry(consumer, tier string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.mqRetry[consumer+"\x00"+tier]++
	r.mu.Unlock()
}

// IncMQDead 递增消息队列死信指标计数。
func (r *Registry) IncMQDead(consumer string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.mqDead[consumer]++
	r.mu.Unlock()
}

// IncMQUnrouted 递增消息队列未路由消息指标计数。
func (r *Registry) IncMQUnrouted(eventType string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.mqUnrouted[eventType]++
	r.mu.Unlock()
}

// AddCollector 添加收集器。
func (r *Registry) AddCollector(collector func() []Sample) {
	if r == nil || collector == nil {
		return
	}
	r.mu.Lock()
	r.collectors = append(r.collectors, collector)
	r.mu.Unlock()
}

// Handler 处理当前 HTTP 请求并写入响应。
func (r *Registry) Handler(c *gin.Context) {
	if r == nil {
		c.Status(http.StatusNotFound)
		return
	}
	if r.metricsToken != "" {
		got := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(r.metricsToken)) != 1 {
			c.Status(http.StatusUnauthorized)
			return
		}
	}
	c.Header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	c.String(http.StatusOK, r.render())
}

// render 渲染字符串。
func (r *Registry) render() string {
	r.mu.RLock()
	httpValues := make(map[HTTPKey]HTTPValue, len(r.http))
	for key, value := range r.http {
		httpValues[key] = value
	}
	outboxValues := make(map[string]uint64, len(r.outbox))
	for key, value := range r.outbox {
		outboxValues[key] = value
	}
	orderExpiryValues := make(map[string]uint64, len(r.orderExpiry))
	for key, value := range r.orderExpiry {
		orderExpiryValues[key] = value
	}
	paymentValues := make(map[string]uint64, len(r.payment))
	for key, value := range r.payment {
		paymentValues[key] = value
	}
	serviceAreaValues := make(map[string]uint64, len(r.serviceArea))
	for key, value := range r.serviceArea {
		serviceAreaValues[key] = value
	}
	mqConsumeValues := copyCounterMap(r.mqConsume)
	mqRetryValues := copyCounterMap(r.mqRetry)
	mqDeadValues := copyCounterMap(r.mqDead)
	mqUnroutedValues := copyCounterMap(r.mqUnrouted)
	collectors := append([]func() []Sample(nil), r.collectors...)
	r.mu.RUnlock()

	var builder strings.Builder
	builder.WriteString("# HELP jxe_process_uptime_seconds Process uptime.\n# TYPE jxe_process_uptime_seconds gauge\n")
	fmt.Fprintf(&builder, "jxe_process_uptime_seconds{instance=%s} %.3f\n", quote(r.instanceID), time.Since(r.startedAt).Seconds())
	builder.WriteString("# HELP jxe_http_requests_total HTTP requests by route and result.\n# TYPE jxe_http_requests_total counter\n")
	builder.WriteString("# HELP jxe_http_request_duration_seconds_sum HTTP request duration sum.\n# TYPE jxe_http_request_duration_seconds_sum counter\n")
	keys := make([]HTTPKey, 0, len(httpValues))
	for key := range httpValues {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
	for _, key := range keys {
		value := httpValues[key]
		labels := fmt.Sprintf("method=%s,route=%s,status=%s,error_code=%s", quote(key.Method), quote(key.Route), quote(strconv.Itoa(key.Status)), quote(key.ErrorCode))
		fmt.Fprintf(&builder, "jxe_http_requests_total{%s} %d\n", labels, value.Count)
		fmt.Fprintf(&builder, "jxe_http_request_duration_seconds_sum{%s} %.6f\n", labels, value.DurationSum)
	}
	builder.WriteString("# HELP jxe_outbox_publish_total Outbox publish results.\n# TYPE jxe_outbox_publish_total counter\n")
	results := make([]string, 0, len(outboxValues))
	for result := range outboxValues {
		results = append(results, result)
	}
	sort.Strings(results)
	for _, result := range results {
		fmt.Fprintf(&builder, "jxe_outbox_publish_total{result=%s} %d\n", quote(result), outboxValues[result])
	}
	writeCounterMap(&builder, "jxe_order_expiry_total", "Order expiry worker results.", "result", orderExpiryValues)
	builder.WriteString("# HELP jxe_payment_operations_total Payment provider and callback results.\n# TYPE jxe_payment_operations_total counter\n")
	paymentKeys := make([]string, 0, len(paymentValues))
	for key := range paymentValues {
		paymentKeys = append(paymentKeys, key)
	}
	sort.Strings(paymentKeys)
	for _, key := range paymentKeys {
		parts := strings.SplitN(key, "\x00", 2)
		fmt.Fprintf(&builder, "jxe_payment_operations_total{provider=%s,result=%s} %d\n", quote(parts[0]), quote(parts[1]), paymentValues[key])
	}
	builder.WriteString("# HELP jxe_service_area_resolve_total Service area resolution results.\n# TYPE jxe_service_area_resolve_total counter\n")
	serviceAreaKeys := make([]string, 0, len(serviceAreaValues))
	for key := range serviceAreaValues {
		serviceAreaKeys = append(serviceAreaKeys, key)
	}
	sort.Strings(serviceAreaKeys)
	for _, key := range serviceAreaKeys {
		parts := strings.SplitN(key, "\x00", 2)
		fmt.Fprintf(&builder, "jxe_service_area_resolve_total{result=%s,cache=%s} %d\n", quote(parts[0]), quote(parts[1]), serviceAreaValues[key])
	}
	writeTwoLabelCounter(&builder, "jxe_mq_consume_total", "MQ consumer results.", "consumer", "result", mqConsumeValues)
	writeTwoLabelCounter(&builder, "jxe_mq_retry_total", "MQ consumer retries.", "consumer", "tier", mqRetryValues)
	writeCounterMap(&builder, "jxe_mq_dead_total", "MQ dead letters by consumer.", "consumer", mqDeadValues)
	writeCounterMap(&builder, "jxe_mq_unrouted_total", "Unrouted MQ events by registered event type.", "event_type", mqUnroutedValues)
	collectorMetadata := make(map[string]bool)
	for _, collector := range collectors {
		for _, sample := range collector() {
			family := sample.Family
			if family == "" {
				family = sample.Name
			}
			if !collectorMetadata[family] {
				if sample.Help != "" {
					fmt.Fprintf(&builder, "# HELP %s %s\n", family, sample.Help)
				}
				if sample.Type != "" {
					fmt.Fprintf(&builder, "# TYPE %s %s\n", family, sample.Type)
				}
				collectorMetadata[family] = true
			}
			fmt.Fprintf(&builder, "%s%s %.6f\n", sample.Name, formatLabels(sample.Labels), sample.Value)
		}
	}
	return builder.String()
}

// copyCounterMap 复制计数器 Map。
func copyCounterMap(source map[string]uint64) map[string]uint64 {
	result := make(map[string]uint64, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

// writeTwoLabelCounter 写入双标签计数器。
func writeTwoLabelCounter(builder *strings.Builder, name, help, firstLabel, secondLabel string, values map[string]uint64) {
	fmt.Fprintf(builder, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		parts := strings.SplitN(key, "\x00", 2)
		if len(parts) != 2 {
			continue
		}
		fmt.Fprintf(builder, "%s{%s=%s,%s=%s} %d\n", name, firstLabel, quote(parts[0]), secondLabel, quote(parts[1]), values[key])
	}
}

// writeCounterMap 写入计数器 Map。
func writeCounterMap(builder *strings.Builder, name string, help string, label string, values map[string]uint64) {
	fmt.Fprintf(builder, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(builder, "%s{%s=%s} %d\n", name, label, quote(key), values[key])
	}
}

// formatLabels 格式化标签。
func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+quote(labels[key]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// quote 返回符合指标文本格式的转义字符串。
func quote(value string) string {
	return strconv.Quote(value)
}
