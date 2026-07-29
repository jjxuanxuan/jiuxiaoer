package core

import (
	"time"

	"gorm.io/datatypes"
)

type Transaction struct {
	ID                      uint64
	TransactionNo           string
	LotID                   uint64
	OwnerCustomerID         uint64
	TransactionType         string
	QuantityDelta           int
	BeforeAvailableQuantity uint
	AfterAvailableQuantity  uint
	BizType                 string
	BizID                   uint64
	ActionKey               string
	MetadataJSON            datatypes.JSON
	RequestID               *string
	CreatedAt               time.Time
}

func (Transaction) TableName() string { return "wine_ticket_transactions" }
