package gift

import "time"

type Gift struct {
	ID                 uint64
	GiftNo             string
	GiverCustomerID    uint64
	ReceiverCustomerID *uint64
	IssuerMerchantID   uint64
	ProductID          uint64
	RedeemCityCode     string
	Quantity           uint
	Message            *string
	Status             string
	ClaimDeadline      time.Time
	EarliestExpiresAt  time.Time
	Version            uint
	ClaimedAt          *time.Time
	CancelledAt        *time.Time
	ReturnedAt         *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (Gift) TableName() string { return "wine_ticket_gifts" }

type GiftAllocation struct {
	ID              uint64
	GiftID          uint64
	SourceLotID     uint64
	ReceiverLotID   *uint64
	Quantity        uint
	SourceExpiresAt time.Time
	Status          string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (GiftAllocation) TableName() string { return "wine_ticket_gift_allocations" }

type GiftClaimToken struct {
	ID                 uint64
	GiftID             uint64
	TokenDigest        string
	IssuedByCustomerID uint64
	ExpiresAt          time.Time
	ConsumedAt         *time.Time
	RevokedAt          *time.Time
	RequestID          *string
	CreatedAt          time.Time
}

func (GiftClaimToken) TableName() string { return "wine_ticket_gift_claim_tokens" }

type activeRenewalGuard struct {
	ID     uint64
	LotID  uint64
	Status string
}

func (activeRenewalGuard) TableName() string { return "wine_ticket_renewals" }
