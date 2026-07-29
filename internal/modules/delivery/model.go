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
	ScheduledStartAt     *time.Time
	ScheduledEndAt       *time.Time
	NotBeforeAt          *time.Time
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
	OrderType            string     `gorm:"->;column:order_type"`
	SettlementMode       string     `gorm:"->;column:settlement_mode"`
}

// TableName 返回当前数据模型对应的数据库表名。
func (DeliveryOrder) TableName() string { return "delivery_orders" }

type Order struct {
	ID                uint64
	OrderNo           string
	OrderType         string
	SettlementMode    string
	CustomerID        uint64
	MerchantID        uint64
	ShopID            uint64
	Status            string
	PayStatus         string
	DeliveryStatus    string
	GoodsAmount       int64
	DiscountAmount    int64
	DeliveryFeeAmount int64
	PayableAmount     int64
	PaidAmount        int64
	Remark            *string
	AddressSnapshot   datatypes.JSON
	Version           int
	PaidAt            *time.Time
	CancelledAt       *time.Time
	CompletedAt       *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Order) TableName() string { return "orders" }

// OrderItem 是历史订单明细。配送详情刻意使用 ProductSnapshot，
// 避免后续商品目录修改改写骑手曾看到的内容。
type OrderItem struct {
	ID              uint64
	OrderID         uint64
	ShopProductID   uint64
	ProductID       uint64
	ProductSnapshot datatypes.JSON
	Quantity        int
	SalePriceAmount int64
	TotalAmount     int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (OrderItem) TableName() string { return "order_items" }

// Shop 只包含已分配骑手所需的门店字段。
// 联系人与地址历史仍来自 DeliveryOrder.PickupSnapshot。
type Shop struct {
	ID               uint64
	Name             string
	Phone            *string
	Province         *string
	City             string
	District         string
	Address          string
	Latitude         *float64
	Longitude        *float64
	CoordinateSystem string
	Status           string
	BusinessStatus   string
}

// TableName 返回当前数据模型对应的数据库表名。
func (Shop) TableName() string { return "shops" }

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
	EventID      *string
	ActorType    string
	ActorID      uint64
	AccountID    *uint64
	Action       string
	ResourceType string
	ResourceID   uint64
	ShopID       *uint64
	OrderID      *uint64
	DeliveryID   *uint64
	BeforeData   datatypes.JSON
	AfterData    datatypes.JSON
	Result       string
	ErrorCode    *string
	ReasonCode   *string
	BeforeStatus *string
	AfterStatus  *string
	Version      *uint64
	RequestID    *string
	IP           *string
	IPHash       *string
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
