package address

import "time"

type CustomerAddress struct {
	ID               uint64
	CustomerID       uint64
	ContactName      string
	ContactPhone     string
	Province         string
	City             string
	CityCode         *string
	District         string
	DistrictCode     *string
	AddressDetail    string
	Doorplate        *string
	POIID            *string `gorm:"column:poi_id"`
	FormattedAddress *string
	Latitude         *float64
	Longitude        *float64
	CoordinateSystem string `gorm:"default:gcj02"`
	LocationSource   string
	GeocodeProvider  *string
	GeocodeStatus    string
	GeocodedAt       *time.Time
	IsDefault        bool
	Version          uint32
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeletedAt        *time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (CustomerAddress) TableName() string { return "customer_addresses" }
