package printjob

import (
	"context"
	"time"

	"gorm.io/datatypes"
)

type Setting struct {
	ID                  uint64
	ShopID              uint64
	Provider            string
	ProviderConfigRef   *string
	DeviceIDCiphertext  []byte
	DeviceIDMask        string
	DeviceStatus        string
	LastHealthAt        *time.Time
	LastHealthErrorCode *string
	TemplateID          uint64
	Copies              uint8
	AutoPrintEvents     datatypes.JSON
	Enabled             bool
	Version             uint
	CreatedBy           uint64
	UpdatedBy           uint64
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Setting) TableName() string { return "print_settings" }

type Task struct {
	ID                   uint64
	TaskNo               string
	EventID              string
	OrderID              uint64
	ShopID               uint64
	EventType            string
	TemplateID           uint64
	TemplateVersion      string
	RenderPayload        datatypes.JSON
	PayloadSchemaVersion string
	ReprintSeq           uint
	SourceTaskID         *uint64
	Provider             string
	ProviderRequestID    *string
	ProviderStatus       *string
	SubmittedAt          *time.Time
	ConfirmedAt          *time.Time
	CallbackDeadlineAt   *time.Time
	Status               string
	Attempts             uint
	NextRetryAt          *time.Time
	LockedUntil          *time.Time
	LockedBy             *string
	LastErrorCode        *string
	LastErrorSafe        *string
	SucceededAt          *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
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

type Template struct {
	ID                   uint64
	TemplateCode         string
	Version              string
	PaperWidthMM         uint16
	PayloadSchemaVersion string
	TemplateBody         string
	Status               string
	CreatedBy            uint64
	PublishedBy          *uint64
	PublishedAt          *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

func (Template) TableName() string { return "print_templates" }

type SettingCreateReq struct {
	ShopID          string   `json:"shop_id" binding:"required"`
	Enabled         bool     `json:"enabled"`
	Provider        string   `json:"provider" binding:"required,max=32"`
	DeviceID        string   `json:"device_id" binding:"required,min=1,max=128"`
	TemplateID      string   `json:"template_id" binding:"required"`
	Copies          uint8    `json:"copies" binding:"required,min=1,max=3"`
	AutoPrintEvents []string `json:"auto_print_events" binding:"required,min=1"`
}

type SettingPatchReq struct {
	Enabled         *bool     `json:"enabled,omitempty"`
	Provider        *string   `json:"provider,omitempty" binding:"omitempty,max=32"`
	DeviceID        *string   `json:"device_id,omitempty" binding:"omitempty,min=1,max=128"`
	TemplateID      *string   `json:"template_id,omitempty"`
	Copies          *uint8    `json:"copies,omitempty" binding:"omitempty,min=1,max=3"`
	AutoPrintEvents *[]string `json:"auto_print_events,omitempty" binding:"omitempty,min=1"`
	Version         uint      `json:"version" binding:"required,min=1"`
}

type ReprintReq struct {
	Reason string `json:"reason" binding:"required,min=2,max=255"`
}

type RetryReq struct {
	Reason string `json:"reason" binding:"required,min=2,max=255"`
}

type SettingDTO struct {
	ID                  string   `json:"id"`
	ShopID              string   `json:"shop_id"`
	Provider            string   `json:"provider"`
	DeviceIDMask        string   `json:"device_id_mask"`
	TemplateID          string   `json:"template_id"`
	Copies              uint8    `json:"copies"`
	AutoPrintEvents     []string `json:"auto_print_events"`
	Enabled             bool     `json:"enabled"`
	Version             uint     `json:"version"`
	DeviceStatus        string   `json:"device_status"`
	LastHealthAt        string   `json:"last_health_at,omitempty"`
	LastHealthErrorCode string   `json:"last_health_error_code,omitempty"`
}

type TaskDTO struct {
	ID                   string                 `json:"id"`
	TaskNo               string                 `json:"task_no"`
	OrderID              string                 `json:"order_id"`
	ShopID               string                 `json:"shop_id"`
	EventType            string                 `json:"event_type"`
	TemplateID           string                 `json:"template_id"`
	TemplateVersion      string                 `json:"template_version"`
	PayloadSchemaVersion string                 `json:"payload_schema_version"`
	SourceTaskID         string                 `json:"source_task_id,omitempty"`
	ReprintSeq           uint                   `json:"reprint_seq"`
	Provider             string                 `json:"provider"`
	ProviderRequestID    string                 `json:"provider_request_id,omitempty"`
	ProviderStatus       string                 `json:"provider_status,omitempty"`
	Status               string                 `json:"status"`
	Attempts             uint                   `json:"attempts"`
	NextRetryAt          string                 `json:"next_retry_at,omitempty"`
	LastErrorCode        string                 `json:"last_error_code,omitempty"`
	SucceededAt          string                 `json:"succeeded_at,omitempty"`
	RenderSummary        *PrintRenderSummaryDTO `json:"render_summary,omitempty"`
	CreatedAt            string                 `json:"created_at"`
}

type PrintRenderSummaryDTO struct {
	ItemKindCount int    `json:"item_kind_count"`
	TotalQuantity int    `json:"total_quantity"`
	PayableAmount int64  `json:"payable_amount"`
	PaperWidthMM  uint16 `json:"paper_width_mm"`
	LineCount     int    `json:"line_count,omitempty"`
	ContentHash   string `json:"content_hash"`
}

type TestPrintDTO struct {
	TaskID            string `json:"task_id"`
	ProviderRequestID string `json:"provider_request_id"`
	Status            string `json:"status"`
	SubmittedAt       string `json:"submitted_at"`
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
