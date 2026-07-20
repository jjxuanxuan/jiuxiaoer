package member

import (
	"gorm.io/datatypes"
	"time"
)

type Profile struct {
	CustomerID      uint64
	CurrentGrowth   int64
	TierCode        string
	RuleSetID       uint64
	TierEffectiveAt time.Time
	Version         uint32
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Profile) TableName() string { return "member_profiles" }

type RuleSet struct {
	ID                      uint64
	Version, Status, Reason string
	EffectiveAt             time.Time
	CreatedBy               uint64
	ActivatedBy             *uint64
	ActivatedAt             *time.Time
	CreatedAt, UpdatedAt    time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (RuleSet) TableName() string { return "member_tier_rule_sets" }

type Rule struct {
	ID, RuleSetID      uint64
	TierCode, TierName string
	MinGrowth          int64
	SortOrder          int
	BenefitsSnapshot   datatypes.JSON
	CreatedAt          time.Time
}

// TableName 返回当前数据模型对应的数据库表名。
func (Rule) TableName() string { return "member_tier_rules" }
