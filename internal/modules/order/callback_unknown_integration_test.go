package order_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestUnknownPaymentCallbackStaysFailed(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run unknown payment callback acceptance")
	}
	db := openPaymentAcceptanceDB(t)
	eventID := "unknown-payment-must-retry"
	t.Cleanup(func() {
		db.Exec("DELETE FROM payment_callbacks WHERE provider=? AND provider_event_id=?", "wechat", eventID)
	})

	cfg := config.Load()
	cfg.WeChat.PayEnabled = true
	cfg.WeChat.PayMockEnabled = true
	provider := &paymentAcceptanceProvider{callback: order.PaymentCallbackEvent{
		EventID: eventID, PaymentNo: "PAYMENT-DOES-NOT-EXIST", ProviderTradeNo: "provider-unknown",
		Status: "SUCCESS", Amount: 100, Currency: "CNY",
	}}
	service := order.NewService(cfg, db, snowflake.New(987)).WithPaymentProvider(provider, metrics.New("callback-unknown", ""))
	router := gin.New()
	router.POST("/payments/:provider/callbacks", order.NewHandler(service).PaymentCallback)

	for attempt := 0; attempt < 2; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/payments/wechat/callbacks", bytes.NewReader([]byte(`{"unknown":true}`)))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("attempt %d status=%d body=%s", attempt+1, response.Code, response.Body.String())
		}
	}
	var status, code string
	if err := db.Table("payment_callbacks").Select("process_status,error_code").
		Where("provider=? AND provider_event_id=?", "wechat", eventID).
		Row().Scan(&status, &code); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || code != "PAYMENT_NOT_FOUND" {
		t.Fatalf("callback status=%q code=%q", status, code)
	}
}
