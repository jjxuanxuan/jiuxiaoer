package mq

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	envelopeSpecVersion     = "1.0"
	defaultEventMaxBytes    = 64 << 10
	absoluteMessageMaxBytes = 256 << 10
)

var eventTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)+$`)

type EventEnvelope struct {
	SpecVersion   string          `json:"spec_version"`
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  uint            `json:"event_version"`
	AggregateType string          `json:"aggregate_type"`
	AggregateID   string          `json:"aggregate_id"`
	OccurredAt    time.Time       `json:"occurred_at"`
	Producer      string          `json:"producer"`
	RequestID     string          `json:"request_id,omitempty"`
	TraceID       string          `json:"trace_id,omitempty"`
	PartitionKey  string          `json:"partition_key"`
	Payload       json.RawMessage `json:"payload"`
	Metadata      EventMetadata   `json:"metadata"`
}

type EventMetadata struct {
	Environment     string `json:"environment"`
	SchemaRef       string `json:"schema_ref"`
	ReplayOfEventID string `json:"replay_of_event_id,omitempty"`
}

// BuildEnvelope 构建消息信封。
func BuildEnvelope(event OutboxEvent, definition EventDefinition, environment string) (EventEnvelope, error) {
	requestID := ""
	if event.RequestID != nil {
		requestID = *event.RequestID
	}
	eventVersion := event.EventVersion
	if eventVersion == 0 {
		eventVersion = definition.Version
	}
	producer := event.Producer
	if producer == "" {
		producer = definition.Owner
	}
	schemaRef := event.SchemaRef
	if schemaRef == "" {
		schemaRef = fmt.Sprintf("event://%s/%d", event.EventType, eventVersion)
	}
	partitionKey := event.PartitionKey
	if partitionKey == "" {
		partitionKey = event.AggregateType + ":" + strconv.FormatUint(event.AggregateID, 10)
	}
	envelope := EventEnvelope{
		SpecVersion: envelopeSpecVersion, EventID: event.EventID, EventType: event.EventType,
		EventVersion: eventVersion, AggregateType: event.AggregateType,
		AggregateID: strconv.FormatUint(event.AggregateID, 10), OccurredAt: event.CreatedAt,
		Producer: producer, RequestID: requestID, PartitionKey: partitionKey,
		Payload: json.RawMessage(event.Payload), Metadata: EventMetadata{Environment: environment, SchemaRef: schemaRef, ReplayOfEventID: event.ReplayOfEventID},
	}
	return envelope, ValidateEnvelope(envelope, definition)
}

// DecodeEnvelope 解码消息信封。
func DecodeEnvelope(body []byte, registry *EventRegistry) (EventEnvelope, EventDefinition, error) {
	if len(body) == 0 || len(body) > absoluteMessageMaxBytes {
		return EventEnvelope{}, EventDefinition{}, fmt.Errorf("MQ_MESSAGE_SIZE_INVALID")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	var envelope EventEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return EventEnvelope{}, EventDefinition{}, fmt.Errorf("MQ_ENVELOPE_INVALID: %w", err)
	}
	definition, ok := registry.Lookup(envelope.EventType)
	if !ok {
		return EventEnvelope{}, EventDefinition{}, fmt.Errorf("MQ_EVENT_UNREGISTERED")
	}
	if err := ValidateEnvelope(envelope, definition); err != nil {
		return EventEnvelope{}, EventDefinition{}, err
	}
	return envelope, definition, nil
}

// ValidateEnvelope 校验消息信封是否合法。
func ValidateEnvelope(envelope EventEnvelope, definition EventDefinition) error {
	if envelope.SpecVersion != envelopeSpecVersion {
		return fmt.Errorf("MQ_SPEC_VERSION_UNSUPPORTED")
	}
	if _, err := uuid.Parse(envelope.EventID); err != nil {
		return fmt.Errorf("MQ_EVENT_ID_INVALID")
	}
	if envelope.EventType != definition.EventType || !eventTypePattern.MatchString(envelope.EventType) {
		return fmt.Errorf("MQ_EVENT_TYPE_INVALID")
	}
	if envelope.EventVersion != definition.Version {
		return fmt.Errorf("MQ_EVENT_VERSION_UNSUPPORTED")
	}
	if envelope.AggregateType == "" || envelope.AggregateID == "" || envelope.Producer == "" || envelope.PartitionKey == "" {
		return fmt.Errorf("MQ_ENVELOPE_REQUIRED_FIELD_MISSING")
	}
	if envelope.OccurredAt.IsZero() {
		return fmt.Errorf("MQ_OCCURRED_AT_INVALID")
	}
	if len(envelope.Payload) == 0 || !json.Valid(envelope.Payload) {
		return fmt.Errorf("MQ_PAYLOAD_INVALID")
	}
	var payload any
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return fmt.Errorf("MQ_PAYLOAD_INVALID")
	}
	payloadObject, ok := payload.(map[string]any)
	if !ok {
		return fmt.Errorf("MQ_PAYLOAD_MUST_BE_OBJECT")
	}
	for _, field := range definition.RequiredFields {
		if value, exists := payloadObject[field]; !exists || value == nil {
			return fmt.Errorf("MQ_PAYLOAD_REQUIRED_FIELD_MISSING: %s", field)
		}
	}
	if sensitivePath := findSensitiveField(payload, "payload"); sensitivePath != "" {
		return fmt.Errorf("MQ_SENSITIVE_FIELD_FORBIDDEN: %s", sensitivePath)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("MQ_ENVELOPE_INVALID: %w", err)
	}
	maxBytes := definition.MaxBytes
	if maxBytes == 0 {
		maxBytes = defaultEventMaxBytes
	}
	if len(encoded) > maxBytes || len(encoded) > absoluteMessageMaxBytes {
		return fmt.Errorf("MQ_MESSAGE_TOO_LARGE")
	}
	return nil
}

// findSensitiveField 查找敏感信息 Field。
func findSensitiveField(value any, path string) string {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
			if isSensitiveKey(normalized) {
				return path + "." + key
			}
			if found := findSensitiveField(nested, path+"."+key); found != "" {
				return found
			}
		}
	case []any:
		for index, nested := range typed {
			if found := findSensitiveField(nested, fmt.Sprintf("%s[%d]", path, index)); found != "" {
				return found
			}
		}
	}
	return ""
}

// isSensitiveKey 判断敏感信息密钥是否成立。
func isSensitiveKey(key string) bool {
	if key == "token" || key == "password" || key == "secret" || key == "verification_code" || key == "id_card_number" || key == "document_no" || key == "document_number" || key == "identity_number" || key == "pickup_code" || key == "delivery_code" || key == "sms_code" || key == "auth_code" || key == "private_key" {
		return true
	}
	return strings.HasSuffix(key, "_token") || strings.HasSuffix(key, "_password") || strings.HasSuffix(key, "_secret") || strings.HasSuffix(key, "_private_key")
}
