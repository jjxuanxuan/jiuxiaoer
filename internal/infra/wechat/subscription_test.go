package wechat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/notification"
)

func TestSubscriptionMessageProviderUsesConsentRecipientMappingAndTokenCache(
	t *testing.T,
) {
	var tokenCalls atomic.Int32
	var sendCalls atomic.Int32
	var mu sync.Mutex
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case accessTokenPath:
			tokenCalls.Add(1)
			if r.URL.Query().Get("appid") != "wx-reminder-app" ||
				r.URL.Query().Get("secret") != "app-secret" {
				t.Fatal("token request did not use configured app credentials")
			}
			_, _ = w.Write([]byte(`{"access_token":"memory-only-token","expires_in":7200}`))
		case subscriptionSendPath:
			sendCalls.Add(1)
			if r.URL.Query().Get("access_token") != "memory-only-token" {
				t.Fatal("send request did not reuse the cached access token")
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			requests = append(requests, body)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := newSubscriptionTestProvider(t, server.URL)
	payload := []byte(`{
		"product_short_name":"典藏干红",
		"remaining_bottles":6,
		"expiry_date":"2026-08-03"
	}`)
	for _, requestID := range []string{"reminder-1", "reminder-2"} {
		result, err := provider.Send(context.Background(), notification.SendRequest{
			ProviderRequestID: requestID,
			TemplateID:        "consented-template-id",
			Recipient:         "active-openid",
			Payload:           payload,
		})
		if err != nil || result.Status != "succeeded" ||
			result.ProviderRequestID != requestID {
			t.Fatalf("send result=%+v err=%v", result, err)
		}
	}
	if tokenCalls.Load() != 1 || sendCalls.Load() != 2 {
		t.Fatalf("token calls=%d send calls=%d", tokenCalls.Load(), sendCalls.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("captured requests=%d", len(requests))
	}
	body := requests[0]
	if body["touser"] != "active-openid" ||
		body["template_id"] != "consented-template-id" ||
		body["page"] != "pages/wine-ticket/cabinet" {
		t.Fatalf("unexpected controlled envelope: %#v", body)
	}
	data, ok := body["data"].(map[string]any)
	if !ok || len(data) != 3 {
		t.Fatalf("unexpected template data: %#v", body["data"])
	}
	assertTemplateValue(t, data, "thing1", "典藏干红")
	assertTemplateValue(t, data, "number2", "6")
	assertTemplateValue(t, data, "date3", "2026-08-03")
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"memory-only-token", "lot_id", "customer_id", "remind_days",
	} {
		if string(encoded) != "" && containsBytes(encoded, forbidden) {
			t.Fatalf("outbound body contains forbidden field %q: %s", forbidden, encoded)
		}
	}
}

func TestSubscriptionMessageProviderMapsKnownAndUnknownFailures(t *testing.T) {
	t.Run("known exhausted consent", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == accessTokenPath {
				_, _ = w.Write([]byte(`{"access_token":"access-1","expires_in":7200}`))
				return
			}
			_, _ = w.Write([]byte(`{"errcode":43101,"errmsg":"user refuse"}`))
		}))
		defer server.Close()
		provider := newSubscriptionTestProvider(t, server.URL)
		_, err := provider.Send(context.Background(), subscriptionTestRequest())
		var providerErr *notification.ProviderError
		if !errors.As(err, &providerErr) ||
			providerErr.Code != "wechat_subscription_quota_exhausted" ||
			providerErr.Retryable || providerErr.Unknown {
			t.Fatalf("unexpected provider error: %#v", err)
		}
	})

	t.Run("network outcome unknown is never resent", func(t *testing.T) {
		var sendCalls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == accessTokenPath {
				_, _ = w.Write([]byte(`{"access_token":"access-1","expires_in":7200}`))
				return
			}
			sendCalls.Add(1)
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("test server does not support hijacking")
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_ = connection.Close()
		}))
		defer server.Close()
		provider := newSubscriptionTestProvider(t, server.URL)
		_, err := provider.Send(context.Background(), subscriptionTestRequest())
		var providerErr *notification.ProviderError
		if !errors.As(err, &providerErr) || !providerErr.Unknown ||
			providerErr.Code != "wechat_subscription_result_unknown" {
			t.Fatalf("unexpected provider error: %#v", err)
		}
		if sendCalls.Load() != 1 {
			t.Fatalf("unknown send was retried: calls=%d", sendCalls.Load())
		}
	})
}

func TestSubscriptionMessageProviderRefreshesExplicitlyRejectedTokenOnce(t *testing.T) {
	var tokenCalls atomic.Int32
	var sendCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case accessTokenPath:
			call := tokenCalls.Add(1)
			_, _ = w.Write([]byte(`{"access_token":"access-` + string(rune('0'+call)) + `","expires_in":7200}`))
		case subscriptionSendPath:
			call := sendCalls.Add(1)
			if call == 1 {
				_, _ = w.Write([]byte(`{"errcode":42001,"errmsg":"expired"}`))
				return
			}
			if r.URL.Query().Get("access_token") != "access-2" {
				t.Fatal("explicit token rejection did not refresh cached token")
			}
			_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
		}
	}))
	defer server.Close()
	provider := newSubscriptionTestProvider(t, server.URL)
	result, err := provider.Send(context.Background(), subscriptionTestRequest())
	if err != nil || result.Status != "succeeded" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if tokenCalls.Load() != 2 || sendCalls.Load() != 2 {
		t.Fatalf("token calls=%d send calls=%d", tokenCalls.Load(), sendCalls.Load())
	}
}

func newSubscriptionTestProvider(
	t *testing.T,
	baseURL string,
) *SubscriptionMessageProvider {
	t.Helper()
	wechatConfig := config.Load().WeChat
	wechatConfig.MiniAppID = "wx-reminder-app"
	wechatConfig.MiniAppSecret = "app-secret"
	wechatConfig.APIBaseURL = baseURL
	wechatConfig.HTTPTimeout = time.Second
	wineTicketConfig := config.Load().WineTicket
	wineTicketConfig.WeChatReminderProviderEnabled = true
	wineTicketConfig.WeChatReminderProductNameField = "thing1"
	wineTicketConfig.WeChatReminderRemainingQuantityField = "number2"
	wineTicketConfig.WeChatReminderExpiryDateField = "date3"
	wineTicketConfig.WeChatReminderPage = "pages/wine-ticket/cabinet"
	provider, err := NewSubscriptionMessageProvider(wechatConfig, wineTicketConfig)
	if err != nil {
		t.Fatal(err)
	}
	realProvider, ok := provider.(*SubscriptionMessageProvider)
	if !ok {
		t.Fatalf("provider type=%T", provider)
	}
	return realProvider
}

func subscriptionTestRequest() notification.SendRequest {
	return notification.SendRequest{
		ProviderRequestID: "reminder-1",
		TemplateID:        "consented-template-id",
		Recipient:         "active-openid",
		Payload: []byte(`{
			"product_short_name":"典藏干红",
			"remaining_bottles":6,
			"expiry_date":"2026-08-03"
		}`),
	}
}

func assertTemplateValue(
	t *testing.T,
	data map[string]any,
	field, expected string,
) {
	t.Helper()
	item, ok := data[field].(map[string]any)
	if !ok || item["value"] != expected {
		t.Fatalf("field %s=%#v want %q", field, data[field], expected)
	}
}

func containsBytes(value []byte, expected string) bool {
	for index := 0; index+len(expected) <= len(value); index++ {
		if string(value[index:index+len(expected)]) == expected {
			return true
		}
	}
	return false
}
