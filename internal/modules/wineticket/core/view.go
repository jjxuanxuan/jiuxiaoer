package core

// ProductSummaryDTO 是嵌入套餐、购买、酒柜、礼赠、核销、续期和退款响应的
// 共享不可变商品投影。
type ProductSummaryDTO struct {
	ProductID string  `json:"product_id"`
	Name      string  `json:"name"`
	BrandName *string `json:"brand_name"`
	Spec      *string `json:"spec"`
	ImageURL  *string `json:"image_url"`
}
