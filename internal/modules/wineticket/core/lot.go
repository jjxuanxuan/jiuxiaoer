package core

import "time"

type Lot struct {
	ID                uint64
	LotNo             string
	OwnerCustomerID   uint64
	PurchaseID        uint64
	SourceType        string
	SourceLotID       *uint64
	SourceGiftID      *uint64
	IssuerMerchantID  uint64
	ProductID         uint64
	RedeemCityCode    string
	TotalQuantity     uint
	AvailableQuantity uint
	OriginalExpiresAt time.Time
	ExpiresAt         time.Time
	ExpiryChangedAt   time.Time
	RenewalCount      uint
	EverUsed          bool
	Status            string
	Version           uint
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func (Lot) TableName() string { return "wine_ticket_lots" }
