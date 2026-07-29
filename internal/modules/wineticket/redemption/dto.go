package redemption

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

// RedemptionCreateRequest 映射公开 JSON Schema。
// 所有数据库标识在 HTTP 边界都保持为正十进制字符串。
type RedemptionCreateRequest struct {
	ProductID          string  `json:"product_id"`
	Quantity           uint    `json:"quantity"`
	AddressID          string  `json:"address_id"`
	AddressVersion     uint    `json:"address_version"`
	DeliveryTimeSlotID string  `json:"delivery_time_slot_id"`
	Remark             *string `json:"remark,omitempty"`
}

type RedemptionCancelRequest struct {
	ExpectedVersion uint `json:"expected_version"`
}

type RedemptionSlotQuery struct {
	ProductID      uint64
	Quantity       uint
	AddressID      uint64
	AddressVersion uint
	DateFrom       time.Time
	DateTo         time.Time
}

type RedemptionDeliveryTimeSlotDTO struct {
	SlotID                    string `json:"slot_id"`
	ShopID                    string `json:"shop_id"`
	ShopName                  string `json:"shop_name"`
	IssuerMerchantID          string `json:"issuer_merchant_id"`
	IssuerMerchantDisplayName string `json:"issuer_merchant_display_name"`
	ScheduledStartAt          string `json:"scheduled_start_at"`
	ScheduledEndAt            string `json:"scheduled_end_at"`
	CutoffAt                  string `json:"cutoff_at"`
	AvailabilityStatus        string `json:"availability_status"`
	RemainingCapacity         uint   `json:"remaining_capacity"`
	Version                   uint   `json:"version"`
}

type RedemptionTimelineItemDTO struct {
	Status      string `json:"status"`
	OccurredAt  string `json:"occurred_at"`
	SafeMessage string `json:"safe_message,omitempty"`
}

type RedemptionAllocationDTO struct {
	LotNo    string `json:"lot_no"`
	Quantity uint   `json:"quantity"`
	Status   string `json:"status"`
}

type RedemptionDTO struct {
	RedemptionNo      string                      `json:"redemption_no"`
	OrderNo           string                      `json:"order_no"`
	Product           core.ProductSummaryDTO      `json:"product"`
	Quantity          uint                        `json:"quantity"`
	AddressSummary    string                      `json:"address_summary"`
	ShopName          string                      `json:"shop_name"`
	ScheduledStartAt  string                      `json:"scheduled_start_at"`
	ScheduledEndAt    string                      `json:"scheduled_end_at"`
	Status            string                      `json:"status"`
	CanCancel         bool                        `json:"can_cancel"`
	DeliveryStatus    string                      `json:"delivery_status"`
	Timeline          []RedemptionTimelineItemDTO `json:"timeline"`
	AllocationSummary []RedemptionAllocationDTO   `json:"allocation_summary"`
	CancelResult      *string                     `json:"cancel_result"`
	ReturnResult      *string                     `json:"return_result"`
	Version           uint                        `json:"version"`
	CreatedAt         string                      `json:"created_at"`
	UpdatedAt         string                      `json:"updated_at"`

	cursorID uint64
}

func (s *RedemptionService) redemptionDTOByID(
	ctx context.Context,
	db *gorm.DB,
	customerID uint64,
	redemptionID uint64,
) (RedemptionDTO, error) {
	row, err := s.repo.customerRedemptionByID(ctx, db, customerID, redemptionID)
	if err != nil {
		return RedemptionDTO{}, err
	}
	items, err := s.redemptionDTOs(ctx, db, []redemptionView{row})
	if err != nil {
		return RedemptionDTO{}, err
	}
	return items[0], nil
}

