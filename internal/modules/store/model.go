package store

import (
	"time"

	"gorm.io/datatypes"
)

type Order struct {
	ID                uint64
	OrderNo           string
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
	PaidAt            *time.Time
	CancelledAt       *time.Time
	CompletedAt       *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
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
}

// TableName 返回当前数据模型对应的数据库表名。
func (OrderLog) TableName() string { return "order_logs" }

type DeliveryOrder struct {
	ID                uint64
	OrderID           uint64
	ShopID            uint64
	RiderID           *uint64
	Status            string
	DispatchStatus    string
	PickupReadyStatus string
	PickupReadyAt     *time.Time
	PickupSnapshot    datatypes.JSON
	RecipientSnapshot datatypes.JSON
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

type ShopProductRow struct {
	ID                  uint64
	MerchantID          uint64
	ShopID              uint64
	ProductID           uint64
	CategoryID          uint64
	Name                string
	BrandName           *string
	Spec                *string
	ImageURL            *string
	SalePriceAmount     int64
	OriginalPriceAmount int64
	Status              string
	SortOrder           int
	AvailableQty        int
	ReservedQty         int
	LockedQty           int
	AgeRestricted       bool
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
	SourceType         string
	SourceID           uint64
	IdempotencyKey     *string
}

// TableName 返回当前数据模型对应的数据库表名。
func (StockRecord) TableName() string { return "stock_records" }

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
