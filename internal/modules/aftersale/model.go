package aftersale

import (
	"time"

	"gorm.io/datatypes"
)

type AfterSale struct {
	ID, OrderID, CustomerID, MerchantID, ShopID                         uint64
	AfterSaleNo, Type, RequestedResolution, Status                      string
	InitiatorType, SourceType                                           string
	SourceID                                                            *uint64
	ApprovedResolution, ReasonCode                                      *string
	RequestedAmount, ApprovedAmount, RefundedAmount, CompensationAmount int64
	IncludeDeliveryFee                                                  bool
	Description                                                         string
	AppealedAt, ApprovedAt, RejectedAt, ClosedAt                        *time.Time
	SubmittedAt, CreatedAt, UpdatedAt                                   time.Time
	Version                                                             uint32
}

// TableName 返回当前数据模型对应的数据库表名。
func (AfterSale) TableName() string { return "after_sales" }

type Item struct {
	ID, AfterSaleID, OrderID, OrderItemID, ShopProductID, ProductID uint64
	RequestedQuantity, ApprovedQuantity                             int
	RequestedAmount, ApprovedAmount, RefundedAmount                 int64
	ReturnDisposition                                               string
}

// TableName 返回当前数据模型对应的数据库表名。
func (Item) TableName() string { return "after_sale_items" }

type Evidence struct {
	ID, AfterSaleID                              uint64
	TokenID, ObjectKey, MimeType, SHA256, Status string
	SizeBytes                                    uint64
	CreatedAt                                    time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Evidence) TableName() string { return "after_sale_evidence" }

type History struct {
	ID, AfterSaleID, ActorID                            uint64
	ActorType, Action                                   string
	FromStatus, ToStatus, ReasonCode, Remark, RequestID *string
	CreatedAt                                           time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (History) TableName() string { return "after_sale_history" }

type OrderRow struct {
	ID, CustomerID, MerchantID, ShopID            uint64
	Status, PayStatus                             string
	PaidAmount, DeliveryFeeAmount, RefundedAmount int64
	AfterSaleStatus                               string
	CompletedAt                                   *time.Time
	Version                                       int
	AddressSnapshot                               datatypes.JSON
}

type OrderItemRow struct {
	ID, OrderID, ShopProductID, ProductID uint64
	Quantity                              int
	TotalAmount                           int64
	ProductSnapshot                       datatypes.JSON
}

type PaymentRow struct {
	ID, OrderID                           uint64
	PaymentNo, Provider, Status, Currency string
	ProviderTradeNo                       *string
	Amount, RefundedAmount                int64
	Version                               int
}

type Refund struct {
	ID, AfterSaleID, OrderID, PaymentID  uint64
	RefundNo, Provider, Status, Currency string
	Reason                               string
	NotifyURL                            *string
	Amount, TotalAmount                  int64
	RequestedAt                          time.Time
	NextRetryAt                          *time.Time
	Version                              uint32
}

// TableName 返回当前数据模型对应的数据库表名。
func (Refund) TableName() string { return "refunds" }

type RefundItem struct {
	ID, RefundID, AfterSaleItemID uint64
	Amount                        int64
	Quantity                      int
}

// TableName 返回当前数据模型对应的数据库表名。
func (RefundItem) TableName() string { return "refund_items" }

type Replacement struct {
	ID, AfterSaleID, OriginalOrderID, ShopID uint64
	ReplacementNo, Status                    string
	AddressSnapshot, ItemsJSON               datatypes.JSON
	Version                                  uint32
}

// TableName 返回当前数据模型对应的数据库表名。
func (Replacement) TableName() string { return "replacement_fulfillments" }

type ReturnReceipt struct {
	ID, AfterSaleID, ShopID, ReceivedBy uint64
	ReceiptNo, Disposition              string
	SealedPackageIntact, GoodsIntact    bool
	Remark                              *string
	ReceivedAt                          time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (ReturnReceipt) TableName() string { return "return_receipts" }

type ProductStock struct {
	ID, ShopProductID, ShopID, ProductID uint64
	AvailableQty, ReservedQty, LockedQty int
	Version                              int
}

// TableName 返回当前数据模型对应的数据库表名。
func (ProductStock) TableName() string { return "product_stocks" }

type StockRecord struct {
	ID, ShopProductID, ShopID, ProductID  uint64
	ChangeType, SourceType                string
	QuantityDelta                         int
	BeforeAvailableQty, AfterAvailableQty int
	TotalQuantityDelta                    int
	BeforeTotalQty, AfterTotalQty         int
	SourceID                              *uint64
	IdempotencyKey                        *string
}

// TableName 返回当前数据模型对应的数据库表名。
func (StockRecord) TableName() string { return "stock_records" }

type Compensation struct {
	ID, AfterSaleID, CustomerID             uint64
	CompensationNo, Type, AssetType, Status string
	Amount                                  int64
	Reason                                  *string
}

// TableName 返回当前数据模型对应的数据库表名。
func (Compensation) TableName() string { return "compensation_ledger" }

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

type OutboxEvent struct {
	ID, AggregateID                           uint64
	EventID, EventType, AggregateType, Status string
	Payload                                   datatypes.JSON
	RequestID                                 *string
}

// TableName 返回当前数据模型对应的数据库表名。
func (OutboxEvent) TableName() string { return "outbox_events" }
