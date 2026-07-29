package dispatch

import (
	"time"

	"gorm.io/datatypes"
)

type Policy struct {
	ID                       uint64
	PolicyCode               string
	ScopeType                string
	ScopeID                  string
	Version                  uint
	Mode                     string
	AutoRounds               uint8
	OfferTTLSeconds          uint
	GrabTTLSeconds           uint
	CandidateLimit           uint
	OfferCandidateLimit      uint
	HeartbeatFreshSeconds    uint
	LocationFreshSeconds     uint
	MaxLocationAccuracyM     uint
	MaxPickupDistanceM       uint
	MaxActiveOrdersDefault   uint8
	IdleFullScoreSeconds     uint
	ScoreWeights             datatypes.JSON
	RejectionCooldownSeconds uint
	Status                   string
	PublishedAt              *time.Time
	PublishedBy              *uint64
	RowVersion               uint
	CreatedAt                time.Time
	UpdatedAt                time.Time
	CreatedBy                uint64
	UpdatedBy                uint64
}

// TableName 返回当前数据模型对应的数据库表名。
func (Policy) TableName() string { return "dispatch_policies" }

type Job struct {
	ID               uint64
	JobNo            string
	DeliveryOrderID  uint64
	OrderID          uint64
	ShopID           uint64
	DispatchSeq      uint
	PolicyID         *uint64
	PolicyVersion    string
	PolicySnapshot   datatypes.JSON
	Mode             string
	Status           string
	RoundNo          uint8
	CandidateCursor  uint
	GrabOpenedAt     *time.Time
	GrabExpiresAt    *time.Time
	NextActionAt     time.Time
	LockedUntil      *time.Time
	LockedBy         *string
	Attempts         uint
	LastErrorCode    *string
	LastErrorSafe    *string
	StatusReasonCode *string
	StatusReasonSafe *string
	FirstStartedAt   *time.Time
	AssignedAt       *time.Time
	AssignedRiderID  *uint64
	Version          uint
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Job) TableName() string { return "dispatch_jobs" }

type Candidate struct {
	ID                  uint64
	JobID               uint64
	RiderID             uint64
	RankNo              uint
	Eligible            bool
	ExclusionCodes      datatypes.JSON
	DistanceM           *uint
	ActiveOrders        uint8
	MaxActiveOrders     uint8
	HeartbeatAgeSeconds *uint
	LocationAgeSeconds  *uint
	DistanceScore       *float64
	LoadScore           *float64
	IdleScore           *float64
	FreshnessScore      *float64
	RejectionPenalty    *float64
	FinalScore          *float64
	ScoreVersion        string
	InputSnapshot       datatypes.JSON
	CreatedAt           time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Candidate) TableName() string { return "dispatch_candidates" }

type Offer struct {
	ID                  uint64
	OfferNo             string
	JobID               uint64
	DeliveryOrderID     uint64
	RiderID             uint64
	RoundNo             uint8
	CandidateID         uint64
	Status              string
	ExpiresAt           time.Time
	RespondedAt         *time.Time
	RejectionReasonCode *string
	RejectionRemark     *string
	Version             uint
	RequestID           *string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Offer) TableName() string { return "dispatch_offers" }

type RiderRuntimeState struct {
	RiderID          uint64
	WorkStatus       string
	Latitude         *float64
	Longitude        *float64
	CoordinateSystem string `gorm:"default:gcj02"`
	AccuracyM        *float64
	CapturedAt       *time.Time
	HeartbeatAt      *time.Time
	DeviceIDHash     *string
	LastSequence     uint64
	OnlineSince      *time.Time
	LastAssignedAt   *time.Time
	MaxActiveOrders  *uint8
	Version          uint
	UpdatedAt        time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (RiderRuntimeState) TableName() string { return "rider_runtime_states" }

type RiderServiceShop struct {
	ID        uint64
	RiderID   uint64
	ShopID    uint64
	Status    string
	Source    string
	CreatedAt time.Time
	UpdatedAt time.Time
	CreatedBy *uint64
	UpdatedBy *uint64
}

// TableName 返回当前数据模型对应的数据库表名。
func (RiderServiceShop) TableName() string { return "rider_service_shops" }

type DeliveryOrder struct {
	ID                    uint64
	OrderID               uint64
	ShopID                uint64
	RiderID               *uint64
	Status                string
	AssignmentVersion     uint
	DispatchStatus        string
	CurrentDispatchJobID  *uint64
	DispatchModeSnapshot  *string
	DispatchPolicyVersion *string
	PickupReadyStatus     string
	PickupReadyAt         *time.Time
	PickupSnapshot        datatypes.JSON
	RecipientSnapshot     datatypes.JSON
	ScheduledStartAt      *time.Time
	ScheduledEndAt        *time.Time
	NotBeforeAt           *time.Time
	AcceptedAt            *time.Time
	PickedUpAt            *time.Time
	StartedAt             *time.Time
	CompletedAt           *time.Time
	CancelledAt           *time.Time
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (DeliveryOrder) TableName() string { return "delivery_orders" }

type Assignment struct {
	ID              uint64
	DeliveryOrderID uint64
	DispatchJobID   *uint64
	OfferID         *uint64
	FromRiderID     *uint64
	ToRiderID       uint64
	AssignmentType  string
	ScoreSnapshot   datatypes.JSON
	Status          string
	ReasonCode      *string
	Reason          *string
	ActorType       string
	ActorID         uint64
	VersionBefore   uint
	VersionAfter    uint
	RequestID       *string
	CreatedAt       time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Assignment) TableName() string { return "delivery_assignments" }

type domainOrder struct {
	ID              uint64
	OrderNo         string
	OrderType       string
	SettlementMode  string
	CustomerID      uint64
	MerchantID      uint64
	ShopID          uint64
	Status          string
	PayStatus       string
	PaidAmount      int64
	DeliveryStatus  string
	AddressSnapshot datatypes.JSON
	Version         uint
}

// TableName 返回当前数据模型对应的数据库表名。
func (domainOrder) TableName() string { return "orders" }

type riderRow struct {
	ID                uint64
	AccountID         uint64
	Status            string
	WorkStatus        string
	WorkStatusVersion uint
	ReviewStatus      string
	Capabilities      datatypes.JSON
}

// TableName 返回当前数据模型对应的数据库表名。
func (riderRow) TableName() string { return "riders" }

type shopRow struct {
	ID               uint64
	Name             string
	CityCode         *string
	District         string
	Address          string
	Latitude         *float64
	Longitude        *float64
	CoordinateSystem string
	Phone            *string
}

// TableName 返回当前数据模型对应的数据库表名。
func (shopRow) TableName() string { return "shops" }
