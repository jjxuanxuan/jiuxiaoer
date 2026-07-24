package delivery

type DeliveryOrderDTO struct {
	ViewType            string                       `json:"view_type"`
	ID                  string                       `json:"id"`
	OrderID             string                       `json:"order_id"`
	ShopID              string                       `json:"shop_id"`
	RiderID             string                       `json:"rider_id,omitempty"`
	Status              string                       `json:"status"`
	AssignmentVersion   uint                         `json:"assignment_version"`
	DispatchStatus      string                       `json:"dispatch_status"`
	PickupReadyStatus   string                       `json:"pickup_ready_status"`
	PickupReadyAt       string                       `json:"pickup_ready_at,omitempty"`
	PickupSnapshot      DeliveryPickupSnapshotDTO    `json:"pickup_snapshot"`
	RecipientSnapshot   DeliveryRecipientSnapshotDTO `json:"recipient_snapshot"`
	CreatedAt           string                       `json:"created_at,omitempty"`
	AcceptedAt          string                       `json:"accepted_at,omitempty"`
	PickedUpAt          string                       `json:"picked_up_at,omitempty"`
	PickedUpVerifiedAt  string                       `json:"picked_up_verified_at,omitempty"`
	StartedAt           string                       `json:"started_at,omitempty"`
	CompletedAt         string                       `json:"completed_at,omitempty"`
	CompletedVerifiedAt string                       `json:"completed_verified_at,omitempty"`
	ShopName            string                       `json:"shop_name,omitempty"`
	DestinationDistrict string                       `json:"destination_district,omitempty"`
	ItemCount           int                          `json:"item_count,omitempty"`
	PickupDistanceM     uint                         `json:"pickup_distance_m,omitempty"`
	GrabExpiresAt       string                       `json:"grab_expires_at,omitempty"`
}

// CandidateDeliverySummaryDTO 刻意比已分配配送的信息范围更窄。
// 它包含判断是否抢单所需的足够信息，但不含精确取货或收件人快照。
type CandidateDeliverySummaryDTO struct {
	ViewType            string `json:"view_type"`
	ID                  string `json:"id"`
	OrderID             string `json:"order_id"`
	ShopID              string `json:"shop_id"`
	ShopName            string `json:"shop_name"`
	DestinationDistrict string `json:"destination_district"`
	ItemCount           int    `json:"item_count"`
	PickupDistanceM     uint   `json:"pickup_distance_m"`
	GrabExpiresAt       string `json:"grab_expires_at"`
	AssignmentVersion   uint   `json:"assignment_version"`
}

// AssignedDeliverySummaryDTO 仅在列表查询检查骑手所有权后返回。
// 其强类型快照无法携带历史 JSON 中存储的验证信息或服务商秘密。
type AssignedDeliverySummaryDTO struct {
	ViewType          string                       `json:"view_type"`
	ID                string                       `json:"id"`
	OrderID           string                       `json:"order_id"`
	ShopID            string                       `json:"shop_id"`
	RiderID           string                       `json:"rider_id"`
	Status            string                       `json:"status"`
	AssignmentVersion uint                         `json:"assignment_version"`
	DispatchStatus    string                       `json:"dispatch_status"`
	PickupReadyStatus string                       `json:"pickup_ready_status"`
	PickupSnapshot    DeliveryPickupSnapshotDTO    `json:"pickup_snapshot"`
	RecipientSnapshot DeliveryRecipientSnapshotDTO `json:"recipient_snapshot"`
	PickupReadyAt     string                       `json:"pickup_ready_at,omitempty"`
	AcceptedAt        string                       `json:"accepted_at,omitempty"`
	PickedUpAt        string                       `json:"picked_up_at,omitempty"`
	StartedAt         string                       `json:"started_at,omitempty"`
	CompletedAt       string                       `json:"completed_at,omitempty"`
	CreatedAt         string                       `json:"created_at"`
}

// DeliveryDetailDTO 是已分配骑手的履约视图。只有仓储层在同一读取事务中
// 验证当前有效分配后，才会返回敏感联系人和地址数据。
type DeliveryDetailDTO struct {
	ID                  string                        `json:"id"`
	OrderID             string                        `json:"order_id"`
	ShopID              string                        `json:"shop_id"`
	RiderID             string                        `json:"rider_id"`
	Status              string                        `json:"status"`
	Version             uint                          `json:"version"`
	AssignmentVersion   uint                          `json:"assignment_version"`
	DispatchStatus      string                        `json:"dispatch_status"`
	PickupReadyStatus   string                        `json:"pickup_ready_status"`
	PickupSnapshot      *DeliveryPickupSnapshotDTO    `json:"pickup_snapshot"`
	RecipientSnapshot   *DeliveryRecipientSnapshotDTO `json:"recipient_snapshot"`
	PickupContact       DeliveryContactDTO            `json:"pickup_contact"`
	RecipientContact    DeliveryContactDTO            `json:"recipient_contact"`
	Order               DeliveryDetailOrderDTO        `json:"order"`
	Shop                DeliveryDetailShopDTO         `json:"shop"`
	Items               []DeliveryDetailItemDTO       `json:"items"`
	CreatedAt           string                        `json:"created_at,omitempty"`
	UpdatedAt           string                        `json:"updated_at,omitempty"`
	PickupReadyAt       string                        `json:"pickup_ready_at,omitempty"`
	AcceptedAt          string                        `json:"accepted_at,omitempty"`
	PickedUpAt          string                        `json:"picked_up_at,omitempty"`
	PickedUpVerifiedAt  string                        `json:"picked_up_verified_at,omitempty"`
	StartedAt           string                        `json:"started_at,omitempty"`
	CompletedAt         string                        `json:"completed_at,omitempty"`
	CompletedVerifiedAt string                        `json:"completed_verified_at,omitempty"`
	CancelledAt         string                        `json:"cancelled_at,omitempty"`
}

