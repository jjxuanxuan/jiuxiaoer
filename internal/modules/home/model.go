package home

import (
	"time"

	"gorm.io/datatypes"
)

type Slot struct {
	ID          uint64
	CityCode    *string
	SlotType    string
	SlotKey     string
	Title       string
	PayloadJSON datatypes.JSON
	StartAt     *time.Time
	EndAt       *time.Time
	Status      string
	SortOrder   int
	Version     uint32
	CreatedBy   uint64
	UpdatedBy   uint64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Slot) TableName() string { return "home_slots" }

type Category struct {
	ID        uint64
	Name      string
	SortOrder int
}

type AuditLog struct {
	ID           uint64
	ActorType    string
	ActorID      uint64
	Action       string
	ResourceType string
	ResourceID   uint64
	BeforeData   datatypes.JSON
	AfterData    datatypes.JSON
	Result       string
}

// TableName 返回当前数据模型对应的数据库表名。
func (AuditLog) TableName() string { return "audit_logs" }

type OutboxEvent struct {
	ID            uint64
	EventID       string
	EventType     string
	AggregateType string
	AggregateID   uint64
	Payload       datatypes.JSON
	Status        string
	RetryCount    int
}

// TableName 返回当前数据模型对应的数据库表名。
func (OutboxEvent) TableName() string { return "outbox_events" }
