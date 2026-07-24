package mq

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"jiuxiaoer-admin/backend-go/internal/infra/rabbitmq"
)

const (
	exchangeName            = "jxe.events.topic.v2"
	unroutedExchangeName    = "jxe.events.unrouted"
	deadExchangeName        = "jxe.events.dead"
	unroutedQueueName       = "jxe.events.unrouted.queue"
	cacheQueueName          = "jxe.cache.v1.queue"
	notificationQueueName   = "jxe.notification.v1.queue"
	printQueueName          = "jxe.print.v1.queue"
	securityQueueName       = "jxe.security.v1.queue"
	dispatchQueueName       = "jxe.dispatch.v1.queue"
	realtimeQueueName       = "jxe.realtime.v1.queue"
	topologyContractVersion = "v1"
)

type ExchangeSpec struct {
	Name      string
	Kind      string
	Durable   bool
	Alternate string
}

type QueueSpec struct {
	Name       string
	Consumer   string
	RoutingKey string
	DeadQueue  bool
	RetryDelay time.Duration
}

type BindingSpec struct {
	Exchange   string
	Queue      string
	RoutingKey string
}

type Topology struct {
	Version   string
	Exchanges []ExchangeSpec
	Queues    []QueueSpec
	Bindings  []BindingSpec
}

// DefaultTopology 返回默认项拓扑。
func DefaultTopology() Topology {
	topology := Topology{
		Version: topologyContractVersion,
		Exchanges: []ExchangeSpec{
			{Name: exchangeName, Kind: "topic", Durable: true, Alternate: unroutedExchangeName},
			{Name: unroutedExchangeName, Kind: "fanout", Durable: true},
			{Name: deadExchangeName, Kind: "topic", Durable: true},
		},
	}
	consumers := []struct {
		name     string
		queue    string
		bindings []string
		retries  []time.Duration
	}{
		{name: "notification", queue: notificationQueueName, bindings: []string{"order.paid", "payment.succeeded", "store.order.*", "delivery.*", "delivery.incident.*", "dispatch.offer.created", "dispatch.grab.opened", "dispatch.manual_required", "order.cancelled", "refund.succeeded", "account.*.requested"}, retries: []time.Duration{10 * time.Second, time.Minute, 10 * time.Minute}},
		{name: "print", queue: printQueueName, bindings: []string{"print.task.ready", "print.task.retry_requested"}, retries: []time.Duration{10 * time.Second, time.Minute, 10 * time.Minute}},
		{name: "cache", queue: cacheQueueName, bindings: []string{"cache.invalidate"}, retries: []time.Duration{5 * time.Second, 30 * time.Second, 5 * time.Minute}},
		{name: "security", queue: securityQueueName, bindings: []string{"delivery.verification.*", "delivery.reassigned", "delivery.force_completed", "dispatch.policy.published", "identity.verification.*", "account.status_changed", "account.password_reset.requested"}, retries: []time.Duration{time.Minute, 10 * time.Minute}},
		{name: "dispatch", queue: dispatchQueueName, bindings: []string{"dispatch.job.ready", "dispatch.job.retry_requested", "dispatch.policy.published"}, retries: []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}},
		{name: "realtime", queue: realtimeQueueName, bindings: []string{"order.paid", "dispatch.offer.created", "dispatch.offer.rejected", "dispatch.offer.expired", "dispatch.grab.opened", "dispatch.manual_required", "delivery.assigned", "delivery.reassigned", "order.cancelled"}, retries: []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}},
	}
	for _, consumer := range consumers {
		topology.Queues = append(topology.Queues, QueueSpec{Name: consumer.queue, Consumer: consumer.name})
		for _, routingKey := range consumer.bindings {
			topology.Bindings = append(topology.Bindings, BindingSpec{Exchange: exchangeName, Queue: consumer.queue, RoutingKey: routingKey})
		}
		deadQueue := fmt.Sprintf("jxe.%s.dead.v1.queue", consumer.name)
		topology.Queues = append(topology.Queues, QueueSpec{Name: deadQueue, Consumer: consumer.name, DeadQueue: true})
		topology.Bindings = append(topology.Bindings, BindingSpec{Exchange: deadExchangeName, Queue: deadQueue, RoutingKey: consumer.name + ".dead"})
		for _, delay := range consumer.retries {
			retryExchange := retryExchangeName(consumer.name, delay)
			topology.Exchanges = append(topology.Exchanges, ExchangeSpec{Name: retryExchange, Kind: "topic", Durable: true})
			retryQueue := retryQueueName(consumer.name, delay)
			topology.Queues = append(topology.Queues, QueueSpec{
				Name: retryQueue, Consumer: consumer.name, RetryDelay: delay,
			})
			topology.Bindings = append(topology.Bindings, BindingSpec{Exchange: retryExchange, Queue: retryQueue, RoutingKey: "#"})
		}
	}
	topology.Queues = append(topology.Queues, QueueSpec{Name: unroutedQueueName, Consumer: "unrouted", DeadQueue: true})
	topology.Bindings = append(topology.Bindings, BindingSpec{Exchange: unroutedExchangeName, Queue: unroutedQueueName})
	return topology
}

