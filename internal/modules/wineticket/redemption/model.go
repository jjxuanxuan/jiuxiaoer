package redemption

import (
	"time"

	"gorm.io/datatypes"
)

type DeliveryTimeSlot struct {
	ID             uint64
	ShopID         uint64
	ServiceDate    time.Time
	StartTime      string
	EndTime        string
	CutoffAt       time.Time
	CapacityOrders uint
	ReservedOrders uint
	Status         string
	ActiveSlotKey  *uint `gorm:"->"`
	Version        uint
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CreatedBy      *uint64
	UpdatedBy      *uint64
}

func (DeliveryTimeSlot) TableName() string { return "delivery_time_slots" }

type Redemption struct {
	ID                       uint64
	RedemptionNo             string
	CustomerID               uint64
	IssuerMerchantID         uint64
	ProductID                uint64
	ShopID                   uint64
	ShopProductID            uint64
	DeliveryTimeSlotID       uint64
	OrderID                  uint64
	Quantity                 uint
	AddressID                uint64
	AddressVersion           uint
	AddressSnapshot          datatypes.JSON
	DeliveryTimeSlotSnapshot datatypes.JSON
	ProductSnapshot          datatypes.JSON
	Status                   string
	Version                  uint
	ScheduledStartAt         time.Time
	ScheduledEndAt           time.Time
	NotBeforeAt              time.Time
	PickedUpAt               *time.Time
	CompletedAt              *time.Time
	CancelledAt              *time.Time
	RestoredAt               *time.Time
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

func (Redemption) TableName() string { return "wine_ticket_redemptions" }

type RedemptionAllocation struct {
	ID              uint64
	RedemptionID    uint64
	LotID           uint64
	Quantity        uint
	SourceExpiresAt time.Time
	Status          string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (RedemptionAllocation) TableName() string { return "wine_ticket_redemption_allocations" }

type activeRenewalGuard struct {
	ID    uint64
	LotID uint64
}

func (activeRenewalGuard) TableName() string { return "wine_ticket_renewals" }

const (
	RedemptionStatusScheduled        = "scheduled"
	RedemptionStatusAssigned         = "assigned"
	RedemptionStatusPickedUp         = "picked_up"
	RedemptionStatusDelivered        = "delivered"
	RedemptionStatusCancelled        = "cancelled"
	RedemptionStatusReturnInProgress = "return_in_progress"
	RedemptionStatusRestored         = "restored"
	RedemptionStatusException        = "exception"

	RedemptionAllocationStatusHeld     = "held"
	RedemptionAllocationStatusConsumed = "consumed"
	RedemptionAllocationStatusRestored = "restored"

	TransactionTypeRedemptionHold    = "redemption_hold"
	TransactionTypeRedemptionRestore = "redemption_restore"
	TransactionTypeExpiry            = "expiry"

	redemptionOrderType      = "wine_ticket_redemption"
	redemptionSettlementMode = "wine_ticket"
	redemptionPayStatus      = "not_required"
	redemptionQuantityMax    = uint(1000)
	redemptionRemarkRuneMax  = 200
	redemptionDispatchLead   = 30 * time.Minute
)

type redemptionAddressRecord struct {
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
	CoordinateSystem string
	LocationSource   string
	GeocodeProvider  *string
	GeocodeStatus    string
	GeocodedAt       *time.Time
	Version          uint
	DeletedAt        *time.Time
}

func (redemptionAddressRecord) TableName() string { return "customer_addresses" }

type redemptionAddressSnapshot struct {
	SchemaVersion    int      `json:"schema_version"`
	AddressID        string   `json:"address_id"`
	AddressVersion   uint     `json:"address_version"`
	ContactName      string   `json:"contact_name"`
	ContactPhone     string   `json:"contact_phone"`
	Province         string   `json:"province"`
	City             string   `json:"city"`
	CityCode         string   `json:"city_code"`
	District         string   `json:"district"`
	DistrictCode     string   `json:"district_code"`
	AddressDetail    string   `json:"address_detail"`
	Doorplate        *string  `json:"doorplate,omitempty"`
	POIID            *string  `json:"poi_id,omitempty"`
	FormattedAddress *string  `json:"formatted_address,omitempty"`
	Latitude         *float64 `json:"latitude,omitempty"`
	Longitude        *float64 `json:"longitude,omitempty"`
	CoordinateSystem string   `json:"coordinate_system"`
	LocationSource   string   `json:"location_source"`
	GeocodeProvider  *string  `json:"geocode_provider,omitempty"`
	GeocodeStatus    string   `json:"geocode_status"`
}

type redemptionSlotSnapshot struct {
	SchemaVersion             int    `json:"schema_version"`
	SlotID                    string `json:"slot_id"`
	SlotVersion               uint   `json:"slot_version"`
	ShopID                    string `json:"shop_id"`
	ShopName                  string `json:"shop_name"`
	IssuerMerchantID          string `json:"issuer_merchant_id"`
	IssuerMerchantDisplayName string `json:"issuer_merchant_display_name"`
	ScheduledStartAt          string `json:"scheduled_start_at"`
	ScheduledEndAt            string `json:"scheduled_end_at"`
	CutoffAt                  string `json:"cutoff_at"`
}

type redemptionProductSnapshot struct {
	SchemaVersion int     `json:"schema_version"`
	ProductID     string  `json:"product_id"`
	Name          string  `json:"name"`
	BrandName     *string `json:"brand_name,omitempty"`
	Spec          *string `json:"spec,omitempty"`
	ImageURL      *string `json:"image_url,omitempty"`
}

type redemptionSlotRelation struct {
	SlotID               uint64    `gorm:"column:slot_id"`
	ShopID               uint64    `gorm:"column:shop_id"`
	ServiceDate          time.Time `gorm:"column:service_date"`
	StartTime            string    `gorm:"column:start_time"`
	EndTime              string    `gorm:"column:end_time"`
	CutoffAt             time.Time `gorm:"column:cutoff_at"`
	CapacityOrders       uint      `gorm:"column:capacity_orders"`
	ReservedOrders       uint      `gorm:"column:reserved_orders"`
	SlotStatus           string    `gorm:"column:slot_status"`
	SlotVersion          uint      `gorm:"column:slot_version"`
	ShopMerchantID       uint64    `gorm:"column:shop_merchant_id"`
	ShopName             string    `gorm:"column:shop_name"`
	ShopStatus           string    `gorm:"column:shop_status"`
	ShopBusinessStatus   string    `gorm:"column:shop_business_status"`
	MerchantID           uint64    `gorm:"column:merchant_id"`
	MerchantName         string    `gorm:"column:merchant_name"`
	MerchantStatus       string    `gorm:"column:merchant_status"`
	MerchantReview       string    `gorm:"column:merchant_review_status"`
	ShopProductID        uint64    `gorm:"column:shop_product_id"`
	SPMerchantID         uint64    `gorm:"column:shop_product_merchant_id"`
	SPShopID             uint64    `gorm:"column:shop_product_shop_id"`
	SPProductID          uint64    `gorm:"column:shop_product_product_id"`
	ShopProductStatus    string    `gorm:"column:shop_product_status"`
	ProductID            uint64    `gorm:"column:product_id"`
	ProductName          string    `gorm:"column:product_name"`
	ProductBrandName     *string   `gorm:"column:product_brand_name"`
	ProductSpec          *string   `gorm:"column:product_spec"`
	ProductImageURL      *string   `gorm:"column:product_image_url"`
	ProductStatus        string    `gorm:"column:product_status"`
	ProductAgeRestricted bool      `gorm:"column:product_age_restricted"`
	CategoryStatus       string    `gorm:"column:category_status"`
	StockID              uint64    `gorm:"column:stock_id"`
	StockAvailableQty    int       `gorm:"column:stock_available_qty"`
	StockReservedQty     int       `gorm:"column:stock_reserved_qty"`
	StockLockedQty       int       `gorm:"column:stock_locked_qty"`
	StockVersion         int       `gorm:"column:stock_version"`
}

type redemptionView struct {
	Redemption `gorm:"embedded"`

	OrderNo             string     `gorm:"column:order_no"`
	OrderStatus         string     `gorm:"column:order_status"`
	OrderPayStatus      string     `gorm:"column:order_pay_status"`
	OrderDeliveryStatus string     `gorm:"column:order_delivery_status"`
	OrderType           string     `gorm:"column:order_type"`
	SettlementMode      string     `gorm:"column:settlement_mode"`
	ShopName            string     `gorm:"column:shop_name"`
	ProductName         string     `gorm:"column:product_name"`
	ProductBrandName    *string    `gorm:"column:product_brand_name"`
	ProductSpec         *string    `gorm:"column:product_spec"`
	ProductImageURL     *string    `gorm:"column:product_image_url"`
	DeliveryOrderID     *uint64    `gorm:"column:delivery_order_id"`
	DeliveryStatus      *string    `gorm:"column:delivery_status"`
	RiderID             *uint64    `gorm:"column:rider_id"`
	AcceptedAt          *time.Time `gorm:"column:accepted_at"`
	DeliveryPickedUpAt  *time.Time `gorm:"column:delivery_picked_up_at"`
	DeliveryCompletedAt *time.Time `gorm:"column:delivery_completed_at"`
	DeliveryCancelledAt *time.Time `gorm:"column:delivery_cancelled_at"`
}

type redemptionAllocationView struct {
	RedemptionAllocation `gorm:"embedded"`

	LotNo        string    `gorm:"column:lot_no"`
	LotStatus    string    `gorm:"column:lot_status"`
	LotExpiresAt time.Time `gorm:"column:lot_expires_at"`
}

type PhysicalStock struct {
	ID            uint64
	ShopProductID uint64
	ShopID        uint64
	ProductID     uint64
	AvailableQty  int
	ReservedQty   int
	LockedQty     int
	Version       int
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DeletedAt     *time.Time
}

func (PhysicalStock) TableName() string { return "product_stocks" }
