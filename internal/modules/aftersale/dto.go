package aftersale

import "jiuxiaoer-admin/backend-go/internal/pkg/pagination"

type CreateReq struct {
	OrderID             string          `json:"order_id" binding:"required"`
	Type                string          `json:"type" binding:"required,oneof=unopened_return damaged missing_item out_of_stock late_delivery other"`
	RequestedResolution string          `json:"requested_resolution" binding:"required,oneof=refund_only return_and_refund replacement compensation"`
	Items               []CreateItemReq `json:"items" binding:"required,min=1,max=50,dive"`
	IncludeDeliveryFee  bool            `json:"include_delivery_fee"`
	Description         string          `json:"description" binding:"required,min=5,max=1000"`
	EvidenceTokens      []string        `json:"evidence_tokens" binding:"max=9,dive,min=8,max=1024"`
}
type CreateItemReq struct {
	OrderItemID     string `json:"order_item_id" binding:"required"`
	Quantity        int    `json:"quantity" binding:"required,min=1"`
	RequestedAmount int64  `json:"requested_amount" binding:"min=0"`
}
type EvidenceReq struct {
	EvidenceTokens []string `json:"evidence_tokens" binding:"required,min=1,max=9,dive,min=8,max=1024"`
	Version        uint32   `json:"version" binding:"required,min=1"`
}
type WithdrawReq struct {
	Reason  string `json:"reason" binding:"max=1000"`
	Version uint32 `json:"version" binding:"required,min=1"`
}
type AppealReq struct {
	Remark         string   `json:"remark" binding:"required,min=5,max=1000"`
	EvidenceTokens []string `json:"evidence_tokens" binding:"max=9,dive,min=8,max=1024"`
	Version        uint32   `json:"version" binding:"required,min=1"`
}
type ReviewReq struct {
	Decision          string            `json:"decision" binding:"required,oneof=approve reject request_evidence escalate"`
	Resolution        string            `json:"resolution" binding:"omitempty,oneof=refund_only return_and_refund replacement compensation"`
	ApprovedItems     []ApprovedItemReq `json:"approved_items" binding:"max=50,dive"`
	RefundDeliveryFee bool              `json:"refund_delivery_fee"`
	ReasonCode        string            `json:"reason_code" binding:"max=64"`
	Remark            string            `json:"remark" binding:"max=1000"`
	Version           uint32            `json:"version" binding:"required,min=1"`
}
type ApprovedItemReq struct {
	AfterSaleItemID string `json:"after_sale_item_id" binding:"required"`
	Quantity        int    `json:"quantity" binding:"required,min=1"`
	Amount          int64  `json:"amount" binding:"min=0"`
}
type ReturnReceiptReq struct {
	Disposition         string `json:"disposition" binding:"required,oneof=restock damaged discard"`
	SealedPackageIntact bool   `json:"sealed_package_intact"`
	GoodsIntact         bool   `json:"goods_intact"`
	Remark              string `json:"remark" binding:"max=1000"`
	Version             uint32 `json:"version" binding:"required,min=1"`
}
type ReplacementReq struct {
	Version uint32 `json:"version" binding:"required,min=1"`
}
type ReturnReceiptDTO struct {
	ID                  string `json:"id"`
	ReceiptNo           string `json:"receipt_no"`
	AfterSaleID         string `json:"after_sale_id"`
	Disposition         string `json:"disposition"`
	SealedPackageIntact bool   `json:"sealed_package_intact"`
	GoodsIntact         bool   `json:"goods_intact"`
	ReceivedAt          string `json:"received_at"`
}
type ReplacementDTO struct {
	ID            string `json:"id"`
	ReplacementNo string `json:"replacement_no"`
	AfterSaleID   string `json:"after_sale_id"`
	Status        string `json:"status"`
	Version       uint32 `json:"version"`
}

type DTO struct {
	ID                  string        `json:"id"`
	AfterSaleNo         string        `json:"after_sale_no"`
	OrderID             string        `json:"order_id"`
	CustomerID          string        `json:"customer_id"`
	MerchantID          string        `json:"merchant_id"`
	ShopID              string        `json:"shop_id"`
	InitiatorType       string        `json:"initiator_type"`
	SourceType          string        `json:"source_type"`
	SourceID            string        `json:"source_id,omitempty"`
	Type                string        `json:"type"`
	RequestedResolution string        `json:"requested_resolution"`
	ApprovedResolution  string        `json:"approved_resolution,omitempty"`
	Status              string        `json:"status"`
	RequestedAmount     int64         `json:"requested_amount"`
	ApprovedAmount      int64         `json:"approved_amount"`
	RefundedAmount      int64         `json:"refunded_amount"`
	CompensationAmount  int64         `json:"compensation_amount"`
	IncludeDeliveryFee  bool          `json:"include_delivery_fee"`
	ReasonCode          string        `json:"reason_code,omitempty"`
	Description         string        `json:"description"`
	Version             uint32        `json:"version"`
	Items               []ItemDTO     `json:"items,omitempty"`
	Evidence            []EvidenceDTO `json:"evidence,omitempty"`
	History             []HistoryDTO  `json:"history,omitempty"`
	SubmittedAt         string        `json:"submitted_at"`
	CreatedAt           string        `json:"created_at"`
	UpdatedAt           string        `json:"updated_at"`
}
type ItemDTO struct {
	ID                string `json:"id"`
	OrderItemID       string `json:"order_item_id"`
	ShopProductID     string `json:"shop_product_id"`
	ProductID         string `json:"product_id"`
	RequestedQuantity int    `json:"requested_quantity"`
	ApprovedQuantity  int    `json:"approved_quantity"`
	RequestedAmount   int64  `json:"requested_amount"`
	ApprovedAmount    int64  `json:"approved_amount"`
	RefundedAmount    int64  `json:"refunded_amount"`
	ReturnDisposition string `json:"return_disposition"`
}
type EvidenceDTO struct {
	ID        string `json:"id"`
	MimeType  string `json:"mime_type"`
	SizeBytes uint64 `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}
type HistoryDTO struct {
	Action     string `json:"action"`
	ActorType  string `json:"actor_type"`
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
	ReasonCode string `json:"reason_code,omitempty"`
	Remark     string `json:"remark,omitempty"`
	CreatedAt  string `json:"created_at"`
}
type ListQuery struct {
	pagination.Query
	Status, Type string
}
