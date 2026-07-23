package order

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestCustomerOrderListFiltersUseClosedContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("GET", "/?status=paid&order_by=created_at%20desc%2Cid%20desc", nil)
	filters, err := customerOrderListFiltersFromGin(ctx)
	if err != nil || filters.Status != "paid" {
		t.Fatalf("unexpected customer order filters: %+v err=%v", filters, err)
	}
	ctx.Request = httptest.NewRequest("GET", "/?filter=status:paid", nil)
	if _, err := customerOrderListFiltersFromGin(ctx); problem.FromError(err).ErrorCode != "VALIDATION_INVALID_QUERY" {
		t.Fatalf("generic filter must be rejected, got %v", err)
	}
}

func TestOrderDetailProjectionUsesHistoricalSnapshotsAndSafeSummaries(t *testing.T) {
	now := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	remark := "历史备注"
	row := Order{
		ID: 100, OrderNo: "O100", ShopID: 4201, Status: "paid", PayStatus: "succeeded", DeliveryStatus: "pending_assign",
		GoodsAmount: 10000, DiscountAmount: 500, DeliveryFeeAmount: 500, PayableAmount: 10000, PaidAmount: 10000,
		Remark: &remark, Version: 3, CreatedAt: now.Add(-time.Hour), UpdatedAt: now, PaidAt: &now,
		AddressSnapshot: datatypes.JSON(`{
			"contact_name":"张三","contact_phone":"13800138000","province":"广东省","city":"深圳市","city_code":"440300",
			"district":"南山区","district_code":"440305","address_detail":"科技园1号","formatted_address":"深圳市南山区科技园1号",
			"latitude":22.54,"longitude":113.93,"coordinate_system":"gcj02","location_source":"map_pin","geocode_provider":"amap",
			"geocode_status":"verified","address_version":2
		}`),
		DeliveryPromiseSnapshot: datatypes.JSON(`{
			"schema_version":2,"service_area_version":7,"selection_source":"automatic","resolved_at":"2026-07-22T09:00:00Z",
			"route":{"provider":"amap","raw_payload":"must-not-leak"},
			"delivery_promise":{"delivery_fee_amount":500,"eta_min_minutes":20,"eta_max_minutes":40,"route_source":"amap","confirmed":true}
		}`),
		ComplianceSnapshot: datatypes.JSON(`{
			"policy_version":"cp1-v2","status":"verified","adult_result":"adult","would_allow":true,
			"verification_level":"identity_and_liveness","checked_at":"2026-07-22T09:00:00Z",
			"verification_id":"must-not-leak","age_restricted_shop_product_ids":["8001"]
		}`),
	}
	items := []OrderItem{{
		ID: 200, OrderID: 100, ShopProductID: 8001, ProductID: 7001, Quantity: 2,
		ProductSnapshot: datatypes.JSON(`{"name":"下单时商品名","spec":"750ml","image_url":"https://example.test/old.jpg","age_restricted":true}`),
		SalePriceAmount: 5000, TotalAmount: 10000,
	}}
	providerTradeNo := "wx-secret-trade-no"
	payment := Payment{PaymentNo: "P100", OrderID: 100, Channel: "miniapp", Provider: "wechat", ProviderTradeNo: &providerTradeNo, Status: "succeeded", Amount: 10000, Currency: "CNY", PaidAt: &now, ClientPayload: datatypes.JSON(`{"paySign":"secret"}`)}

	dto := orderDetailDTO(row, OrderShop{ID: 4201, Name: "南山店"}, items, payment, true)
	if dto.ItemSummary.Name != "下单时商品名" || dto.Items[0].ImageURL != "https://example.test/old.jpg" || dto.AddressSnapshot.SnapshotQuality != "complete" {
		t.Fatalf("unexpected order detail snapshot projection: %+v", dto)
	}
	if dto.ComplianceSummary.Status != "verified" || dto.DeliveryPromise == nil || dto.DeliveryPromise.RouteSource != "amap" || dto.PaymentSummary == nil {
		t.Fatalf("unexpected safe summaries: %+v", dto)
	}
	payload, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"verification_id", "adult_result", "raw_payload", providerTradeNo, "paySign", "client_payload", "provider_trade_no"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("order detail leaked %q: %s", forbidden, payload)
		}
	}
}

func TestCustomerOrderAddressProjectionMarksLegacyWithoutFabrication(t *testing.T) {
	dto := customerOrderAddressProjection(datatypes.JSON(`{"contact_name":"李四","contact_phone":"13900139000","province":"广东省","city":"深圳市","district":"南山区","address_detail":"旧地址"}`))
	if dto.SnapshotQuality != "legacy_incomplete" || dto.CityCode != nil || dto.Latitude != nil || dto.FormattedAddress != nil {
		t.Fatalf("legacy address was fabricated: %+v", dto)
	}
}
