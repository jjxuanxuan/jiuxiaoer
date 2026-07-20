package address

import (
	"jiuxiaoer-admin/backend-go/internal/modules/customerlocation"
	"jiuxiaoer-admin/backend-go/internal/modules/servicearea"
)

type AddressUpsertReq struct {
	ContactName      string   `json:"contact_name" binding:"required,min=1,max=64"`
	ContactPhone     string   `json:"contact_phone" binding:"required"`
	Province         string   `json:"province" binding:"required,max=64"`
	City             string   `json:"city" binding:"required,max=64"`
	CityCode         string   `json:"city_code" binding:"required"`
	District         string   `json:"district" binding:"required,max=64"`
	DistrictCode     string   `json:"district_code"`
	AddressDetail    string   `json:"address_detail" binding:"required,min=1,max=255"`
	Doorplate        string   `json:"doorplate" binding:"max=128"`
	POIID            string   `json:"poi_id" binding:"max=64"`
	FormattedAddress string   `json:"formatted_address" binding:"max=255"`
	LocationSource   string   `json:"location_source"`
	Latitude         *float64 `json:"latitude"`
	Longitude        *float64 `json:"longitude"`
	CoordinateSystem string   `json:"coordinate_system"`
	IsDefault        bool     `json:"is_default"`
}

type AddressUpdateReq struct {
	ContactName      string   `json:"contact_name" binding:"required,min=1,max=64"`
	ContactPhone     string   `json:"contact_phone" binding:"required"`
	Province         string   `json:"province" binding:"required,max=64"`
	City             string   `json:"city" binding:"required,max=64"`
	CityCode         string   `json:"city_code" binding:"required"`
	District         string   `json:"district" binding:"required,max=64"`
	DistrictCode     string   `json:"district_code"`
	AddressDetail    string   `json:"address_detail" binding:"required,min=1,max=255"`
	Doorplate        string   `json:"doorplate" binding:"max=128"`
	POIID            string   `json:"poi_id" binding:"max=64"`
	FormattedAddress string   `json:"formatted_address" binding:"max=255"`
	LocationSource   string   `json:"location_source"`
	Latitude         *float64 `json:"latitude"`
	Longitude        *float64 `json:"longitude"`
	CoordinateSystem string   `json:"coordinate_system"`
	Version          uint32   `json:"version" binding:"required,min=1"`
}

type AddressDTO struct {
	ID                 string                              `json:"id"`
	ContactName        string                              `json:"contact_name"`
	ContactPhoneMasked string                              `json:"contact_phone_masked"`
	Province           string                              `json:"province"`
	City               string                              `json:"city"`
	CityCode           string                              `json:"city_code"`
	District           string                              `json:"district"`
	DistrictCode       string                              `json:"district_code,omitempty"`
	AddressDetail      string                              `json:"address_detail"`
	Doorplate          string                              `json:"doorplate,omitempty"`
	POIID              string                              `json:"poi_id,omitempty"`
	FormattedAddress   string                              `json:"formatted_address,omitempty"`
	Latitude           *float64                            `json:"latitude,omitempty"`
	Longitude          *float64                            `json:"longitude,omitempty"`
	CoordinateSystem   string                              `json:"coordinate_system"`
	LocationSource     string                              `json:"location_source"`
	GeocodeProvider    string                              `json:"geocode_provider,omitempty"`
	GeocodeStatus      string                              `json:"geocode_status"`
	GeocodedAt         string                              `json:"geocoded_at,omitempty"`
	IsDefault          bool                                `json:"is_default"`
	Version            uint32                              `json:"version"`
	Serviceability     *customerlocation.ServiceabilityDTO `json:"serviceability,omitempty"`
	ServiceShop        *servicearea.ShopDTO                `json:"service_shop,omitempty"`
	DeliveryPromise    *servicearea.DeliveryPromiseDTO     `json:"delivery_promise,omitempty"`
}
