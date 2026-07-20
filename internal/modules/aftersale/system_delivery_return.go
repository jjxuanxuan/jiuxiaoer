package aftersale

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

var ErrDeliveryReturnManualReview = errors.New("delivery return requires manual review")

type SystemDeliveryReturnRequest struct {
	DeliveryReturnID uint64
	OrderID          uint64
	ApprovedBy       uint64
	ReasonCode       string
	Description      string
}

type SystemDeliveryReturnResult struct {
	AfterSaleID uint64
	RefundID    uint64
	RefundNo    string
	Amount      int64
}

// CreateSystemDeliveryReturnWithTx creates the system after-sale, its item
// ledger, and the refund reservation in the caller's transaction. It never
// commits independently, so delivery-return approval is all-or-nothing.
func (s *Service) CreateSystemDeliveryReturnWithTx(ctx context.Context, tx *gorm.DB, req SystemDeliveryReturnRequest) (SystemDeliveryReturnResult, error) {
	if tx == nil || req.DeliveryReturnID == 0 || req.OrderID == 0 || req.ApprovedBy == 0 {
		return SystemDeliveryReturnResult{}, problem.Internal("invalid system after-sale transaction input")
	}
	if !s.cfg.AfterSale.Enabled || !s.cfg.AfterSale.RefundExecutionEnabled || !s.cfg.DeliveryReturn.SystemAfterSaleEnabled {
		return SystemDeliveryReturnResult{}, problem.New(503, "SYSTEM_AFTERSALE_DISABLED", "Service Unavailable", "system delivery-return after-sale is disabled")
	}
	if existing, err := s.repo.BySource(ctx, tx, "delivery_return", req.DeliveryReturnID); err == nil {
		refund, refundErr := s.repo.RefundByAfterSale(ctx, tx, existing.ID)
		if refundErr != nil {
			return SystemDeliveryReturnResult{}, refundErr
		}
		return SystemDeliveryReturnResult{AfterSaleID: existing.ID, RefundID: refund.ID, RefundNo: refund.RefundNo, Amount: refund.Amount}, nil
	} else if !repositoryNotFound(err) {
		return SystemDeliveryReturnResult{}, err
	}

	order, err := s.repo.LockOrder(ctx, tx, req.OrderID)
	if err != nil {
		return SystemDeliveryReturnResult{}, problem.Conflict("AFTER_SALE_NOT_ELIGIBLE", "delivery return order not found")
	}
	if order.PayStatus != "succeeded" || order.PaidAmount <= 0 {
		return SystemDeliveryReturnResult{}, problem.Conflict("AFTER_SALE_NOT_ELIGIBLE", "delivery return order has no successful payment")
	}
	payment, err := s.repo.LockPayment(ctx, tx, req.OrderID)
	if err != nil || payment.Amount <= 0 || strings.TrimSpace(payment.Provider) == "" || strings.TrimSpace(payment.Currency) == "" {
		return SystemDeliveryReturnResult{}, problem.Conflict("AFTER_SALE_NOT_ELIGIBLE", "succeeded refundable payment not found")
	}
	conflict, err := s.repo.ActiveConflicts(ctx, tx, req.OrderID)
	if err != nil {
		return SystemDeliveryReturnResult{}, err
	}
	reserved, err := s.repo.ReservedRefund(ctx, tx, payment.ID)
	if err != nil {
		return SystemDeliveryReturnResult{}, err
	}
	// P0 deliberately refuses partial automation. Any existing customer claim,
	// reservation, or successful refund moves the return to disputed.
	if conflict || reserved > 0 || payment.RefundedAmount > 0 || order.RefundedAmount > 0 {
		return SystemDeliveryReturnResult{}, ErrDeliveryReturnManualReview
	}
	remaining := payment.Amount - payment.RefundedAmount - reserved
	if remaining <= 0 {
		return SystemDeliveryReturnResult{}, ErrDeliveryReturnManualReview
	}
	orderItems, err := s.repo.AllOrderItems(ctx, tx, req.OrderID)
	if err != nil || len(orderItems) == 0 {
		return SystemDeliveryReturnResult{}, problem.Conflict("AFTER_SALE_NOT_ELIGIBLE", "delivery return order items not found")
	}

	fee := order.DeliveryFeeAmount
	if fee < 0 {
		fee = 0
	}
	if fee > remaining {
		fee = remaining
	}
	goodsRefund := remaining - fee
	allocations := allocateSystemRefund(orderItems, goodsRefund)
	now := s.now().UTC()
	afterSaleID := s.ids.Next()
	resolution := "refund_only"
	reason := strings.TrimSpace(req.ReasonCode)
	description := strings.TrimSpace(req.Description)
	if description == "" {
		description = "system-created after-sale for failed delivery return"
	}
	row := AfterSale{
		ID: afterSaleID, AfterSaleNo: "AS" + idString(afterSaleID), OrderID: order.ID,
		CustomerID: order.CustomerID, MerchantID: order.MerchantID, ShopID: order.ShopID,
		InitiatorType: "system", SourceType: "delivery_return", SourceID: &req.DeliveryReturnID,
		Type: "other", RequestedResolution: resolution, ApprovedResolution: &resolution,
		Status: "refund_processing", RequestedAmount: remaining, ApprovedAmount: remaining,
		IncludeDeliveryFee: fee > 0, ReasonCode: optional(reason), Description: description,
		SubmittedAt: now, ApprovedAt: &now, Version: 1,
	}
	items := make([]Item, 0, len(orderItems))
	for index, source := range orderItems {
		amount := allocations[index]
		items = append(items, Item{
			ID: s.ids.Next(), AfterSaleID: afterSaleID, OrderID: order.ID, OrderItemID: source.ID,
			ShopProductID: source.ShopProductID, ProductID: source.ProductID,
			RequestedQuantity: source.Quantity, ApprovedQuantity: source.Quantity,
			RequestedAmount: amount, ApprovedAmount: amount, ReturnDisposition: "none",
		})
	}
	history := s.history(afterSaleID, "admin", req.ApprovedBy, "system_approve", nil, strPtr("refund_processing"), reason, description)
	audit := s.audit(ctx, "admin", req.ApprovedBy, "after_sale.system_delivery_return", afterSaleID, nil, row)
	outbox := s.outbox(ctx, "after_sale.approved", afterSaleID, map[string]any{
		"after_sale_id": idString(afterSaleID), "order_id": idString(order.ID),
		"source_type": "delivery_return", "source_id": idString(req.DeliveryReturnID),
	})
	if err := s.repo.Create(ctx, tx, &row, items, nil, history, audit, outbox); err != nil {
		return SystemDeliveryReturnResult{}, err
	}
	if err := s.createRefundTask(ctx, tx, row, items, remaining, now); err != nil {
		return SystemDeliveryReturnResult{}, err
	}
	refund, err := s.repo.RefundByAfterSale(ctx, tx, afterSaleID)
	if err != nil {
		return SystemDeliveryReturnResult{}, err
	}
	if err := s.repo.UpdateOrderSummary(ctx, tx, order.ID, "processing"); err != nil {
		return SystemDeliveryReturnResult{}, err
	}
	return SystemDeliveryReturnResult{AfterSaleID: afterSaleID, RefundID: refund.ID, RefundNo: refund.RefundNo, Amount: remaining}, nil
}

// allocateSystemRefund deterministically assigns the refundable goods amount
// using immutable order-item totals. The final item absorbs integer rounding.
func allocateSystemRefund(items []OrderItemRow, amount int64) []int64 {
	result := make([]int64, len(items))
	if len(items) == 0 || amount <= 0 {
		return result
	}
	var weight int64
	for _, item := range items {
		if item.TotalAmount > 0 {
			weight += item.TotalAmount
		}
	}
	if weight <= 0 {
		result[len(result)-1] = amount
		return result
	}
	var assigned int64
	for index := range items {
		if index == len(items)-1 {
			result[index] = amount - assigned
			break
		}
		if items[index].TotalAmount > 0 {
			result[index] = amount * items[index].TotalAmount / weight
			assigned += result[index]
		}
	}
	return result
}

func (r SystemDeliveryReturnResult) String() string {
	return fmt.Sprintf("after_sale=%d refund=%d amount=%d", r.AfterSaleID, r.RefundID, r.Amount)
}