func (s *RedemptionService) redemptionDTOs(
	ctx context.Context,
	db *gorm.DB,
	rows []redemptionView,
) ([]RedemptionDTO, error) {
	if len(rows) == 0 {
		return []RedemptionDTO{}, nil
	}
	allocations, err := s.repo.allocationViews(ctx, db, sortedRedemptionIDs(rows))
	if err != nil {
		return nil, err
	}
	byRedemption := make(map[uint64][]redemptionAllocationView, len(rows))
	for _, allocation := range allocations {
		byRedemption[allocation.RedemptionID] = append(
			byRedemption[allocation.RedemptionID], allocation,
		)
	}
	items := make([]RedemptionDTO, 0, len(rows))
	for _, row := range rows {
		item, err := redemptionViewDTO(row, byRedemption[row.ID])
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func redemptionViewDTO(
	row redemptionView,
	allocations []redemptionAllocationView,
) (RedemptionDTO, error) {
	product, err := redemptionProductSummary(row)
	if err != nil {
		return RedemptionDTO{}, err
	}
	addressSummary, err := redemptionSafeAddressSummary(row.AddressSnapshot)
	if err != nil {
		return RedemptionDTO{}, err
	}
	shopName := strings.TrimSpace(row.ShopName)
	var slotSnapshot redemptionSlotSnapshot
	if len(row.DeliveryTimeSlotSnapshot) != 0 &&
		json.Unmarshal(row.DeliveryTimeSlotSnapshot, &slotSnapshot) == nil &&
		slotSnapshot.SchemaVersion == 1 &&
		strings.TrimSpace(slotSnapshot.ShopName) != "" {
		shopName = strings.TrimSpace(slotSnapshot.ShopName)
	}
	deliveryStatus := row.OrderDeliveryStatus
	if row.DeliveryStatus != nil && strings.TrimSpace(*row.DeliveryStatus) != "" {
		deliveryStatus = strings.TrimSpace(*row.DeliveryStatus)
	}
	allocationDTOs := make([]RedemptionAllocationDTO, 0, len(allocations))
	hasExpiredRestore := false
	for _, allocation := range allocations {
		allocationDTOs = append(allocationDTOs, RedemptionAllocationDTO{
			LotNo: allocation.LotNo, Quantity: allocation.Quantity, Status: allocation.Status,
		})
		if allocation.Status == RedemptionAllocationStatusRestored &&
			allocation.LotStatus == LotStatusExpired {
			hasExpiredRestore = true
		}
	}
	if len(allocations) > 0 && allocationViewQuantity(allocations) != row.Quantity {
		return RedemptionDTO{}, problem.Internal("wine ticket redemption allocation projection is inconsistent")
	}
	var cancelResult *string
	if row.Status == RedemptionStatusCancelled {
		value := "酒票已按原有效期恢复"
		if hasExpiredRestore {
			value = "酒票已按原有效期恢复，已到期部分已同步核销"
		}
		cancelResult = &value
	}
	var returnResult *string
	switch row.Status {
	case RedemptionStatusReturnInProgress:
		value := "退回处理中，完整收货后恢复酒票"
		returnResult = &value
	case RedemptionStatusRestored:
		value := "退回验收完成，酒票已按原有效期恢复"
		returnResult = &value
	case RedemptionStatusException:
		value := "履约事实待复核，请联系客服"
		returnResult = &value
	}
	return RedemptionDTO{
		RedemptionNo:      row.RedemptionNo,
		OrderNo:           row.OrderNo,
		Product:           product,
		Quantity:          row.Quantity,
		AddressSummary:    addressSummary,
		ShopName:          shopName,
		ScheduledStartAt:  formatShanghai(row.ScheduledStartAt),
		ScheduledEndAt:    formatShanghai(row.ScheduledEndAt),
		Status:            row.Status,
		CanCancel:         redemptionCanCancel(row, deliveryStatus),
		DeliveryStatus:    deliveryStatus,
		Timeline:          redemptionTimeline(row),
		AllocationSummary: allocationDTOs,
		CancelResult:      cancelResult,
		ReturnResult:      returnResult,
		Version:           row.Version,
		CreatedAt:         formatShanghai(row.CreatedAt),
		UpdatedAt:         formatShanghai(row.UpdatedAt),
		cursorID:          row.ID,
	}, nil
}

func redemptionProductSummary(row redemptionView) (core.ProductSummaryDTO, error) {
	var snapshot redemptionProductSnapshot
	if len(row.ProductSnapshot) != 0 &&
		json.Unmarshal(row.ProductSnapshot, &snapshot) == nil &&
		snapshot.SchemaVersion == 1 &&
		snapshot.ProductID != "" &&
		strings.TrimSpace(snapshot.Name) != "" {
		if productID, err := parseExternalID(snapshot.ProductID, "product_id"); err == nil &&
			productID == row.ProductID {
			return core.ProductSummaryDTO{
				ProductID: snapshot.ProductID, Name: snapshot.Name,
				BrandName: snapshot.BrandName, Spec: snapshot.Spec, ImageURL: snapshot.ImageURL,
			}, nil
		}
	}
	if row.ProductID == 0 || strings.TrimSpace(row.ProductName) == "" {
		return core.ProductSummaryDTO{}, problem.Internal("wine ticket redemption product snapshot is invalid")
	}
	return core.ProductSummaryDTO{
		ProductID: idString(row.ProductID), Name: row.ProductName,
		BrandName: row.ProductBrandName, Spec: row.ProductSpec, ImageURL: row.ProductImageURL,
	}, nil
}

func redemptionSafeAddressSummary(raw []byte) (string, error) {
	var snapshot redemptionAddressSnapshot
	if len(raw) == 0 || json.Unmarshal(raw, &snapshot) != nil ||
		snapshot.SchemaVersion != 1 || snapshot.AddressID == "" ||
		snapshot.AddressVersion == 0 {
		return "", problem.Internal("wine ticket redemption address snapshot is invalid")
	}
	parts := make([]string, 0, 4)
	if value := maskRedemptionName(snapshot.ContactName); value != "" {
		parts = append(parts, value)
	}
	if value := maskRedemptionPhone(snapshot.ContactPhone); value != "" {
		parts = append(parts, value)
	}
	location := strings.TrimSpace(strings.Join([]string{
		strings.TrimSpace(snapshot.Province),
		strings.TrimSpace(snapshot.City),
		strings.TrimSpace(snapshot.District),
	}, " "))
	if location != "" {
		parts = append(parts, location)
	}
	if len(parts) == 0 {
		return "", problem.Internal("wine ticket redemption address summary is empty")
	}
	return strings.Join(parts, " "), nil
}

func maskRedemptionName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !utf8.ValidString(value) {
		return ""
	}
	runes := []rune(value)
	if len(runes) == 1 {
		return string(runes[0]) + "**"
	}
	return string(runes[0]) + strings.Repeat("*", len(runes)-1)
}

func maskRedemptionPhone(value string) string {
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) {
		return ""
	}
	runes := []rune(value)
	if len(runes) < 7 {
		return ""
	}
	return string(runes[:3]) + "****" + string(runes[len(runes)-4:])
}

