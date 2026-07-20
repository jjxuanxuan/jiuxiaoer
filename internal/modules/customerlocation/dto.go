package customerlocation

import (
	"time"

	"jiuxiaoer-admin/backend-go/internal/modules/servicearea"
)

type ResolveRequest struct {
	Source           string     `json:"source" binding:"required,oneof=device_location saved_address"`
	Latitude         *float64   `json:"latitude"`
	Longitude        *float64   `json:"longitude"`
	CoordinateSystem string     `json:"coordinate_system"`
	AccuracyM        *float64   `json:"accuracy_m"`
	CapturedAt       *time.Time `json:"captured_at"`
	AddressID        string     `json:"address_id"`
	CityCodeHint     string     `json:"city_code_hint"`
}

type CityContextRequest struct {
	CityCode string `json:"city_code" binding:"required"`
	Source   string `json:"source" binding:"required,eq=manual_city"`
}

type SwitchShopRequest struct {
	ShopID          string `json:"shop_id" binding:"required"`
	ExpectedVersion uint32 `json:"expected_version" binding:"required,min=1"`
}

type AdministrativeLocationDTO struct {
	Province         string `json:"province"`
	City             string `json:"city"`
	CityCode         string `json:"city_code"`
	District         string `json:"district,omitempty"`
	DistrictCode     string `json:"district_code,omitempty"`
	Township         string `json:"township,omitempty"`
	TownCode         string `json:"town_code,omitempty"`
	FormattedAddress string `json:"formatted_address,omitempty"`
}

type ServiceabilityDTO struct {
	Serviceable bool   `json:"serviceable"`
	ReasonCode  string `json:"reason_code,omitempty"`
}

type LocationContext struct {
	ID                 string                          `json:"id"`
	Version            uint32                          `json:"version"`
	ActorType          string                          `json:"actor_type"`
	ActorID            string                          `json:"actor_id,omitempty"`
	SessionHash        string                          `json:"session_hash,omitempty"`
	Source             string                          `json:"source"`
	LocationLevel      string                          `json:"location_level"`
	Latitude           *float64                        `json:"latitude,omitempty"`
	Longitude          *float64                        `json:"longitude,omitempty"`
	CoordinateSystem   string                          `json:"coordinate_system,omitempty"`
	AccuracyM          *float64                        `json:"accuracy_m,omitempty"`
	Location           AdministrativeLocationDTO       `json:"location"`
	AddressID          string                          `json:"address_id,omitempty"`
	AddressVersion     uint32                          `json:"address_version,omitempty"`
	ServiceShop        *servicearea.ShopDTO            `json:"service_shop,omitempty"`
	CandidateShops     []servicearea.ShopDTO           `json:"candidate_shops,omitempty"`
	SelectionSource    string                          `json:"selection_source,omitempty"`
	ServiceAreaVersion uint32                          `json:"service_area_version,omitempty"`
	DeliveryPromise    *servicearea.DeliveryPromiseDTO `json:"delivery_promise,omitempty"`
	Serviceability     ServiceabilityDTO               `json:"serviceability"`
	Degraded           bool                            `json:"degraded"`
	ResolutionSource   string                          `json:"resolution_source"`
	CreatedAt          time.Time                       `json:"created_at"`
	ExpiresAt          time.Time                       `json:"expires_at"`
}

type ResolveResponse struct {
	LocationContextID string                          `json:"location_context_id"`
	Version           uint32                          `json:"version"`
	ExpiresAt         time.Time                       `json:"expires_at"`
	LocationLevel     string                          `json:"location_level"`
	Location          AdministrativeLocationDTO       `json:"location"`
	Serviceability    ServiceabilityDTO               `json:"serviceability"`
	ServiceShop       *servicearea.ShopDTO            `json:"service_shop,omitempty"`
	DeliveryPromise   *servicearea.DeliveryPromiseDTO `json:"delivery_promise,omitempty"`
	BrowseOnly        bool                            `json:"browse_only,omitempty"`
	Degraded          bool                            `json:"degraded"`
	ResolutionSource  string                          `json:"resolution_source"`
}

