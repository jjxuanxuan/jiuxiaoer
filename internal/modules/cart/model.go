package cart

type Cart struct {
	ID         uint64
	CustomerID uint64
}

// TableName 返回当前数据模型对应的数据库表名。
func (Cart) TableName() string { return "carts" }

type CartItem struct {
	ID            uint64
	CartID        uint64
	ShopProductID uint64
	ShopID        uint64
	ProductID     uint64
	Quantity      int
	Selected      bool
}

// TableName 返回当前数据模型对应的数据库表名。
func (CartItem) TableName() string { return "cart_items" }

type ShopProductRow struct {
	ShopProductID     uint64
	ShopID            uint64
	ProductID         uint64
	Name              string
	BrandName         *string
	Spec              *string
	ImageURL          *string
	SalePriceAmount   int64
	ProductStatus     string
	CategoryStatus    string
	ShopProductStatus string
	ShopStatus        string
	BusinessStatus    string
	AvailableQty      int
}

type CartItemRow struct {
	ID                uint64
	ShopProductID     uint64
	ShopID            uint64
	ProductID         uint64
	Name              string
	BrandName         *string
	Spec              *string
	ImageURL          *string
	Quantity          int
	SalePriceAmount   int64
	Selected          bool
	ProductStatus     string
	CategoryStatus    string
	ShopProductStatus string
	ShopStatus        string
	BusinessStatus    string
	AvailableQty      int
}
