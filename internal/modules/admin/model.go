package admin

import (
	"time"

	"gorm.io/datatypes"
)

// 商品
type Product struct {
	ID                    uint64
	CategoryID            uint64
	Name                  string
	BrandName             *string
	Spec                  *string
	ImageURL              *string
	Description           *string
	SalePriceAmount       int64
	OriginalPriceAmount   int64
	Status                string
	ReturnEligible        bool
	ReturnPolicyCode      string
	ReturnPolicyVersion   string
	SealedPackageRequired bool
	AgeRestricted         bool
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Product) TableName() string { return "products" }

// 商户
type Merchant struct {
	ID           uint64
	Code         string
	Name         string
	ContactName  *string
	ContactPhone *string
	LicenseNo    *string
	Status       string
	ReviewStatus string
	ReviewRemark *string
	ReviewedAt   *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Merchant) TableName() string { return "merchants" }

// 订单
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
	AddressSnapshot   datatypes.JSON
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Order) TableName() string { return "orders" }

// 订单项
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

// 库存
type StockRow struct {
	ID                uint64
	ShopProductID     uint64
	ShopID            uint64
	MerchantID        uint64
	ProductID         uint64
	ProductName       string
	AvailableQty      int
	ReservedQty       int
	LockedQty         int
	LowStockThreshold int
	Version           int
	UpdatedAt         time.Time
}

// 商品库存
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

// 库存流水
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

// 审计日记
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
	CreatedAt    time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (AuditLog) TableName() string { return "audit_logs" }

// 发件箱事件
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
