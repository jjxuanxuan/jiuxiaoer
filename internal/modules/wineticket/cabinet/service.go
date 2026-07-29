package cabinet

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

type Service struct {
	repo *Repository
	now  func() time.Time
}

func NewService(db *gorm.DB) *Service {
	return &Service{
		repo: NewRepository(db),
		now:  time.Now,
	}
}

func (s *Service) WithClock(now func() time.Time) *Service {
	if now != nil {
		s.now = now
	}
	return s
}

func (s *Service) nowShanghai() time.Time {
	return core.NowShanghai(s.now)
}

func (s *Service) Cabinet(ctx context.Context, claims *auth.Claims, query pagination.Query) (CabinetDTO, error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_cabinet:view")
	if err != nil {
		return CabinetDTO{}, err
	}
	now := s.nowShanghai()
	rows, err := s.repo.ActiveCustomerLotFacts(ctx, customerID, now)
	if err != nil {
		return CabinetDTO{}, err
	}

	type groupKey struct {
		IssuerMerchantID uint64
		RedeemCityCode   string
		ProductID        uint64
	}
	groups := make(map[groupKey]*CabinetItemDTO)
	for _, row := range rows {
		key := groupKey{
			IssuerMerchantID: row.IssuerMerchantID,
			RedeemCityCode:   row.RedeemCityCode,
			ProductID:        row.ProductID,
		}
		item := groups[key]
		if item == nil {
			item = &CabinetItemDTO{
				IssuerMerchantID: core.IDString(row.IssuerMerchantID),
				RedeemCityCode:   row.RedeemCityCode,
				GiftSourceLotNo:  row.LotNo,
				Product: core.ProductSummaryDTO{
					ProductID: core.IDString(row.ProductID), Name: row.ProductName,
					BrandName: row.ProductBrandName, Spec: row.ProductSpec,
					ImageURL: row.ProductImageURL,
				},
				IssuerMerchantDisplayName: row.IssuerMerchantName,
				NearestExpiresAt:          core.FormatShanghai(row.ExpiresAt),
				Actions:                   []string{},
				cursorID:                  row.ID,
			}
			groups[key] = item
		}
		item.AvailableQuantity += row.AvailableQuantity
		item.HeldQuantity += row.HeldQuantity()
		item.ExtractedQuantity += row.ExtractedQuantity
		item.LotCount++
		if row.ID > item.cursorID {
			item.cursorID = row.ID
		}
		if row.Status == core.LotStatusActive && row.AvailableQuantity > 0 && row.ExpiresAt.After(now) {
			if !containsString(item.Actions, "redeem") {
				item.Actions = append(item.Actions, "redeem")
			}
			if !containsString(item.Actions, "gift") {
				item.Actions = append(item.Actions, "gift")
			}
		}
		if renewalEligible(row, now) && !containsString(item.Actions, "renew") {
			item.Actions = append(item.Actions, "renew")
		}
	}

	items := make([]CabinetItemDTO, 0, len(groups))
	for _, item := range groups {
		nearest, _ := parseFormattedShanghai(item.NearestExpiresAt)
		item.ExpiringSoon = !nearest.After(now.AddDate(0, 0, 7))
		items = append(items, *item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].cursorID == items[j].cursorID {
			return items[i].GiftSourceLotNo < items[j].GiftSourceLotNo
		}
		return items[i].cursorID > items[j].cursorID
	})
	if len(query.Cursor) > 0 {
		if len(query.Cursor) != 1 {
			return CabinetDTO{}, problem.InvalidArgument("PAGE_TOKEN_INVALID", "invalid cabinet cursor")
		}
		cursor, err := strconv.ParseUint(query.Cursor[0], 10, 64)
		if err != nil || cursor == 0 {
			return CabinetDTO{}, problem.InvalidArgument("PAGE_TOKEN_INVALID", "invalid cabinet cursor")
		}
		filtered := items[:0]
		for _, item := range items {
			if item.cursorID < cursor {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	next := ""
	if len(items) > query.PageSize {
		items = items[:query.PageSize]
		next = pagination.NextPageTokenWithCursor(query, core.IDString(items[len(items)-1].cursorID))
	}
	if items == nil {
		items = []CabinetItemDTO{}
	}
	return CabinetDTO{Items: items, NextPageToken: next, ServerTime: core.FormatShanghai(now)}, nil
}

func (s *Service) ListLots(
	ctx context.Context,
	claims *auth.Claims,
	query pagination.Query,
	productIDRaw, status string,
) ([]LotDTO, string, error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_lot:list")
	if err != nil {
		return nil, "", err
	}
	var productID uint64
	if strings.TrimSpace(productIDRaw) != "" {
		productID, err = parseExternalID(productIDRaw, "product_id")
		if err != nil {
			return nil, "", err
		}
	}
	status = strings.TrimSpace(status)
	if status != "" && !validLotStatus(status) {
		return nil, "", problem.InvalidArgument("VALIDATION_FAILED", "invalid lot status")
	}
	rows, err := s.repo.ListCustomerLots(ctx, customerID, query, productID, status)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		rows = rows[:query.PageSize]
		next = pagination.NextPageTokenWithCursor(query, core.IDString(rows[len(rows)-1].ID))
	}
	now := s.nowShanghai()
	items := make([]LotDTO, 0, len(rows))
	for _, row := range rows {
		item, err := lotRecordDTO(row, now, nil)
		if err != nil {
			return nil, "", err
		}
		items = append(items, item)
	}
	return items, next, nil
}

func (s *Service) Lot(ctx context.Context, claims *auth.Claims, lotNo string) (LotDTO, error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_lot:view")
	if err != nil {
		return LotDTO{}, err
	}
	if err := core.ValidateBusinessNo(lotNo, "lot_no"); err != nil {
		return LotDTO{}, err
	}
	row, err := s.repo.CustomerLotByNo(ctx, customerID, lotNo)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return LotDTO{}, problem.NotFound("WT_LOT_NOT_FOUND", "wine ticket lot not found")
	}
	if err != nil {
		return LotDTO{}, err
	}
	transactions, err := s.repo.LatestCustomerLotTransactions(ctx, customerID, row.ID, 10)
	if err != nil {
		return LotDTO{}, err
	}
	transactionDTOs := make([]WineTicketTransactionDTO, 0, len(transactions))
	for _, transaction := range transactions {
		transactionDTOs = append(transactionDTOs, transactionRecordDTO(transaction))
	}
	return lotRecordDTO(row, s.nowShanghai(), transactionDTOs)
}

