package product

import "time"

type Category struct {
	ID            uint64
	ParentID      *uint64
	Name          string
	SortOrder     int
	Status        string
	AgeRestricted bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DeletedAt     *time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Category) TableName() string { return "categories" }

type Product struct {
	ID                  uint64
	CategoryID          uint64
	Name                string
	BrandName           *string
	Spec                *string
	ImageURL            *string
	Description         *string
	SalePriceAmount     int64
	OriginalPriceAmount int64
	Status              string
	AgeRestricted       bool
}

// TableName 返回当前数据模型对应的数据库表名。
func (Product) TableName() string { return "products" }

type ProductRow struct {
	ID                  uint64
	CategoryID          uint64
	ShopID              uint64
	ShopProductID       uint64
	SortOrder           int
	Name                string
	BrandName           *string
	Spec                *string
	ImageURL            *string
	Description         *string
	SalePriceAmount     int64
	OriginalPriceAmount int64
	Status              string
	ShopProductStatus   string
	ShopStatus          string
	BusinessStatus      string
	AvailableQty        int
	AgeRestricted       bool
}
