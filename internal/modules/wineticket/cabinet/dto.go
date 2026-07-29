package cabinet

import "jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"

type CabinetDTO struct {
	Items         []CabinetItemDTO `json:"items"`
	NextPageToken string           `json:"next_page_token"`
	ServerTime    string           `json:"server_time"`
}

type CabinetItemDTO struct {
	IssuerMerchantID          string                 `json:"issuer_merchant_id"`
	RedeemCityCode            string                 `json:"redeem_city_code"`
	GiftSourceLotNo           string                 `json:"gift_source_lot_no"`
	Product                   core.ProductSummaryDTO `json:"product"`
	AvailableQuantity         uint                   `json:"available_quantity"`
	HeldQuantity              uint                   `json:"held_quantity"`
	ExtractedQuantity         uint                   `json:"extracted_quantity"`
	LotCount                  uint                   `json:"lot_count"`
	IssuerMerchantDisplayName string                 `json:"issuer_merchant_display_name"`
	NearestExpiresAt          string                 `json:"nearest_expires_at"`
	ExpiringSoon              bool                   `json:"expiring_soon"`
	Actions                   []string               `json:"actions"`

	cursorID uint64
}

type LotDTO struct {
	LotNo                     string                     `json:"lot_no"`
	Product                   core.ProductSummaryDTO     `json:"product"`
	PurchaseNo                *string                    `json:"purchase_no,omitempty"`
	SourceType                string                     `json:"source_type"`
	TotalQuantity             uint                       `json:"total_quantity"`
	AvailableQuantity         uint                       `json:"available_quantity"`
	HeldQuantity              uint                       `json:"held_quantity"`
	ExtractedQuantity         uint                       `json:"extracted_quantity"`
	RedeemCityCode            string                     `json:"redeem_city_code"`
	IssuerMerchantID          string                     `json:"issuer_merchant_id"`
	IssuerMerchantDisplayName string                     `json:"issuer_merchant_display_name"`
	OriginalExpiresAt         string                     `json:"original_expires_at"`
	ExpiresAt                 string                     `json:"expires_at"`
	RenewalCount              uint                       `json:"renewal_count"`
	EverUsed                  bool                       `json:"ever_used"`
	Status                    string                     `json:"status"`
	Actions                   []string                   `json:"actions"`
	ActiveHolds               []string                   `json:"active_holds"`
	RenewalEligible           bool                       `json:"renewal_eligible"`
	RenewalIneligibleReason   *string                    `json:"renewal_ineligible_reason"`
	LatestTransactions        []WineTicketTransactionDTO `json:"latest_transactions"`
	Version                   uint                       `json:"version"`
}

type WineTicketTransactionDTO struct {
	TransactionNo          string                 `json:"transaction_no"`
	LotNo                  string                 `json:"lot_no"`
	Product                core.ProductSummaryDTO `json:"product"`
	TransactionType        string                 `json:"transaction_type"`
	QuantityDelta          int                    `json:"quantity_delta"`
	AfterAvailableQuantity uint                   `json:"after_available_quantity"`
	BizType                string                 `json:"biz_type,omitempty"`
	BizNo                  string                 `json:"biz_no,omitempty"`
	BizStatus              *string                `json:"biz_status"`
	CreatedAt              string                 `json:"created_at"`
}
