package ops

import (
	"gorm.io/datatypes"
	"time"
)

type Delivery struct {
	ID                  uint64
	OrderID             uint64
	ShopID              uint64
	RiderID             *uint64
	Status              string
	AssignmentVersion   uint
	CompletedAt         *time.Time
	CompletedVerifiedAt *time.Time
	UpdatedAt           time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Delivery) TableName() string { return "delivery_orders" }

type Order struct {
	ID             uint64
	Status         string
	DeliveryStatus string
	CompletedAt    *time.Time
	Version        uint
}

// TableName 返回当前数据模型对应的数据库表名。
func (Order) TableName() string { return "orders" }

type Assignment struct {
	ID              uint64
	DeliveryOrderID uint64
	FromRiderID     *uint64
	ToRiderID       uint64
	AssignmentType  string
	Status          string
	ReasonCode      *string
	Reason          *string
	ActorType       string
	ActorID         uint64
	VersionBefore   uint
	VersionAfter    uint
	RequestID       *string
	CreatedAt       time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Assignment) TableName() string { return "delivery_assignments" }

type AssignmentReq struct {
	RiderID         string `json:"rider_id" binding:"required"`
	ReasonCode      string `json:"reason_code" binding:"required,max=64"`
	Reason          string `json:"reason" binding:"required,min=2,max=255"`
	ExpectedVersion uint   `json:"expected_version" binding:"required,min=1"`
}
type ForceCompleteReq struct {
	ReasonCode      string `json:"reason_code" binding:"required,max=64"`
	Reason          string `json:"reason" binding:"required,min=2,max=255"`
	ExpectedVersion uint   `json:"expected_version" binding:"required,min=1"`
}
type CancelReq struct {
	ReasonCode      string `json:"reason_code" binding:"required,max=64"`
	Reason          string `json:"reason" binding:"required,min=2,max=255"`
	ExpectedVersion uint   `json:"expected_version" binding:"required,min=1"`
}
type DeliveryDTO struct {
	ID                string `json:"id"`
	OrderID           string `json:"order_id"`
	RiderID           string `json:"rider_id,omitempty"`
	Status            string `json:"status"`
	AssignmentVersion uint   `json:"assignment_version"`
	CompletedAt       string `json:"completed_at,omitempty"`
}
type AssignmentDTO struct {
	ID              string `json:"id"`
	DeliveryOrderID string `json:"delivery_order_id"`
	FromRiderID     string `json:"from_rider_id,omitempty"`
	ToRiderID       string `json:"to_rider_id"`
	AssignmentType  string `json:"assignment_type"`
	Status          string `json:"status"`
	ReasonCode      string `json:"reason_code,omitempty"`
	Reason          string `json:"reason,omitempty"`
	ActorID         string `json:"actor_id"`
	VersionBefore   uint   `json:"version_before"`
	VersionAfter    uint   `json:"version_after"`
	CreatedAt       string `json:"created_at"`
}
type Outbox struct {
	ID            uint64
	EventID       string
	EventType     string
	AggregateType string
	AggregateID   uint64
	Payload       datatypes.JSON
	Status        string
	RequestID     *string
}

// TableName 返回当前数据模型对应的数据库表名。
func (Outbox) TableName() string { return "outbox_events" }
