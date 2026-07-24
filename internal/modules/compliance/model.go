package compliance

import "time"

const (
	StatusCreating = "creating_session"
	StatusPending  = "pending"
	StatusVerified = "verified"
	StatusRejected = "rejected"
	StatusExpired  = "expired"
	StatusError    = "error"
	StatusRevoked  = "revoked"

	AdultUnknown = "unknown"
	AdultAdult   = "adult"
	AdultMinor   = "minor"
)

// Request 表示一个服务商会话生命周期。为兼容增量迁移，表中仍保留旧版证件列，
// 但会话流程绝不写入原始身份材料或派生的证件与姓名哈希。
type Request struct {
	ID                  uint64
	RequestNo           string
	CustomerID          uint64
	Provider            string
	ProviderRequestID   *string
	DocumentType        string
	DocumentHash        string
	NameHash            string
	MaskedName          string
	MaskedDocumentNo    string
	Purpose             string
	StateHash           string
	Status              string
	AdultResult         string
	VerificationLevel   string
	PolicyVersion       string
	ConsentVersion      string
	BirthDate           *time.Time
	FailureCode         *string
	Attempts            uint
	SessionExpiresAt    *time.Time
	ExpiresAt           *time.Time
	VerifiedAt          *time.Time
	ResultHash          *string
	CallbackEventID     *string
	CallbackPayloadHash *string
	CallbackReceivedAt  *time.Time
	RevokedAt           *time.Time
	RevokedReason       *string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Request) TableName() string { return "identity_verification_requests" }

// Realname 是某个客户当前的服务端授权事实。成年验证默认长期有效；
// ExpiresAt 可为空，仅在服务商或策略提供有效期边界时填充。
type Realname struct {
	CustomerID        uint64
	RequestID         uint64
	Status            string
	Provider          string
	ProviderSubject   *string
	MaskedName        string
	MaskedDocumentNo  string
	AdultResult       string
	VerificationLevel string
	PolicyVersion     string
	ResultHash        *string
	BirthDate         *time.Time
	VerifiedAt        *time.Time
	ExpiresAt         *time.Time
	RevokedAt         *time.Time
	RevokedReason     *string
	Version           uint
	UpdatedAt         time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Realname) TableName() string { return "customer_realname_verifications" }

type Callback struct {
	ID                uint64
	Provider          string
	ProviderEventID   string
	ProviderRequestID string
	PayloadHash       string
	SignatureValid    bool
	ProcessStatus     string
	ErrorCode         *string
	ReceivedAt        time.Time
	ProcessedAt       *time.Time
	RequestID         *string
	CreatedAt         time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Callback) TableName() string { return "identity_verification_callbacks" }

type CreateSessionReq struct {
	Purpose           string `json:"purpose" binding:"omitempty,oneof=alcohol_purchase"`
	VerificationLevel string `json:"verification_level" binding:"required,oneof=identity identity_and_liveness"`
	ConsentVersion    string `json:"consent_version" binding:"required,min=1,max=64"`
}

type ReviewReq struct {
	Decision  string `json:"decision" binding:"required,oneof=approved rejected"`
	Reason    string `json:"reason" binding:"required,min=2,max=255"`
	ExpiresAt string `json:"valid_until"`
}

type VerificationDTO struct {
	ID                string `json:"verification_id,omitempty"`
	CustomerID        string `json:"customer_id,omitempty"`
	Status            string `json:"status"`
	Provider          string `json:"provider,omitempty"`
	AdultResult       string `json:"adult_result"`
	VerificationLevel string `json:"verification_level,omitempty"`
	SessionURL        string `json:"session_url,omitempty"`
	SessionExpiresAt  string `json:"session_expires_at,omitempty"`
	VerifiedAt        string `json:"verified_at,omitempty"`
	ValidUntil        string `json:"valid_until,omitempty"`
	RevokedAt         string `json:"revoked_at,omitempty"`
}

type ProviderSessionRequest struct {
	VerificationID    string
	SubjectReference  string
	Purpose           string
	VerificationLevel string
	State             string
}

type ProviderSession struct {
	RequestID string
	URL       string
	ExpiresAt time.Time
}

type ProviderCallback struct {
	EventID           string
	ProviderRequestID string
	State             string
}

type ProviderResult struct {
	RequestID         string
	Subject           string
	Status            string
	AdultResult       string
	VerificationLevel string
	ValidUntil        *time.Time
	ResultReference   string
}
