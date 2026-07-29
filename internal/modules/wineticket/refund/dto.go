package refund

type RefundCreateRequest struct {
	ReasonCode              string `json:"reason_code"`
	ReasonText              string `json:"reason_text,omitempty"`
	ExpectedPurchaseVersion uint   `json:"expected_purchase_version"`
	QuoteToken              string `json:"quote_token"`
}

type RefundEligibilityCheckDTO struct {
	Code        string `json:"code"`
	Passed      bool   `json:"passed"`
	SafeMessage string `json:"safe_message"`
}

type RefundIneligibleReasonDTO struct {
	Code        string `json:"code"`
	SafeMessage string `json:"safe_message"`
}

type RefundLotSummaryDTO struct {
	LotNo             string `json:"lot_no"`
	TotalQuantity     uint   `json:"total_quantity"`
	AvailableQuantity uint   `json:"available_quantity"`
	HeldQuantity      uint   `json:"held_quantity"`
	ExpiresAt         string `json:"expires_at"`
}

type RefundQuoteDTO struct {
	Eligible                bool                        `json:"eligible"`
	EligibilityChecks       []RefundEligibilityCheckDTO `json:"eligibility_checks"`
	IneligibleReasons       []RefundIneligibleReasonDTO `json:"ineligible_reasons"`
	AllowedReasonCodes      []string                    `json:"allowed_reason_codes"`
	RefundableAmount        int64                       `json:"refundable_amount"`
	Currency                string                      `json:"currency"`
	ExpectedPurchaseVersion uint                        `json:"expected_purchase_version"`
	RefundWindowEndsAt      string                      `json:"refund_window_ends_at"`
	QuoteExpiresAt          string                      `json:"quote_expires_at"`
	QuoteToken              string                      `json:"quote_token"`
	LotSummaries            []RefundLotSummaryDTO       `json:"lot_summaries"`
	PolicySummary           string                      `json:"policy_summary"`
	RefundRouteSummary      string                      `json:"refund_route_summary"`
	EstimatedArrivalSummary string                      `json:"estimated_arrival_summary"`
}

type RefundDTO struct {
	RefundNo          string  `json:"refund_no"`
	PurchaseNo        string  `json:"purchase_no"`
	RefundKind        string  `json:"refund_kind"`
	Amount            int64   `json:"amount"`
	Currency          string  `json:"currency"`
	Status            string  `json:"status"`
	ProviderStatus    *string `json:"provider_status"`
	SafeStatusMessage string  `json:"safe_status_message"`
	EntitlementStatus string  `json:"entitlement_status"`
	Version           uint    `json:"version"`
	RequestedAt       string  `json:"requested_at"`
	UpdatedAt         string  `json:"updated_at"`
	SucceededAt       *string `json:"succeeded_at"`
}