// retryExchangeName 重试Exchange Name。
func retryExchangeName(consumer string, delay time.Duration) string {
	label := fmt.Sprintf("%ds", int(delay.Seconds()))
	if delay >= time.Minute && delay%time.Minute == 0 {
		label = fmt.Sprintf("%dm", int(delay.Minutes()))
	}
	return fmt.Sprintf("jxe.%s.retry.%s.v1", consumer, label)
}

// retryQueueName 重试队列 Name。
func retryQueueName(consumer string, delay time.Duration) string {
	label := fmt.Sprintf("%ds", int(delay.Seconds()))
	if delay >= time.Minute && delay%time.Minute == 0 {
		label = fmt.Sprintf("%dm", int(delay.Minutes()))
	}
	return fmt.Sprintf("jxe.%s.retry.%s.v1.queue", consumer, label)
}

// DeclareTopology 返回Declare 拓扑。
func DeclareTopology(channel *amqp.Channel, topology Topology) error {
	for _, exchange := range topology.Exchanges {
		args := exchangeArguments(exchange)
		// 备用交换机必须先于引用它们的主交换机存在。
		if exchange.Name == exchangeName {
			continue
		}
		if err := channel.ExchangeDeclare(exchange.Name, exchange.Kind, exchange.Durable, false, false, false, args); err != nil {
			return fmt.Errorf("declare exchange %s: %w", exchange.Name, err)
		}
	}
	for _, exchange := range topology.Exchanges {
		if exchange.Name != exchangeName {
			continue
		}
		if err := channel.ExchangeDeclare(exchange.Name, exchange.Kind, exchange.Durable, false, false, false, exchangeArguments(exchange)); err != nil {
			return fmt.Errorf("declare exchange %s: %w", exchange.Name, err)
		}
	}
	for _, queue := range topology.Queues {
		args := queueArguments(queue)
		if _, err := channel.QueueDeclare(queue.Name, true, false, false, false, args); err != nil {
			return fmt.Errorf("declare queue %s: %w", queue.Name, err)
		}
	}
	for _, binding := range topology.Bindings {
		if err := channel.QueueBind(binding.Queue, binding.RoutingKey, binding.Exchange, false, nil); err != nil {
			return fmt.Errorf("bind %s to %s with %s: %w", binding.Queue, binding.Exchange, binding.RoutingKey, err)
		}
	}
	return nil
}

// exchangeArguments 返回交换机参数。
func exchangeArguments(exchange ExchangeSpec) amqp.Table {
	if exchange.Alternate == "" {
		return nil
	}
	return amqp.Table{"alternate-exchange": exchange.Alternate}
}

// queueArguments 返回队列 Arguments。
func queueArguments(queue QueueSpec) amqp.Table {
	if queue.RetryDelay > 0 {
		arguments := amqp.Table{"x-message-ttl": int32(queue.RetryDelay.Milliseconds()), "x-dead-letter-exchange": exchangeName}
		if queue.RoutingKey != "" {
			arguments["x-dead-letter-routing-key"] = queue.RoutingKey
		}
		return arguments
	}
	if !queue.DeadQueue {
		return amqp.Table{"x-dead-letter-exchange": deadExchangeName, "x-dead-letter-routing-key": queue.Consumer + ".dead"}
	}
	return nil
}

