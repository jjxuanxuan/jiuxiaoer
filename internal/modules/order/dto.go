package order

type OrderCreateReq struct {
	ShopID    string               `json:"shop_id" binding:"required"`
	AddressID string               `json:"address_id" binding:"required"`
	Items     []OrderCreateItemReq `json:"items" binding:"required,min=1,max=50"`
	Remark    string               `json:"remark" binding:"max=255"`
}

type OrderCreateItemReq struct {
	ShopProductID string `json:"shop_product_id" binding:"required"`
	Quantity      int    `json:"quantity" binding:"required,min=1,max=99"`
}

type OrderCancelReq struct {
	Reason          string `json:"reason" binding:"max=255"`
	ReasonCode      string `json:"reason_code" binding:"omitempty,max=64"`
	ExpectedVersion *uint  `json:"expected_version" binding:"required"`
}

type MockPayReq struct {
	Channel string `json:"channel" binding:"required"`
}

type PaymentCreateReq struct {
	Provider      string         `json:"provider" binding:"required,oneof=wechat"`
	ClientType    string         `json:"client_type" binding:"required,oneof=miniapp"`
	ReturnContext map[string]any `json:"return_context" binding:"omitempty"`
}

type OrderCreateResp struct {
	OrderID         string `json:"order_id"`
	OrderNo         string `json:"order_no"`
	Status          string `json:"status"`
	PayableAmount   int64  `json:"payable_amount"`
	ExpiresAt       string `json:"expires_at"`
	DeliveryPromise any    `json:"delivery_promise,omitempty"`
}

type OrderDTO struct {
	ID                string         `json:"id"`
	OrderNo           string         `json:"order_no"`
	CustomerID        string         `json:"customer_id"`
	MerchantID        string         `json:"merchant_id"`
	ShopID            string         `json:"shop_id"`
	Status            string         `json:"status"`
	PayStatus         string         `json:"pay_status"`
	DeliveryStatus    string         `json:"delivery_status"`
	GoodsAmount       int64          `json:"goods_amount"`
	DiscountAmount    int64          `json:"discount_amount"`
	DeliveryFeeAmount int64          `json:"delivery_fee_amount"`
	PayableAmount     int64          `json:"payable_amount"`
	PaidAmount        int64          `json:"paid_amount"`
	Items             []OrderItemDTO `json:"items,omitempty"`
	CreatedAt         string         `json:"created_at,omitempty"`
	ExpiresAt         string         `json:"expires_at,omitempty"`
}

type OrderItemDTO struct {
	ID              string `json:"id"`
	ShopProductID   string `json:"shop_product_id"`
	ProductID       string `json:"product_id"`
	Name            string `json:"name"`
	BrandName       string `json:"brand_name,omitempty"`
	Spec            string `json:"spec,omitempty"`
	ImageURL        string `json:"image_url,omitempty"`
	Quantity        int    `json:"quantity"`
	SalePriceAmount int64  `json:"sale_price_amount"`
	TotalAmount     int64  `json:"total_amount"`
}

type OrderShopSummaryDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type OrderItemSummaryDTO struct {
	ProductID string `json:"product_id"`
	Name      string `json:"name"`
	Spec      string `json:"spec,omitempty"`
	ImageURL  string `json:"image_url,omitempty"`
	Quantity  int    `json:"quantity"`
}

type OrderSummaryDTO struct {
	ID             string              `json:"id"`
	OrderNo        string              `json:"order_no"`
	ShopID         string              `json:"shop_id"`
	Status         string              `json:"status"`
	PayStatus      string              `json:"pay_status"`
	DeliveryStatus string              `json:"delivery_status"`
	PayableAmount  int64               `json:"payable_amount"`
	ShopSummary    OrderShopSummaryDTO `json:"shop_summary"`
	ItemSummary    OrderItemSummaryDTO `json:"item_summary"`
	ItemKindCount  int                 `json:"item_kind_count"`
	TotalQuantity  int                 `json:"total_quantity"`
	CreatedAt      string              `json:"created_at"`
	UpdatedAt      string              `json:"updated_at"`
}

type CustomerOrderAddressSnapshotDTO struct {
	SnapshotQuality  string   `json:"snapshot_quality"`
	ContactName      string   `json:"contact_name"`
	ContactPhone     string   `json:"contact_phone"`
	Province         string   `json:"province"`
	City             string   `json:"city"`
	CityCode         *string  `json:"city_code"`
	District         string   `json:"district"`
	DistrictCode     *string  `json:"district_code"`
	AddressDetail    string   `json:"address_detail"`
	Doorplate        *string  `json:"doorplate"`
	POIID            *string  `json:"poi_id"`
	FormattedAddress *string  `json:"formatted_address"`
	Latitude         *float64 `json:"latitude"`
	Longitude        *float64 `json:"longitude"`
	CoordinateSystem *string  `json:"coordinate_system"`
	LocationSource   *string  `json:"location_source"`
	GeocodeProvider  *string  `json:"geocode_provider"`
	GeocodeStatus    *string  `json:"geocode_status"`
	AddressVersion   uint32   `json:"address_version"`
}

