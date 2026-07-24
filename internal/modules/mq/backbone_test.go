package mq

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"

	"jiuxiaoer-admin/backend-go/internal/config"
	rabbitinfra "jiuxiaoer-admin/backend-go/internal/infra/rabbitmq"
)

// TestEventEnvelopeV1AndSensitivePayloadGuard 验证 V1 事件信封和敏感载荷保护。
func TestEventEnvelopeV1AndSensitivePayloadGuard(t *testing.T) {
	registry := MustDefaultEventRegistry()
	definition, _ := registry.Lookup("cache.invalidate")
	event := OutboxEvent{EventID: uuid.NewString(), EventType: "cache.invalidate", AggregateType: "product", AggregateID: 9007199254740993, Payload: datatypes.JSON(`{"keys":["product:1"]}`), CreatedAt: time.Now()}
	envelope, err := BuildEnvelope(event, definition, "test")
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	if envelope.AggregateID != "9007199254740993" || envelope.SpecVersion != "1.0" || envelope.Metadata.SchemaRef != "event://cache.invalidate/1" {
		t.Fatalf("unexpected envelope: %+v", envelope)
	}
	body, _ := json.Marshal(envelope)
	decoded, _, err := DecodeEnvelope(body, registry)
	if err != nil || decoded.EventID != envelope.EventID {
		t.Fatalf("decode envelope: %+v %v", decoded, err)
	}

	event.Payload = datatypes.JSON(`{"nested":{"access_token":"must-not-publish"}}`)
	if _, err := BuildEnvelope(event, definition, "test"); err == nil || !strings.Contains(err.Error(), "MQ_SENSITIVE_FIELD_FORBIDDEN") {
		t.Fatalf("expected sensitive payload rejection, got %v", err)
	}
}

// TestEnvelopeIgnoresUnknownOptionalFields 验证消息信封忽略未知可选字段。
func TestEnvelopeIgnoresUnknownOptionalFields(t *testing.T) {
	registry := MustDefaultEventRegistry()
	definition, _ := registry.Lookup("cache.invalidate")
	event := OutboxEvent{EventID: uuid.NewString(), EventType: "cache.invalidate", AggregateType: "product", AggregateID: 1, Payload: datatypes.JSON(`{"keys":["product:1"]}`), CreatedAt: time.Now()}
	envelope, err := BuildEnvelope(event, definition, "test")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(envelope)
	body = append(body[:len(body)-1], []byte(`,"future_optional":"supported"}`)...)
	if _, _, err := DecodeEnvelope(body, registry); err != nil {
		t.Fatalf("compatible consumers must ignore unknown optional fields: %v", err)
	}
}

// TestRegistryRoutableEventsHaveConsumerBindings 验证注册表中可路由事件具有消费者绑定。
func TestRegistryRoutableEventsHaveConsumerBindings(t *testing.T) {
	registry := MustDefaultEventRegistry()
	topology := DefaultTopology()
	queueByConsumer := map[string]string{"notification": notificationQueueName, "print": printQueueName, "cache": cacheQueueName, "security": securityQueueName, "dispatch": dispatchQueueName, "realtime": realtimeQueueName}
	for _, definition := range registry.Definitions() {
		if !definition.Routable() {
			continue
		}
		for _, consumer := range definition.Consumers {
			queue := queueByConsumer[consumer]
			found := false
			for _, binding := range topology.Bindings {
				if binding.Exchange == exchangeName && binding.Queue == queue && topicMatches(binding.RoutingKey, definition.EventType) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("event %s has no %s binding", definition.EventType, consumer)
			}
		}
	}
}

