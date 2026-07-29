package deliveryreturn

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	refundmodule "jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type closureRefundProvider struct{ state refundmodule.State }

func (p *closureRefundProvider) Code() string { return "wechat" }
func (p *closureRefundProvider) Refund(context.Context, refundmodule.Input) (refundmodule.State, error) {
	return p.state, nil
}
func (p *closureRefundProvider) QueryRefund(context.Context, string) (refundmodule.State, error) {
	return p.state, nil
}
func (p *closureRefundProvider) ParseRefundCallback(_ context.Context, request *http.Request) (refundmodule.CallbackEvent, error) {
	return refundmodule.CallbackEvent{EventID: request.Header.Get("X-Event-ID"), MchID: "local-mch", State: p.state}, nil
}

type closureFixture struct {
	orderID, paymentID, deliveryID, shopID, riderID, customerID, merchantID uint64
	merchantUserID, adminID, restockProductID, restockShopProductID         uint64
	paymentNo                                                               string
}

func insertClosureFixture(t *testing.T, tx *gorm.DB, ids *snowflake.Generator) closureFixture {
	t.Helper()
	fx := closureFixture{
		orderID: ids.Next(), paymentID: ids.Next(), deliveryID: ids.Next(), shopID: ids.Next(), riderID: ids.Next(),
		customerID: ids.Next(), merchantID: ids.Next(), merchantUserID: ids.Next(), adminID: ids.Next(),
		restockProductID: ids.Next(), restockShopProductID: ids.Next(),
	}
	fx.paymentNo = "PAY-DR-" + idString(fx.paymentID)
	now := time.Now().UTC()
	mustClosureExec(t, tx, `INSERT INTO orders
		(id,order_no,customer_id,merchant_id,shop_id,status,pay_status,delivery_status,goods_amount,delivery_fee_amount,payable_amount,paid_amount,refunded_amount,after_sale_status)
		VALUES (?,?,?,?,?,'delivering','succeeded','delivering',1800,200,2000,2000,0,'none')`,
		fx.orderID, "ORDER-DR-"+idString(fx.orderID), fx.customerID, fx.merchantID, fx.shopID)
	mustClosureExec(t, tx, `INSERT INTO payments
		(id,payment_no,biz_type,biz_id,order_id,customer_id,channel,provider,status,amount,refunded_amount,currency,paid_at)
		VALUES (?,?,'retail_order',?,?,?,'miniapp','wechat','succeeded',2000,0,'CNY',?)`,
		fx.paymentID, fx.paymentNo, fx.orderID, fx.orderID, fx.customerID, now)
	discardItemID, discardShopProductID, discardProductID := ids.Next(), ids.Next(), ids.Next()
	restockItemID := ids.Next()
	mustClosureExec(t, tx, `INSERT INTO order_items
		(id,order_id,shop_product_id,product_id,product_snapshot,quantity,sale_price_amount,total_amount)
		VALUES (?,?,?,?,?,2,600,1200)`, discardItemID, fx.orderID, discardShopProductID, discardProductID,
		`{"name":"prepared meal","return_policy":{"eligible":false,"policy_code":"food-discard","policy_version":"1"}}`)
	mustClosureExec(t, tx, `INSERT INTO order_items
		(id,order_id,shop_product_id,product_id,product_snapshot,quantity,sale_price_amount,total_amount)
		VALUES (?,?,?,?,?,1,600,600)`, restockItemID, fx.orderID, fx.restockShopProductID, fx.restockProductID,
		`{"name":"sealed bottle","return_policy":{"eligible":true,"policy_code":"sealed-goods","policy_version":"2"}}`)
	mustClosureExec(t, tx, `INSERT INTO product_stocks (id,shop_product_id,shop_id,product_id,available_qty,reserved_qty,version) VALUES (?,?,?,?,3,0,1)`,
		ids.Next(), discardShopProductID, fx.shopID, discardProductID)
	mustClosureExec(t, tx, `INSERT INTO product_stocks (id,shop_product_id,shop_id,product_id,available_qty,reserved_qty,version) VALUES (?,?,?,?,7,0,1)`,
		ids.Next(), fx.restockShopProductID, fx.shopID, fx.restockProductID)
	pickedUp := now.Add(-10 * time.Minute)
	mustClosureExec(t, tx, `INSERT INTO delivery_orders
		(id,order_id,shop_id,rider_id,status,assignment_version,picked_up_at,started_at)
		VALUES (?,?,?,?, 'delivering',3,?,?)`, fx.deliveryID, fx.orderID, fx.shopID, fx.riderID, pickedUp, pickedUp)
	return fx
}

