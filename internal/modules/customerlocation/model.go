package customerlocation

import "time"

type ServiceCity struct {
	ID                  uint64
	CityCode            string
	ProvinceCode        string
	Name                string
	Pinyin              string
	Status              string
	SortOrder           int
	DefaultBrowseShopID *uint64
	Version             uint32
	PublishedAt         *time.Time
	CreatedBy           uint64
	UpdatedBy           uint64
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

func (ServiceCity) TableName() string { return "service_cities" }

type ServiceCityAdcode struct {
	ID            uint64
	ServiceCityID uint64
	ADCode        string `gorm:"column:adcode"`
	StandardName  string
	Level         string
	CreatedBy     uint64
	UpdatedBy     uint64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (ServiceCityAdcode) TableName() string { return "service_city_adcodes" }

type DeliveryPromisePolicy struct {
	ID            uint64
	PolicyCode    string
	Version       uint32
	Title         string
	Summary       string
	TermsURL      *string
	Status        string
	EffectiveFrom *time.Time
	EffectiveTo   *time.Time
	PublishedAt   *time.Time
	PublishedBy   *uint64
	CreatedBy     uint64
	UpdatedBy     uint64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (DeliveryPromisePolicy) TableName() string { return "delivery_promise_policies" }

type SavedAddress struct {
	ID               uint64
	CustomerID       uint64
	Province         string
	City             string
	CityCode         *string
	District         string
	DistrictCode     *string
	FormattedAddress *string
	Latitude         *float64
	Longitude        *float64
	CoordinateSystem string
	GeocodeStatus    string
	Version          uint32
	DeletedAt        *time.Time
}

type mappedCity struct {
	ServiceCity
	StandardName string
}
