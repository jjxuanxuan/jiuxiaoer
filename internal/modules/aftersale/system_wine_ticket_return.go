package aftersale

import (
	"context"
	"strings"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

type SystemWineTicketReturnRequest struct {
	DeliveryReturnID uint64
	OrderID          uint64
	ApprovedBy       uint64
	ReasonCode       string
	Description      string
}

type SystemWineTicketReturnResult struct {
	AfterSaleID uint64
}

// CreateSystemWineTicketReturnWithTx 创建共享实物收货流程所需的
// 零金额售后单及明细记录，不创建支付、退款或退款明细。
func (s *Service) CreateSystemWineTicketReturnWithTx(
	ctx context.Context,
	tx *gorm.DB,
	req SystemWineTicketReturnRequest,
) (SystemWineTicketReturnResult, error) {
	if tx == nil || req.DeliveryReturnID == 0 || req.OrderID == 0 || req.ApprovedBy == 0 {
		return SystemWineTicketReturnResult{}, problem.Internal("invalid wine-ticket system after-sale transaction input")
	}
	if !s.cfg.AfterSale.Enabled || !s.cfg.DeliveryReturn.SystemAfterSaleEnabled {
		return SystemWineTicketReturnResult{}, problem.New(
			503,
			"SYSTEM_AFTERSALE_DISABLED",
			"Service Unavailable",
			"system delivery-return after-sale is disabled",
		)
	}
	if existing, err := s.repo.BySource(ctx, tx, "delivery_return", req.DeliveryReturnID); err == nil {
		if existing.OrderID != req.OrderID ||
			existing.RequestedAmount != 0 ||
			existing.ApprovedAmount != 0 ||
			existing.RefundedAmount != 0 {
			return SystemWineTicketReturnResult{}, problem.Internal("wine-ticket system after-sale replay mismatch")
		}
		refundCount, countErr := s.repo.RefundCountByAfterSale(ctx, tx, existing.ID)
		if countErr != nil {
			return SystemWineTicketReturnResult{}, countErr
		}
		if refundCount != 0 {
			return SystemWineTicketReturnResult{}, problem.Internal("wine-ticket system after-sale unexpectedly has a refund")
		}
		return SystemWineTicketReturnResult{AfterSaleID: existing.ID}, nil
	} else if !repositoryNotFound(err) {
		return SystemWineTicketReturnResult{}, err
	}

	order, err := s.repo.LockOrder(ctx, tx, req.OrderID)
	if err != nil {
		return SystemWineTicketReturnResult{}, problem.Conflict("AFTER_SALE_NOT_ELIGIBLE", "delivery return order not found")
	}
	if order.OrderType != "wine_ticket_redemption" ||
		order.SettlementMode != "wine_ticket" ||
		order.PayStatus != "not_required" ||
		order.PaidAmount != 0 {
		return SystemWineTicketReturnResult{}, problem.Conflict(
			"AFTER_SALE_NOT_ELIGIBLE",
			"delivery return order is not a zero-cash wine-ticket redemption",
		)
	}
	paymentCount, err := s.repo.PaymentCountByOrder(ctx, tx, order.ID)
	if err != nil {
		return SystemWineTicketReturnResult{}, err
	}
	if paymentCount != 0 {
		return SystemWineTicketReturnResult{}, problem.Internal("wine-ticket redemption order unexpectedly has a payment")
	}
	conflict, err := s.repo.ActiveConflicts(ctx, tx, order.ID)
	if err != nil {
		return SystemWineTicketReturnResult{}, err
	}
	if conflict {
		return SystemWineTicketReturnResult{}, ErrDeliveryReturnManualReview
	}
	orderItems, err := s.repo.AllOrderItems(ctx, tx, order.ID)
	if err != nil || len(orderItems) == 0 {
		return SystemWineTicketReturnResult{}, problem.Conflict("AFTER_SALE_NOT_ELIGIBLE", "delivery return order items not found")
	}
	for _, item := range orderItems {
		if item.Quantity <= 0 || item.TotalAmount != 0 {
			return SystemWineTicketReturnResult{}, problem.Internal("wine-ticket redemption order item is not zero-cash")
		}
	}

	now := s.now().In(time.Local).Truncate(time.Millisecond)
	afterSaleID := s.ids.Next()
	resolution := "wine_ticket_restore"
	reason := strings.TrimSpace(req.ReasonCode)
	description := strings.TrimSpace(req.Description)
	if description == "" {
		description = "system-created after-sale for wine-ticket delivery return"
	}
	row := AfterSale{
		ID: afterSaleID, AfterSaleNo: "AS" + idString(afterSaleID), OrderID: order.ID,
		CustomerID: order.CustomerID, MerchantID: order.MerchantID, ShopID: order.ShopID,
		InitiatorType: "system", SourceType: "delivery_return", SourceID: &req.DeliveryReturnID,
		Type: "other", RequestedResolution: resolution, ApprovedResolution: &resolution,
		Status: "return_processing", RequestedAmount: 0, ApprovedAmount: 0,
		IncludeDeliveryFee: false, ReasonCode: optional(reason), Description: description,
		SubmittedAt: now, ApprovedAt: &now, Version: 1,
	}
	items := make([]Item, 0, len(orderItems))
	for _, source := range orderItems {
		items = append(items, Item{
			ID: s.ids.Next(), AfterSaleID: afterSaleID, OrderID: order.ID, OrderItemID: source.ID,
			ShopProductID: source.ShopProductID, ProductID: source.ProductID,
			RequestedQuantity: source.Quantity, ApprovedQuantity: source.Quantity,
			RequestedAmount: 0, ApprovedAmount: 0, ReturnDisposition: "none",
		})
	}
	history := s.history(
		afterSaleID,
		"admin",
		req.ApprovedBy,
		"system_approve",
		nil,
		strPtr("return_processing"),
		reason,
		description,
	)
	audit := s.audit(ctx, "admin", req.ApprovedBy, "after_sale.system_wine_ticket_return", afterSaleID, nil, row)
	outbox := s.outbox(ctx, "after_sale.approved", afterSaleID, map[string]any{
		"after_sale_id": idString(afterSaleID),
		"order_id":      idString(order.ID),
		"source_type":   "delivery_return",
		"source_id":     idString(req.DeliveryReturnID),
		"settlement":    "wine_ticket_restore",
	})
	if err := s.repo.Create(ctx, tx, &row, items, nil, history, audit, outbox); err != nil {
		return SystemWineTicketReturnResult{}, err
	}
	if err := s.repo.UpdateOrderSummary(ctx, tx, order.ID, "processing"); err != nil {
		return SystemWineTicketReturnResult{}, err
	}
	return SystemWineTicketReturnResult{AfterSaleID: afterSaleID}, nil
}
