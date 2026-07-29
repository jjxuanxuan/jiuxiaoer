package ops

import (
	"time"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/redemption"
)

const (
	DeliveryTimeSlotStatusOpen   = "open"
	DeliveryTimeSlotStatusClosed = "closed"

	slotAdminChangedEvent = "wine_ticket.delivery_time_slot_changed"
	slotAdminMaxUint32    = uint64(1<<32 - 1)
)

// SlotAdminCreateRequest 表示完整且不可变的配送窗口。
// 服务负责生成 slot_id，以零预约量创建并开放该时段。
type SlotAdminCreateRequest struct {
	ShopID         string `json:"shop_id"`
	ServiceDate    string `json:"service_date"`
	StartTime      string `json:"start_time"`
	EndTime        string `json:"end_time"`
	CutoffAt       string `json:"cutoff_at"`
	CapacityOrders uint   `json:"capacity_orders"`
}

// SlotAdminUpdateRequest 只变更可修改的容量控制字段。
// 窗口和门店标识不会出现在该契约中。
type SlotAdminUpdateRequest struct {
	CapacityOrders  uint    `json:"capacity_orders"`
	Status          string  `json:"status"`
	CutoffAt        *string `json:"cutoff_at,omitempty"`
	ExpectedVersion uint    `json:"expected_version"`
}

type SlotAdminDTO struct {
	SlotID                    string `json:"slot_id"`
	ShopID                    string `json:"shop_id"`
	ShopName                  string `json:"shop_name"`
	IssuerMerchantID          string `json:"issuer_merchant_id"`
	IssuerMerchantDisplayName string `json:"issuer_merchant_display_name"`
	ScheduledStartAt          string `json:"scheduled_start_at"`
	ScheduledEndAt            string `json:"scheduled_end_at"`
	CutoffAt                  string `json:"cutoff_at"`
	AvailabilityStatus        string `json:"availability_status"`
	RemainingCapacity         uint   `json:"remaining_capacity"`
	Version                   uint   `json:"version"`
	ServiceDate               string `json:"service_date"`
	StartTime                 string `json:"start_time"`
	EndTime                   string `json:"end_time"`
	CapacityOrders            uint   `json:"capacity_orders"`
	ReservedOrders            uint   `json:"reserved_orders"`
	Status                    string `json:"status"`
	CreatedAt                 string `json:"created_at"`
	UpdatedAt                 string `json:"updated_at"`
}

type slotAdminCreateInput struct {
	ShopID         uint64
	ServiceDate    time.Time
	StartTime      string
	EndTime        string
	StartAt        time.Time
	EndAt          time.Time
	CutoffAt       time.Time
	CapacityOrders uint
}

type slotAdminUpdateInput struct {
	CapacityOrders  uint
	Status          string
	CutoffAt        *time.Time
	ExpectedVersion uint
}

type slotAdminListFilter struct {
	ShopID          *uint64
	ServiceDate     *time.Time
	AuthorizedShops []uint64
}

type slotAdminRecord struct {
	redemption.DeliveryTimeSlot
	ShopName     string `gorm:"column:shop_name"`
	MerchantID   uint64 `gorm:"column:merchant_id"`
	MerchantName string `gorm:"column:merchant_name"`
}

type slotAdminShop struct {
	ID             uint64
	MerchantID     uint64
	Name           string
	CityCode       *string
	Status         string
	BusinessStatus string
	DeletedAt      *time.Time
}

func (slotAdminShop) TableName() string { return "shops" }

type slotAdminMerchant struct {
	ID           uint64
	Name         string
	Status       string
	ReviewStatus string
	DeletedAt    *time.Time
}

func (slotAdminMerchant) TableName() string { return "merchants" }
