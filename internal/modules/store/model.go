package store

import (
	"time"

	"gorm.io/datatypes"
)

type Order struct {
	ID                      uint64
	OrderNo                 string
	CustomerID              uint64
	MerchantID              uint64
	ShopID                  uint64
	Status                  string
	PayStatus               string
	DeliveryStatus          string
	GoodsAmount             int64
	DiscountAmount          int64
	DeliveryFeeAmount       int64
	PayableAmount           int64
	PaidAmount              int64
	Remark                  *string
	AddressSnapshot         datatypes.JSON
	DeliveryPromiseSnapshot datatypes.JSON
	ComplianceSnapshot      datatypes.JSON
	ExpiresAt               *time.Time
	CancelSource            *string
	CancelReasonCode        *string
	Version                 int
	PaidAt                  *time.Time
	CancelledAt             *time.Time
	CompletedAt             *time.Time
	CreatedAt               time.Time
	UpdatedAt               time.Time
	DeletedAt               *time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Order) TableName() string { return "orders" }

type OrderItem struct {
	ID              uint64
	OrderID         uint64
	ShopProductID   uint64
	ProductID       uint64
	ProductSnapshot datatypes.JSON
	Quantity        int
	SalePriceAmount int64
	TotalAmount     int64
	DeletedAt       *time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (OrderItem) TableName() string { return "order_items" }

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
	CreatedAt  time.Time
	DeletedAt  *time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (OrderLog) TableName() string { return "order_logs" }

type DeliveryOrder struct {
	ID                uint64
	OrderID           uint64
	ShopID            uint64
	RiderID           *uint64
	Status            string
	AssignmentVersion uint
	DispatchStatus    string
	PickupReadyStatus string
	PickupReadyAt     *time.Time
	PickupSnapshot    datatypes.JSON
	RecipientSnapshot datatypes.JSON
	DeletedAt         *time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (DeliveryOrder) TableName() string { return "delivery_orders" }

type Shop struct {
	ID               uint64
	MerchantID       uint64
	Name             string
	Phone            *string
	Province         *string
	City             string
	CityCode         *string
	District         string
	Address          string
	Latitude         *float64
	Longitude        *float64
	CoordinateSystem string
	Status           string
	BusinessStatus   string
	DeletedAt        *time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Shop) TableName() string { return "shops" }

type Product struct {
	ID                  uint64
	CategoryID          uint64
	Name                string
	BrandName           *string
	Spec                *string
	ImageURL            *string
	Description         *string
	SalePriceAmount     int64
	OriginalPriceAmount int64
	Status              string
	AgeRestricted       bool
}

// TableName 返回当前数据模型对应的数据库表名。
func (Product) TableName() string { return "products" }

type ShopProduct struct {
	ID              uint64
	MerchantID      uint64
	ShopID          uint64
	ProductID       uint64
	SalePriceAmount int64
	Status          string
	SortOrder       int
}

// TableName 返回当前数据模型对应的数据库表名。
func (ShopProduct) TableName() string { return "shop_products" }

type ProductStock struct {
	ID                uint64
	ShopProductID     uint64
	ShopID            uint64
	ProductID         uint64
	AvailableQty      int
	ReservedQty       int
	LockedQty         int
	LowStockThreshold int
	Version           int
}

// TableName 返回当前数据模型对应的数据库表名。
func (ProductStock) TableName() string { return "product_stocks" }

type Payment struct {
	ID             uint64
	PaymentNo      string
	OrderID        uint64
	Channel        string
	Provider       string
	Status         string
	Amount         int64
	Currency       string
	RefundedAmount int64
	ExpiresAt      *time.Time
	PaidAt         *time.Time
	CreatedAt      time.Time
	DeletedAt      *time.Time
}

// TableName 返回当前数据模型对应的数据表名。
func (Payment) TableName() string { return "payments" }

type ShopProductRow struct {
	ID                   uint64
	MerchantID           uint64
	ShopID               uint64
	ProductID            uint64
	CategoryID           uint64
	Name                 string
	BrandName            *string
	Spec                 *string
	ImageURL             *string
	SalePriceAmount      int64
	OriginalPriceAmount  int64
	Status               string
	SortOrder            int
	AvailableQty         int
	ReservedQty          int
	LockedQty            int
	LowStockThreshold    int
	Version              int
	UpdatedAt            time.Time  `gorm:"-"`
	StockUpdatedAt       *time.Time `gorm:"column:stock_updated_at"`
	ShopProductUpdatedAt time.Time  `gorm:"column:shop_product_updated_at"`
	AgeRestricted        bool
}

type StoreOrderListFilters struct {
	ShopID   uint64
	Status   string
	Keyword  string
	OrderNo  string
	PaidFrom *time.Time
	PaidTo   *time.Time
}

type StoreInventoryFilters struct {
	ShopID       uint64
	Status       string
	Keyword      string
	LowStockOnly bool
}

type StockRecord struct {
	ID                 uint64
	ShopProductID      uint64
	ShopID             uint64
	ProductID          uint64
	ChangeType         string
	QuantityDelta      int
	BeforeAvailableQty int
	AfterAvailableQty  int
	TotalQuantityDelta int
	BeforeTotalQty     int
	AfterTotalQty      int
	SourceType         string
	SourceID           uint64
	IdempotencyKey     *string
}

// TableName 返回当前数据模型对应的数据库表名。
func (StockRecord) TableName() string { return "stock_records" }

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