func closureClaims(fx closureFixture) (*auth.Claims, *auth.Claims, *auth.Claims) {
	rider := &auth.Claims{AccountType: "rider", RiderID: idString(fx.riderID), Permissions: []string{"delivery_return:create", "delivery_return:view_own", "delivery_return:arrive"}}
	admin := &auth.Claims{AccountType: "admin", AdminUserID: idString(fx.adminID), Permissions: []string{"delivery_return:approve", "delivery_return:view_all", "delivery_return:list_all"}}
	store := &auth.Claims{AccountType: "merchant", MerchantUserID: idString(fx.merchantUserID), MerchantID: idString(fx.merchantID), AuthorizedShopIDs: []string{idString(fx.shopID)}, Permissions: []string{"delivery_return:receive_shop", "delivery_return:view_shop", "delivery_return:list_shop"}}
	return rider, admin, store
}

func closureReceiveRequest(arrived DTO, fx closureFixture) ReceiveReq {
	items := make([]ReceiveItemReq, 0, len(arrived.Items))
	for _, item := range arrived.Items {
		disposition := "discard"
		if item.ProductID == idString(fx.restockProductID) {
			disposition = "restock"
		}
		items = append(items, ReceiveItemReq{AfterSaleItemID: item.AfterSaleItemID, ReceivedQuantity: item.ExpectedQuantity, Disposition: disposition})
	}
	return ReceiveReq{ExpectedVersion: 3, HandoffCode: arrived.HandoffCode, Items: items}
}