type TopologyDrift struct {
	Resource string `json:"resource"`
	Name     string `json:"name"`
	Problem  string `json:"problem"`
}

type QueueObservation struct {
	Name                 string
	Ready                int
	Unacknowledged       int
	Consumers            int
	HeadMessageTimestamp int64
}

type managedExchange struct {
	Name      string         `json:"name"`
	Kind      string         `json:"type"`
	Durable   bool           `json:"durable"`
	Arguments map[string]any `json:"arguments"`
}

type managedQueue struct {
	Name                   string         `json:"name"`
	Durable                bool           `json:"durable"`
	Arguments              map[string]any `json:"arguments"`
	MessagesReady          int            `json:"messages_ready"`
	MessagesUnacknowledged int            `json:"messages_unacknowledged"`
	Consumers              int            `json:"consumers"`
	HeadMessageTimestamp   any            `json:"head_message_timestamp"`
}

type managedBinding struct {
	Source          string `json:"source"`
	Destination     string `json:"destination"`
	DestinationType string `json:"destination_type"`
	RoutingKey      string `json:"routing_key"`
}

// VerifyManagedTopology 核验Managed 拓扑是否有效。
// VerifyManagedTopology 使用 RabbitMQ 只读管理 API，因为 AMQP 被动声明
// 不会公开备用交换机、死信交换机或 TTL 值。管理状态不可用时布尔值为 false，
// 调用方应返回部分响应或降级到较弱的被动检查。
func VerifyManagedTopology(ctx context.Context, manager *rabbitmq.Manager, topology Topology) ([]TopologyDrift, map[string]QueueObservation, bool) {
	var exchanges []managedExchange
	var queues []managedQueue
	var bindings []managedBinding
	if manager == nil || manager.ManagementGet(ctx, "exchanges", &exchanges) != nil || manager.ManagementGet(ctx, "queues", &queues) != nil || manager.ManagementGet(ctx, "bindings", &bindings) != nil {
		return nil, nil, false
	}
	drift := make([]TopologyDrift, 0)
	exchangeByName := make(map[string]managedExchange, len(exchanges))
	for _, item := range exchanges {
		exchangeByName[item.Name] = item
	}
	for _, expected := range topology.Exchanges {
		actual, ok := exchangeByName[expected.Name]
		if !ok {
			drift = append(drift, TopologyDrift{Resource: "exchange", Name: expected.Name, Problem: "missing"})
			continue
		}
		if actual.Kind != expected.Kind || actual.Durable != expected.Durable {
			drift = append(drift, TopologyDrift{Resource: "exchange", Name: expected.Name, Problem: "type_or_durability_conflict"})
		}
		for key, value := range exchangeArguments(expected) {
			if !topologyArgumentEqual(actual.Arguments[key], value) {
				drift = append(drift, TopologyDrift{Resource: "exchange", Name: expected.Name, Problem: "argument_conflict:" + key})
			}
		}
	}
	queueByName := make(map[string]managedQueue, len(queues))
	observations := make(map[string]QueueObservation, len(topology.Queues))
	for _, item := range queues {
		queueByName[item.Name] = item
	}
	for _, expected := range topology.Queues {
		actual, ok := queueByName[expected.Name]
		if !ok {
			drift = append(drift, TopologyDrift{Resource: "queue", Name: expected.Name, Problem: "missing"})
			continue
		}
		observations[expected.Name] = QueueObservation{Name: expected.Name, Ready: actual.MessagesReady, Unacknowledged: actual.MessagesUnacknowledged, Consumers: actual.Consumers, HeadMessageTimestamp: unixTimestamp(actual.HeadMessageTimestamp)}
		if actual.Durable != true {
			drift = append(drift, TopologyDrift{Resource: "queue", Name: expected.Name, Problem: "durability_conflict"})
		}
		for key, value := range queueArguments(expected) {
			if !topologyArgumentEqual(actual.Arguments[key], value) {
				drift = append(drift, TopologyDrift{Resource: "queue", Name: expected.Name, Problem: "argument_conflict:" + key})
			}
		}
	}
	expectedBindings := make(map[string]bool, len(topology.Bindings))
	for _, expected := range topology.Bindings {
		expectedBindings[bindingIdentity(expected.Exchange, expected.Queue, expected.RoutingKey)] = false
	}
	for _, actual := range bindings {
		if actual.DestinationType != "queue" {
			continue
		}
		identity := bindingIdentity(actual.Source, actual.Destination, actual.RoutingKey)
		if _, ok := expectedBindings[identity]; ok {
			expectedBindings[identity] = true
		}
	}
	for identity, found := range expectedBindings {
		if !found {
			drift = append(drift, TopologyDrift{Resource: "binding", Name: identity, Problem: "missing"})
		}
	}
	sort.Slice(drift, func(i, j int) bool {
		if drift[i].Resource == drift[j].Resource {
			if drift[i].Name == drift[j].Name {
				return drift[i].Problem < drift[j].Problem
			}
			return drift[i].Name < drift[j].Name
		}
		return drift[i].Resource < drift[j].Resource
	})
	return drift, observations, true
}

