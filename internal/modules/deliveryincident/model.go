package deliveryincident

import (
	"time"

	"gorm.io/datatypes"
)

const (
	TypeOutOfStock          = "out_of_stock"
	TypeAlcoholDamaged      = "alcohol_damaged"
	TypeCustomerRefused     = "customer_refused"
	TypeCustomerUnreachable = "customer_unreachable"

	StagePickup   = "pickup"
	StageDelivery = "delivery"

	StatusEvidenceRequired = "evidence_required"
	StatusOpen             = "open"
	StatusAcknowledged     = "acknowledged"
	StatusResolved         = "resolved"
	StatusRejected         = "rejected"
)

var activeStatuses = []string{StatusEvidenceRequired, StatusOpen, StatusAcknowledged}

type Incident struct {
	ID                        uint64
	IncidentNo                string
	DeliveryOrderID           uint64
	OrderID                   uint64
	ShopID                    uint64
	RiderID                   uint64
	Type                      string
	Stage                     string
	Status                    string
	Priority                  string
	ReasonCode                *string
	Description               string
	DeliveryStatusSnapshot    string
	AssignmentVersionSnapshot uint
	ContactAttemptCount       uint
	FirstContactAt            *time.Time
	LastContactAt             *time.Time
	DistanceToDestinationM    *uint
	LocationAccuracyM         *float64
	LocationCapturedAt        *time.Time
	AcknowledgedBy            *uint64
	AcknowledgedAt            *time.Time
	ResolvedBy                *uint64
	ResolvedAt                *time.Time
	ResolutionCode            *string
	ResolutionNote            *string
	RejectedBy                *uint64
	RejectedAt                *time.Time
	RejectionCode             *string
	RejectionReason           *string
	ReportedAt                time.Time
	Version                   uint
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
}

func (Incident) TableName() string { return "delivery_incidents" }

type Item struct {
	ID            uint64
	IncidentID    uint64
	OrderItemID   uint64
	ShopProductID *uint64
	ProductID     *uint64
	Quantity      uint
	ItemSnapshot  datatypes.JSON
	CreatedAt     time.Time
}

func (Item) TableName() string { return "delivery_incident_items" }

type Evidence struct {
	ID         uint64
	IncidentID uint64
	TokenID    string
	ObjectKey  string
	MimeType   string
	SizeBytes  uint64
	SHA256     string
	ScanStatus string
	CreatedAt  time.Time
}

func (Evidence) TableName() string { return "delivery_incident_evidence" }

type History struct {
	ID         uint64
	IncidentID uint64
	ActorType  string
	ActorID    *uint64
	Action     string
	FromStatus *string
	ToStatus   string
	ReasonCode *string
	Remark     *string
	RequestID  string
	CreatedAt  time.Time
}

func (History) TableName() string { return "delivery_incident_history" }

type DeliveryOrder struct {
	ID                uint64
	OrderID           uint64
	ShopID            uint64
	RiderID           *uint64
	Status            string
	AssignmentVersion uint
	RecipientSnapshot datatypes.JSON
	AcceptedAt        *time.Time
	PickedUpAt        *time.Time
	CompletedAt       *time.Time
	CancelledAt       *time.Time
}

func (DeliveryOrder) TableName() string { return "delivery_orders" }

type OrderItemRow struct {
	ID              uint64
	OrderID         uint64
	ShopProductID   uint64
	ProductID       uint64
	ProductSnapshot datatypes.JSON
	Quantity        int
}

func (OrderItemRow) TableName() string { return "order_items" }

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
	Incident Incident
	Items    []Item
	Evidence []Evidence
	History  []History
}

type ListFilters struct {
	Type         string
	Status       string
	Stage        string
	ShopID       *uint64
	RiderID      *uint64
	IncidentNo   string
	OrderNo      string
	ReportedFrom *time.Time
	ReportedTo   *time.Time
}
