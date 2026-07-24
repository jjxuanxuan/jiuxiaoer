package search

import "time"

const (
	ScopeGlobal = "global"
	ScopeCity   = "city"
)

// History 为每个客户存储一个规范化关键词。
type History struct {
	ID                uint64 `gorm:"primaryKey;autoIncrement:false"`
	CustomerID        uint64 `gorm:"uniqueIndex:uk_customer_search_history_keyword"`
	Keyword           string
	NormalizedKeyword string `gorm:"uniqueIndex:uk_customer_search_history_keyword"`
	SearchCount       uint32
	LastSearchedAt    time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func (History) TableName() string { return "customer_search_histories" }

// DailyStat 是热词排行持久且匿名的事实来源。
type DailyStat struct {
	ID                uint64    `gorm:"primaryKey;autoIncrement:false"`
	StatDate          time.Time `gorm:"uniqueIndex:uk_search_keyword_daily_scope"`
	ScopeType         string    `gorm:"uniqueIndex:uk_search_keyword_daily_scope"`
	ScopeID           string    `gorm:"uniqueIndex:uk_search_keyword_daily_scope"`
	NormalizedKeyword string    `gorm:"uniqueIndex:uk_search_keyword_daily_scope"`
	DisplayKeyword    string
	SearchCount       uint64
	LastSearchedAt    time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func (DailyStat) TableName() string { return "search_keyword_daily_stats" }

type hotAggregate struct {
	NormalizedKeyword string `json:"normalized_keyword"`
	DisplayKeyword    string `json:"display_keyword"`
	SearchCount       uint64 `json:"search_count"`
}
