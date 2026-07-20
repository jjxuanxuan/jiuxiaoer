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
	ID                 string `json:"id"`
	ShopProductID      string `json:"shop_product_id"`
	ShopID             string `json:"shop_id"`
	ProductID          string `json:"product_id"`
	Name               string `json:"name"`
	BrandName          string `json:"brand_name,omitempty"`
	Spec               string `json:"spec,omitempty"`
	ImageURL           string `json:"image_url,omitempty"`
	Quantity           int    `json:"quantity"`
	SalePriceAmount    int64  `json:"sale_price_amount"`
	TotalAmount        int64  `json:"total_amount"`
	Selected           bool   `json:"selected"`
	AvailabilityStatus string `json:"availability_status"`
}
