package printjob

import (
	"context"
	"time"

	"gorm.io/datatypes"
)

type Setting struct {
	ID                 uint64
	ShopID             uint64
	Provider           string
	DeviceIDCiphertext []byte
	DeviceIDMask       string
	TemplateID         uint64
	Copies             uint8
	AutoPrintEvents    datatypes.JSON
	Enabled            bool
	Version            uint
	CreatedBy          uint64
	UpdatedBy          uint64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Setting) TableName() string { return "print_settings" }

type Task struct {
	ID                uint64
	TaskNo            string
	EventID           string
	OrderID           uint64
	ShopID            uint64
	EventType         string
	TemplateID        uint64
	TemplateVersion   string
	RenderPayload     datatypes.JSON
	ReprintSeq        uint
	Provider          string
	ProviderRequestID *string
	Status            string
	Attempts          uint
	NextRetryAt       *time.Time
	LockedUntil       *time.Time
	LockedBy          *string
	LastErrorCode     *string
	LastErrorSafe     *string
	SucceededAt       *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Task) TableName() string { return "print_tasks" }

type Attempt struct {
	ID                uint64
	PrintTaskID       uint64
	AttemptNo         uint
	Operation         string
	ProviderRequestID *string
	RequestHash       string
	Result            string
	ProviderStatus    *string
	ErrorCode         *string
	DurationMS        uint
	StartedAt         time.Time
	FinishedAt        time.Time
	RequestID         *string
}

// TableName 返回当前数据模型对应的数据库表名。
func (Attempt) TableName() string { return "print_attempts" }

type SettingPatchReq struct {
	Enabled         bool     `json:"enabled"`
	Provider        string   `json:"provider" binding:"required,max=32"`
	DeviceID        string   `json:"device_id" binding:"required,min=1,max=128"`
	TemplateID      string   `json:"template_id" binding:"required"`
	Copies          uint8    `json:"copies" binding:"required,min=1,max=3"`
	AutoPrintEvents []string `json:"auto_print_events" binding:"required,min=1"`
	Version         uint     `json:"version" binding:"required,min=1"`
}

type ReprintReq struct {
	Reason string `json:"reason" binding:"required,min=2,max=255"`
}

type RetryReq struct {
	Reason string `json:"reason" binding:"required,min=2,max=255"`
}

type SettingDTO struct {
	ID              string   `json:"id"`
	ShopID          string   `json:"shop_id"`
	Provider        string   `json:"provider"`
	DeviceIDMask    string   `json:"device_id_mask"`
	TemplateID      string   `json:"template_id"`
	Copies          uint8    `json:"copies"`
	AutoPrintEvents []string `json:"auto_print_events"`
	Enabled         bool     `json:"enabled"`
	Version         uint     `json:"version"`
}

type TaskDTO struct {
	ID                string `json:"id"`
	TaskNo            string `json:"task_no"`
	OrderID           string `json:"order_id"`
	ShopID            string `json:"shop_id"`
	EventType         string `json:"event_type"`
	TemplateID        string `json:"template_id"`
	TemplateVersion   string `json:"template_version"`
	ReprintSeq        uint   `json:"reprint_seq"`
	Provider          string `json:"provider"`
	ProviderRequestID string `json:"provider_request_id,omitempty"`
	Status            string `json:"status"`
	Attempts          uint   `json:"attempts"`
	NextRetryAt       string `json:"next_retry_at,omitempty"`
	LastErrorCode     string `json:"last_error_code,omitempty"`
	SucceededAt       string `json:"succeeded_at,omitempty"`
	CreatedAt         string `json:"created_at"`
}

type PrintRequest struct {
	TaskNo            string
	ProviderRequestID string
	DeviceID          string
	Copies            uint8
	Payload           []byte
}

type PrintResult struct {
	ProviderRequestID string
	Status            string
}

type Provider interface {
	Submit(ctx context.Context, req PrintRequest) (PrintResult, error)
	Query(ctx context.Context, providerRequestID string) (PrintResult, error)
}