func (s *Service) ListTransactions(
	ctx context.Context,
	claims *auth.Claims,
	query pagination.Query,
	lotNo, transactionType string,
) ([]WineTicketTransactionDTO, string, error) {
	customerID, err := customerIDWithPermission(claims, "wine_ticket_transaction:list")
	if err != nil {
		return nil, "", err
	}
	lotNo = strings.TrimSpace(lotNo)
	if lotNo != "" {
		if err := core.ValidateBusinessNo(lotNo, "lot_no"); err != nil {
			return nil, "", err
		}
	}
	transactionType = strings.TrimSpace(transactionType)
	if transactionType != "" && !validTransactionType(transactionType) {
		return nil, "", problem.InvalidArgument("VALIDATION_FAILED", "invalid transaction_type")
	}
	rows, err := s.repo.ListCustomerTransactions(ctx, customerID, query, lotNo, transactionType)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		rows = rows[:query.PageSize]
		next = pagination.NextPageTokenWithCursor(query, core.IDString(rows[len(rows)-1].ID))
	}
	items := make([]WineTicketTransactionDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, transactionRecordDTO(row))
	}
	return items, next, nil
}

func lotRecordDTO(row LotFactRecord, now time.Time, transactions []WineTicketTransactionDTO) (LotDTO, error) {
	var policy core.RenewalPolicy
	if err := core.DecodePolicyJSON(row.RenewalPolicy, &policy, "schema_version", "enabled", "extension_days", "max_count", "grace_days", "fee_amount"); err != nil {
		return LotDTO{}, problem.Internal("wine ticket renewal policy snapshot is invalid")
	}
	if transactions == nil {
		transactions = []WineTicketTransactionDTO{}
	}
	holds := make([]string, 0, 4)
	if row.RedemptionHeld > 0 {
		holds = append(holds, "redemption")
	}
	if row.GiftHeld > 0 {
		holds = append(holds, "gift")
	}
	if row.ActiveRenewalCount > 0 {
		holds = append(holds, "renewal")
	}
	if row.RefundHeld > 0 {
		holds = append(holds, "refund")
	}
	eligible := renewalEligibleWithPolicy(row, policy, now)
	var ineligibleReason *string
	if !eligible {
		reason := renewalIneligibleReason(row, policy, now)
		ineligibleReason = &reason
	}
	actions := []string{}
	if row.Status == core.LotStatusActive && row.AvailableQuantity > 0 && row.ExpiresAt.After(now) {
		actions = append(actions, "redeem", "gift")
		if eligible {
			actions = append(actions, "renew")
		}
	}
	var purchaseNo *string
	// 接收方批次会在内部保留购买血缘，但不会向接收方或原购买人暴露。
	if row.SourceType == core.LotSourcePurchase {
		purchaseNo = stringPointer(row.PurchaseNo)
	}
	return LotDTO{
		LotNo: row.LotNo,
		Product: core.ProductSummaryDTO{
			ProductID: core.IDString(row.ProductID), Name: row.ProductName,
			BrandName: row.ProductBrandName, Spec: row.ProductSpec, ImageURL: row.ProductImageURL,
		},
		PurchaseNo: purchaseNo, SourceType: row.SourceType,
		TotalQuantity: row.TotalQuantity, AvailableQuantity: row.AvailableQuantity,
		HeldQuantity: row.HeldQuantity(), ExtractedQuantity: row.ExtractedQuantity,
		RedeemCityCode:            row.RedeemCityCode,
		IssuerMerchantID:          core.IDString(row.IssuerMerchantID),
		IssuerMerchantDisplayName: row.IssuerMerchantName,
		OriginalExpiresAt:         core.FormatShanghai(row.OriginalExpiresAt),
		ExpiresAt:                 core.FormatShanghai(row.ExpiresAt), RenewalCount: row.RenewalCount,
		EverUsed: row.EverUsed, Status: row.Status, Actions: actions,
		ActiveHolds: holds, RenewalEligible: eligible,
		RenewalIneligibleReason: ineligibleReason,
		LatestTransactions:      transactions, Version: row.Version,
	}, nil
}

