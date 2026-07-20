package order

import (
	"time"

	"gorm.io/datatypes"
)

type CustomerAddress struct {
	ID               uint64
	CustomerID       uint64
	ContactName      string
	ContactPhone     string
	Province         string
	City             string
	CityCode         *string
	District         string
	DistrictCode     *string
	AddressDetail    string
	Doorplate        *string
	POIID            *string `gorm:"column:poi_id"`
	FormattedAddress *string
	Latitude         *float64
	Longitude        *float64
	CoordinateSystem string
	LocationSource   string
	GeocodeProvider  *string
	GeocodeStatus    string
	GeocodedAt       *time.Time
	Version          uint32
}

// TableName 返回当前数据模型对应的数据库表名。
func (CustomerAddress) TableName() string { return "customer_addresses" }

type ShopProductRow struct {
	ShopProductID         uint64
	ShopID                uint64
	MerchantID            uint64
	ProductID             uint64
	CategoryID            uint64
	Name                  string
	BrandName             *string
	Spec                  *string
	ImageURL              *string
	SalePriceAmount       int64
	ReturnEligible        bool
	ReturnPolicyCode      string
	ReturnPolicyVersion   string
	SealedPackageRequired bool
	AgeRestricted         bool
	ProductStatus         string
	ShopProductStatus     string
	ShopStatus            string
	BusinessStatus        string
}

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
	IdempotencyKey          *string
	ExpiresAt               *time.Time
	CancelSource            *string
	CancelReasonCode        *string
	Version                 int
	PaidAt                  *time.Time
	CancelledAt             *time.Time
	CompletedAt             *time.Time
	CreatedAt               time.Time
	UpdatedAt               time.Time
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

type Payment struct {
	ID               uint64
	PaymentNo        string
	OrderID          uint64
	CustomerID       uint64
	Channel          string
	Provider         string
	ProviderTradeNo  *string
	ProviderStatus   *string
	ProviderPrepayID *string
	Status           string
	Amount           int64
	Currency         string
	ClientPayload    datatypes.JSON
	ExpiresAt        *time.Time
	PaidAt           *time.Time
	FailedAt         *time.Time
	FailureCode      *string
	RefundedAmount   int64
	Version          int
	IdempotencyKey   *string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Payment) TableName() string { return "payments" }

type PaymentCallback struct {
	ID              uint64
	Provider        string
	ProviderEventID string
	ProviderTradeNo *string
	PaymentID       *uint64
	PayloadHash     string
	SignatureValid  bool
	ProcessStatus   string
	ErrorCode       *string
	ReceivedAt      time.Time
	ProcessedAt     *time.Time
	RequestID       *string
}

// TableName 返回当前数据模型对应的数据库表名。
func (PaymentCallback) TableName() string { return "payment_callbacks" }

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
