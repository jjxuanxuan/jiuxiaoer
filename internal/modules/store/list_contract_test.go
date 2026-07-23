package store

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestStoreOrderListFiltersUseClosedQueryContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	request := httptest.NewRequest("GET", "/?shop_id=4201&status=paid&keyword=O2026&paid_from=2026-07-22T00:00:00Z&paid_to=2026-07-23T00:00:00Z&order_by=created_at%20desc%2Cid%20desc", nil)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = request
	filters, err := storeOrderListFiltersFromGin(ctx)
	if err != nil {
		t.Fatalf("parse store order filters: %v", err)
	}
	if filters.ShopID != 4201 || filters.Status != "paid" || filters.Keyword != "O2026" || filters.PaidFrom == nil || filters.PaidTo == nil {
		t.Fatalf("unexpected store order filters: %+v", filters)
	}

	ctx.Request = httptest.NewRequest("GET", "/?filter=status:paid", nil)
	if _, err := storeOrderListFiltersFromGin(ctx); problem.FromError(err).ErrorCode != "VALIDATION_INVALID_QUERY" {
		t.Fatalf("generic filter must be rejected, got %v", err)
	}
}

func TestStoreInventoryFiltersAndProjectionExposeReconciledStock(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("GET", "/?shop_id=4201&status=on_sale&keyword=%E7%BA%A2%E9%85%92&low_stock_only=true&order_by=updated_at%20desc%2Cid%20desc", nil)
	filters, err := storeInventoryFiltersFromGin(ctx)
	if err != nil {
		t.Fatalf("parse inventory filters: %v", err)
	}
	if filters.ShopID != 4201 || !filters.LowStockOnly || filters.Keyword != "红酒" {
		t.Fatalf("unexpected inventory filters: %+v", filters)
	}

	now := time.Date(2026, 7, 22, 9, 30, 0, 0, time.UTC)
	dto := shopProductDTO(ShopProductRow{
		ID: 8001, MerchantID: 4001, ShopID: 4201, ProductID: 7001, CategoryID: 6001,
		Name: "红酒", SalePriceAmount: 9900, Status: "on_sale", AvailableQty: 3,
		ReservedQty: 2, LockedQty: 1, LowStockThreshold: 5, Version: 7, UpdatedAt: now,
	})
	if dto.ShopProductID != dto.ID || dto.TotalQty != 6 || !dto.LowStock || dto.LowStockThreshold != 5 || dto.Version != 7 || dto.UpdatedAt != now.Format(time.RFC3339) {
		t.Fatalf("unexpected inventory projection: %+v", dto)
	}
}

func TestStoreOrderSummaryUsesSnapshotsAndMasksAddress(t *testing.T) {
	remark := "请轻放"
	now := time.Date(2026, 7, 22, 9, 30, 0, 0, time.UTC)
	row := Order{
		ID: 1, OrderNo: "O1", ShopID: 4201, Status: "paid", PayStatus: "succeeded", DeliveryStatus: "pending_assign",
		PayableAmount: 9900, Remark: &remark, Version: 2, CreatedAt: now.Add(-time.Minute), UpdatedAt: now, PaidAt: &now,
		AddressSnapshot: datatypes.JSON(`{"contact_phone":"13800138000","district":"南山区","formatted_address":"南山区科技园科苑路15号"}`),
	}
	items := []OrderItem{{ID: 2, OrderID: 1, ProductID: 7001, ProductSnapshot: datatypes.JSON(`{"name":"历史商品名","spec":"750ml","image_url":"https://example.test/old.jpg"}`), Quantity: 2}}
	dto := storeOrderSummaryDTO(row, Shop{ID: 4201, Name: "南山店"}, items)
	if dto.ItemSummary.Name != "历史商品名" || dto.TotalQuantity != 2 || !dto.HasRemark || dto.CustomerContactMask != "138****8000" {
		t.Fatalf("unexpected store order summary: %+v", dto)
	}
	if dto.AddressSummary == "南山区科技园科苑路15号" || dto.AddressSummary == "" {
		t.Fatalf("address summary was not masked: %q", dto.AddressSummary)
	}
}