// ProjectLot 向运营报表包开放酒柜读模型映射，
// 同时避免泄露仓储内部实现或重复客户侧投影规则。
func ProjectLot(
	row LotFactRecord,
	now time.Time,
	transactions []WineTicketTransactionDTO,
) (LotDTO, error) {
	return lotRecordDTO(row, now, transactions)
}

func transactionRecordDTO(row TransactionRecord) WineTicketTransactionDTO {
	return WineTicketTransactionDTO{
		TransactionNo: row.TransactionNo, LotNo: row.LotNo,
		Product: core.ProductSummaryDTO{
			ProductID: core.IDString(row.ProductID), Name: row.ProductName,
			BrandName: row.ProductBrandName, Spec: row.ProductSpec, ImageURL: row.ProductImageURL,
		},
		TransactionType: row.TransactionType, QuantityDelta: row.QuantityDelta,
		AfterAvailableQuantity: row.AfterAvailableQuantity,
		BizType:                row.BizType, BizNo: row.BizNo, BizStatus: row.BizStatus,
		CreatedAt: core.FormatShanghai(row.CreatedAt),
	}
}

func renewalEligible(row LotFactRecord, now time.Time) bool {
	var policy core.RenewalPolicy
	if err := json.Unmarshal(row.RenewalPolicy, &policy); err != nil {
		return false
	}
	return renewalEligibleWithPolicy(row, policy, now)
}

func renewalEligibleWithPolicy(row LotFactRecord, policy core.RenewalPolicy, now time.Time) bool {
	return policy.Enabled && policy.SchemaVersion == 1 &&
		row.Status == core.LotStatusActive && row.AvailableQuantity > 0 &&
		row.ExpiresAt.After(now) && row.RenewalCount < uint(policy.MaxCount) &&
		row.RedemptionHeld == 0 && row.GiftHeld == 0 && row.RefundHeld == 0 &&
		row.ActiveRenewalCount == 0
}

func renewalIneligibleReason(row LotFactRecord, policy core.RenewalPolicy, now time.Time) string {
	switch {
	case !policy.Enabled:
		return "renewal_disabled"
	case row.Status != core.LotStatusActive || row.AvailableQuantity == 0:
		return "lot_not_active"
	case !row.ExpiresAt.After(now):
		return "lot_expired"
	case row.RenewalCount >= uint(policy.MaxCount):
		return "renewal_limit_reached"
	default:
		return "active_hold_exists"
	}
}

func validLotStatus(status string) bool {
	return status == core.LotStatusActive || status == core.LotStatusDepleted ||
		status == core.LotStatusExpired || status == core.LotStatusRefunded
}

func validTransactionType(value string) bool {
	switch value {
	case "purchase_issue", "redemption_hold", "redemption_restore",
		"redemption_return_restore", "redemption_return_expire",
		"gift_hold", "gift_claim", "gift_restore",
		"refund_hold", "refund_restore", "expiry":
		return true
	default:
		return false
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func ContainsAction(values []string, target string) bool {
	return containsString(values, target)
}

func ValidLotStatus(status string) bool {
	return validLotStatus(status)
}

func parseFormattedShanghai(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}
