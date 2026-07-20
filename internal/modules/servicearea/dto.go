package servicearea

import "time"

type ResolveInput struct {
	CityCode  string
	Latitude  float64
	Longitude float64
}

type DeliveryPromiseDTO struct {
	DeliveryFeeAmount           int64      `json:"delivery_fee_amount"`
	FreeDeliveryThresholdAmount *int64     `json:"free_delivery_threshold_amount,omitempty"`
	ETAMinMinutes               uint16     `json:"eta_min_minutes"`
	ETAMaxMinutes               uint16     `json:"eta_max_minutes"`
	OvertimePolicyCode          string     `json:"overtime_policy_code,omitempty"`
	Policy                      *PolicyDTO `json:"policy,omitempty"`
	RouteDistanceM              *uint64    `json:"route_distance_m,omitempty"`
	RouteDurationSeconds        *uint64    `json:"route_duration_seconds,omitempty"`
	RouteSource                 string     `json:"route_source,omitempty"`
	Confirmed                   bool       `json:"confirmed"`
}

type PolicyDTO struct {
	Code     string `json:"code"`
	Version  uint32 `json:"version"`
	Title    string `json:"title"`
	Summary  string `json:"summary"`
	TermsURL string `json:"terms_url,omitempty"`
}

type ShopDTO struct {
	ID                   string             `json:"id"`
	MerchantID           string             `json:"merchant_id"`
	Name                 string             `json:"name"`
	CityCode             string             `json:"city_code"`
	District             string             `json:"district"`
	Address              string             `json:"address"`
	Latitude             float64            `json:"latitude"`
	Longitude            float64            `json:"longitude"`
	DistanceM            int64              `json:"distance_m"`
	RouteDistanceM       *uint64            `json:"route_distance_m,omitempty"`
	RouteDurationSeconds *uint64            `json:"route_duration_seconds,omitempty"`
	Degraded             bool               `json:"degraded"`
	Selectable           bool               `json:"selectable"`
	Selected             bool               `json:"selected,omitempty"`
	Priority             int                `json:"priority,omitempty"`
	SelectionSource      string             `json:"selection_source,omitempty"`
	ServiceAreaVersion   uint32             `json:"service_area_version"`
	DeliveryPromise      DeliveryPromiseDTO `json:"delivery_promise"`
}

type ResolveDTO struct {
	ServiceShop ShopDTO   `json:"service_shop"`
	ResolvedAt  time.Time `json:"resolved_at"`
}
