package wechatpay

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"

	"jiuxiaoer-admin/backend-go/internal/config"
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