// TestPaidOrderEventRoutesToRealtime 确保已提交的支付事实可以到达商户
// WebSocket 消费者。以 (consumer_name,event_id) 为键的消费者回执
// 仍作为重复投递屏障。
func TestPaidOrderEventRoutesToRealtime(t *testing.T) {
	registry := MustDefaultEventRegistry()
	topology := DefaultTopology()
	for _, eventType := range []string{"order.paid"} {
		definition, ok := registry.Lookup(eventType)
		if !ok {
			t.Fatalf("missing event definition %s", eventType)
		}
		hasConsumer := false
		for _, consumer := range definition.Consumers {
			if consumer == "realtime" {
				hasConsumer = true
				break
			}
		}
		if !hasConsumer {
			t.Fatalf("%s is not registered for realtime", eventType)
		}
		hasBinding := false
		for _, binding := range topology.Bindings {
			if binding.Queue == realtimeQueueName && topicMatches(binding.RoutingKey, eventType) {
				hasBinding = true
				break
			}
		}
		if !hasBinding {
			t.Fatalf("%s has no realtime queue binding", eventType)
		}
	}
	// order.paid 是当前支付成功事务发出的唯一事件。
	// 不让 payment.succeeded 进入实时通道，可防止未来兼容别名
	// 为同一笔支付产生第二个弹窗。
	legacy, _ := registry.Lookup("payment.succeeded")
	for _, consumer := range legacy.Consumers {
		if consumer == "realtime" {
			t.Fatal("payment.succeeded must not be a second realtime source")
		}
	}
}

// TestAllLiteralOutboxEventsAreRegistered 验证所有字面量发件箱事件均已注册。
func TestAllLiteralOutboxEventsAreRegistered(t *testing.T) {
	registry := MustDefaultEventRegistry()
	emitted := collectLiteralOutboxEvents(t, "..")
	for _, dynamic := range []string{"asset.hold.commit", "asset.hold.release", "asset.hold.expire"} {
		emitted[dynamic] = true
	}
	for eventType := range emitted {
		if _, ok := registry.Lookup(eventType); !ok {
			t.Errorf("emitted event %q is missing from the registry", eventType)
		}
	}
	for _, required := range []string{"member.rule.activated", "member.tier.changed", "asset.transaction.posted", "asset.transaction.reversed", "asset.hold.created", "asset.lot.expired"} {
		definition, ok := registry.Lookup(required)
		if !ok || definition.Status != EventNoConsumer {
			t.Errorf("phase-two event %s must be registered as no_consumer", required)
		}
	}
}

// TestTopologyHasIsolatedRetryAndDeadQueues 验证拓扑具有隔离的重试队列和死信队列。
func TestTopologyHasIsolatedRetryAndDeadQueues(t *testing.T) {
	topology := DefaultTopology()
	seen := map[string]bool{}
	for _, queue := range topology.Queues {
		if seen[queue.Name] {
			t.Fatalf("duplicate queue %s", queue.Name)
		}
		seen[queue.Name] = true
	}
	for _, name := range []string{
		"jxe.notification.retry.10s.v1.queue", "jxe.notification.retry.1m.v1.queue", "jxe.notification.retry.10m.v1.queue", "jxe.notification.dead.v1.queue",
		"jxe.print.dead.v1.queue", "jxe.cache.dead.v1.queue", "jxe.security.dead.v1.queue", unroutedQueueName,
		"jxe.dispatch.retry.1s.v1.queue", "jxe.dispatch.retry.5s.v1.queue", "jxe.dispatch.retry.30s.v1.queue", "jxe.dispatch.dead.v1.queue",
		"jxe.realtime.retry.1s.v1.queue", "jxe.realtime.retry.5s.v1.queue", "jxe.realtime.retry.30s.v1.queue", "jxe.realtime.dead.v1.queue",
	} {
		if !seen[name] {
			t.Errorf("missing topology queue %s", name)
		}
	}
}

