package refund

import (
	"time"

	"gorm.io/datatypes"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	purchasedomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/purchase"
)

type WineTicketRefund struct {
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

func (WineTicketRefund) TableName() string { return "wine_ticket_refunds" }

type RefundAllocation struct {
	ID                 uint64
	WineTicketRefundID uint64
	LotID              uint64
	Quantity           uint
	SourceExpiresAt    time.Time
	Status             string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (RefundAllocation) TableName() string { return "wine_ticket_refund_allocations" }

const (
	PurchasePaymentBusiness = purchasedomain.PurchasePaymentBusiness
	PurchaseStatusIssued    = purchasedomain.PurchaseStatusIssued

	LotSourcePurchase = core.LotSourcePurchase

	LotStatusActive   = core.LotStatusActive
	LotStatusDepleted = core.LotStatusDepleted
	LotStatusRefunded = core.LotStatusRefunded

	TransactionTypePurchaseIssue = core.TransactionTypePurchaseIssue
)

const (
	WineTicketPurchaseRefundBusiness = "wine_ticket_purchase_refund"

	PurchaseStatusRefundHolding   = "refund_holding"
	PurchaseStatusRefundException = "refund_exception"
	PurchaseStatusRefunded        = "refunded"

	RefundKindUserUnused          = "user_unused"
	RefundKindIssueCompensation   = "issuance_compensation"
	RefundStatusHolding           = "holding"
	RefundStatusSubmitting        = "submitting"
	RefundStatusProcessing        = "processing"
	RefundStatusSubmissionUnknown = "submission_unknown"
	RefundStatusRetryPending      = "retry_pending"
	RefundStatusException         = "exception"
	RefundStatusSucceeded         = "succeeded"
	RefundStatusCancelled         = "cancelled"

	RefundAllocationHeld      = "held"
	RefundAllocationConsumed  = "consumed"
	RefundAllocationRestored  = "restored"
	RefundAllocationException = "exception"

	TransactionTypeRefundHold    = "refund_hold"
	TransactionTypeRefundRestore = "refund_restore"

	refundQuoteTTL = 5 * time.Minute
)

var wineTicketRefundActiveStatuses = []string{
	RefundStatusHolding,
	RefundStatusSubmitting,
	RefundStatusProcessing,
	RefundStatusSubmissionUnknown,
	RefundStatusRetryPending,
	RefundStatusException,
}

var renewalActiveStatuses = []string{
	"pending_payment",
	"payment_unknown",
	"applying",
	"compensating_refund",
	"refund_exception",
}

func ActiveStatuses() []string {
	return append([]string(nil), wineTicketRefundActiveStatuses...)
}

type refundQuoteClaims struct {
	SchemaVersion           uint8  `json:"v"`
	CustomerID              string `json:"customer_id"`
	PurchaseID              string `json:"purchase_id"`
	PurchaseNo              string `json:"purchase_no"`
	ExpectedPurchaseVersion uint   `json:"expected_purchase_version"`
	Amount                  int64  `json:"amount"`
	Currency                string `json:"currency"`
	Eligible                bool   `json:"eligible"`
	RefundWindowEndsAtMS    int64  `json:"refund_window_ends_at_ms"`
	QuoteExpiresAtMS        int64  `json:"quote_expires_at_ms"`
	LotDigest               string `json:"lot_digest"`
	PolicyDigest            string `json:"policy_digest"`
}

type refundEligibilityFacts struct {
	Purchase          purchasedomain.Purchase
	Payment           refundPayment
	OriginalLots      []core.Lot
	HeldByLot         map[uint64]uint
	Policy            core.RefundPolicy
	WindowEndsAt      time.Time
	RefundableAmount  int64
	HistoryCount      int64
	ActiveHoldCount   int64
	ActiveRefundCount int64
	IssueCount        int64
	IssueQuantity     int64
}

// refundPayment 是酒票领域所需的精确公共资金投影，
// 不包含零售订单或售后字段。
type refundPayment struct {
	ID              uint64
	PaymentNo       string
	BizType         *string
	BizID           *uint64
	OrderID         *uint64
	CustomerID      uint64
	Provider        string
	ProviderTradeNo *string
	ProviderStatus  *string
	Status          string
	Amount          int64
	RefundedAmount  int64
	Currency        string
	PaidAt          *time.Time
	Version         int
}

func (refundPayment) TableName() string { return "payments" }

type refundRecord struct {
	WineTicketRefund `gorm:"embedded"`

	PurchaseNo           string    `gorm:"column:purchase_no"`
	CommonStatus         string    `gorm:"column:common_status"`
	CommonProviderStatus *string   `gorm:"column:common_provider_status"`
	CommonFailureCode    *string   `gorm:"column:common_failure_code"`
	CommonUpdatedAt      time.Time `gorm:"column:common_updated_at"`
	HeldCount            uint      `gorm:"column:held_count"`
	ConsumedCount        uint      `gorm:"column:consumed_count"`
	RestoredCount        uint      `gorm:"column:restored_count"`
	ExceptionCount       uint      `gorm:"column:exception_count"`
}

type refundEligibilitySnapshot struct {
	SchemaVersion          uint8                       `json:"schema_version"`
	CustomerID             string                      `json:"customer_id"`
	PurchaseNo             string                      `json:"purchase_no"`
	PurchaseVersion        uint                        `json:"purchase_version"`
	PaymentID              string                      `json:"payment_id"`
	PaymentNo              string                      `json:"payment_no"`
	Amount                 int64                       `json:"amount"`
	Currency               string                      `json:"currency"`
	RefundWindowEndsAt     string                      `json:"refund_window_ends_at"`
	RequestedAt            string                      `json:"requested_at"`
	Policy                 core.RefundPolicy           `json:"policy"`
	EligibilityChecks      []RefundEligibilityCheckDTO `json:"eligibility_checks"`
	OriginalLotCount       int                         `json:"original_lot_count"`
	OriginalBottleQuantity uint                        `json:"original_bottle_quantity"`
	LotDigest              string                      `json:"lot_digest"`
	PolicyDigest           string                      `json:"policy_digest"`
}

type refundSettlementSnapshot struct {
	RefundKind          string
	Business            WineTicketRefund
	Purchase            purchasedomain.Purchase
	Lots                []core.Lot
	Allocations         []RefundAllocation
	AllCommonRefunds    []commonRefundRow
	CurrentCommonRefund commonRefundRow
	Payment             refundPayment
}

// commonRefundRow 映射共享退款表，同时避免酒票 API 层依赖零售订单。
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

type refundTransactionMetadata struct {
	RefundNo    string `json:"refund_no"`
	PurchaseNo  string `json:"purchase_no"`
	Source      string `json:"source"`
	RuleVersion uint8  `json:"rule_version"`
}

func refundJSON(value any) datatypes.JSON {
	// jsonData 是包内统一的安全序列化函数，不向调用方暴露错误。
	// 该局部名称使退款文件保持实现自包含。
	return jsonData(value)
}
