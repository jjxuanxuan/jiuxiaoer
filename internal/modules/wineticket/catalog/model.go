package catalog

import (
	"time"

	"gorm.io/datatypes"
)

const (
	PackageTypeStockpile = "stockpile"
	PackageTypeCorporate = "corporate"
	PackageTypeGift      = "gift"

	PackageStatusDraft       = "draft"
	PackageStatusPublished   = "published"
	PackageStatusUnpublished = "unpublished"
	PackageStatusArchived    = "archived"
)

// Package 是可变的套餐目录记录。PackageVersion 是同一 PackageCode 下的业务版本，
// Version 是记录级比较并交换版本。
type Package struct {
	ID                      uint64
	PackageNo               string
	PackageCode             string
	PackageVersion          uint
	IssuerMerchantID        uint64
	SettlementShopID        uint64
	SettlementShopProductID uint64
	ProductID               uint64
	RedeemCityCode          string
	PackageType             string
	Name                    string
	Subtitle                *string
	CoverImageURL           *string
	BottleQuantity          uint
	SalePriceAmount         int64
	MinPurchaseQuantity     uint
	MaxPurchaseQuantity     uint
	ValidityDays            uint
	PerCustomerLimit        *uint
	RefundPolicy            datatypes.JSON
	RenewalPolicy           datatypes.JSON
	DeliveryPolicy          datatypes.JSON
	Status                  string
	SaleStartAt             *time.Time
	SaleEndAt               *time.Time
	PublishedAt             *time.Time
	PublishedBy             *uint64
	Version                 uint
	CreatedAt               time.Time
	UpdatedAt               time.Time
	DeletedAt               *time.Time
	CreatedBy               *uint64
	UpdatedBy               *uint64
}

func (Package) TableName() string { return "wine_ticket_packages" }

// PackageRecord 增加套餐响应所需的只读商品投影，
// 不会把商品字段转化为套餐持久化状态。
type PackageRecord struct {
	Package `gorm:"embedded"`

	ProductName      string  `gorm:"column:product_name"`
	ProductBrandName *string `gorm:"column:product_brand_name"`
	ProductSpec      *string `gorm:"column:product_spec"`
	ProductImageURL  *string `gorm:"column:product_image_url"`
}

type PackageListFilter struct {
	PackageType string
}

// SettlementRelation 以单个关联事实加载，避免发布校验误将不同记录中的
// 门店、门店商品、商户和商品拼接为有效关系。
type SettlementRelation struct {
	MerchantID            uint64 `gorm:"column:merchant_id"`
	MerchantStatus        string `gorm:"column:merchant_status"`
	MerchantReviewStatus  string `gorm:"column:merchant_review_status"`
	ShopID                uint64 `gorm:"column:shop_id"`
	ShopMerchantID        uint64 `gorm:"column:shop_merchant_id"`
	ShopStatus            string `gorm:"column:shop_status"`
	ShopBusinessStatus    string `gorm:"column:shop_business_status"`
	ShopProductID         uint64 `gorm:"column:shop_product_id"`
	ShopProductMerchant   uint64 `gorm:"column:shop_product_merchant_id"`
	ShopProductShop       uint64 `gorm:"column:shop_product_shop_id"`
	ShopProductProduct    uint64 `gorm:"column:shop_product_product_id"`
	ShopProductStatus     string `gorm:"column:shop_product_status"`
	ProductID             uint64 `gorm:"column:product_id"`
	ProductStatus         string `gorm:"column:product_status"`
	ProductCategoryStatus string `gorm:"column:product_category_status"`
	ProductAgeRestricted  bool   `gorm:"column:product_age_restricted"`
}
