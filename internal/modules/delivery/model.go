package delivery

import (
	"time"

	"gorm.io/datatypes"
)

type DeliveryOrder struct {
	ID                   uint64
	OrderID              uint64
	ShopID               uint64
	RiderID              *uint64
	Status               string
	AssignmentVersion    uint
	DispatchStatus       string
	CurrentDispatchJobID *uint64
	PickupReadyStatus    string
	PickupReadyAt        *time.Time
	PickupSnapshot       datatypes.JSON
	RecipientSnapshot    datatypes.JSON
	AcceptedAt           *time.Time
	PickedUpAt           *time.Time
	PickedUpVerifiedAt   *time.Time
	StartedAt            *time.Time
	CompletedAt          *time.Time
	CompletedVerifiedAt  *time.Time
	CancelledAt          *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
	ShopName             string     `gorm:"->;column:shop_name"`
	DestinationDistrict  string     `gorm:"->;column:destination_district"`
	ItemCount            int        `gorm:"->;column:item_count"`
	PickupDistanceM      *uint      `gorm:"->;column:pickup_distance_m"`
	GrabExpiresAt        *time.Time `gorm:"->;column:grab_expires_at"`
}

// TableName 返回当前数据模型对应的数据库表名。
func (DeliveryOrder) TableName() string { return "delivery_orders" }

type Order struct {
	ID             uint64
	OrderNo        string
	CustomerID     uint64
	MerchantID     uint64
	ShopID         uint64
	Status         string
	PayStatus      string
	DeliveryStatus string
	CompletedAt    *time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Order) TableName() string { return "orders" }

type OrderLog struct {
	ID         uint64
	OrderID    uint64
	ActorType  string
	ActorID    uint64
	Action     string
	FromStatus *string
	ToStatus   *string
	Remark     *string
	RequestID  *string
}

// TableName 返回当前数据模型对应的数据库表名。
func (OrderLog) TableName() string { return "order_logs" }

type AuditLog struct {
	ID           uint64
	ActorType    string
	ActorID      uint64
	Action       string
	ResourceType string
	ResourceID   uint64
	BeforeData   datatypes.JSON
	AfterData    datatypes.JSON
	Result       string
	RequestID    *string
	IP           *string
	UserAgent    *string
}

// TableName 返回当前数据模型对应的数据库表名。
func (AuditLog) TableName() string { return "audit_logs" }

type OutboxEvent struct {
	ID            uint64
	EventID       string
	EventType     string
	AggregateType string
	AggregateID   uint64
	Payload       datatypes.JSON
	Status        string
	RetryCount    int
	RequestID     *string
}

// TableName 返回当前数据模型对应的数据库表名。
func (OutboxEvent) TableName() string { return "outbox_events" }
