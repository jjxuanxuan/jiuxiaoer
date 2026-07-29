package realtime

import (
	"time"

	"gorm.io/datatypes"
)

const (
	recipientRider    = "rider"
	recipientMerchant = "merchant"
	relayPending      = "pending"
	relayRelayed      = "relayed"
	relayExpired      = "expired"
	relayDead         = "dead"
)

type Delivery struct {
	ID              uint64
	SourceEventID   string
	SourceEventType string
	ClientEventType string
	RecipientType   string
	RecipientID     uint64
	AggregateType   string
	AggregateID     uint64
	PayloadSnapshot datatypes.JSON
	SoundKey        *string
	OccurredAt      time.Time
	ExpiresAt       time.Time
	RelayStatus     string
	RelayAttempts   uint
	NextRelayAt     time.Time
	RelayedAt       *time.Time
	LastErrorCode   *string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Delivery) TableName() string { return "realtime_deliveries" }

type Acknowledgement struct {
	ID                 uint64
	RealtimeDeliveryID uint64
	RiderID            uint64
	DeviceHash         string
	AckType            string
	ClientOccurredAt   *time.Time
	ReceivedAt         time.Time
	ErrorCode          *string
	ClientVersion      string
	Platform           string
	CreatedAt          time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Acknowledgement) TableName() string { return "realtime_acknowledgements" }

type Wakeup struct {
	DeliveryID uint64    `json:"delivery_id"`
	RiderID    uint64    `json:"rider_id"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// StoreOrderPaidEvent 是商户新订单唤醒中客户端可见的完整载荷。
// 订单详情仍只能通过限定范围的门店订单 API 获取。
type StoreOrderPaidEvent struct {
	EventID    string    `json:"event_id"`
	EventType  string    `json:"event_type,omitempty"`
	OrderID    string    `json:"order_id"`
	ShopID     string    `json:"shop_id"`
	SoundKey   string    `json:"sound_key"`
	OccurredAt time.Time `json:"occurred_at"`
}

// MerchantWakeup 包含仅在 API 实例间使用的路由元数据。
// 只有 MerchantEvent 部分会转发给 WebSocket 客户端。
type MerchantWakeup struct {
	AccountID uint64              `json:"account_id"`
	Event     StoreOrderPaidEvent `json:"event"`
}
