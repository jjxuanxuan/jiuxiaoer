package asset

import "time"

type Command struct {
	CustomerID     uint64
	AssetType      string
	Unit           string
	Amount         int64
	SourceType     string
	SourceID       string
	Action         string
	IdempotencyKey string
	ActorType      string
	ActorID        uint64
	OccurredAt     time.Time
	ExpiresAt      *time.Time
	Metadata       map[string]any
}

type FreezeCommand struct {
	Command
	ReservationKey string
	HoldExpiresAt  *time.Time
}

type HoldCommand struct {
	HoldID         uint64
	Amount         int64
	SourceType     string
	SourceID       string
	Action         string
	IdempotencyKey string
	ActorType      string
	ActorID        uint64
}

type ReverseCommand struct {
	OriginalTransactionID uint64
	Amount                int64
	SourceType            string
	SourceID              string
	IdempotencyKey        string
	ActorType             string
	ActorID               uint64
}

type TransactionDTO struct {
	ID             string         `json:"id"`
	TransactionNo  string         `json:"transaction_no"`
	CustomerID     string         `json:"customer_id,omitempty"`
	AssetType      string         `json:"asset_type"`
	Unit           string         `json:"unit"`
	Action         string         `json:"action"`
	SourceType     string         `json:"source_type"`
	SourceID       string         `json:"source_id,omitempty"`
	Amount         int64          `json:"amount"`
	AvailableDelta int64          `json:"available_delta,omitempty"`
	FrozenDelta    int64          `json:"frozen_delta,omitempty"`
	BalanceAfter   int64          `json:"balance_after,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	OccurredAt     string         `json:"occurred_at"`
	PostedAt       string         `json:"posted_at"`
}

type AssetSummaryDTO struct {
	AssetType          string `json:"asset_type"`
	Unit               string `json:"unit"`
	AvailableAmount    int64  `json:"available_amount"`
	FrozenAmount       int64  `json:"frozen_amount"`
	NextExpiringAmount int64  `json:"next_expiring_amount"`
	NextExpiresAt      string `json:"next_expires_at,omitempty"`
	UpdatedAt          string `json:"updated_at,omitempty"`
}

type HoldDTO struct {
	ID              string `json:"id"`
	HoldNo          string `json:"hold_no"`
	ReservationKey  string `json:"reservation_key"`
	AssetType       string `json:"asset_type"`
	Unit            string `json:"unit"`
	Status          string `json:"status"`
	OriginalAmount  int64  `json:"original_amount"`
	CommittedAmount int64  `json:"committed_amount"`
	ReleasedAmount  int64  `json:"released_amount"`
	Version         uint32 `json:"version"`
}

type AdjustmentCreateReq struct {
	CustomerID   string   `json:"customer_id" binding:"required"`
	AssetType    string   `json:"asset_type" binding:"required,oneof=growth_value wine_coin balance"`
	Direction    string   `json:"direction" binding:"required,oneof=credit debit"`
	Amount       int64    `json:"amount" binding:"required,min=1"`
	ReasonCode   string   `json:"reason_code" binding:"required,min=2,max=64"`
	Reason       string   `json:"reason" binding:"required,min=5,max=500"`
	EvidenceRefs []string `json:"evidence_refs" binding:"max=9,dive,max=512"`
}

type AdjustmentReviewReq struct {
	Version uint32 `json:"version" binding:"required,min=1"`
	Remark  string `json:"remark" binding:"max=500"`
}

type AdjustmentDTO struct {
	ID                 string `json:"id"`
	AdjustmentNo       string `json:"adjustment_no"`
	CustomerID         string `json:"customer_id"`
	AssetType          string `json:"asset_type"`
	Unit               string `json:"unit"`
	Direction          string `json:"direction"`
	ReasonCode         string `json:"reason_code"`
	Reason             string `json:"reason"`
	Status             string `json:"status"`
	Amount             int64  `json:"amount"`
	CreatedBy          string `json:"created_by"`
	ReviewedBy         string `json:"reviewed_by,omitempty"`
	AssetTransactionID string `json:"asset_transaction_id,omitempty"`
	Version            uint32 `json:"version"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
}

type ReconcileReq struct {
	Scope   string `json:"scope" binding:"required,oneof=all customer account transaction"`
	ScopeID string `json:"scope_id" binding:"max=128"`
	DryRun  *bool  `json:"dry_run"`
}

type ReconciliationDTO struct {
	ID              string `json:"id"`
	JobNo           string `json:"job_no"`
	Scope           string `json:"scope"`
	ScopeID         string `json:"scope_id,omitempty"`
	Mode            string `json:"mode"`
	Status          string `json:"status"`
	ScannedCount    uint64 `json:"scanned_count"`
	DifferenceCount uint64 `json:"difference_count"`
	CriticalCount   uint64 `json:"critical_count"`
	CreatedAt       string `json:"created_at"`
	FinishedAt      string `json:"finished_at,omitempty"`
}
