package cart

type CartItemAddReq struct {
	ShopProductID string `json:"shop_product_id" binding:"required"`
	Quantity      int    `json:"quantity" binding:"required,min=1,max=99"`
}

type CartItemUpdateReq struct {
	Quantity int `json:"quantity" binding:"required,min=1,max=99"`
}

type CartItemSelectionReq struct {
	Selected bool `json:"selected"`
}

type CartSelectionReq struct {
	Selected bool   `json:"selected"`
	ShopID   string `json:"shop_id" binding:"required"`
}

type CartResp struct {
	Items            []CartItemDTO `json:"items"`
	TotalQuantity    int           `json:"total_quantity"`
	TotalAmount      int64         `json:"total_amount"`
	UnavailableCount int           `json:"unavailable_count"`
}

type CartItemDTO struct {
	ID                 string  `json:"id"`
	ShopProductID      string  `json:"shop_product_id"`
	ShopID             string  `json:"shop_id"`
	ProductID          string  `json:"product_id"`
	Name               string  `json:"name"`
	BrandName          string  `json:"brand_name,omitempty"`
	Spec               string  `json:"spec,omitempty"`
	ImageURL           string  `json:"image_url,omitempty"`
	Quantity           int     `json:"quantity"`
	SalePriceAmount    int64   `json:"sale_price_amount"`
	TotalAmount        int64   `json:"total_amount"`
	Selected           bool    `json:"selected"`
	AvailabilityStatus string  `json:"availability_status"`
	Available          bool    `json:"available"`
	UnavailableReason  *string `json:"unavailable_reason"`
}

type FrequentPurchaseResp struct {
	ShopID       string                `json:"shop_id"`
	LookbackDays int                   `json:"lookback_days"`
	Items        []FrequentPurchaseDTO `json:"items"`
}

type FrequentPurchaseDTO struct {
	ProductID           string  `json:"product_id"`
	ShopProductID       string  `json:"shop_product_id,omitempty"`
	ShopID              string  `json:"shop_id"`
	Name                string  `json:"name"`
	BrandName           string  `json:"brand_name,omitempty"`
	Spec                string  `json:"spec,omitempty"`
	ImageURL            string  `json:"image_url,omitempty"`
	PurchaseCount       int     `json:"purchase_count"`
	PurchasedQuantity   int     `json:"purchased_quantity"`
	LastQuantity        int     `json:"last_quantity"`
	RecommendedQuantity int     `json:"recommended_quantity"`
	LastSalePriceAmount int64   `json:"last_sale_price_amount"`
	SalePriceAmount     int64   `json:"sale_price_amount"`
	LastPurchasedAt     string  `json:"last_purchased_at"`
	AvailableQty        int     `json:"available_qty"`
	AvailabilityStatus  string  `json:"availability_status"`
	Available           bool    `json:"available"`
	UnavailableReason   *string `json:"unavailable_reason"`
}

type RepurchaseReq struct {
	Items            []RepurchaseItemReq `json:"items" binding:"required,min=1,dive"`
	ReplaceSelection bool                `json:"replace_selection"`
}

type RepurchaseItemReq struct {
	ProductID string `json:"product_id" binding:"required"`
	Quantity  int    `json:"quantity" binding:"required,min=1,max=99"`
}

type RepurchaseResp struct {
	ShopID         string                 `json:"shop_id"`
	Results        []RepurchaseItemResult `json:"results"`
	SucceededCount int                    `json:"succeeded_count"`
	FailedCount    int                    `json:"failed_count"`
	Cart           CartResp               `json:"cart"`
}

type RepurchaseItemResult struct {
	ProductID         string `json:"product_id"`
	ShopProductID     string `json:"shop_product_id,omitempty"`
	RequestedQuantity int    `json:"requested_quantity"`
	CartQuantity      int    `json:"cart_quantity,omitempty"`
	Status            string `json:"status"`
	ErrorCode         string `json:"error_code,omitempty"`
}

type parsedRepurchaseItem struct {
	ProductID uint64
	RawID     string
	Quantity  int
}

type repurchaseCandidate struct {
	Product        ShopProductRow
	TargetQuantity int
}
