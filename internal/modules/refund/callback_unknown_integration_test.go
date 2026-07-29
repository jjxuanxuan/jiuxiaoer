package refund

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestUnknownRefundCallbackStaysFailed(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run unknown refund callback acceptance")
	}
	db := openRefundAcceptanceDB(t)
	eventID := "unknown-refund-must-retry"
	t.Cleanup(func() {
		db.Exec("DELETE FROM refund_callbacks WHERE provider=? AND provider_event_id=?", "wechat", eventID)
	})

	provider := &acceptanceProvider{callbackState: State{
		RefundNo: "REFUND-DOES-NOT-EXIST", PaymentNo: "PAYMENT-DOES-NOT-EXIST",
		Status: "SUCCESS", Amount: 100, TotalAmount: 100, Currency: "CNY",
	}}
	cfg := config.Load()
	cfg.WeChat.PayMockEnabled = true
	service := NewService(cfg, db, snowflake.New(986), provider)
	router := gin.New()
	router.POST("/refunds/:provider/callbacks", NewHandler(service).Callback)

	for attempt := 0; attempt < 2; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/refunds/wechat/callbacks", bytes.NewReader([]byte(`{"unknown":true}`)))
		request.Header.Set("X-Event-ID", eventID)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("attempt %d status=%d body=%s", attempt+1, response.Code, response.Body.String())
		}
	}
	var status, code string
	if err := db.Table("refund_callbacks").Select("process_status,error_code").
		Where("provider=? AND provider_event_id=?", "wechat", eventID).
		Row().Scan(&status, &code); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || code != "REFUND_NOT_FOUND" {
		t.Fatalf("callback status=%q code=%q", status, code)
	}
}
