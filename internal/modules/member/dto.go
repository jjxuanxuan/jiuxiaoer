package member

import "jiuxiaoer-admin/backend-go/internal/pkg/pagination"

type ProfileDTO struct {
	CustomerID       string `json:"customer_id"`
	TierCode         string `json:"tier_code"`
	TierName         string `json:"tier_name"`
	NextTierCode     string `json:"next_tier_code,omitempty"`
	RuleVersion      string `json:"rule_version"`
	EvaluatedAt      string `json:"evaluated_at"`
	GrowthValue      int64  `json:"growth_value"`
	GrowthToNextTier int64  `json:"growth_to_next_tier,omitempty"`
	Version          uint32 `json:"version"`
}
type TierReq struct {
	TierCode  string         `json:"tier_code" binding:"required,oneof=normal silver gold"`
	TierName  string         `json:"tier_name" binding:"required,min=2,max=64"`
	MinGrowth int64          `json:"min_growth" binding:"min=0"`
	Benefits  map[string]any `json:"benefits"`
}
type RuleSetCreateReq struct {
	Version     string    `json:"version" binding:"required,min=1,max=32"`
	EffectiveAt string    `json:"effective_at" binding:"required"`
	Reason      string    `json:"reason" binding:"required,min=5,max=500"`
	Tiers       []TierReq `json:"tiers" binding:"required,len=3,dive"`
}
type ActivateReq struct {
	ExpectedStatus string `json:"expected_status" binding:"omitempty,oneof=draft"`
}
type TierDTO struct {
	TierCode  string         `json:"tier_code"`
	TierName  string         `json:"tier_name"`
	MinGrowth int64          `json:"min_growth"`
	Benefits  map[string]any `json:"benefits"`
}
type RuleSetDTO struct {
	ID          string    `json:"id"`
	Version     string    `json:"version"`
	Status      string    `json:"status"`
	EffectiveAt string    `json:"effective_at"`
	Reason      string    `json:"reason"`
	Tiers       []TierDTO `json:"tiers"`
	CreatedBy   string    `json:"created_by"`
	ActivatedBy string    `json:"activated_by,omitempty"`
	CreatedAt   string    `json:"created_at"`
	UpdatedAt   string    `json:"updated_at"`
}
type ListQuery struct {
	pagination.Query
	TierCode string
}
