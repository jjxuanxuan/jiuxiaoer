package purchase

import (
	"encoding/json"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
)

type PurchaseCreateRequest struct {
	PackageNo string `json:"package_no"`
	Quantity  uint   `json:"quantity"`
}

// PurchaseDTO 仅包含展示投影。
// 套餐或支付快照、结算标识及数据库数字主键均不得越过客户边界。
type PurchaseDTO struct {
	PurchaseNo              string                 `json:"purchase_no"`
	PackageNo               string                 `json:"package_no"`
	PackageName             string                 `json:"package_name"`
	PackageVersion          uint                   `json:"package_version"`
	PackageSnapshotSummary  string                 `json:"package_snapshot_summary"`
	Product                 core.ProductSummaryDTO `json:"product"`
	PackageType             string                 `json:"package_type"`
	PackageQuantity         uint                   `json:"package_quantity"`
	TotalBottleQuantity     uint                   `json:"total_bottle_quantity"`
	PayableAmount           int64                  `json:"payable_amount"`
	PaidAmount              int64                  `json:"paid_amount"`
	RemainingBottleQuantity uint                   `json:"remaining_bottle_quantity"`
	HeldBottleQuantity      uint                   `json:"held_bottle_quantity"`
	LotCount                uint                   `json:"lot_count"`
	LotSummaries            []PurchaseLotDTO       `json:"lot_summaries"`
	RefundPolicySummary     string                 `json:"refund_policy_summary"`
	RefundEligible          bool                   `json:"refund_eligible"`
	RefundStatus            *string                `json:"refund_status"`
	RefundKind              *string                `json:"refund_kind"`
	SafeStatusMessage       *string                `json:"safe_status_message"`
	LastRequestID           *string                `json:"last_request_id"`
	Currency                string                 `json:"currency"`
	Status                  string                 `json:"status"`
	PaymentStatus           string                 `json:"payment_status"`
	PaymentParameters       map[string]any         `json:"payment_parameters"`
	RefundNo                *string                `json:"refund_no"`
	Version                 uint                   `json:"version"`
	PaidAt                  *string                `json:"paid_at"`
	IssuedAt                *string                `json:"issued_at"`
	CreatedAt               string                 `json:"created_at"`
	UpdatedAt               string                 `json:"updated_at"`
}

type PurchaseLotDTO struct {
	LotNo             string `json:"lot_no"`
	AvailableQuantity uint   `json:"available_quantity"`
	HeldQuantity      uint   `json:"held_quantity"`
	ExpiresAt         string `json:"expires_at"`
	Status            string `json:"status"`
}

func decodePaymentParameters(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var parameters map[string]any
	if err := json.Unmarshal(raw, &parameters); err != nil || len(parameters) == 0 {
		return nil
	}
	return parameters
}
