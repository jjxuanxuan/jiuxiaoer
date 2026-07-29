package refund

import (
	"gorm.io/datatypes"
	"time"
)

type Row struct {
	ID                                                                     uint64
	AfterSaleID, OrderID                                                   *uint64
	PaymentID                                                              uint64
	ReplacesRefundID                                                       *uint64
	RefundNo, Provider, Status, Currency, Reason                           string
	BizType                                                                *string
	BizID                                                                  *uint64
	NotifyURL                                                              *string
	ProviderRefundID, ProviderStatus, FailureCode, FailureDetail, LockedBy *string
	Amount, TotalAmount                                                    int64
	Attempts                                                               uint32
	NextRetryAt, LockedUntil                                               *time.Time
	RequestedAt                                                            time.Time
	ProviderAcceptedAt                                                     *time.Time
	SucceededAt, FailedAt                                                  *time.Time
	Version                                                                uint32
	CreatedAt, UpdatedAt                                                   time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Row) TableName() string { return "refunds" }

type RefundItem struct {
	ID, RefundID, AfterSaleItemID uint64
	Amount                        int64
	Quantity                      int
}

// TableName 返回当前数据模型对应的数据库表名。
func (RefundItem) TableName() string { return "refund_items" }

type Callback struct {
	ID                                                    uint64
	Provider, ProviderEventID, PayloadHash, ProcessStatus string
	RefundID                                              *uint64
	SignatureValid                                        bool
	ErrorCode                                             *string
	ReceivedAt                                            time.Time
	ProcessedAt                                           *time.Time
	RequestID                                             *string
}

// TableName 返回当前数据模型对应的数据库表名。
func (Callback) TableName() string { return "refund_callbacks" }

type Payment struct {
	ID                                    uint64
	PaymentNo, Provider, Status, Currency string
	ProviderTradeNo                       *string
	Amount, RefundedAmount                int64
	Version                               int
}

// TableName 返回当前数据模型对应的数据库表名。
func (Payment) TableName() string { return "payments" }

type Order struct {
	ID                         uint64
	Status                     string
	PaidAmount, RefundedAmount int64
	AfterSaleStatus            string
	Version                    int
}

// TableName 返回当前数据模型对应的数据库表名。
func (Order) TableName() string { return "orders" }

type AfterSale struct {
	ID                             uint64
	Status                         string
	ApprovedAmount, RefundedAmount int64
	Version                        uint32
}

// TableName 返回当前数据模型对应的数据库表名。
func (AfterSale) TableName() string { return "after_sales" }

type AfterSaleItem struct {
	ID                             uint64
	ApprovedAmount, RefundedAmount int64
}

// TableName 返回当前数据模型对应的数据库表名。
func (AfterSaleItem) TableName() string { return "after_sale_items" }

type Outbox struct {
	ID, AggregateID                           uint64
	EventID, EventType, AggregateType, Status string
	Payload                                   datatypes.JSON
	RequestID                                 *string
}

// TableName 返回当前数据模型对应的数据库表名。
func (Outbox) TableName() string { return "outbox_events" }

type Audit struct {
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
func (Audit) TableName() string { return "audit_logs" }
