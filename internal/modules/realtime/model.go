package realtime

import (
	"time"

	"gorm.io/datatypes"
)

const (
	recipientRider = "rider"
	relayPending   = "pending"
	relayRelayed   = "relayed"
	relayExpired   = "expired"
	relayDead      = "dead"
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
