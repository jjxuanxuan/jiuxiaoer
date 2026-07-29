package catalog

import (
	"fmt"
	"strconv"
	"time"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
)

// 输入策略字段使用指针，因为零值和 false 都可能是合法取值。
// 这样 JSON Schema 中的 required 才有实际含义，不会把字段缺失静默解释为显式零值。
type RefundPolicyInput struct {
	SchemaVersion    *int   `json:"schema_version"`
	Enabled          *bool  `json:"enabled"`
	WindowHours      *int   `json:"window_hours"`
	RequireNeverUsed *bool  `json:"require_never_used"`
	FeeAmount        *int64 `json:"fee_amount"`
}

type RenewalPolicyInput struct {
	SchemaVersion *int   `json:"schema_version"`
	Enabled       *bool  `json:"enabled"`
	ExtensionDays *int   `json:"extension_days"`
	MaxCount      *int   `json:"max_count"`
	GraceDays     *int   `json:"grace_days"`
	FeeAmount     *int64 `json:"fee_amount"`
}

type DeliveryPolicyInput struct {
	SchemaVersion       *int  `json:"schema_version"`
	DeliveryFeeIncluded *bool `json:"delivery_fee_included"`
	DispatchLeadMinutes *int  `json:"dispatch_lead_minutes"`
}

type PackageWriteRequest struct {
	PackageCode             string              `json:"package_code"`
	IssuerMerchantID        string              `json:"issuer_merchant_id"`
	SettlementShopID        string              `json:"settlement_shop_id"`
	SettlementShopProductID string              `json:"settlement_shop_product_id"`
	ProductID               string              `json:"product_id"`
	RedeemCityCode          string              `json:"redeem_city_code"`
	Name                    string              `json:"name"`
	Subtitle                *string             `json:"subtitle"`
	CoverImageURL           *string             `json:"cover_image_url"`
	PackageType             string              `json:"package_type"`
	BottleQuantity          uint                `json:"bottle_quantity"`
	SalePriceAmount         int64               `json:"sale_price_amount"`
	MinPurchaseQuantity     uint                `json:"min_purchase_quantity"`
	MaxPurchaseQuantity     uint                `json:"max_purchase_quantity"`
	ValidityDays            uint                `json:"validity_days"`
	SaleStartAt             *string             `json:"sale_start_at"`
	SaleEndAt               *string             `json:"sale_end_at"`
	PerCustomerLimit        *uint               `json:"per_customer_limit"`
	RefundPolicy            RefundPolicyInput   `json:"refund_policy"`
	RenewalPolicy           RenewalPolicyInput  `json:"renewal_policy"`
	DeliveryPolicy          DeliveryPolicyInput `json:"delivery_policy"`
	ExpectedVersion         *uint               `json:"expected_version,omitempty"`
}

type ExpectedVersionRequest struct {
	ExpectedVersion uint `json:"expected_version"`
}

// PackageDTO 是套餐唯一的公开投影，不包含 PackageCode、结算标识或原始策略 JSON。
type PackageDTO struct {
	PackageNo             string                 `json:"package_no"`
	PackageVersion        uint                   `json:"package_version"`
	Product               core.ProductSummaryDTO `json:"product"`
	PackageType           string                 `json:"package_type"`
	Name                  string                 `json:"name"`
	Subtitle              *string                `json:"subtitle"`
	CoverImageURL         *string                `json:"cover_image_url"`
	BottleQuantity        uint                   `json:"bottle_quantity"`
	SalePriceAmount       int64                  `json:"sale_price_amount"`
	MinPurchaseQuantity   uint                   `json:"min_purchase_quantity"`
	MaxPurchaseQuantity   uint                   `json:"max_purchase_quantity"`
	ValidityDays          uint                   `json:"validity_days"`
	RedeemCityCode        string                 `json:"redeem_city_code"`
	RefundPolicySummary   string                 `json:"refund_policy_summary"`
	RenewalPolicySummary  string                 `json:"renewal_policy_summary"`
	DeliveryPolicySummary string                 `json:"delivery_policy_summary"`
	Status                string                 `json:"status"`
	SaleStartAt           *string                `json:"sale_start_at"`
	SaleEndAt             *string                `json:"sale_end_at"`
	Version               uint                   `json:"version"`
	UpdatedAt             string                 `json:"updated_at"`
}

type AdminPackageDTO struct {
	PackageDTO

	PackageCode             string              `json:"package_code"`
	IssuerMerchantID        string              `json:"issuer_merchant_id"`
	SettlementShopID        string              `json:"settlement_shop_id"`
	SettlementShopProductID string              `json:"settlement_shop_product_id"`
	ProductID               string              `json:"product_id"`
	PerCustomerLimit        *uint               `json:"per_customer_limit"`
	RefundPolicy            core.RefundPolicy   `json:"refund_policy"`
	RenewalPolicy           core.RenewalPolicy  `json:"renewal_policy"`
	DeliveryPolicy          core.DeliveryPolicy `json:"delivery_policy"`
	PublishedAt             *string             `json:"published_at"`
	PublishedBy             *string             `json:"published_by"`
	CreatedAt               string              `json:"created_at"`
	CreatedBy               *string             `json:"created_by"`
	UpdatedBy               *string             `json:"updated_by"`
}

