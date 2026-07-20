package reconciliation

import (
	"io"
	"time"

	"gorm.io/datatypes"
)

const (
	BillTypeTradeAll     = "trade_all"
	BillTypeFundflowBase = "fundflow_basic"

	DiscrepancyMissingLocal          = "missing_local"
	DiscrepancyMissingWeChat         = "missing_wechat"
	DiscrepancyAmountMismatch        = "amount_mismatch"
	DiscrepancyStatusMismatch        = "status_mismatch"
	DiscrepancyTransactionIDMismatch = "transaction_id_mismatch"
	DiscrepancyRefundMismatch        = "refund_mismatch"
)

// BillFile is a streamed WeChat bill plus the digest returned by the apply API.
// The caller must close Body. WeChat currently fixes HashType to SHA1.
type BillFile struct {
	Body              io.ReadCloser
	HashType          string
	ExpectedHash      string
	ProviderRequestID string
	DownloadRequestID string
}

type Run struct {
	ID                                   uint64
	BillDate                             time.Time `gorm:"type:date"`
	BillType, Status                     string
	StartedAt, CompletedAt               *time.Time
	HashType, ExpectedHash, ComputedHash *string
	ProviderRequestID, DownloadRequestID *string
	RowCount, DiscrepancyCount           uint64
	StatsJSON                            datatypes.JSON
	ErrorCode, ErrorDetail               *string
	Version                              uint32
	CreatedAt, UpdatedAt                 time.Time
}

func (Run) TableName() string { return "wechat_bill_reconciliation_runs" }

type Observation struct {
	ID                                            uint64
	RunID, LineNo                                 uint64
	EntryKind                                     string
	BusinessNo, ProviderTradeNo, ProviderRefundNo *string
	ProviderStatus, Currency                      *string
	Amount                                        *int64
	OccurredAt                                    *time.Time
	RawHash                                       string
	CreatedAt                                     time.Time
}

func (Observation) TableName() string { return "wechat_bill_observations" }

type Discrepancy struct {
	ID                                            uint64
	RunID                                         uint64
	BillDate                                      time.Time `gorm:"type:date"`
	BillType, DiscrepancyType                     string
	BusinessNo, ProviderTradeNo, ProviderRefundNo *string
	LocalValue                                    datatypes.JSON
	WeChatValue                                   datatypes.JSON `gorm:"column:wechat_value"`
	Status                                        string
	HandlingNote                                  *string
	HandledBy                                     *uint64
	HandledAt                                     *time.Time
	DedupeKey                                     string
	CreatedAt, UpdatedAt                          time.Time
}

func (Discrepancy) TableName() string { return "wechat_bill_discrepancies" }

type parsedEntry struct {
	LineNo                            uint64
	Kind, BusinessNo                  string
	ProviderTradeNo, ProviderRefundNo string
	ProviderStatus, Currency          string
	Amount                            *int64
	OccurredAt                        *time.Time
	RawHash                           string
}

type localPayment struct {
	ID                              uint64
	Amount                          int64
	PaymentNo, Status, Currency     string
	ProviderTradeNo, ProviderStatus *string
	PaidAt                          *time.Time
}

type localRefund struct {
	ID, PaymentID                    uint64
	RefundNo, Status, Currency       string
	ProviderRefundID, ProviderStatus *string
	Amount                           int64
	RequestedAt                      time.Time
	ProviderAcceptedAt               *time.Time
}
