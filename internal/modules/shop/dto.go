package shop

import "jiuxiaoer-admin/backend-go/internal/pkg/pagination"

type ListQuery struct {
	pagination.Query
	City      string
	District  string
	Keyword   string
	CityCode  string
	Latitude  *float64
	Longitude *float64
}

type ShopDTO struct {
	ID                          string  `json:"id"`
	MerchantID                  string  `json:"merchant_id"`
	Name                        string  `json:"name"`
	Phone                       string  `json:"phone,omitempty"`
	City                        string  `json:"city"`
	CityCode                    string  `json:"city_code,omitempty"`
	District                    string  `json:"district"`
	Address                     string  `json:"address"`
	Latitude                    float64 `json:"latitude,omitempty"`
	Longitude                   float64 `json:"longitude,omitempty"`
	CoordinateSystem            string  `json:"coordinate_system"`
	Status                      string  `json:"status"`
	BusinessStatus              string  `json:"business_status"`
	DistanceM                   *int64  `json:"distance_m,omitempty"`
	Serviceable                 *bool   `json:"serviceable,omitempty"`
	DeliveryFeeAmount           int64   `json:"delivery_fee_amount"`
	FreeDeliveryThresholdAmount *int64  `json:"free_delivery_threshold_amount,omitempty"`
	DeliveryETAMin              uint16  `json:"delivery_eta_min"`
	DeliveryETAMax              uint16  `json:"delivery_eta_max"`
	OvertimePolicyCode          string  `json:"overtime_policy_code,omitempty"`
}
