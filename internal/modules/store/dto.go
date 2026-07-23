package store

// StoreOrderActionReq binds every merchant fulfilment transition to the
// version the client last observed. A pointer makes version zero a valid,
// explicitly supplied value while still rejecting a missing field.
type StoreOrderActionReq struct {
	ExpectedVersion *uint `json:"expected_version" binding:"required"`
}

// StoreOrderSummaryDTO 是商家订单列表的最小核单投影。列表不下发完整
// 地址、手机号、顾客 ID 或全部商品，详细履约信息必须通过对象鉴权后的详情接口获取。
type StoreOrderSummaryDTO struct {
	ID                  string                   `json:"id"`
	OrderNo             string                   `json:"order_no"`
	ShopID              string                   `json:"shop_id"`
	Status              string                   `json:"status"`
	PayStatus           string                   `json:"pay_status"`
	DeliveryStatus      string                   `json:"delivery_status"`
	PayableAmount       int64                    `json:"payable_amount"`
	ShopSummary         StoreShopSummaryDTO      `json:"shop_summary"`
	ItemSummary         StoreOrderItemSummaryDTO `json:"item_summary"`
	ItemKindCount       int                      `json:"item_kind_count"`
	TotalQuantity       int                      `json:"total_quantity"`
	AddressSummary      string                   `json:"address_summary"`
	CustomerContactMask string                   `json:"customer_contact_mask"`
	HasRemark           bool                     `json:"has_remark"`
	Version             int                      `json:"version"`
	CreatedAt           string                   `json:"created_at"`
	UpdatedAt           string                   `json:"updated_at"`
	PaidAt              string                   `json:"paid_at,omitempty"`
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

// StoreOrderDetailDTO 是商家专用订单详情投影。它与顾客订单详情分离，
// 不包含完整手机号、精确经纬度、内部支付流水或日志操作者 ID。
type StoreOrderDetailDTO struct {
	ID                  string                       `json:"id"`
	OrderNo             string                       `json:"order_no"`
	ShopID              string                       `json:"shop_id"`
	Status              string                       `json:"status"`
	PayStatus           string                       `json:"pay_status"`
	DeliveryStatus      string                       `json:"delivery_status"`
	PayableAmount       int64                        `json:"payable_amount"`
	ShopSummary         StoreShopSummaryDTO          `json:"shop_summary"`
	ItemSummary         StoreOrderItemSummaryDTO     `json:"item_summary"`
	ItemKindCount       int                          `json:"item_kind_count"`
	TotalQuantity       int                          `json:"total_quantity"`
	CreatedAt           string                       `json:"created_at"`
	UpdatedAt           string                       `json:"updated_at,omitempty"`
	Items               []OrderItemDTO               `json:"items"`
	AddressSnapshot     StoreOrderAddressSnapshotDTO `json:"address_snapshot"`
	Remark              string                       `json:"remark"`
	DeliveryPromise     *StoreDeliveryPromiseDTO     `json:"delivery_promise"`
	ComplianceSummary   StoreComplianceSummaryDTO    `json:"compliance_summary"`
	GoodsAmount         int64                        `json:"goods_amount"`
	DiscountAmount      int64                        `json:"discount_amount"`
	DeliveryFeeAmount   int64                        `json:"delivery_fee_amount"`
	PaidAmount          int64                        `json:"paid_amount"`
	CancelSource        string                       `json:"cancel_source,omitempty"`
	CancelReasonCode    string                       `json:"cancel_reason_code,omitempty"`
	Version             int                          `json:"version"`
	ExpiresAt           string                       `json:"expires_at,omitempty"`
	PaidAt              string                       `json:"paid_at,omitempty"`
	CancelledAt         string                       `json:"cancelled_at,omitempty"`
	CompletedAt         string                       `json:"completed_at,omitempty"`
	PaymentSummary      *StorePaymentSummaryDTO      `json:"payment_summary"`
	CustomerContactMask string                       `json:"customer_contact_mask"`
	DeliverySummary     *StoreDeliverySummaryDTO     `json:"delivery_summary"`
	RecentLogs          []StoreOrderLogSummaryDTO    `json:"recent_logs"`
}

type StoreShopSummaryDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type StoreOrderItemSummaryDTO struct {
	ProductID string `json:"product_id"`
	Name      string `json:"name"`
	Spec      string `json:"spec,omitempty"`
	ImageURL  string `json:"image_url,omitempty"`
	Quantity  int    `json:"quantity"`
}

// StoreOrderAddressSnapshotDTO 只保留门店履约需要的地址字段。
// 联系电话在 CustomerContactMask 中单独脱敏返回，精确位置永不进入该 DTO。
type StoreOrderAddressSnapshotDTO struct {
	SnapshotQuality  string  `json:"snapshot_quality"`
	ContactNameMask  string  `json:"contact_name_mask"`
	Province         string  `json:"province"`
	City             string  `json:"city"`
	CityCode         *string `json:"city_code,omitempty"`
	District         string  `json:"district"`
	DistrictCode     *string `json:"district_code,omitempty"`
	AddressDetail    string  `json:"address_detail"`
	Doorplate        *string `json:"doorplate,omitempty"`
	FormattedAddress *string `json:"formatted_address,omitempty"`
	AddressVersion   uint32  `json:"address_version"`
}

type StoreDeliveryPolicySummaryDTO struct {
	Code     string `json:"code"`
	Version  uint32 `json:"version"`
	Title    string `json:"title"`
	Summary  string `json:"summary"`
	TermsURL string `json:"terms_url,omitempty"`
}

type StoreDeliveryPromiseDTO struct {
	SchemaVersion               uint32                         `json:"schema_version,omitempty"`
	ServiceAreaVersion          uint32                         `json:"service_area_version,omitempty"`
	SelectionSource             string                         `json:"selection_source,omitempty"`
	DeliveryFeeAmount           int64                          `json:"delivery_fee_amount"`
	FreeDeliveryThresholdAmount *int64                         `json:"free_delivery_threshold_amount,omitempty"`
	ETAMinMinutes               uint16                         `json:"eta_min_minutes"`
	ETAMaxMinutes               uint16                         `json:"eta_max_minutes"`
	OvertimePolicyCode          string                         `json:"overtime_policy_code,omitempty"`
	Policy                      *StoreDeliveryPolicySummaryDTO `json:"policy,omitempty"`
	RouteDistanceM              *uint64                        `json:"route_distance_m,omitempty"`
	RouteDurationSeconds        *uint64                        `json:"route_duration_seconds,omitempty"`
	RouteSource                 string                         `json:"route_source,omitempty"`
	Confirmed                   bool                           `json:"confirmed"`
	ResolvedAt                  string                         `json:"resolved_at,omitempty"`
}

type StoreComplianceSummaryDTO struct {
	AgeRestricted     bool   `json:"age_restricted"`
	Status            string `json:"status"`
	PolicyVersion     string `json:"policy_version,omitempty"`
	VerificationLevel string `json:"verification_level,omitempty"`
	CheckedAt         string `json:"checked_at,omitempty"`
}

type StorePaymentSummaryDTO struct {
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

type StoreDeliverySummaryDTO struct {
	DeliveryOrderID   string `json:"delivery_order_id"`
	RiderID           string `json:"rider_id,omitempty"`
	Status            string `json:"status"`
	PickupReadyStatus string `json:"pickup_ready_status"`
	AssignmentVersion uint   `json:"assignment_version"`
}

type StoreOrderLogSummaryDTO struct {
	ID         string `json:"id"`
	Action     string `json:"action"`
	ActorType  string `json:"actor_type"`
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
	CreatedAt  string `json:"created_at"`
}

type BusinessStatusReq struct {
	BusinessStatus string `json:"business_status" binding:"required,oneof=open closed resting"`
}

type ShopProductCreateReq struct {
	ShopID              string `json:"shop_id" binding:"required"`
	ProductID           string `json:"product_id" binding:"required"`
	SalePriceAmount     int64  `json:"sale_price_amount" binding:"required,min=1"`
	Status              string `json:"status" binding:"omitempty,oneof=draft on_sale off_sale"`
	SortOrder           int    `json:"sort_order"`
	InitialAvailableQty int    `json:"initial_available_qty" binding:"min=0"`
}

type ShopProductUpdateReq struct {
	SalePriceAmount *int64  `json:"sale_price_amount" binding:"omitempty,min=1"`
	Status          *string `json:"status" binding:"omitempty,oneof=draft on_sale off_sale"`
	SortOrder       *int    `json:"sort_order"`
}

type StockAdjustReq struct {
	QuantityDelta int    `json:"quantity_delta" binding:"required"`
	Reason        string `json:"reason" binding:"max=255"`
}

type ShopProductDTO struct {
	ID                  string `json:"id"`
	ShopProductID       string `json:"shop_product_id"`
	MerchantID          string `json:"merchant_id"`
	ShopID              string `json:"shop_id"`
	ProductID           string `json:"product_id"`
	CategoryID          string `json:"category_id"`
	Name                string `json:"name"`
	BrandName           string `json:"brand_name,omitempty"`
	Spec                string `json:"spec,omitempty"`
	ImageURL            string `json:"image_url,omitempty"`
	SalePriceAmount     int64  `json:"sale_price_amount"`
	OriginalPriceAmount int64  `json:"original_price_amount"`
	Status              string `json:"status"`
	SortOrder           int    `json:"sort_order"`
	AvailableQty        int    `json:"available_qty"`
	ReservedQty         int    `json:"reserved_qty"`
	LockedQty           int    `json:"locked_qty"`
	TotalQty            int    `json:"total_qty"`
	LowStockThreshold   int    `json:"low_stock_threshold"`
	LowStock            bool   `json:"low_stock"`
	Version             int    `json:"version"`
	UpdatedAt           string `json:"updated_at"`
	AgeRestricted       bool   `json:"age_restricted"`
}