func redemptionCanCancel(row redemptionView, deliveryStatus string) bool {
	if row.Status != RedemptionStatusScheduled && row.Status != RedemptionStatusAssigned {
		return false
	}
	if row.PickedUpAt != nil || row.CompletedAt != nil ||
		row.DeliveryPickedUpAt != nil || row.DeliveryCompletedAt != nil {
		return false
	}
	switch deliveryStatus {
	case "picked_up", "delivering", "completed", "delivered", "returning", "cancelled":
		return false
	default:
		return true
	}
}

func redemptionTimeline(row redemptionView) []RedemptionTimelineItemDTO {
	items := []RedemptionTimelineItemDTO{{
		Status: RedemptionStatusScheduled, OccurredAt: formatShanghai(row.CreatedAt),
		SafeMessage: "酒票已核销，预约配送已创建",
	}}
	if row.AcceptedAt != nil {
		items = append(items, RedemptionTimelineItemDTO{
			Status: RedemptionStatusAssigned, OccurredAt: formatShanghai(*row.AcceptedAt),
			SafeMessage: "骑手已接单",
		})
	} else if row.Status == RedemptionStatusAssigned {
		items = append(items, RedemptionTimelineItemDTO{
			Status: RedemptionStatusAssigned, OccurredAt: formatShanghai(row.UpdatedAt),
			SafeMessage: "配送任务已分配",
		})
	}
	pickedUpAt := firstRedemptionTime(row.PickedUpAt, row.DeliveryPickedUpAt)
	if pickedUpAt != nil {
		items = append(items, RedemptionTimelineItemDTO{
			Status: RedemptionStatusPickedUp, OccurredAt: formatShanghai(*pickedUpAt),
			SafeMessage: "商品已取货",
		})
	}
	completedAt := firstRedemptionTime(row.CompletedAt, row.DeliveryCompletedAt)
	if completedAt != nil && row.Status == RedemptionStatusDelivered {
		items = append(items, RedemptionTimelineItemDTO{
			Status: RedemptionStatusDelivered, OccurredAt: formatShanghai(*completedAt),
			SafeMessage: "配送已完成",
		})
	}
	if row.CancelledAt != nil {
		items = append(items, RedemptionTimelineItemDTO{
			Status: RedemptionStatusCancelled, OccurredAt: formatShanghai(*row.CancelledAt),
			SafeMessage: "提酒已取消，酒票已按规则恢复",
		})
	}
	switch row.Status {
	case RedemptionStatusReturnInProgress:
		items = append(items, RedemptionTimelineItemDTO{
			Status: row.Status, OccurredAt: formatShanghai(row.UpdatedAt),
			SafeMessage: "商品退回处理中",
		})
	case RedemptionStatusRestored:
		occurredAt := row.UpdatedAt
		if row.RestoredAt != nil {
			occurredAt = *row.RestoredAt
		}
		items = append(items, RedemptionTimelineItemDTO{
			Status: row.Status, OccurredAt: formatShanghai(occurredAt),
			SafeMessage: "退回验收完成，酒票已恢复",
		})
	case RedemptionStatusException:
		items = append(items, RedemptionTimelineItemDTO{
			Status: row.Status, OccurredAt: formatShanghai(row.UpdatedAt),
			SafeMessage: "履约事实待复核",
		})
	}
	return items
}

func firstRedemptionTime(values ...*time.Time) *time.Time {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func allocationViewQuantity(rows []redemptionAllocationView) uint {
	var total uint
	for _, row := range rows {
		if total > ^uint(0)-row.Quantity {
			return ^uint(0)
		}
		total += row.Quantity
	}
	return total
}
