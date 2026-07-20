package search

import "time"

const (
	SourceManual  = "manual"
	SourceHistory = "history"
	SourceHot     = "hot"
)

type EventRequest struct {
	Keyword string `json:"keyword"`
	Source  string `json:"source"`
}

type HistoryDTO struct {
	ID             string    `json:"id"`
	Keyword        string    `json:"keyword"`
	LastSearchedAt time.Time `json:"last_searched_at"`
}

type HotKeywordDTO struct {
	Rank        int    `json:"rank"`
	Keyword     string `json:"keyword"`
	SourceScope string `json:"source_scope"`
	CityCode    string `json:"city_code,omitempty"`
}

type DiscoveryResponse struct {
	History     []HistoryDTO    `json:"history"`
	HotKeywords []HotKeywordDTO `json:"hot_keywords"`
	GeneratedAt time.Time       `json:"generated_at"`
}

type EventResponse struct {
	HistoryItem   HistoryDTO `json:"history_item"`
	CountedForHot bool       `json:"counted_for_hot"`
}

type ClearResponse struct {
	DeletedCount int64 `json:"deleted_count"`
}