func closureRefund(t *testing.T, tx *gorm.DB, afterSaleIDRaw string) refundmodule.Row {
	t.Helper()
	afterSaleID := mustReturnID(t, afterSaleIDRaw)
	var row refundmodule.Row
	if err := tx.Where("after_sale_id=?", afterSaleID).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func assertAtomicApproval(t *testing.T, tx *gorm.DB, returnIDRaw string, fx closureFixture, approved DTO) {
	t.Helper()
	returnID, afterSaleID := mustReturnID(t, returnIDRaw), mustReturnID(t, approved.AfterSaleID)
	var afterSales, refunds, refundItems int64
	tx.Table("after_sales").Where("id=? AND source_type='delivery_return' AND source_id=? AND initiator_type='system'", afterSaleID, returnID).Count(&afterSales)
	tx.Table("refunds").Where("after_sale_id=? AND amount=2000", afterSaleID).Count(&refunds)
	tx.Table("refund_items").Where("refund_id IN (SELECT id FROM refunds WHERE after_sale_id=?)", afterSaleID).Count(&refundItems)
	var deliveryStatus, orderStatus string
	tx.Table("delivery_orders").Select("status").Where("id=?", fx.deliveryID).Scan(&deliveryStatus)
	tx.Table("orders").Select("status").Where("id=?", fx.orderID).Scan(&orderStatus)
	if afterSales != 1 || refunds != 1 || refundItems != 2 || deliveryStatus != "returning" || orderStatus != "refunding" {
		t.Fatalf("approval atomicity after_sales=%d refunds=%d refund_items=%d delivery=%s order=%s", afterSales, refunds, refundItems, deliveryStatus, orderStatus)
	}
}

func assertClosureExactlyOnce(t *testing.T, tx *gorm.DB, returnIDRaw string, refundID uint64) {
	t.Helper()
	returnID := mustReturnID(t, returnIDRaw)
	var closedHistory, closedEvents, receipts, stockRows, callbackRows, refundEvents int64
	tx.Model(&History{}).Where("delivery_return_id=? AND action='close'", returnID).Count(&closedHistory)
	tx.Table("outbox_events").Where("aggregate_type='delivery_return' AND aggregate_id=? AND event_type='delivery.return_closed'", returnID).Count(&closedEvents)
	tx.Table("return_receipts").Where("after_sale_id IN (SELECT after_sale_id FROM refunds WHERE id=?)", refundID).Count(&receipts)
	tx.Table("stock_records").Where("source_type='delivery_return' AND source_id=?", returnID).Count(&stockRows)
	tx.Table("refund_callbacks").Where("refund_id=?", refundID).Count(&callbackRows)
	tx.Table("outbox_events").Where("aggregate_type='refund' AND aggregate_id=? AND event_type='refund.succeeded'", refundID).Count(&refundEvents)
	if closedHistory != 1 || closedEvents != 1 || receipts != 1 || stockRows != 1 || callbackRows != 1 || refundEvents != 1 {
		t.Fatalf("exactly-once mismatch close_history=%d close_events=%d receipts=%d stock=%d callbacks=%d refund_events=%d", closedHistory, closedEvents, receipts, stockRows, callbackRows, refundEvents)
	}
}

func stockAvailable(t *testing.T, tx *gorm.DB, shopProductID uint64) int {
	t.Helper()
	var quantity int
	if err := tx.Table("product_stocks").Select("available_qty").Where("shop_product_id=?", shopProductID).Scan(&quantity).Error; err != nil {
		t.Fatal(err)
	}
	return quantity
}

func mustClosureExec(t *testing.T, tx *gorm.DB, query string, args ...any) {
	t.Helper()
	if err := tx.Exec(query, args...).Error; err != nil {
		t.Fatalf("fixture query failed: %v", err)
	}
}

func mustReturnID(t *testing.T, raw string) uint64 {
	t.Helper()
	id, err := parseID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func cleanupClosureFixture(t *testing.T, db *gorm.DB, fx closureFixture) {
	t.Helper()
	var returnIDs, afterSaleIDs, refundIDs, receiptIDs []uint64
	_ = db.Table("delivery_returns").Where("order_id=?", fx.orderID).Pluck("id", &returnIDs).Error
	_ = db.Table("after_sales").Where("order_id=?", fx.orderID).Pluck("id", &afterSaleIDs).Error
	_ = db.Table("refunds").Where("order_id=?", fx.orderID).Pluck("id", &refundIDs).Error
	if len(afterSaleIDs) > 0 {
		_ = db.Table("return_receipts").Where("after_sale_id IN ?", afterSaleIDs).Pluck("id", &receiptIDs).Error
	}
	statements := []struct {
		query string
		args  []any
	}{
		{"DELETE FROM return_receipt_items WHERE return_receipt_id IN ?", []any{receiptIDs}},
		{"DELETE FROM return_receipts WHERE after_sale_id IN ?", []any{afterSaleIDs}},
		{"DELETE FROM refund_callbacks WHERE refund_id IN ?", []any{refundIDs}},
		{"DELETE FROM refund_items WHERE refund_id IN ?", []any{refundIDs}},
		{"DELETE FROM refunds WHERE order_id=?", []any{fx.orderID}},
		{"DELETE FROM after_sale_history WHERE after_sale_id IN ?", []any{afterSaleIDs}},
		{"DELETE FROM after_sale_evidence WHERE after_sale_id IN ?", []any{afterSaleIDs}},
		{"DELETE FROM after_sale_items WHERE after_sale_id IN ?", []any{afterSaleIDs}},
		{"DELETE FROM after_sales WHERE order_id=?", []any{fx.orderID}},
		{"DELETE FROM stock_records WHERE source_type='delivery_return' AND source_id IN ?", []any{returnIDs}},
		{"DELETE FROM delivery_return_history WHERE delivery_return_id IN ?", []any{returnIDs}},
		{"DELETE FROM delivery_returns WHERE order_id=?", []any{fx.orderID}},
		{"DELETE FROM outbox_events WHERE (aggregate_type='delivery_return' AND aggregate_id IN ?) OR (aggregate_type='after_sale' AND aggregate_id IN ?) OR (aggregate_type='refund' AND aggregate_id IN ?)", []any{returnIDs, afterSaleIDs, refundIDs}},
		{"DELETE FROM audit_logs WHERE (resource_type='delivery_return' AND resource_id IN ?) OR (resource_type='after_sale' AND resource_id IN ?) OR (resource_type='refund' AND resource_id IN ?)", []any{returnIDs, afterSaleIDs, refundIDs}},
		{"DELETE FROM idempotency_keys WHERE actor_id IN ?", []any{[]uint64{fx.riderID, fx.adminID, fx.merchantUserID}}},
		{"DELETE FROM product_stocks WHERE shop_id=?", []any{fx.shopID}},
		{"DELETE FROM order_items WHERE order_id=?", []any{fx.orderID}},
		{"DELETE FROM delivery_orders WHERE id=?", []any{fx.deliveryID}},
		{"DELETE FROM payments WHERE id=?", []any{fx.paymentID}},
		{"DELETE FROM orders WHERE id=?", []any{fx.orderID}},
	}
	for _, statement := range statements {
		skip := false
		for _, arg := range statement.args {
			switch values := arg.(type) {
			case []uint64:
				if len(values) == 0 {
					skip = true
				}
			}
		}
		if skip {
			continue
		}
		if err := db.Exec(statement.query, statement.args...).Error; err != nil {
			t.Errorf("cleanup query failed: %v", err)
		}
	}
}

func sampleValue(samples []metrics.Sample, name string) float64 {
	for _, sample := range samples {
		if sample.Name == name {
			return sample.Value
		}
	}
	return 0
}

func assertRequestFacts(t *testing.T, tx *gorm.DB, created DTO, deliveryID uint64) {
	t.Helper()
	returnID, err := parseID(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	var histories, audits, events, afterSales, refunds int64
	tx.Model(&History{}).Where("delivery_return_id=?", returnID).Count(&histories)
	tx.Table("audit_logs").Where("resource_type='delivery_return' AND resource_id=?", returnID).Count(&audits)
	tx.Table("outbox_events").Where("aggregate_type='delivery_return' AND aggregate_id=? AND event_type='delivery.return_requested'", returnID).Count(&events)
	tx.Table("after_sales").Where("source_type='delivery_return' AND source_id=?", returnID).Count(&afterSales)
	tx.Table("refunds").Where("after_sale_id IN (SELECT id FROM after_sales WHERE source_type='delivery_return' AND source_id=?)", returnID).Count(&refunds)
	if histories != 1 || audits != 1 || events != 1 || afterSales != 0 || refunds != 0 {
		t.Fatalf("request fact invariant histories=%d audits=%d events=%d after_sales=%d refunds=%d", histories, audits, events, afterSales, refunds)
	}
	var status string
	if err := tx.Table("delivery_orders").Select("status").Where("id=?", deliveryID).Scan(&status).Error; err != nil || status != "delivering" {
		t.Fatalf("request changed delivery status: status=%q err=%v", status, err)
	}
}

func assertIntegrationProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var details *problem.Details
	if !errors.As(err, &details) || details.ErrorCode != code {
		t.Fatalf("expected problem %s, got %v", code, err)
	}
}