type ServiceCityDTO struct {
	CityCode         string `json:"city_code"`
	Name             string `json:"name"`
	Pinyin           string `json:"pinyin"`
	ProvinceCode     string `json:"province_code"`
	LocationRequired bool   `json:"location_required"`
}

type SwitchShopResponse struct {
	Version         uint32                         `json:"version"`
	ServiceShop     servicearea.ShopDTO            `json:"service_shop"`
	DeliveryPromise servicearea.DeliveryPromiseDTO `json:"delivery_promise"`
	SelectionSource string                         `json:"selection_source"`
	CartImpact      string                         `json:"cart_impact"`
}

type Actor struct {
	Type        string
	ID          string
	SessionHash string
}

type ClientMeta struct {
	IP string
}

type OrderResolution struct {
	Context        LocationContext
	AddressVersion uint32
}

type CoveredADCodeRequest struct {
	ADCode       string `json:"adcode" binding:"required"`
	StandardName string `json:"standard_name" binding:"required,max=64"`
	Level        string `json:"level" binding:"required,oneof=city district county"`
}

type ServiceCityWriteRequest struct {
	CityCode            string                 `json:"city_code" binding:"required"`
	ProvinceCode        string                 `json:"province_code" binding:"required"`
	Name                string                 `json:"name" binding:"required,max=64"`
	Pinyin              string                 `json:"pinyin" binding:"required,max=128"`
	SortOrder           int                    `json:"sort_order"`
	DefaultBrowseShopID string                 `json:"default_browse_shop_id"`
	CoveredADCodes      []CoveredADCodeRequest `json:"covered_adcodes" binding:"required,min=1,max=100"`
	ExpectedVersion     uint32                 `json:"expected_version,omitempty"`
}

type ResourceStatusRequest struct {
	Status          string `json:"status" binding:"required"`
	ExpectedVersion uint32 `json:"expected_version" binding:"required,min=1"`
}

type ServiceCityAdminDTO struct {
	ID                  string                 `json:"id"`
	CityCode            string                 `json:"city_code"`
	ProvinceCode        string                 `json:"province_code"`
	Name                string                 `json:"name"`
	Pinyin              string                 `json:"pinyin"`
	Status              string                 `json:"status"`
	SortOrder           int                    `json:"sort_order"`
	DefaultBrowseShopID string                 `json:"default_browse_shop_id,omitempty"`
	Version             uint32                 `json:"version"`
	CoveredADCodes      []CoveredADCodeRequest `json:"covered_adcodes"`
}

type PromisePolicyWriteRequest struct {
	PolicyCode      string `json:"policy_code" binding:"required,max=64"`
	Version         uint32 `json:"version" binding:"required,min=1"`
	Title           string `json:"title" binding:"required,max=128"`
	Summary         string `json:"summary" binding:"required,max=255"`
	TermsURL        string `json:"terms_url" binding:"omitempty,max=512"`
	EffectiveFrom   string `json:"effective_from"`
	EffectiveTo     string `json:"effective_to"`
	ExpectedVersion uint32 `json:"expected_version,omitempty"`
}

type PromisePolicyAdminDTO struct {
	ID            string `json:"id"`
	PolicyCode    string `json:"policy_code"`
	Version       uint32 `json:"version"`
	Title         string `json:"title"`
	Summary       string `json:"summary"`
	TermsURL      string `json:"terms_url,omitempty"`
	Status        string `json:"status"`
	EffectiveFrom string `json:"effective_from,omitempty"`
	EffectiveTo   string `json:"effective_to,omitempty"`
}

func responseFromContext(value LocationContext) ResolveResponse {
	return ResolveResponse{
		LocationContextID: value.ID, Version: value.Version, ExpiresAt: value.ExpiresAt,
		LocationLevel: value.LocationLevel, Location: value.Location, Serviceability: value.Serviceability,
		ServiceShop: value.ServiceShop, DeliveryPromise: value.DeliveryPromise,
		BrowseOnly: value.LocationLevel == "city", Degraded: value.Degraded, ResolutionSource: value.ResolutionSource,
	}
}
