package order

type OrderCreateReq struct {
	ShopID    string               `json:"shop_id" binding:"required"`
	AddressID string               `json:"address_id" binding:"required"`
	Items     []OrderCreateItemReq `json:"items" binding:"required,min=1,max=50"`
	Remark    string               `json:"remark" binding:"max=255"`
}

type OrderCreateItemReq struct {
	ShopProductID string `json:"shop_product_id" binding:"required"`
	Quantity      int    `json:"quantity" binding:"required,min=1,max=99"`
}

type OrderCancelReq struct {
	Reason          string `json:"reason" binding:"max=255"`
	ReasonCode      string `json:"reason_code" binding:"omitempty,max=64"`
	ExpectedVersion uint   `json:"expected_version"`
}

type MockPayReq struct {
	Channel string `json:"channel" binding:"required"`
}

type PaymentCreateReq struct {
	Provider      string         `json:"provider" binding:"required,oneof=wechat"`
	ClientType    string         `json:"client_type" binding:"required,oneof=miniapp"`
	ReturnContext map[string]any `json:"return_context" binding:"omitempty"`
}

type OrderCreateResp struct {
	OrderID         string `json:"order_id"`
	OrderNo         string `json:"order_no"`
	Status          string `json:"status"`
	PayableAmount   int64  `json:"payable_amount"`
	ExpiresAt       string `json:"expires_at"`
	DeliveryPromise any    `json:"delivery_promise,omitempty"`
}

type OrderDTO struct {
	ID                string         `json:"id"`
	OrderNo           string         `json:"order_no"`
	CustomerID        string         `json:"customer_id"`
	MerchantID        string         `json:"merchant_id"`
	ShopID            string         `json:"shop_id"`
	Status            string         `json:"status"`
	PayStatus         string         `json:"pay_status"`
	DeliveryStatus    string         `json:"delivery_status"`
	GoodsAmount       int64          `json:"goods_amount"`
	DiscountAmount    int64          `json:"discount_amount"`
	DeliveryFeeAmount int64          `json:"delivery_fee_amount"`
	PayableAmount     int64          `json:"payable_amount"`
	PaidAmount        int64          `json:"paid_amount"`
	Items             []OrderItemDTO `json:"items,omitempty"`
	CreatedAt         string         `json:"created_at,omitempty"`
	ExpiresAt         string         `json:"expires_at,omitempty"`
}

type OrderItemDTO struct {
	ID              string `json:"id"`
	ShopProductID   string `json:"shop_product_id"`
	ProductID       string `json:"product_id"`
	Name            string `json:"name"`
	BrandName       string `json:"brand_name,omitempty"`
	Spec            string `json:"spec,omitempty"`
	Quantity        int    `json:"quantity"`
	SalePriceAmount int64  `json:"sale_price_amount"`
	TotalAmount     int64  `json:"total_amount"`
}

type PaymentDTO struct {
	ID              string         `json:"id"`
	PaymentNo       string         `json:"payment_no"`
	OrderID         string         `json:"order_id"`
	Channel         string         `json:"channel"`
	Provider        string         `json:"provider"`
	ProviderTradeNo string         `json:"provider_trade_no,omitempty"`
	Status          string         `json:"status"`
	ProviderStatus  string         `json:"provider_status,omitempty"`
	Amount          int64          `json:"amount"`
	Currency        string         `json:"currency"`
	ClientPayload   map[string]any `json:"client_payload,omitempty"`
	ExpiresAt       string         `json:"expires_at,omitempty"`
	PaidAt          string         `json:"paid_at,omitempty"`
	RefundedAmount  int64          `json:"refunded_amount"`
}
