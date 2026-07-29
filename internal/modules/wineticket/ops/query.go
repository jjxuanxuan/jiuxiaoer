package ops

import (
	"context"
	"strings"
	"time"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/cabinet"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/purchase"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

// AdminPurchaseDTO 是经过安全约束的运营投影。
// 它在客户 DTO 上增加路由标识，但绝不暴露 OpenID、手机号、
// 实名记录、支付机构原始载荷或套餐结算快照。
type AdminPurchaseDTO struct {
	purchase.PurchaseDTO
	CustomerID              string  `json:"customer_id"`
	PackageCode             string  `json:"package_code"`
	IssuerMerchantID        string  `json:"issuer_merchant_id"`
	SettlementShopID        string  `json:"settlement_shop_id"`
	SettlementShopProductID string  `json:"settlement_shop_product_id"`
	PaymentNo               string  `json:"payment_no"`
	ProviderStatus          *string `json:"provider_status"`
}

// AdminLotDTO 为运营保留血缘业务编号，
// 除内部十进制客户 ID 外，不暴露任何客户身份数据。
type AdminLotDTO struct {
	cabinet.LotDTO
	OwnerCustomerID string  `json:"owner_customer_id"`
	PurchaseNo      string  `json:"purchase_no"`
	SourceLotNo     *string `json:"source_lot_no,omitempty"`
	SourceGiftNo    *string `json:"source_gift_no,omitempty"`
}

type AdminPurchaseFilter struct {
	CustomerID       uint64
	PurchaseNo       string
	Status           string
	PackageCode      string
	IssuerMerchantID uint64
	CreatedFrom      *time.Time
	CreatedTo        *time.Time
}

type AdminLotFilter struct {
	OwnerCustomerID  uint64
	LotNo            string
	PurchaseNo       string
	Status           string
	ProductID        uint64
	IssuerMerchantID uint64
	ExpiresBefore    *time.Time
}

func (r *Repository) ListAdminPurchases(
	ctx context.Context,
	query pagination.Query,
	filter AdminPurchaseFilter,
) ([]purchase.PurchaseRecord, error) {
	db := purchaseProjection(r.db.WithContext(ctx))
	if filter.CustomerID != 0 {
		db = db.Where("purchase.customer_id = ?", filter.CustomerID)
	}
	if filter.PurchaseNo != "" {
		db = db.Where("purchase.purchase_no = ?", filter.PurchaseNo)
	}
	if filter.Status != "" {
		db = db.Where("purchase.status = ?", filter.Status)
	}
	if filter.PackageCode != "" {
		db = db.Where("package.package_code = ?", filter.PackageCode)
	}
	if filter.IssuerMerchantID != 0 {
		db = db.Where("purchase.issuer_merchant_id = ?", filter.IssuerMerchantID)
	}
	if filter.CreatedFrom != nil {
		db = db.Where("purchase.created_at >= ?", *filter.CreatedFrom)
	}
	if filter.CreatedTo != nil {
		db = db.Where("purchase.created_at < ?", *filter.CreatedTo)
	}
	var err error
	db, err = pagination.ApplyIDCursor(db, query, "purchase.id", "desc")
	if err != nil {
		return nil, err
	}
	var rows []purchase.PurchaseRecord
	err = db.Order("purchase.id DESC").Limit(query.PageSize + 1).Scan(&rows).Error
	return rows, err
}

func (r *Repository) ListAdminLots(
	ctx context.Context,
	query pagination.Query,
	filter AdminLotFilter,
) ([]purchase.LotFactRecord, error) {
	db := lotFactProjection(r.db.WithContext(ctx))
	if filter.OwnerCustomerID != 0 {
		db = db.Where("lot.owner_customer_id = ?", filter.OwnerCustomerID)
	}
	if filter.LotNo != "" {
		db = db.Where("lot.lot_no = ?", filter.LotNo)
	}
	if filter.PurchaseNo != "" {
		db = db.Where("purchase.purchase_no = ?", filter.PurchaseNo)
	}
	if filter.Status != "" {
		db = db.Where("lot.status = ?", filter.Status)
	}
	if filter.ProductID != 0 {
		db = db.Where("lot.product_id = ?", filter.ProductID)
	}
	if filter.IssuerMerchantID != 0 {
		db = db.Where("lot.issuer_merchant_id = ?", filter.IssuerMerchantID)
	}
	if filter.ExpiresBefore != nil {
		db = db.Where("lot.expires_at < ?", *filter.ExpiresBefore)
	}
	var err error
	db, err = pagination.ApplyIDCursor(db, query, "lot.id", "desc")
	if err != nil {
		return nil, err
	}
	var rows []purchase.LotFactRecord
	err = db.Order("lot.id DESC").Limit(query.PageSize + 1).Scan(&rows).Error
	return rows, err
}

func (s *Service) ListAdminPurchases(
	ctx context.Context,
	claims *auth.Claims,
	query pagination.Query,
	filter AdminPurchaseFilter,
) ([]AdminPurchaseDTO, string, error) {
	if _, err := adminIDWithPermission(claims, "wine_ticket_purchase:list_all"); err != nil {
		return nil, "", err
	}
	if err := validateAdminPurchaseFilter(filter); err != nil {
		return nil, "", err
	}
	rows, err := s.repo.ListAdminPurchases(ctx, query, filter)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		rows = rows[:query.PageSize]
		next = pagination.NextPageTokenWithCursor(query, idString(rows[len(rows)-1].ID))
	}
	base, err := s.purchaseDTOs(ctx, rows)
	if err != nil {
		return nil, "", err
	}
	items := make([]AdminPurchaseDTO, 0, len(rows))
	for index, row := range rows {
		items = append(items, AdminPurchaseDTO{
			PurchaseDTO:             base[index],
			CustomerID:              idString(row.CustomerID),
			PackageCode:             row.PackageCode,
			IssuerMerchantID:        idString(row.IssuerMerchantID),
			SettlementShopID:        idString(row.SettlementShopID),
			SettlementShopProductID: idString(row.SettlementShopProductID),
			PaymentNo:               row.PaymentNo,
			ProviderStatus:          row.ProviderStatus,
		})
	}
	return items, next, nil
}

