package reminder

type NotificationConsentCreateRequest struct {
	Scene           string `json:"scene"`
	TemplateCode    string `json:"template_code"`
	ConsentResult   string `json:"consent_result"`
	ProviderReceipt string `json:"provider_receipt,omitempty"`
}

type NotificationConsentDTO struct {
	ConsentID     string `json:"consent_id"`
	Scene         string `json:"scene"`
	TemplateCode  string `json:"template_code"`
	ConsentResult string `json:"consent_result"`
	Status        string `json:"status"`
	ConsentedAt   string `json:"consented_at"`
}
