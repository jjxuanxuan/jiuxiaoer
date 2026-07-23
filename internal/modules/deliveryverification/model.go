package deliveryverification

import "time"

type Verification struct {
	ID                     uint64
	DeliveryOrderID        uint64
	Stage                  string
	ModeSnapshot           string
	CodeHash               string
	CodeCiphertext         []byte
	CodeMask               string
	PolicyVersion          string
	SecretKeyVersion       string
	Status                 string
	FailedAttempts         uint
	MaxAttempts            uint
	ExpiresAt              time.Time
	ActivatedAt            *time.Time
	InvalidatedAt          *time.Time
	InvalidationReasonCode *string
	LockedUntil            *time.Time
	VerifiedAt             *time.Time
	VerifiedByType         *string
	VerifiedByID           *uint64
	OverrideReasonCode     *string
	OverrideReason         *string
	Version                uint
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Verification) TableName() string { return "delivery_verifications" }

type Attempt struct {
	ID              uint64
	VerificationID  uint64
	DeliveryOrderID uint64
	Stage           string
	ActorType       string
	ActorID         uint64
	AccountID       *uint64
	Result          string
	FailureCode     *string
	AttemptNo       uint
	RequestID       *string
	IPHash          *string
	DeviceIDHash    *string
	ModeSnapshot    string
	CreatedAt       time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Attempt) TableName() string { return "delivery_verification_attempts" }

type CodeReq struct {
	PickupCode   string `json:"pickup_code"`
	DeliveryCode string `json:"delivery_code"`
}
type UnlockReq struct {
	Stage           string `json:"stage" binding:"required,oneof=pickup delivery"`
	ReasonCode      string `json:"reason_code" binding:"required,max=64"`
	Reason          string `json:"reason" binding:"required,min=2,max=255"`
	ExpectedVersion uint   `json:"expected_version" binding:"required,min=1"`
}
type VerificationDTO struct {
	DeliveryOrderID   string `json:"delivery_order_id"`
	Stage             string `json:"stage"`
	Status            string `json:"status"`
	Code              string `json:"code,omitempty"`
	CodeMask          string `json:"code_mask"`
	FailedAttempts    uint   `json:"failed_attempts,omitempty"`
	RemainingAttempts uint   `json:"remaining_attempts,omitempty"`
	ExpiresAt         string `json:"expires_at"`
	LockedUntil       string `json:"locked_until,omitempty"`
	VerifiedAt        string `json:"verified_at,omitempty"`
	Version           uint   `json:"version"`
}