// TestManagedTopologyDetectsAlternateExchangeArgumentDrift 验证受管拓扑能发现备用交换机参数偏移。
func TestManagedTopologyDetectsAlternateExchangeArgumentDrift(t *testing.T) {
	topology := DefaultTopology()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(request.URL.Path, "/exchanges/"):
			items := make([]map[string]any, 0, len(topology.Exchanges))
			for _, exchange := range topology.Exchanges {
				arguments := exchangeArguments(exchange)
				if exchange.Name == exchangeName {
					arguments = nil // 模拟没有备用交换机的旧版交换机。
				}
				items = append(items, map[string]any{"name": exchange.Name, "type": exchange.Kind, "durable": exchange.Durable, "arguments": arguments})
			}
			_ = json.NewEncoder(response).Encode(items)
		case strings.Contains(request.URL.Path, "/queues/"):
			items := make([]map[string]any, 0, len(topology.Queues))
			for _, queue := range topology.Queues {
				items = append(items, map[string]any{"name": queue.Name, "durable": true, "arguments": queueArguments(queue), "messages_ready": 0, "messages_unacknowledged": 0, "consumers": 0})
			}
			_ = json.NewEncoder(response).Encode(items)
		case strings.Contains(request.URL.Path, "/bindings/"):
			items := make([]map[string]any, 0, len(topology.Bindings))
			for _, binding := range topology.Bindings {
				items = append(items, map[string]any{"source": binding.Exchange, "destination": binding.Queue, "destination_type": "queue", "routing_key": binding.RoutingKey})
			}
			_ = json.NewEncoder(response).Encode(items)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	manager, err := rabbitinfra.Open(context.Background(), config.RabbitMQConfig{
		URL: "amqp://guest:guest@127.0.0.1:1/test-vhost", ManagementURL: server.URL + "/api",
		DialTimeout: 10 * time.Millisecond, ReconnectMin: time.Millisecond, ReconnectMax: time.Millisecond,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	drift, _, complete := VerifyManagedTopology(context.Background(), manager, topology)
	if !complete {
		t.Fatal("expected complete management topology snapshot")
	}
	found := false
	for _, item := range drift {
		if item.Resource == "exchange" && item.Name == exchangeName && item.Problem == "argument_conflict:alternate-exchange" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected alternate exchange drift, got %+v", drift)
	}
}

// TestRegistrySchemaFilesExistAndAreValidJSON 验证注册表模式文件存在且为有效 JSON。
func TestRegistrySchemaFilesExistAndAreValidJSON(t *testing.T) {
	seen := map[string]bool{}
	for _, definition := range MustDefaultEventRegistry().Definitions() {
		path := filepath.Join("contracts", strings.SplitN(definition.PayloadSchema, "#", 2)[0])
		if seen[path] {
			continue
		}
		seen[path] = true
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read payload schema %s: %v", path, err)
		}
		if !json.Valid(body) {
			t.Fatalf("payload schema %s is not valid JSON", path)
		}
	}
	for _, path := range []string{"contracts/schemas/envelope.v1.json"} {
		body, err := os.ReadFile(path)
		if err != nil || !json.Valid(body) {
			t.Fatalf("invalid contract schema %s: %v", path, err)
		}
	}
}

// collectLiteralOutboxEvents 收集Literal 发件箱事件 Events。
func collectLiteralOutboxEvents(t *testing.T, root string) map[string]bool {
	t.Helper()
	result := map[string]bool{}
	files, err := filepath.Glob(filepath.Join(root, "*", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.CallExpr:
				name := calledName(typed.Fun)
				if name == "createOutbox" || name == "createOutboxWithID" || name == "outbox" || name == "event" || name == "CreateOutbox" {
					for _, argument := range typed.Args {
						addEventLiteral(result, argument)
					}
				}
			case *ast.CompositeLit:
				for _, element := range typed.Elts {
					if pair, ok := element.(*ast.KeyValueExpr); ok {
						if identifier, ok := pair.Key.(*ast.Ident); ok && identifier.Name == "EventType" {
							addEventLiteral(result, pair.Value)
						}
						if literal, ok := pair.Key.(*ast.BasicLit); ok && literal.Kind == token.STRING {
							key, _ := strconv.Unquote(literal.Value)
							if key == "event_type" {
								addEventLiteral(result, pair.Value)
							}
						}
					}
				}
			}
			return true
		})
	}
	return result
}

// addEventLiteral 添加事件 Literal。
func addEventLiteral(result map[string]bool, expression ast.Expr) {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return
	}
	value, err := strconv.Unquote(literal.Value)
	if err == nil && eventTypePattern.MatchString(value) {
		result[value] = true
	}
}

// calledName 返回已调用的处理器名称。
func calledName(expression ast.Expr) string {
	switch typed := expression.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		return typed.Sel.Name
	default:
		return ""
	}
}

// topicMatches 判断主题是否匹配。
func topicMatches(pattern, routingKey string) bool {
	patternParts, keyParts := strings.Split(pattern, "."), strings.Split(routingKey, ".")
	for len(patternParts) > 0 {
		part := patternParts[0]
		patternParts = patternParts[1:]
		if part == "#" {
			return true
		}
		if len(keyParts) == 0 {
			return false
		}
		if part != "*" && part != keyParts[0] {
			return false
		}
		keyParts = keyParts[1:]
	}
	return len(keyParts) == 0
}