type DeliveryDetailOrderDTO struct {
	ID                string `json:"id"`
	OrderNo           string `json:"order_no"`
	Status            string `json:"status"`
	PayStatus         string `json:"pay_status"`
	DeliveryStatus    string `json:"delivery_status"`
	GoodsAmount       int64  `json:"goods_amount"`
	DiscountAmount    int64  `json:"discount_amount"`
	DeliveryFeeAmount int64  `json:"delivery_fee_amount"`
	PayableAmount     int64  `json:"payable_amount"`
	PaidAmount        int64  `json:"paid_amount"`
	Remark            string `json:"remark,omitempty"`
	Version           int    `json:"version"`
	PaidAt            string `json:"paid_at,omitempty"`
	CancelledAt       string `json:"cancelled_at,omitempty"`
	CompletedAt       string `json:"completed_at,omitempty"`
	CreatedAt         string `json:"created_at,omitempty"`
	UpdatedAt         string `json:"updated_at,omitempty"`
}

type DeliveryDetailShopDTO struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Phone            string   `json:"phone,omitempty"`
	Province         string   `json:"province,omitempty"`
	City             string   `json:"city"`
	District         string   `json:"district"`
	Address          string   `json:"address"`
	Latitude         *float64 `json:"latitude,omitempty"`
	Longitude        *float64 `json:"longitude,omitempty"`
	CoordinateSystem string   `json:"coordinate_system,omitempty"`
	Status           string   `json:"status"`
	BusinessStatus   string   `json:"business_status"`
}

type DeliveryDetailItemDTO struct {
	ID              string                     `json:"id"`
	ShopProductID   string                     `json:"shop_product_id"`
	ProductID       string                     `json:"product_id"`
	ProductSnapshot DeliveryProductSnapshotDTO `json:"product_snapshot"`
	Quantity        int                        `json:"quantity"`
	SalePriceAmount int64                      `json:"sale_price_amount"`
	TotalAmount     int64                      `json:"total_amount"`
}

type DeliveryContactDTO struct {
	Name             string `json:"name,omitempty"`
	Phone            string `json:"phone,omitempty"`
	FormattedAddress string `json:"formatted_address,omitempty"`
}

type DeliveryPickupSnapshotDTO struct {
	ShopID           string   `json:"shop_id,omitempty"`
	Name             string   `json:"name,omitempty"`
	Phone            string   `json:"phone,omitempty"`
	District         string   `json:"district,omitempty"`
	Address          string   `json:"address,omitempty"`
	Latitude         *float64 `json:"latitude,omitempty"`
	Longitude        *float64 `json:"longitude,omitempty"`
	CoordinateSystem string   `json:"coordinate_system,omitempty"`
}

type DeliveryRecipientSnapshotDTO struct {
	ContactName      string   `json:"contact_name,omitempty"`
	ContactPhone     string   `json:"contact_phone,omitempty"`
	Province         string   `json:"province,omitempty"`
	City             string   `json:"city,omitempty"`
	CityCode         string   `json:"city_code,omitempty"`
	District         string   `json:"district,omitempty"`
	DistrictCode     string   `json:"district_code,omitempty"`
	AddressDetail    string   `json:"address_detail,omitempty"`
	Doorplate        string   `json:"doorplate,omitempty"`
	POIID            string   `json:"poi_id,omitempty"`
	FormattedAddress string   `json:"formatted_address,omitempty"`
	Latitude         *float64 `json:"latitude,omitempty"`
	Longitude        *float64 `json:"longitude,omitempty"`
	CoordinateSystem string   `json:"coordinate_system,omitempty"`
	LocationSource   string   `json:"location_source,omitempty"`
	GeocodeProvider  string   `json:"geocode_provider,omitempty"`
	GeocodeStatus    string   `json:"geocode_status,omitempty"`
	AddressVersion   uint32   `json:"address_version,omitempty"`
}

type DeliveryReturnPolicySnapshotDTO struct {
	Eligible              bool   `json:"eligible"`
	PolicyCode            string `json:"policy_code"`
	PolicyVersion         string `json:"policy_version"`
	SealedPackageRequired bool   `json:"sealed_package_required"`
}

type DeliveryProductSnapshotDTO struct {
	Name          string                          `json:"name"`
	BrandName     string                          `json:"brand_name"`
	Spec          string                          `json:"spec"`
	ImageURL      string                          `json:"image_url"`
	AgeRestricted bool                            `json:"age_restricted"`
	ReturnPolicy  DeliveryReturnPolicySnapshotDTO `json:"return_policy"`
}
