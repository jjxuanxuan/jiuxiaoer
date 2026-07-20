package store

type StoreOrderDTO struct {
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

type BusinessStatusReq struct {
	BusinessStatus string `json:"business_status" binding:"required,oneof=open closed resting"`
}

type ShopProductCreateReq struct {
	ShopID              string `json:"shop_id" binding:"required"`
	ProductID           string `json:"product_id" binding:"required"`
	SalePriceAmount     int64  `json:"sale_price_amount" binding:"required,min=1"`
	Status              string `json:"status" binding:"omitempty,oneof=draft on_sale off_sale"`
	SortOrder           int    `json:"sort_order"`
	InitialAvailableQty int    `json:"initial_available_qty" binding:"min=0"`
}

type ShopProductUpdateReq struct {
	SalePriceAmount *int64  `json:"sale_price_amount" binding:"omitempty,min=1"`
	Status          *string `json:"status" binding:"omitempty,oneof=draft on_sale off_sale"`
	SortOrder       *int    `json:"sort_order"`
}

type StockAdjustReq struct {
	QuantityDelta int    `json:"quantity_delta" binding:"required"`
	Reason        string `json:"reason" binding:"max=255"`
}

type ShopProductDTO struct {
	ID                  string `json:"id"`
	MerchantID          string `json:"merchant_id"`
	ShopID              string `json:"shop_id"`
	ProductID           string `json:"product_id"`
	CategoryID          string `json:"category_id"`
	Name                string `json:"name"`
	BrandName           string `json:"brand_name,omitempty"`
	Spec                string `json:"spec,omitempty"`
	ImageURL            string `json:"image_url,omitempty"`
	SalePriceAmount     int64  `json:"sale_price_amount"`
	OriginalPriceAmount int64  `json:"original_price_amount"`
	Status              string `json:"status"`
	SortOrder           int    `json:"sort_order"`
	AvailableQty        int    `json:"available_qty"`
	ReservedQty         int    `json:"reserved_qty"`
	LockedQty           int    `json:"locked_qty"`
	AgeRestricted       bool   `json:"age_restricted"`
}
