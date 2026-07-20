package wechatpay

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/wechatpay-apiv3/wechatpay-go/core"
	"github.com/wechatpay-apiv3/wechatpay-go/services/refunddomestic"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/paygateway"
)

// TestFakeProviderRequiresValidCallbackSignature 验证Fake 提供器 Requires 有效回调签名的预期行为。
func TestFakeProviderRequiresValidCallbackSignature(t *testing.T) {
	cfg := config.Load().WeChat
	cfg.PayMockEnabled = true
	provider, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("new fake provider: %v", err)
	}
	defer provider.Shutdown()
	body := []byte(`{"event_id":"event-1","payment_no":"PAY1","status":"SUCCESS","amount":100,"currency":"CNY"}`)
	request, _ := http.NewRequest(http.MethodPost, "/callback", bytes.NewReader(body))
	if _, err := provider.ParseCallback(context.Background(), request); err == nil {
		t.Fatal("expected unsigned callback to fail")
	}

	mac := hmac.New(sha256.New, []byte(FakeCallbackSecret))
	_, _ = mac.Write(body)
	request, _ = http.NewRequest(http.MethodPost, "/callback", bytes.NewReader(body))
	request.Header.Set("X-JXE-Fake-Signature", hex.EncodeToString(mac.Sum(nil)))
	event, err := provider.ParseCallback(context.Background(), request)
	if err != nil || event.EventID != "event-1" || event.Amount != 100 {
		t.Fatalf("parse signed callback: event=%+v err=%v", event, err)
	}
}

func TestProviderCallErrorPreservesWechatMetadata(t *testing.T) {
	err := providerCallError("refund.query", nil, &core.APIError{StatusCode: http.StatusNotFound, Code: "RESOURCE_NOT_EXISTS", Message: "not found", Header: http.Header{"Request-Id": []string{"wx-request-id"}}})
	providerErr, ok := paygateway.As(err)
	if !ok || providerErr.Operation != "refund.query" || providerErr.HTTPStatus != http.StatusNotFound || providerErr.Code != "RESOURCE_NOT_EXISTS" || providerErr.RequestID != "wx-request-id" || providerErr.Retryable {
		t.Fatalf("unexpected provider error: %+v", providerErr)
	}
}

func TestProviderCallErrorClassifiesRetryableWechatFailures(t *testing.T) {
	for _, apiErr := range []*core.APIError{
		{StatusCode: http.StatusInternalServerError, Code: "SYSTEM_ERROR"},
		{StatusCode: http.StatusTooManyRequests, Code: "FREQUENCY_LIMITED"},
	} {
		providerErr, ok := paygateway.As(providerCallError("refund.create", nil, apiErr))
		if !ok || !providerErr.Retryable {
			t.Fatalf("expected retryable error: %+v", providerErr)
		}
	}
	providerErr, ok := paygateway.As(providerCallError("refund.create", nil, &core.APIError{StatusCode: http.StatusForbidden, Code: "NOT_ENOUGH"}))
	if !ok || providerErr.Retryable {
		t.Fatalf("NOT_ENOUGH is a known failed application and must require operator action: %+v", providerErr)
	}
}

func TestRefundNotificationMatchesOfficialShapeWithoutCurrency(t *testing.T) {
	body := []byte(`{
		"mchid":"1900000100",
		"transaction_id":"1008450740201411110005820873",
		"out_trade_no":"PAY1",
		"refund_id":"50000000382019052709732678859",
		"out_refund_no":"RF1",
		"refund_status":"SUCCESS",
		"success_time":"2026-07-20T12:00:00+08:00",
		"amount":{"total":999,"refund":999,"payer_total":999,"payer_refund":999}
	}`)
	var notification refundNotification
	if err := notification.UnmarshalJSON(body); err != nil {
		t.Fatalf("unmarshal official refund notification: %v", err)
	}
	state := refundNotificationState(&notification)
	if state.CurrencyRequired || state.Currency != "" || state.Amount != 999 || state.TotalAmount != 999 || state.RefundNo != "RF1" || state.PaymentNo != "PAY1" {
		t.Fatalf("unexpected state: %+v", state)
	}
	want, _ := time.Parse(time.RFC3339, "2026-07-20T12:00:00+08:00")
	if state.SucceededAt == nil || !state.SucceededAt.Equal(want) {
		t.Fatalf("unexpected success time: %v", state.SucceededAt)
	}
}

func TestRefundAPIStateRequiresCurrency(t *testing.T) {
	refundID, refundNo, paymentNo, status := "5031", "RF1", "PAY1", refunddomestic.STATUS_SUCCESS
	state := refundState(&refunddomestic.Refund{RefundId: &refundID, OutRefundNo: &refundNo, OutTradeNo: &paymentNo, Status: &status, Amount: &refunddomestic.Amount{}})
	if !state.CurrencyRequired || state.Currency != "" {
		t.Fatalf("refund API state must preserve a missing required currency: %+v", state)
	}
}
