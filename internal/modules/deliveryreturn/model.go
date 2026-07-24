package deliveryreturn

import (
	"time"

	"gorm.io/datatypes"
)

const (
	StatusRequested = "requested"
	StatusReturning = "returning"
	StatusArrived   = "arrived"
	StatusReceived  = "received"
	StatusClosed    = "closed"
	StatusCancelled = "cancelled"
	StatusDisputed  = "disputed"
	StatusException = "exception"

	ReasonCustomerUnreachable = "customer_unreachable"
	ReasonCustomerRefused     = "customer_refused"
	ReasonAddressWrong        = "address_wrong"
	ReasonDamagedInTransit    = "damaged_in_transit"
	ReasonOther               = "other"
)

var activeStatuses = []string{
	StatusRequested, StatusReturning, StatusArrived, StatusReceived,
	StatusDisputed, StatusException,
}

// Return 是逆向物流事实。资金状态仍保存在售后和退款中；
// 库存处置仍保存在收货明细和库存记录中。
type Return struct {
	ID                    uint64
	ReturnNo              string
	DeliveryOrderID       uint64
	ActiveDeliveryOrderID *uint64
	OrderID               uint64
	ShopID                uint64
	RiderID               uint64
	IncidentID            *uint64
	AfterSaleID           *uint64
	ReasonCode            string
	Status                string
	InitiatorType         string
	InitiatorID           uint64
	RequestNote           *string
	ApprovedBy            *uint64
	ApprovedAt            *time.Time
	HandoffCodeHash       *string
	HandoffCodeExpiresAt  *time.Time
	HandoffFailedAttempts uint
	ReceiptDeadlineAt     *time.Time
	RequestedAt           time.Time
	ArrivedAt             *time.Time
	ReceivedAt            *time.Time
	ClosedAt              *time.Time
	CancelledAt           *time.Time
	Version               uint64
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

func (Return) TableName() string { return "delivery_returns" }

type History struct {
	ID               uint64
	DeliveryReturnID uint64
	FromStatus       *string
	ToStatus         *string
	Action           string
	ActorType        string
	ActorID          *uint64
	RequestID        *string
	IdempotencyKey   *string
	MetadataJSON     datatypes.JSON
	CreatedAt        time.Time
}

func (History) TableName() string { return "delivery_return_history" }

type ReceiptItem struct {
	ID               uint64
	ReturnReceiptID  uint64
	AfterSaleItemID  uint64
	OrderItemID      uint64
	ShopProductID    uint64
	ProductID        uint64
	ExpectedQuantity int
	ReceivedQuantity int
	Disposition      string
	PolicyCode       string
	PolicyVersion    string
	AvailableBefore  *int
	AvailableAfter   *int
	Note             *string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func (ReceiptItem) TableName() string { return "return_receipt_items" }

type DeliveryOrder struct {
	ID                uint64
	OrderID           uint64
	ShopID            uint64
	RiderID           *uint64
	Status            string
	AssignmentVersion uint
	PickedUpAt        *time.Time
	CompletedAt       *time.Time
	CancelledAt       *time.Time
}

func (DeliveryOrder) TableName() string { return "delivery_orders" }

type IncidentRef struct {
	ID              uint64
	DeliveryOrderID uint64
	OrderID         uint64
	ShopID          uint64
	RiderID         uint64
	Status          string
	Type            string
	Version         uint
}

func (IncidentRef) TableName() string { return "delivery_incidents" }

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

func (OutboxEvent) TableName() string { return "outbox_events" }

type Aggregate struct {
	Return       Return
	History      []History
	Items        []AggregateItem
	RefundStatus string
}

type AggregateItem struct {
	AfterSaleItemID  uint64
	OrderItemID      uint64
	ShopProductID    uint64
	ProductID        uint64
	ExpectedQuantity int
	ReceivedQuantity *int
	Disposition      *string
	PolicyCode       string
	PolicyVersion    string
	AvailableBefore  *int
	AvailableAfter   *int
	Note             *string
}

type ReturnReceipt struct {
	ID                  uint64
	ReceiptNo           string
	AfterSaleID         uint64
	ShopID              uint64
	Disposition         string
	SealedPackageIntact bool
	GoodsIntact         bool
	Remark              *string
	ReceivedBy          uint64
	ReceivedAt          time.Time
}

func (ReturnReceipt) TableName() string { return "return_receipts" }

type AfterSale struct {
	ID             uint64
	OrderID        uint64
	ShopID         uint64
	SourceType     string
	SourceID       *uint64
	Status         string
	ApprovedAmount int64
	RefundedAmount int64
	Version        uint32
}

func (AfterSale) TableName() string { return "after_sales" }

type AfterSaleItem struct {
	ID                uint64
	AfterSaleID       uint64
	OrderID           uint64
	OrderItemID       uint64
	ShopProductID     uint64
	ProductID         uint64
	ApprovedQuantity  int
	ApprovedAmount    int64
	RefundedAmount    int64
	ReturnDisposition string
}

func (AfterSaleItem) TableName() string { return "after_sale_items" }

type OrderItem struct {
	ID              uint64
	ProductSnapshot datatypes.JSON
}

func (OrderItem) TableName() string { return "order_items" }

type OrderRef struct {
	ID      uint64
	Version int
}

func (OrderRef) TableName() string { return "orders" }

type ProductStock struct {
	ID            uint64
	ShopProductID uint64
	ShopID        uint64
	ProductID     uint64
	AvailableQty  int
	ReservedQty   int
	LockedQty     int
	Version       int
}

func (ProductStock) TableName() string { return "product_stocks" }

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
	SourceID           *uint64
	IdempotencyKey     *string
	BusinessActionKey  *string
}

func (StockRecord) TableName() string { return "stock_records" }

func validReason(value string) bool {
	switch value {
	case ReasonCustomerUnreachable, ReasonCustomerRefused, ReasonAddressWrong, ReasonDamagedInTransit, ReasonOther:
		return true
	default:
		return false
	}
}

func isActiveStatus(value string) bool {
	for _, status := range activeStatuses {
		if value == status {
			return true
		}
	}
	return false
}

func isTerminalStatus(value string) bool {
	return value == StatusClosed || value == StatusCancelled
}

// validTransition 刻意不提供兜底状态变更。
// 新状态必须同时在此处和数据库约束中添加。
func validTransition(from, to string) bool {
	switch from {
	case StatusRequested:
		return to == StatusReturning || to == StatusCancelled || to == StatusDisputed
	case StatusReturning:
		return to == StatusArrived || to == StatusDisputed || to == StatusException
	case StatusArrived:
		return to == StatusReceived || to == StatusDisputed || to == StatusException
	case StatusReceived:
		return to == StatusClosed || to == StatusException
	case StatusDisputed, StatusException:
		return to == StatusReturning || to == StatusArrived || to == StatusReceived || to == StatusCancelled
	default:
		return false
	}
}
