package purchase

import (
	"time"

	"gorm.io/datatypes"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/catalog"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
)

const (
	PurchasePaymentBusiness = "wine_ticket_purchase"

	PurchaseStatusPendingPayment      = "pending_payment"
	PurchaseStatusPaymentUnknown      = "payment_unknown"
	PurchaseStatusSettlementException = "settlement_exception"
	PurchaseStatusIssued              = "issued"
	PurchaseStatusClosed              = "closed"
	PurchaseStatusRefundHolding       = "refund_holding"
	PurchaseStatusRefundException     = "refund_exception"
	PurchaseStatusRefunded            = "refunded"

	LotSourcePurchase = core.LotSourcePurchase
	LotStatusActive   = core.LotStatusActive

	TransactionTypePurchaseIssue = core.TransactionTypePurchaseIssue

	PackageStatusPublished = catalog.PackageStatusPublished

	PackageTypeStockpile = catalog.PackageTypeStockpile
	PackageTypeCorporate = catalog.PackageTypeCorporate
	PackageTypeGift      = catalog.PackageTypeGift

	WineTicketPurchaseRefundBusiness = "wine_ticket_purchase_refund"
	RefundKindIssueCompensation      = "issuance_compensation"
	RefundStatusHolding              = "holding"
)

var wineTicketRefundActiveStatuses = []string{
	"holding",
	"submitting",
	"processing",
	"submission_unknown",
	"retry_pending",
	"exception",
}

type PurchaseQuota struct {
	ID               uint64
	CustomerID       uint64
	PackageCode      string
	ReservedQuantity uint
	ConsumedQuantity uint
	Version          uint
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func (PurchaseQuota) TableName() string { return "wine_ticket_purchase_quotas" }

type Purchase struct {
	ID                       uint64
	PurchaseNo               string
	CustomerID               uint64
	PackageID                uint64
	PackageVersion           uint
	PaymentID                uint64
	IssuerMerchantID         uint64
	SettlementShopID         uint64
	SettlementShopProductID  uint64
	ProductID                uint64
	RedeemCityCode           string
	PackageQuantity          uint
	BottleQuantityPerPackage uint
	TotalBottleQuantity      uint
	UnitPriceAmount          int64
	PayableAmount            int64
	PaidAmount               int64
	Currency                 string
	PackageSnapshot          datatypes.JSON
	RefundPolicySnapshot     datatypes.JSON
	RenewalPolicySnapshot    datatypes.JSON
	Status                   string
	Version                  uint
	PaidAt                   *time.Time
	IssuedAt                 *time.Time
	RefundedAt               *time.Time
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

func (Purchase) TableName() string { return "wine_ticket_purchases" }

// issuanceCompensationRefund 是购买子域在已支付购买无法安全发放时，
// 创建退款记录所需的最小写入投影。完整退款生命周期仍归退款子域负责。
type issuanceCompensationRefund struct {
	ID                  uint64
	WineTicketRefundNo  string
	PurchaseID          uint64
	CustomerID          uint64
	CurrentRefundID     uint64
	RefundKind          string
	Amount              int64
	Currency            string
	ReasonCode          string
	ReasonText          *string
	EligibilitySnapshot datatypes.JSON
	Status              string
	Version             uint
	RequestedAt         time.Time
	SucceededAt         *time.Time
	ClosedAt            *time.Time
	ActivePurchaseID    *uint64 `gorm:"->"`
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

func (issuanceCompensationRefund) TableName() string { return "wine_ticket_refunds" }

// commonRefundRow 映射自动发放补偿分支使用的共享退款表，
// 仅包含创建退款指令所需的持久化字段。
type commonRefundRow struct {
	ID                 uint64
	AfterSaleID        *uint64
	OrderID            *uint64
	PaymentID          uint64
	ReplacesRefundID   *uint64
	RefundNo           string
	BizType            *string
	BizID              *uint64
	Provider           string
	ProviderRefundID   *string
	Status             string
	Currency           string
	Reason             string
	NotifyURL          *string
	ProviderStatus     *string
	FailureCode        *string
	FailureDetail      *string
	Amount             int64
	TotalAmount        int64
	Attempts           uint32
	NextRetryAt        *time.Time
	LockedUntil        *time.Time
	LockedBy           *string
	RequestedAt        time.Time
	ProviderAcceptedAt *time.Time
	SucceededAt        *time.Time
	FailedAt           *time.Time
	Version            uint32
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (commonRefundRow) TableName() string { return "refunds" }

func refundJSON(value any) datatypes.JSON { return core.JSONData(value) }