// bindingIdentity 返回绑定标识。
func bindingIdentity(exchange, queue, routingKey string) string {
	return exchange + "->" + queue + ":" + routingKey
}

// topologyArgumentEqual 判断拓扑参数是否相等。
func topologyArgumentEqual(actual, expected any) bool {
	if actual == nil || expected == nil {
		return actual == nil && expected == nil
	}
	actualNumber, actualNumeric := numericArgument(actual)
	expectedNumber, expectedNumeric := numericArgument(expected)
	if actualNumeric || expectedNumeric {
		return actualNumeric && expectedNumeric && actualNumber == expectedNumber
	}
	return fmt.Sprint(actual) == fmt.Sprint(expected)
}

// numericArgument 返回数值参数。
func numericArgument(value any) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int8:
		return float64(typed), true
	case int16:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case float32:
		return float64(typed), true
	case float64:
		return typed, !math.IsNaN(typed)
	default:
		return 0, false
	}
}

// unixTimestamp 返回 Unix 时间戳。
func unixTimestamp(value any) int64 {
	var timestamp int64
	switch typed := value.(type) {
	case float64:
		timestamp = int64(typed)
	case int64:
		timestamp = typed
	case string:
		if parsed, err := strconv.ParseInt(typed, 10, 64); err == nil {
			timestamp = parsed
		} else if parsedTime, err := time.Parse(time.RFC3339, typed); err == nil {
			return parsedTime.Unix()
		}
	}
	if timestamp > 1_000_000_000_000 {
		return timestamp / 1000
	}
	return timestamp
}

// VerifyTopology 核验拓扑是否有效。
// VerifyTopology 刻意采用被动方式：管理验证接口不得创建、删除或修复代理资源。
func VerifyTopology(connection *amqp.Connection, topology Topology) []TopologyDrift {
	drift := make([]TopologyDrift, 0)
	for _, exchange := range topology.Exchanges {
		channel, err := connection.Channel()
		if err != nil {
			return []TopologyDrift{{Resource: "connection", Problem: err.Error()}}
		}
		err = channel.ExchangeDeclarePassive(exchange.Name, exchange.Kind, exchange.Durable, false, false, false, nil)
		_ = channel.Close()
		if err != nil {
			drift = append(drift, TopologyDrift{Resource: "exchange", Name: exchange.Name, Problem: "missing_or_incompatible"})
		}
	}
	for _, queue := range topology.Queues {
		channel, err := connection.Channel()
		if err != nil {
			return append(drift, TopologyDrift{Resource: "connection", Problem: err.Error()})
		}
		_, err = channel.QueueDeclarePassive(queue.Name, true, false, false, false, nil)
		_ = channel.Close()
		if err != nil {
			drift = append(drift, TopologyDrift{Resource: "queue", Name: queue.Name, Problem: "missing_or_incompatible"})
		}
	}
	sort.Slice(drift, func(i, j int) bool {
		if drift[i].Resource == drift[j].Resource {
			return drift[i].Name < drift[j].Name
		}
		return drift[i].Resource < drift[j].Resource
	})
	return drift
}
