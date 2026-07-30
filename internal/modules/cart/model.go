package cart

import (
	"database/sql/driver"
	"fmt"
	"time"
)

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

// FrequentPurchaseRow 是已完成订单历史与当前服务门店商品事实的聚合结果。
type FrequentPurchaseRow struct {
	ProductID           uint64
	ShopProductID       uint64
	ShopID              uint64
	Name                string
	BrandName           *string
	Spec                *string
	ImageURL            *string
	PurchaseCount       int
	PurchasedQuantity   int
	LastQuantity        int
	LastSalePriceAmount int64
	LastPurchasedAt     databaseTime
	SalePriceAmount     int64
	ProductStatus       string
	CategoryStatus      string
	ShopProductStatus   string
	ShopStatus          string
	BusinessStatus      string
	AvailableQty        int
}

// databaseTime 兼容 MySQL 驱动的 time.Time 与 SQLite 测试驱动的文本时间。
type databaseTime struct{ time.Time }

func (value databaseTime) Value() (driver.Value, error) {
	if value.Time.IsZero() {
		return nil, nil
	}
	return value.Time, nil
}

func (value *databaseTime) Scan(source any) error {
	switch typed := source.(type) {
	case time.Time:
		value.Time = typed
		return nil
	case string:
		return value.parse(typed)
	case []byte:
		return value.parse(string(typed))
	case nil:
		value.Time = time.Time{}
		return nil
	default:
		return fmt.Errorf("unsupported database time %T", source)
	}
}

func (value *databaseTime) parse(raw string) error {
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		parsed, err := time.Parse(layout, raw)
		if err == nil {
			value.Time = parsed
			return nil
		}
	}
	return fmt.Errorf("invalid database time %q", raw)
}
