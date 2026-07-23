package asset

import (
	"time"

	"gorm.io/datatypes"
)

const (
	TypeGrowth   = "growth_value"
	TypeWineCoin = "wine_coin"
	TypeBalance  = "balance"
	UnitPoint    = "POINT"
	UnitCNY      = "CNY_MINOR"
)

type Account struct {
	ID            uint64
	AccountNo     string
	OwnerType     string
	OwnerID       uint64
	AssetType     string
	Unit          string
	Status        string
	AllowNegative bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Account) TableName() string { return "asset_accounts" }

type Balance struct {
	ID        uint64
	AccountID uint64
	Bucket    string
	Amount    int64
	Version   uint32
	UpdatedAt time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Balance) TableName() string { return "asset_balances" }

type Transaction struct {
	ID                      uint64
	TransactionNo           string
	AssetType               string
	Unit                    string
	Action                  string
	Status                  string
	SourceType              string
	SourceID                string
	IdempotencyKeyHash      string
	RequestHash             string
	ReversalOfTransactionID *uint64
	Amount                  int64
	ActorType               string
	ActorID                 uint64
	Metadata                datatypes.JSON
	OccurredAt              time.Time
	PostedAt                time.Time
	CreatedAt               time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Transaction) TableName() string { return "asset_transactions" }

type Entry struct {
	ID            uint64
	TransactionID uint64
	EntrySeq      uint32
	AccountID     uint64
	Bucket        string
	Delta         int64
	BalanceAfter  int64
	CreatedAt     time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Entry) TableName() string { return "asset_entries" }

type Lot struct {
	ID                 uint64
	AccountID          uint64
	GrantTransactionID uint64
	GrantedAmount      int64
	AvailableAmount    int64
	FrozenAmount       int64
	ConsumedAmount     int64
	ExpiredAmount      int64
	ExpiresAt          *time.Time
	Status             string
	Version            uint32
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Lot) TableName() string { return "asset_lots" }

type Hold struct {
	ID              uint64
	HoldNo          string
	ReservationKey  string
	AccountID       uint64
	AssetType       string
	Unit            string
	OriginalAmount  int64
	CommittedAmount int64
	ReleasedAmount  int64
	Status          string
	SourceType      string
	SourceID        string
	ExpiresAt       *time.Time
	Version         uint32
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Hold) TableName() string { return "asset_holds" }

type HoldLot struct {
	ID              uint64
	HoldID          uint64
	LotID           uint64
	Amount          int64
	CommittedAmount int64
	ReleasedAmount  int64
}

// TableName 返回当前数据模型对应的数据库表名。
func (HoldLot) TableName() string { return "asset_hold_lots" }

type Adjustment struct {
	ID                 uint64
	AdjustmentNo       string
	CustomerID         uint64
	AssetType          string
	Unit               string
	Direction          string
	Amount             int64
	ReasonCode         string
	Reason             string
	EvidenceRefs       datatypes.JSON
	Status             string
	CreatedBy          uint64
	ReviewedBy         *uint64
	ReviewedAt         *time.Time
	AssetTransactionID *uint64
	FailureCode        *string
	IdempotencyKeyHash string
	Version            uint32
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Adjustment) TableName() string { return "asset_adjustments" }

type ReconciliationJob struct {
	ID, RequestedBy                                           uint64
	JobNo, Scope, Mode, Status, RequestID, IdempotencyKeyHash string
	ScopeID                                                   *string
	ScannedCount, DifferenceCount, CriticalCount              uint64
	StartedAt, FinishedAt                                     *time.Time
	CreatedAt, UpdatedAt                                      time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (ReconciliationJob) TableName() string { return "asset_reconciliation_jobs" }

type ReconciliationItem struct {
	ID, JobID                    uint64
	ObjectType, ObjectID         string
	DiffType, Severity, Status   string
	ExpectedAmount, ActualAmount *int64
	Detail                       datatypes.JSON
	CreatedAt, UpdatedAt         time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (ReconciliationItem) TableName() string { return "asset_reconciliation_items" }

type AuditLog struct {
	ID, ActorID, ResourceID                 uint64
	ActorType, Action, ResourceType, Result string
	BeforeData, AfterData                   datatypes.JSON
	EventID                                 *string
	AccountID, ShopID, OrderID, DeliveryID  *uint64
	Version                                 *uint64
	ErrorCode, ReasonCode                   *string
	BeforeStatus, AfterStatus               *string
	RequestID, IP, IPHash, UserAgent        *string
}

// TableName 返回当前数据模型对应的数据库表名。
func (AuditLog) TableName() string { return "audit_logs" }

type Outbox struct {
	ID, AggregateID                           uint64
	EventID, EventType, AggregateType, Status string
	Payload                                   datatypes.JSON
	RequestID                                 *string
}

// TableName 返回当前数据模型对应的数据库表名。
func (Outbox) TableName() string { return "outbox_events" }

type Compensation struct {
	ID, CustomerID, AfterSaleID             uint64
	CompensationNo, Type, AssetType, Status string
	Amount                                  int64
	Reason                                  *string
	AssetTransactionID                      *uint64
	FailureCode                             *string
	Attempts                                uint32
	NextRetryAt, LockedUntil                *time.Time
	LockedBy                                *string
}

// TableName 返回当前数据模型对应的数据库表名。
func (Compensation) TableName() string { return "compensation_ledger" }