type OrderDeliveryPolicySummaryDTO struct {
	Code     string `json:"code"`
	Version  uint32 `json:"version"`
	Title    string `json:"title"`
	Summary  string `json:"summary"`
	TermsURL string `json:"terms_url,omitempty"`
}

type OrderDeliveryPromiseDTO struct {
	SchemaVersion               uint32                         `json:"schema_version,omitempty"`
	ServiceAreaVersion          uint32                         `json:"service_area_version,omitempty"`
	SelectionSource             string                         `json:"selection_source,omitempty"`
	DeliveryFeeAmount           int64                          `json:"delivery_fee_amount"`
	FreeDeliveryThresholdAmount *int64                         `json:"free_delivery_threshold_amount"`
	ETAMinMinutes               uint16                         `json:"eta_min_minutes"`
	ETAMaxMinutes               uint16                         `json:"eta_max_minutes"`
	OvertimePolicyCode          string                         `json:"overtime_policy_code,omitempty"`
	Policy                      *OrderDeliveryPolicySummaryDTO `json:"policy,omitempty"`
	RouteDistanceM              *uint64                        `json:"route_distance_m"`
	RouteDurationSeconds        *uint64                        `json:"route_duration_seconds"`
	RouteSource                 string                         `json:"route_source,omitempty"`
	Confirmed                   bool                           `json:"confirmed"`
	ResolvedAt                  string                         `json:"resolved_at,omitempty"`
}

type OrderComplianceSummaryDTO struct {
	AgeRestricted     bool   `json:"age_restricted"`
	Status            string `json:"status"`
	PolicyVersion     string `json:"policy_version,omitempty"`
	VerificationLevel string `json:"verification_level,omitempty"`
	CheckedAt         string `json:"checked_at,omitempty"`
}

type OrderPaymentSummaryDTO struct {
	PaymentNo      string `json:"payment_no"`
	Status         string `json:"status"`
	Amount         int64  `json:"amount"`
	Currency       string `json:"currency"`
	RefundedAmount int64  `json:"refunded_amount"`
	Channel        string `json:"channel,omitempty"`
	Provider       string `json:"provider,omitempty"`
	ExpiresAt      string `json:"expires_at,omitempty"`
	PaidAt         string `json:"paid_at,omitempty"`
}

type OrderDetailDTO struct {
	ID                string                          `json:"id"`
	OrderNo           string                          `json:"order_no"`
	ShopID            string                          `json:"shop_id"`
	Status            string                          `json:"status"`
	PayStatus         string                          `json:"pay_status"`
	DeliveryStatus    string                          `json:"delivery_status"`
	PayableAmount     int64                           `json:"payable_amount"`
	ShopSummary       OrderShopSummaryDTO             `json:"shop_summary"`
	ItemSummary       OrderItemSummaryDTO             `json:"item_summary"`
	ItemKindCount     int                             `json:"item_kind_count"`
	TotalQuantity     int                             `json:"total_quantity"`
	CreatedAt         string                          `json:"created_at"`
	UpdatedAt         string                          `json:"updated_at"`
	Items             []OrderItemDTO                  `json:"items"`
	AddressSnapshot   CustomerOrderAddressSnapshotDTO `json:"address_snapshot"`
	Remark            string                          `json:"remark"`
	DeliveryPromise   *OrderDeliveryPromiseDTO        `json:"delivery_promise"`
	ComplianceSummary OrderComplianceSummaryDTO       `json:"compliance_summary"`
	GoodsAmount       int64                           `json:"goods_amount"`
	DiscountAmount    int64                           `json:"discount_amount"`
	DeliveryFeeAmount int64                           `json:"delivery_fee_amount"`
	PaidAmount        int64                           `json:"paid_amount"`
	CancelSource      string                          `json:"cancel_source,omitempty"`
	CancelReasonCode  string                          `json:"cancel_reason_code,omitempty"`
	Version           int                             `json:"version"`
	ExpiresAt         string                          `json:"expires_at,omitempty"`
	PaidAt            string                          `json:"paid_at,omitempty"`
	CancelledAt       string                          `json:"cancelled_at,omitempty"`
	CompletedAt       string                          `json:"completed_at,omitempty"`
	PaymentSummary    *OrderPaymentSummaryDTO         `json:"payment_summary"`
}

type PaymentDTO struct {
	ID              string         `json:"id"`
	PaymentNo       string         `json:"payment_no"`
	OrderID         string         `json:"order_id"`
	Channel         string         `json:"channel"`
	Provider        string         `json:"provider"`
	ProviderTradeNo string         `json:"provider_trade_no,omitempty"`
	Status          string         `json:"status"`
	ProviderStatus  string         `json:"provider_status,omitempty"`
	Amount          int64          `json:"amount"`
	Currency        string         `json:"currency"`
	ClientPayload   map[string]any `json:"client_payload,omitempty"`
	ExpiresAt       string         `json:"expires_at,omitempty"`
	PaidAt          string         `json:"paid_at,omitempty"`
	RefundedAmount  int64          `json:"refunded_amount"`
}
