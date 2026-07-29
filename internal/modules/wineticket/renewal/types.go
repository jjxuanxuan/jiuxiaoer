package renewal

import (
	"time"

	"gorm.io/datatypes"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
)

const (
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
)

var renewalActiveStatuses = []string{
	RenewalStatusPendingPayment,
	RenewalStatusPaymentUnknown,
	RenewalStatusApplying,
	RenewalStatusCompensatingRefund,
	RenewalStatusRefundException,
}

type RenewalCreateRequest struct {
	ExpectedLotVersion uint   `json:"expected_lot_version"`
	QuoteToken         string `json:"quote_token"`
}

type RenewalQuoteDTO struct {
	Eligible        bool   `json:"eligible"`
	ReasonCode      string `json:"reason_code"`
	ExtensionDays   uint   `json:"extension_days"`
	FeeAmount       int64  `json:"fee_amount"`
	OldExpiresAt    string `json:"old_expires_at"`
	NewExpiresAt    string `json:"new_expires_at"`
	ExpectedVersion uint   `json:"expected_lot_version"`
	QuoteExpiresAt  string `json:"quote_expires_at"`
	QuoteToken      string `json:"quote_token"`
	RenewalCount    uint   `json:"renewal_count"`
	MaxRenewalCount uint   `json:"max_renewal_count"`
	PolicySummary   string `json:"policy_summary,omitempty"`
}

type RenewalDTO struct {
	RenewalNo                       string         `json:"renewal_no"`
	LotNo                           string         `json:"lot_no"`
	ExtensionDays                   uint           `json:"extension_days"`
	FeeAmount                       int64          `json:"fee_amount"`
	OldExpiresAt                    string         `json:"old_expires_at"`
	NewExpiresAt                    string         `json:"new_expires_at"`
	Status                          string         `json:"status"`
	PaymentParameters               map[string]any `json:"payment_parameters"`
	CompensatingRefundDisplayStatus *string        `json:"compensating_refund_display_status"`
	CompensatingRefundSafeMessage   *string        `json:"compensating_refund_safe_message"`
	Version                         uint           `json:"version"`
	UpdatedAt                       string         `json:"updated_at"`
}

type renewalQuoteClaims struct {
	SchemaVersion      uint8  `json:"v"`
	CustomerID         string `json:"customer_id"`
	LotID              string `json:"lot_id"`
	LotNo              string `json:"lot_no"`
	ExpectedLotVersion uint   `json:"expected_lot_version"`
	ExtensionDays      uint   `json:"extension_days"`
	FeeAmount          int64  `json:"fee_amount"`
	OldExpiresAtMS     int64  `json:"old_expires_at_ms"`
	NewExpiresAtMS     int64  `json:"new_expires_at_ms"`
	QuoteExpiresAtMS   int64  `json:"quote_expires_at_ms"`
	PolicyDigest       string `json:"policy_digest"`
}

type renewalLotPolicy struct {
	core.Lot
	PolicySnapshot datatypes.JSON
	Policy         core.RenewalPolicy
}

type renewalRecord struct {
	Renewal `gorm:"embedded"`

	LotNo                string         `gorm:"column:lot_no"`
	PaymentStatus        *string        `gorm:"column:payment_status"`
	PaymentClientPayload datatypes.JSON `gorm:"column:payment_client_payload"`
	RefundStatus         *string        `gorm:"column:refund_status"`
	RefundProviderStatus *string        `gorm:"column:refund_provider_status"`
	RefundFailureCode    *string        `gorm:"column:refund_failure_code"`
}

type renewalCompensationSnapshot struct {
	SchemaVersion      uint8  `json:"schema_version"`
	ReasonCode         string `json:"reason_code"`
	ProviderPaidAt     string `json:"provider_paid_at,omitempty"`
	SettlementAt       string `json:"settlement_at"`
	OldExpiresAt       string `json:"old_expires_at"`
	NewExpiresAt       string `json:"new_expires_at"`
	ExpectedLotVersion uint   `json:"expected_lot_version"`
}

func renewalTimeFromMillis(value int64) time.Time {
	return time.UnixMilli(value).In(shanghaiLocation).Truncate(time.Millisecond)
}

func validRenewalStatus(value string) bool {
	switch value {
	case RenewalStatusPendingPayment, RenewalStatusPaymentUnknown,
		RenewalStatusApplying, RenewalStatusCompleted, RenewalStatusClosed,
		RenewalStatusCompensatingRefund, RenewalStatusRefundException,
		RenewalStatusRefunded:
		return true
	default:
		return false
	}
}
