package product

import (
	"jiuxiaoer-admin/backend-go/internal/modules/customerlocation"
	"jiuxiaoer-admin/backend-go/internal/modules/servicearea"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
)

type ListQuery struct {
	pagination.Query
	ShopID             string
	CategoryID         string
	Keyword            string
	CityCode           string
	Latitude           *float64
	Longitude          *float64
	LocationContextID  string
	LocationActor      customerlocation.Actor
	locationlessReason string
}

type CategoryDTO struct {
	ID            string `json:"id"`
	ParentID      string `json:"parent_id,omitempty"`
	Name          string `json:"name"`
	SortOrder     int    `json:"sort_order"`
	Status        string `json:"status"`
	AgeRestricted bool   `json:"age_restricted"`
}

type ProductDTO struct {
	ID                  string                          `json:"id"`
	CategoryID          string                          `json:"category_id"`
	ContextType         string                          `json:"context_type"`
	ShopID              string                          `json:"shop_id,omitempty"`
	ShopProductID       string                          `json:"shop_product_id,omitempty"`
	Name                string                          `json:"name"`
	BrandName           string                          `json:"brand_name,omitempty"`
	Spec                string                          `json:"spec,omitempty"`
	ImageURL            string                          `json:"image_url,omitempty"`
	Description         string                          `json:"description,omitempty"`
	SalePriceAmount     *int64                          `json:"sale_price_amount,omitempty"`
	OriginalPriceAmount *int64                          `json:"original_price_amount,omitempty"`
	Status              string                          `json:"status"`
	AvailableQty        *int                            `json:"available_qty,omitempty"`
	Purchasable         bool                            `json:"purchasable"`
	UnavailableReason   *string                         `json:"unavailable_reason"`
	DeliveryPromise     *servicearea.DeliveryPromiseDTO `json:"delivery_promise,omitempty"`
	AgeRestricted       bool                            `json:"age_restricted"`
}