func (s *Service) ListAdminLots(
	ctx context.Context,
	claims *auth.Claims,
	query pagination.Query,
	filter AdminLotFilter,
) ([]AdminLotDTO, string, error) {
	if _, err := adminIDWithPermission(claims, "wine_ticket_lot:list_all"); err != nil {
		return nil, "", err
	}
	if err := validateAdminLotFilter(filter); err != nil {
		return nil, "", err
	}
	rows, err := s.repo.ListAdminLots(ctx, query, filter)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		rows = rows[:query.PageSize]
		next = pagination.NextPageTokenWithCursor(query, idString(rows[len(rows)-1].ID))
	}
	now := s.nowShanghai()
	items := make([]AdminLotDTO, 0, len(rows))
	for _, row := range rows {
		base, err := lotRecordDTO(row, now, nil)
		if err != nil {
			return nil, "", err
		}
		items = append(items, AdminLotDTO{
			LotDTO:          base,
			OwnerCustomerID: idString(row.OwnerCustomerID),
			PurchaseNo:      row.PurchaseNo,
			SourceLotNo:     row.SourceLotNo,
			SourceGiftNo:    row.SourceGiftNo,
		})
	}
	return items, next, nil
}

func validateAdminPurchaseFilter(filter AdminPurchaseFilter) error {
	if filter.PurchaseNo != "" {
		if err := validateBusinessNo(filter.PurchaseNo, "purchase_no"); err != nil {
			return err
		}
	}
	if filter.Status != "" && !validPurchaseStatus(filter.Status) {
		return problem.InvalidArgument("VALIDATION_FAILED", "invalid purchase status")
	}
	if filter.PackageCode != "" &&
		(len(filter.PackageCode) > 64 || !packageCodePattern.MatchString(filter.PackageCode)) {
		return problem.InvalidArgument("VALIDATION_FAILED", "invalid package_code")
	}
	if filter.CreatedFrom != nil && filter.CreatedTo != nil &&
		!filter.CreatedFrom.Before(*filter.CreatedTo) {
		return problem.InvalidArgument("VALIDATION_FAILED", "created_from must be before created_to")
	}
	return nil
}

func validateAdminLotFilter(filter AdminLotFilter) error {
	if filter.LotNo != "" {
		if err := validateBusinessNo(filter.LotNo, "lot_no"); err != nil {
			return err
		}
	}
	if filter.PurchaseNo != "" {
		if err := validateBusinessNo(filter.PurchaseNo, "purchase_no"); err != nil {
			return err
		}
	}
	if filter.Status != "" && !validLotStatus(filter.Status) {
		return problem.InvalidArgument("VALIDATION_FAILED", "invalid lot status")
	}
	return nil
}

func parseOptionalExternalID(raw, field string) (uint64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	return parseExternalID(raw, field)
}

func parseOptionalQueryDateTime(raw, field string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, problem.InvalidArgument("VALIDATION_FAILED", field+" must be RFC3339")
	}
	value = value.UTC().Truncate(time.Millisecond)
	return &value, nil
}