func packageDTO(row PackageRecord) (PackageDTO, error) {
	refund, renewal, delivery, err := policiesFromRecord(row.Package)
	if err != nil {
		return PackageDTO{}, err
	}
	return PackageDTO{
		PackageNo:      row.PackageNo,
		PackageVersion: row.PackageVersion,
		Product: core.ProductSummaryDTO{
			ProductID: idString(row.ProductID),
			Name:      row.ProductName,
			BrandName: row.ProductBrandName,
			Spec:      row.ProductSpec,
			ImageURL:  row.ProductImageURL,
		},
		PackageType:           row.PackageType,
		Name:                  row.Name,
		Subtitle:              row.Subtitle,
		CoverImageURL:         row.CoverImageURL,
		BottleQuantity:        row.BottleQuantity,
		SalePriceAmount:       row.SalePriceAmount,
		MinPurchaseQuantity:   row.MinPurchaseQuantity,
		MaxPurchaseQuantity:   row.MaxPurchaseQuantity,
		ValidityDays:          row.ValidityDays,
		RedeemCityCode:        row.RedeemCityCode,
		RefundPolicySummary:   refundPolicySummary(refund),
		RenewalPolicySummary:  renewalPolicySummary(renewal),
		DeliveryPolicySummary: deliveryPolicySummary(delivery),
		Status:                row.Status,
		SaleStartAt:           optionalTimeString(row.SaleStartAt),
		SaleEndAt:             optionalTimeString(row.SaleEndAt),
		Version:               row.Version,
		UpdatedAt:             formatShanghai(row.UpdatedAt),
	}, nil
}

func adminPackageDTO(row PackageRecord) (AdminPackageDTO, error) {
	public, err := packageDTO(row)
	if err != nil {
		return AdminPackageDTO{}, err
	}
	refund, renewal, delivery, err := policiesFromRecord(row.Package)
	if err != nil {
		return AdminPackageDTO{}, err
	}
	return AdminPackageDTO{
		PackageDTO:              public,
		PackageCode:             row.PackageCode,
		IssuerMerchantID:        idString(row.IssuerMerchantID),
		SettlementShopID:        idString(row.SettlementShopID),
		SettlementShopProductID: idString(row.SettlementShopProductID),
		ProductID:               idString(row.ProductID),
		PerCustomerLimit:        row.PerCustomerLimit,
		RefundPolicy:            refund,
		RenewalPolicy:           renewal,
		DeliveryPolicy:          delivery,
		PublishedAt:             optionalTimeString(row.PublishedAt),
		PublishedBy:             optionalIDString(row.PublishedBy),
		CreatedAt:               formatShanghai(row.CreatedAt),
		CreatedBy:               optionalIDString(row.CreatedBy),
		UpdatedBy:               optionalIDString(row.UpdatedBy),
	}, nil
}

func policiesFromRecord(row Package) (core.RefundPolicy, core.RenewalPolicy, core.DeliveryPolicy, error) {
	var refund core.RefundPolicy
	if err := decodePolicyJSON(row.RefundPolicy, &refund, "schema_version", "enabled", "window_hours", "require_never_used", "fee_amount"); err != nil {
		return core.RefundPolicy{}, core.RenewalPolicy{}, core.DeliveryPolicy{}, fmt.Errorf("decode refund policy: %w", err)
	}
	var renewal core.RenewalPolicy
	if err := decodePolicyJSON(row.RenewalPolicy, &renewal, "schema_version", "enabled", "extension_days", "max_count", "grace_days", "fee_amount"); err != nil {
		return core.RefundPolicy{}, core.RenewalPolicy{}, core.DeliveryPolicy{}, fmt.Errorf("decode renewal policy: %w", err)
	}
	var delivery core.DeliveryPolicy
	if err := decodePolicyJSON(row.DeliveryPolicy, &delivery, "schema_version", "delivery_fee_included", "dispatch_lead_minutes"); err != nil {
		return core.RefundPolicy{}, core.RenewalPolicy{}, core.DeliveryPolicy{}, fmt.Errorf("decode delivery policy: %w", err)
	}
	return refund, renewal, delivery, nil
}

func decodePolicyJSON(raw []byte, out any, required ...string) error {
	return core.DecodePolicyJSON(raw, out, required...)
}

func refundPolicySummary(policy core.RefundPolicy) string {
	return core.RefundPolicySummary(policy)
}

func renewalPolicySummary(policy core.RenewalPolicy) string {
	return core.RenewalPolicySummary(policy)
}

func deliveryPolicySummary(policy core.DeliveryPolicy) string {
	return core.DeliveryPolicySummary(policy)
}

func PackageDTOFromRecord(row PackageRecord) (PackageDTO, error) {
	return packageDTO(row)
}

func AdminPackageDTOFromRecord(row PackageRecord) (AdminPackageDTO, error) {
	return adminPackageDTO(row)
}

func PoliciesFromRecord(
	row Package,
) (core.RefundPolicy, core.RenewalPolicy, core.DeliveryPolicy, error) {
	return policiesFromRecord(row)
}

var shanghaiLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

func formatShanghai(value time.Time) string {
	return value.In(shanghaiLocation).Format(time.RFC3339Nano)
}

func optionalTimeString(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := formatShanghai(*value)
	return &formatted
}

func idString(value uint64) string { return strconv.FormatUint(value, 10) }

func optionalIDString(value *uint64) *string {
	if value == nil {
		return nil
	}
	formatted := idString(*value)
	return &formatted
}
