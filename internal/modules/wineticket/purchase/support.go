package purchase

import (
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/datatypes"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/catalog"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

var (
	businessNoPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	packageCodePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	cityCodePattern    = regexp.MustCompile(`^[0-9A-Za-z_-]+$`)
)

var shanghaiLocation = core.ShanghaiLocation

func formatShanghai(value time.Time) string { return core.FormatShanghai(value) }
func idString(value uint64) string          { return core.IDString(value) }
func jsonData(value any) datatypes.JSON     { return core.JSONData(value) }
func timePtr(value time.Time) *time.Time    { return &value }
func optionalTimeString(value *time.Time) *string {
	return core.OptionalTimeString(value)
}
func refundPolicySummary(policy core.RefundPolicy) string {
	return core.RefundPolicySummary(policy)
}

const (
	maxWineTicketAmount = core.MaxWineTicketAmount
	maxPurchaseQuantity = core.MaxPurchaseQuantity
)

func validatePackageRow(row catalog.Package) error {
	if len(row.PackageCode) == 0 ||
		len(row.PackageCode) > 64 ||
		!packageCodePattern.MatchString(row.PackageCode) ||
		len(row.RedeemCityCode) == 0 ||
		len(row.RedeemCityCode) > 32 ||
		!cityCodePattern.MatchString(row.RedeemCityCode) ||
		utf8.RuneCountInString(strings.TrimSpace(row.Name)) == 0 ||
		utf8.RuneCountInString(strings.TrimSpace(row.Name)) > 128 ||
		(row.Subtitle != nil && utf8.RuneCountInString(*row.Subtitle) > 255) ||
		(row.CoverImageURL != nil &&
			(len(*row.CoverImageURL) > 512 || !validHTTPURL(*row.CoverImageURL))) ||
		row.IssuerMerchantID == 0 ||
		row.SettlementShopID == 0 ||
		row.SettlementShopProductID == 0 ||
		row.ProductID == 0 ||
		!validPackageType(row.PackageType) ||
		row.PackageVersion == 0 ||
		row.BottleQuantity < 1 ||
		row.BottleQuantity > 10_000 ||
		row.SalePriceAmount < 1 ||
		row.SalePriceAmount > maxWineTicketAmount ||
		row.MinPurchaseQuantity < 1 ||
		row.MinPurchaseQuantity > maxPurchaseQuantity ||
		row.MaxPurchaseQuantity < row.MinPurchaseQuantity ||
		row.MaxPurchaseQuantity > maxPurchaseQuantity ||
		row.ValidityDays < 1 ||
		row.ValidityDays > 3650 ||
		(row.PerCustomerLimit != nil &&
			(*row.PerCustomerLimit < row.MinPurchaseQuantity ||
				*row.PerCustomerLimit > maxPurchaseQuantity)) ||
		(row.SaleStartAt != nil &&
			row.SaleEndAt != nil &&
			!row.SaleStartAt.Before(*row.SaleEndAt)) {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"wine ticket package configuration cannot be purchased",
		)
	}
	var refundPolicy core.RefundPolicy
	if err := core.DecodePolicyJSON(
		row.RefundPolicy,
		&refundPolicy,
		"schema_version",
		"enabled",
		"window_hours",
		"require_never_used",
		"fee_amount",
	); err != nil ||
		refundPolicy.SchemaVersion != 1 ||
		refundPolicy.WindowHours < 0 ||
		refundPolicy.WindowHours > 8760 ||
		!refundPolicy.RequireNeverUsed ||
		refundPolicy.FeeAmount != 0 {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"wine ticket package refund policy is invalid",
		)
	}
	var renewalPolicy core.RenewalPolicy
	if err := core.DecodePolicyJSON(
		row.RenewalPolicy,
		&renewalPolicy,
		"schema_version",
		"enabled",
		"extension_days",
		"max_count",
		"grace_days",
		"fee_amount",
	); err != nil ||
		renewalPolicy.SchemaVersion != 1 ||
		renewalPolicy.ExtensionDays < 1 ||
		renewalPolicy.ExtensionDays > 3650 ||
		renewalPolicy.MaxCount < 0 ||
		renewalPolicy.MaxCount > 100 ||
		renewalPolicy.GraceDays != 0 ||
		renewalPolicy.FeeAmount < 0 ||
		renewalPolicy.FeeAmount > maxWineTicketAmount {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"wine ticket package renewal policy is invalid",
		)
	}
	var deliveryPolicy core.DeliveryPolicy
	if err := core.DecodePolicyJSON(
		row.DeliveryPolicy,
		&deliveryPolicy,
		"schema_version",
		"delivery_fee_included",
		"dispatch_lead_minutes",
	); err != nil ||
		deliveryPolicy.SchemaVersion != 1 ||
		!deliveryPolicy.DeliveryFeeIncluded ||
		deliveryPolicy.DispatchLeadMinutes < 0 ||
		deliveryPolicy.DispatchLeadMinutes > 1440 {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"wine ticket package delivery policy is invalid",
		)
	}
	return nil
}

func validPackageType(value string) bool {
	return value == PackageTypeStockpile ||
		value == PackageTypeCorporate ||
		value == PackageTypeGift
}

func validHTTPURL(raw string) bool {
	parsed, err := url.ParseRequestURI(raw)
	return err == nil &&
		(parsed.Scheme == "https" || parsed.Scheme == "http") &&
		parsed.Host != ""
}
