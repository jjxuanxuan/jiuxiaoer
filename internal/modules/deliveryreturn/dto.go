package deliveryreturn

type CreateReq struct {
	ReasonCode              string `json:"reason_code" binding:"required,oneof=customer_unreachable customer_refused address_wrong damaged_in_transit other"`
	Note                    string `json:"note" binding:"max=500"`
	IncidentID              string `json:"incident_id"`
	ExpectedDeliveryVersion uint   `json:"expected_delivery_version" binding:"required,min=1"`
}

type ApproveReq struct {
	ExpectedVersion uint64 `json:"expected_version" binding:"required,min=1"`
	DecisionNote    string `json:"decision_note" binding:"required,max=500"`
}

type ArriveReq struct {
	ExpectedVersion uint64 `json:"expected_version" binding:"required,min=1"`
}

type ReceiveReq struct {
	ExpectedVersion uint64           `json:"expected_version" binding:"required,min=1"`
	HandoffCode     string           `json:"handoff_code" binding:"required,min=6,max=32,alphanum"`
	Items           []ReceiveItemReq `json:"items" binding:"required,min=1,max=200,dive"`
}

type ReceiveItemReq struct {
	AfterSaleItemID  string `json:"after_sale_item_id" binding:"required"`
	ReceivedQuantity int    `json:"received_quantity" binding:"min=0"`
	Disposition      string `json:"disposition" binding:"required,oneof=restock damaged discard"`
	Note             string `json:"note" binding:"max=500"`
}

type ListQuery struct {
	Status string
	Offset int
	Limit  int
}

type HistoryDTO struct {
	ID         string         `json:"id"`
	FromStatus string         `json:"from_status,omitempty"`
	ToStatus   string         `json:"to_status,omitempty"`
	Action     string         `json:"action"`
	ActorType  string         `json:"actor_type"`
	ActorID    string         `json:"actor_id,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	CreatedAt  string         `json:"created_at"`
}

type DTO struct {
	ID                   string       `json:"id"`
	ReturnNo             string       `json:"return_no"`
	OrderID              string       `json:"order_id"`
	DeliveryOrderID      string       `json:"delivery_order_id"`
	ShopID               string       `json:"shop_id"`
	RiderID              string       `json:"rider_id"`
	IncidentID           string       `json:"incident_id,omitempty"`
	AfterSaleID          string       `json:"after_sale_id,omitempty"`
	SettlementType       string       `json:"settlement_type"`
	SettlementBizID      string       `json:"settlement_biz_id,omitempty"`
	SettlementStatus     string       `json:"settlement_status"`
	SettledAt            string       `json:"settled_at,omitempty"`
	ReasonCode           string       `json:"reason_code"`
	Status               string       `json:"status"`
	InitiatorType        string       `json:"initiator_type"`
	LogisticsStatus      string       `json:"logistics_status"`
	RefundStatus         string       `json:"refund_status"`
	InventoryStatus      string       `json:"inventory_status"`
	Version              uint64       `json:"version"`
	AllowedActions       []string     `json:"allowed_actions"`
	RequestedAt          string       `json:"requested_at"`
	ApprovedAt           string       `json:"approved_at,omitempty"`
	ArrivedAt            string       `json:"arrived_at,omitempty"`
	ReceivedAt           string       `json:"received_at,omitempty"`
	ClosedAt             string       `json:"closed_at,omitempty"`
	ReceiptDeadlineAt    string       `json:"receipt_deadline_at,omitempty"`
	HandoffCode          string       `json:"handoff_code,omitempty"`
	HandoffCodeExpiresAt string       `json:"handoff_code_expires_at,omitempty"`
	CreatedAt            string       `json:"created_at"`
	UpdatedAt            string       `json:"updated_at"`
	Items                []ItemDTO    `json:"items,omitempty"`
	History              []HistoryDTO `json:"timeline,omitempty"`
	Deduplicated         bool         `json:"deduplicated,omitempty"`
}

type ItemDTO struct {
	AfterSaleItemID  string `json:"after_sale_item_id"`
	OrderItemID      string `json:"order_item_id"`
	ShopProductID    string `json:"shop_product_id"`
	ProductID        string `json:"product_id"`
	ExpectedQuantity int    `json:"expected_quantity"`
	ReceivedQuantity *int   `json:"received_quantity,omitempty"`
	Disposition      string `json:"disposition,omitempty"`
	PolicyCode       string `json:"policy_code"`
	PolicyVersion    string `json:"policy_version"`
	AvailableBefore  *int   `json:"available_before,omitempty"`
	AvailableAfter   *int   `json:"available_after,omitempty"`
	Note             string `json:"note,omitempty"`
}
