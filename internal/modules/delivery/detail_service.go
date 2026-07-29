package delivery

import (
	"context"
	"errors"
	"strings"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

// Detail 为当前持有有效分配的骑手返回敏感履约视图。
// 列表权限和状态写入权限都不能授权读取敏感履约快照。
func (s *Service) Detail(ctx context.Context, claims *auth.Claims, deliveryIDRaw string) (DeliveryDetailDTO, error) {
	riderID, err := riderIDForDetail(claims)
	if err != nil {
		return DeliveryDetailDTO{}, err
	}
	deliveryID, err := parseID(deliveryIDRaw)
	if err != nil {
		return DeliveryDetailDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid delivery order id")
	}

	deliveryRow, orderRow, shopRow, itemRows, err := s.repo.Detail(ctx, riderID, deliveryID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return DeliveryDetailDTO{}, problem.NotFound("DELIVERY_NOT_FOUND", "delivery order not found")
	}
	if err != nil {
		return DeliveryDetailDTO{}, err
	}
	return deliveryDetailDTO(deliveryRow, orderRow, shopRow, itemRows), nil
}

func riderIDForDetail(claims *auth.Claims) (uint64, error) {
	if claims == nil || claims.AccountType != "rider" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "rider account required")
	}
	allowed := false
	for _, permission := range claims.Permissions {
		if permission == "delivery:view_own" {
			allowed = true
			break
		}
	}
	if !allowed {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "rider detail permission required")
	}
	riderID, err := parseID(claims.RiderID)
	if err != nil {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "invalid rider identity")
	}
	return riderID, nil
}

func deliveryDetailDTO(deliveryRow DeliveryOrder, orderRow Order, shopRow Shop, itemRows []OrderItem) DeliveryDetailDTO {
	pickupSnapshot := jsonMap(deliveryRow.PickupSnapshot)
	recipientSnapshot := jsonMap(deliveryRow.RecipientSnapshot)
	items := make([]DeliveryDetailItemDTO, 0, len(itemRows))
	for _, row := range itemRows {
		items = append(items, DeliveryDetailItemDTO{
			ID:              idString(row.ID),
			ShopProductID:   idString(row.ShopProductID),
			ProductID:       idString(row.ProductID),
			ProductSnapshot: deliveryProductSnapshotDTO(jsonMap(row.ProductSnapshot)),
			Quantity:        row.Quantity,
			SalePriceAmount: row.SalePriceAmount,
			TotalAmount:     row.TotalAmount,
		})
	}
	return DeliveryDetailDTO{
		ID:                idString(deliveryRow.ID),
		OrderID:           idString(deliveryRow.OrderID),
		ShopID:            idString(deliveryRow.ShopID),
		RiderID:           riderIDString(deliveryRow.RiderID),
		Status:            deliveryRow.Status,
		Version:           deliveryRow.AssignmentVersion,
		AssignmentVersion: deliveryRow.AssignmentVersion,
		DispatchStatus:    deliveryRow.DispatchStatus,
		PickupReadyStatus: deliveryRow.PickupReadyStatus,
		PickupSnapshot:    deliveryPickupSnapshotDTO(pickupSnapshot),
		RecipientSnapshot: deliveryRecipientSnapshotDTO(recipientSnapshot),
		PickupContact:     contactFromSnapshot(pickupSnapshot, true),
		RecipientContact:  contactFromSnapshot(recipientSnapshot, false),
		Order: DeliveryDetailOrderDTO{
			ID: idString(orderRow.ID), OrderNo: orderRow.OrderNo, Status: orderRow.Status,
			OrderType:       normalizedDeliveryOrderType(orderRow.OrderType),
			SettlementMode:  normalizedDeliverySettlementMode(orderRow.SettlementMode),
			SettlementLabel: deliverySettlementLabel(orderRow.OrderType, orderRow.SettlementMode),
			PayStatus:       orderRow.PayStatus, DeliveryStatus: orderRow.DeliveryStatus,
			GoodsAmount: orderRow.GoodsAmount, DiscountAmount: orderRow.DiscountAmount,
			DeliveryFeeAmount: orderRow.DeliveryFeeAmount, PayableAmount: orderRow.PayableAmount,
			PaidAmount: orderRow.PaidAmount, Remark: optionalString(orderRow.Remark), Version: orderRow.Version,
			PaidAt: optionalTimeString(orderRow.PaidAt), CancelledAt: optionalTimeString(orderRow.CancelledAt),
			CompletedAt: optionalTimeString(orderRow.CompletedAt), CreatedAt: timeString(orderRow.CreatedAt),
			UpdatedAt: timeString(orderRow.UpdatedAt),
		},
		Shop: DeliveryDetailShopDTO{
			ID: idString(shopRow.ID), Name: shopRow.Name, Phone: optionalString(shopRow.Phone),
			Province: optionalString(shopRow.Province), City: shopRow.City, District: shopRow.District,
			Address: shopRow.Address, Latitude: shopRow.Latitude, Longitude: shopRow.Longitude,
			CoordinateSystem: shopRow.CoordinateSystem, Status: shopRow.Status, BusinessStatus: shopRow.BusinessStatus,
		},
		Items:     items,
		CreatedAt: timeString(deliveryRow.CreatedAt), UpdatedAt: timeString(deliveryRow.UpdatedAt),
		PickupReadyAt: optionalTimeString(deliveryRow.PickupReadyAt), AcceptedAt: optionalTimeString(deliveryRow.AcceptedAt),
		PickedUpAt: optionalTimeString(deliveryRow.PickedUpAt), PickedUpVerifiedAt: optionalTimeString(deliveryRow.PickedUpVerifiedAt),
		StartedAt: optionalTimeString(deliveryRow.StartedAt), CompletedAt: optionalTimeString(deliveryRow.CompletedAt),
		CompletedVerifiedAt: optionalTimeString(deliveryRow.CompletedVerifiedAt), CancelledAt: optionalTimeString(deliveryRow.CancelledAt),
		ScheduledStartAt: optionalTimeString(deliveryRow.ScheduledStartAt),
		ScheduledEndAt:   optionalTimeString(deliveryRow.ScheduledEndAt),
		NotBeforeAt:      optionalTimeString(deliveryRow.NotBeforeAt),
	}
}

func contactFromSnapshot(snapshot map[string]any, pickup bool) DeliveryContactDTO {
	nameKeys := []string{"contact_name", "name", "shop_name"}
	phoneKeys := []string{"contact_phone", "phone"}
	addressKeys := []string{"address_detail", "address"}
	if pickup {
		nameKeys = []string{"name", "shop_name", "contact_name"}
		phoneKeys = []string{"phone", "contact_phone"}
		addressKeys = []string{"address", "address_detail"}
	}
	formatted := snapshotString(snapshot, "formatted_address")
	if formatted == "" {
		parts := []string{
			snapshotString(snapshot, "province"),
			snapshotString(snapshot, "city"),
			snapshotString(snapshot, "district"),
			snapshotString(snapshot, addressKeys...),
			snapshotString(snapshot, "doorplate"),
		}
		formatted = strings.Join(nonEmptyStrings(parts), "")
	}
	return DeliveryContactDTO{
		Name: snapshotString(snapshot, nameKeys...), Phone: snapshotString(snapshot, phoneKeys...),
		FormattedAddress: formatted,
	}
}

func snapshotString(snapshot map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := snapshot[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func nonEmptyStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
