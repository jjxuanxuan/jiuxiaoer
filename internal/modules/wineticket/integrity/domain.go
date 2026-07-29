package integrity

import (
	"time"

	"gorm.io/datatypes"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	purchasedomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/purchase"
)

type Exception struct {
	ID                 uint64
	ExceptionNo        string
	ExceptionType      string
	BizType            string
	BizID              uint64
	BizNo              *string
	IssuerMerchantID   *uint64
	SourceType         string
	SourceID           *uint64
	CorrelationID      *string
	Severity           string
	Status             string
	ExpectedSnapshot   datatypes.JSON
	ActualSnapshot     datatypes.JSON
	ProposedAction     *string
	ProposedReason     *string
	ReviewTicketNo     *string
	ProposedBy         *uint64
	ProposedAt         *time.Time
	ReviewDecision     *string
	ReviewNote         *string
	ReviewedBy         *uint64
	ReviewedAt         *time.Time
	ResolutionResult   datatypes.JSON
	OccurrenceCount    uint
	FirstDetectedAt    time.Time
	LastDetectedAt     time.Time
	ResolvedAt         *time.Time
	Version            uint
	ActiveExceptionKey *string `gorm:"->"`
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (Exception) TableName() string { return "wine_ticket_exceptions" }

const (
	PurchasePaymentBusiness = purchasedomain.PurchasePaymentBusiness

	PurchaseStatusIssued          = purchasedomain.PurchaseStatusIssued
	PurchaseStatusRefundHolding   = "refund_holding"
	PurchaseStatusRefundException = "refund_exception"
	PurchaseStatusRefunded        = "refunded"

	LotSourcePurchase = core.LotSourcePurchase
	LotSourceGift     = core.LotSourceGift

	LotStatusActive   = core.LotStatusActive
	LotStatusDepleted = core.LotStatusDepleted
	LotStatusExpired  = core.LotStatusExpired
	LotStatusRefunded = core.LotStatusRefunded

	TransactionTypePurchaseIssue = core.TransactionTypePurchaseIssue

	RedemptionStatusScheduled        = "scheduled"
	RedemptionStatusAssigned         = "assigned"
	RedemptionStatusPickedUp         = "picked_up"
	RedemptionStatusDelivered        = "delivered"
	RedemptionStatusCancelled        = "cancelled"
	RedemptionStatusReturnInProgress = "return_in_progress"
	RedemptionStatusRestored         = "restored"
	RedemptionStatusException        = "exception"

	RedemptionAllocationStatusHeld     = "held"
	RedemptionAllocationStatusConsumed = "consumed"
	RedemptionAllocationStatusRestored = "restored"

	TransactionTypeRedemptionHold = "redemption_hold"

	redemptionOrderType      = "wine_ticket_redemption"
	redemptionSettlementMode = "wine_ticket"
	redemptionPayStatus      = "not_required"

	GiftStatusPending         = "pending"
	GiftStatusClaimed         = "claimed"
	GiftStatusCancelled       = "cancelled"
	GiftStatusExpiredReturned = "expired_returned"
	GiftStatusException       = "exception"

	GiftAllocationStatusHeld     = "held"
	GiftAllocationStatusClaimed  = "claimed"
	GiftAllocationStatusRestored = "restored"

	TransactionTypeGiftHold    = "gift_hold"
	TransactionTypeGiftClaim   = "gift_claim"
	TransactionTypeGiftRestore = "gift_restore"

	RenewalPaymentBusiness            = "wine_ticket_renewal"
	RenewalCompensationRefundBusiness = "wine_ticket_renewal_compensation"

	RenewalStatusPendingPayment     = "pending_payment"
	RenewalStatusPaymentUnknown     = "payment_unknown"
	RenewalStatusApplying           = "applying"
	RenewalStatusCompleted          = "completed"
	RenewalStatusClosed             = "closed"
	RenewalStatusCompensatingRefund = "compensating_refund"
	RenewalStatusRefundException    = "refund_exception"
	RenewalStatusRefunded           = "refunded"

	WineTicketPurchaseRefundBusiness = "wine_ticket_purchase_refund"

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
	RefundAllocationHeld          = "held"
	RefundAllocationConsumed      = "consumed"
	RefundAllocationRestored      = "restored"
	TransactionTypeRefundHold     = "refund_hold"

	ExceptionStatusInvestigating        = "investigating"
	ExceptionStatusAwaitingExternalFact = "awaiting_external_fact"
	ExceptionStatusPendingReview        = "pending_review"
)

var wineTicketRefundActiveStatuses = []string{
	RefundStatusHolding,
	RefundStatusSubmitting,
	RefundStatusProcessing,
	RefundStatusSubmissionUnknown,
	RefundStatusRetryPending,
	RefundStatusException,
}

var shanghaiLocation = core.ShanghaiLocation

func idString(value uint64) string { return core.IDString(value) }

func nowShanghai(now func() time.Time) time.Time { return core.NowShanghai(now) }
