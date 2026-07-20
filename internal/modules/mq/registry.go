package mq

import (
	_ "embed"
	"fmt"
	"sort"

	"github.com/goccy/go-yaml"
)

type EventStatus string

const (
	EventActive       EventStatus = "active"
	EventInternalTask EventStatus = "internal_task"
	EventAuditOnly    EventStatus = "audit_only"
	EventNoConsumer   EventStatus = "no_consumer"
	EventDeprecated   EventStatus = "deprecated"
)

type EventDefinition struct {
	EventType      string      `yaml:"event_type"`
	Version        uint        `yaml:"version"`
	Owner          string      `yaml:"owner"`
	Status         EventStatus `yaml:"status"`
	Classification string      `yaml:"classification"`
	AggregateType  string      `yaml:"aggregate_type"`
	Consumers      []string    `yaml:"consumers"`
	PayloadSchema  string      `yaml:"payload_schema"`
	RequiredFields []string    `yaml:"required_fields"`
	MaxBytes       int         `yaml:"max_bytes"`
}

type EventRegistry struct {
	events map[string]EventDefinition
}

// NewEventRegistry 创建并初始化事件注册表。
func NewEventRegistry(definitions []EventDefinition) (*EventRegistry, error) {
	registry := &EventRegistry{events: make(map[string]EventDefinition, len(definitions))}
	for _, definition := range definitions {
		if definition.EventType == "" || definition.Version == 0 || definition.Owner == "" || definition.Classification == "" || definition.PayloadSchema == "" {
			return nil, fmt.Errorf("invalid event registry definition for %q", definition.EventType)
		}
		switch definition.Status {
		case EventActive, EventInternalTask, EventAuditOnly, EventNoConsumer, EventDeprecated:
		default:
			return nil, fmt.Errorf("event %q has invalid status %q", definition.EventType, definition.Status)
		}
		if _, exists := registry.events[definition.EventType]; exists {
			return nil, fmt.Errorf("duplicate event registry definition for %q", definition.EventType)
		}
		if definition.MaxBytes == 0 {
			definition.MaxBytes = defaultEventMaxBytes
		}
		if definition.Status == EventActive || definition.Status == EventInternalTask || definition.Status == EventDeprecated || definition.Status == EventAuditOnly {
			if len(definition.Consumers) == 0 {
				return nil, fmt.Errorf("routable event %q has no consumer", definition.EventType)
			}
		} else if len(definition.Consumers) != 0 {
			return nil, fmt.Errorf("non-routable event %q declares consumers", definition.EventType)
		}
		registry.events[definition.EventType] = definition
	}
	return registry, nil
}

// MustDefaultEventRegistry 返回Must 默认项事件注册表。
func MustDefaultEventRegistry() *EventRegistry {
	var contract struct {
		Version string            `yaml:"version"`
		Events  []EventDefinition `yaml:"events"`
	}
	if err := yaml.Unmarshal(defaultRegistryYAML, &contract); err != nil {
		panic(fmt.Errorf("decode embedded event registry: %w", err))
	}
	if contract.Version != topologyContractVersion {
		panic(fmt.Errorf("event registry version %q does not match topology %q", contract.Version, topologyContractVersion))
	}
	registry, err := NewEventRegistry(contract.Events)
	if err != nil {
		panic(err)
	}
	return registry
}

//go:embed contracts/registry.yaml
var defaultRegistryYAML []byte

// Lookup 查询并返回事件 Definition。
func (r *EventRegistry) Lookup(eventType string) (EventDefinition, bool) {
	if r == nil {
		return EventDefinition{}, false
	}
	definition, ok := r.events[eventType]
	return definition, ok
}

// Definitions 返回Definitions。
func (r *EventRegistry) Definitions() []EventDefinition {
	if r == nil {
		return nil
	}
	result := make([]EventDefinition, 0, len(r.events))
	for _, definition := range r.events {
		result = append(result, definition)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].EventType < result[j].EventType })
	return result
}

// Routable 判断Routable。
func (d EventDefinition) Routable() bool {
	return d.Status != EventNoConsumer && len(d.Consumers) > 0
}
