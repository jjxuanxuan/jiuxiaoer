package notification

import (
	"context"
	"time"

	"gorm.io/datatypes"
)

type Template struct {
	ID                 uint64
	TemplateCode       string
	EventType          string
	Channel            string
	ProviderTemplateID *string
	Version            string
	TitleTemplate      string
	BodyTemplate       string
	AllowedFields      datatypes.JSON
	Status             string
	CreatedBy          uint64
	PublishedBy        *uint64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Template) TableName() string { return "notification_templates" }

type Delivery struct {
	ID                uint64
	DeliveryNo        string
	EventID           string
	EventType         string
	RecipientType     string
	RecipientID       uint64
	Channel           string
	TemplateID        uint64
	TemplateVersion   string
	TargetCiphertext  []byte
	TargetMask        *string
	PayloadSnapshot   datatypes.JSON
	ProviderRequestID *string
	Status            string
	Attempts          uint
	NextRetryAt       *time.Time
	LockedUntil       *time.Time
	LockedBy          *string
	LastErrorCode     *string
	SentAt            *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Delivery) TableName() string { return "notification_deliveries" }

type Message struct {
	ID            uint64
	CustomerID    uint64
	SourceEventID string
	Type          string
	Title         string
	Summary       string
	TargetType    *string
	TargetID      *uint64
	ReadAt        *time.Time
	ArchivedAt    *time.Time
	CreatedAt     time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Message) TableName() string { return "message_inboxes" }

type TemplateReq struct {
	TemplateCode       string   `json:"template_code" binding:"required,max=64"`
	EventType          string   `json:"event_type" binding:"required,max=64"`
	Channel            string   `json:"channel" binding:"required,oneof=inbox wechat"`
	ProviderTemplateID string   `json:"provider_template_id" binding:"max=128"`
	Version            string   `json:"version" binding:"required,max=32"`
	TitleTemplate      string   `json:"title_template" binding:"required,max=255"`
	BodyTemplate       string   `json:"body_template" binding:"required,max=2000"`
	AllowedFields      []string `json:"allowed_fields"`
	Status             string   `json:"status" binding:"required,oneof=draft published retired"`
}
type RetryReq struct {
	Reason string `json:"reason" binding:"required,min=2,max=255"`
}
type MessageDTO struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Title      string `json:"title"`
	Summary    string `json:"summary"`
	TargetType string `json:"target_type,omitempty"`
	TargetID   string `json:"target_id,omitempty"`
	ReadAt     string `json:"read_at,omitempty"`
	CreatedAt  string `json:"created_at"`
}
type DeliveryDTO struct {
	ID              string `json:"id"`
	DeliveryNo      string `json:"delivery_no"`
	EventID         string `json:"event_id"`
	EventType       string `json:"event_type"`
	RecipientType   string `json:"recipient_type"`
	RecipientID     string `json:"recipient_id"`
	Channel         string `json:"channel"`
	TemplateID      string `json:"template_id"`
	TemplateVersion string `json:"template_version"`
	Status          string `json:"status"`
	Attempts        uint   `json:"attempts"`
	LastErrorCode   string `json:"last_error_code,omitempty"`
	SentAt          string `json:"sent_at,omitempty"`
	CreatedAt       string `json:"created_at"`
}
type TemplateDTO struct {
	ID                 string   `json:"id"`
	TemplateCode       string   `json:"template_code"`
	EventType          string   `json:"event_type"`
	Channel            string   `json:"channel"`
	ProviderTemplateID string   `json:"provider_template_id,omitempty"`
	Version            string   `json:"version"`
	TitleTemplate      string   `json:"title_template"`
	BodyTemplate       string   `json:"body_template"`
	AllowedFields      []string `json:"allowed_fields"`
	Status             string   `json:"status"`
}

type SendRequest struct {
	ProviderRequestID string
	TemplateID        string
	Recipient         string
	Payload           []byte
}
type SendResult struct {
	ProviderRequestID string
	Status            string
}
type Provider interface {
	Send(context.Context, SendRequest) (SendResult, error)
	Query(context.Context, string) (SendResult, error)
}
