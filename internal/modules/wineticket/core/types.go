package core

const (
	LotSourcePurchase = "purchase"
	LotSourceGift     = "gift"

	LotStatusActive   = "active"
	LotStatusDepleted = "depleted"
	LotStatusExpired  = "expired"
	LotStatusRefunded = "refunded"

	TransactionTypePurchaseIssue           = "purchase_issue"
	TransactionTypeRedemptionHold          = "redemption_hold"
	TransactionTypeRedemptionRestore       = "redemption_restore"
	TransactionTypeRedemptionReturnRestore = "redemption_return_restore"
	TransactionTypeRedemptionReturnExpire  = "redemption_return_expire"
	TransactionTypeGiftHold                = "gift_hold"
	TransactionTypeGiftClaim               = "gift_claim"
	TransactionTypeGiftRestore             = "gift_restore"
	TransactionTypeRefundHold              = "refund_hold"
	TransactionTypeRefundRestore           = "refund_restore"
	TransactionTypeExpiry                  = "expiry"

	MaxWineTicketAmount = int64(100_000_000)
	MaxPurchaseQuantity = uint(10_000)
)
