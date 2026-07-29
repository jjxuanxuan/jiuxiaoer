package renewal

import (
	"time"

	"gorm.io/datatypes"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
)

type Renewal struct {
	ID                   uint64
	RenewalNo            string
	LotID                uint64
	CustomerID           uint64
	PaymentID            *uint64
	CompensatingRefundID *uint64
	OldExpiresAt         time.Time
	NewExpiresAt         time.Time
	ExtensionDays        uint
	FeeAmount            int64
	Currency             string
	PolicySnapshot       datatypes.JSON
	ExpectedLotVersion   uint
	Status               string
	Version              uint
	CompletedAt          *time.Time
	ClosedAt             *time.Time
	RefundedAt           *time.Time
	ActiveLotID          *uint64 `gorm:"->"`
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

func (Renewal) TableName() string { return "wine_ticket_renewals" }

const (
	LotStatusActive   = core.LotStatusActive
	LotStatusExpired  = core.LotStatusExpired
	LotStatusRefunded = core.LotStatusRefunded

	maxWineTicketAmount      = core.MaxWineTicketAmount
	transactionTypeLotExpiry = core.TransactionTypeLotExpiry

	LotSourcePurchase = core.LotSourcePurchase
)

var shanghaiLocation = core.ShanghaiLocation
