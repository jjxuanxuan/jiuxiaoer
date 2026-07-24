package wechat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/config"
)

// TestIdentityClientExchangesCodeAndResolvesPhone 验证身份客户端交换登录码并解析手机号。
func TestIdentityClientExchangesCodeAndResolvesPhone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sns/jscode2session":
			if r.URL.Query().Get("js_code") != "valid-code" {
				t.Fatalf("unexpected js_code: %s", r.URL.Query().Get("js_code"))
			}
			_, _ = w.Write([]byte(`{"openid":"openid-1","unionid":"union-1","session_key":"session-secret"}`))
		case "/cgi-bin/token":
			_, _ = w.Write([]byte(`{"access_token":"access-1","expires_in":7200}`))
		case "/wxa/business/getuserphonenumber":
			if r.URL.Query().Get("access_token") != "access-1" {
				t.Fatal("phone request is missing access token")
			}
			_, _ = w.Write([]byte(`{"errcode":0,"phone_info":{"purePhoneNumber":"13900000001"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := config.Load().WeChat
	cfg.AuthMockEnabled = false
	cfg.MiniAppID = "wx-app-1"
	cfg.MiniAppSecret = "secret"
	cfg.APIBaseURL = server.URL
	cfg.HTTPTimeout = time.Second
	provider := NewIdentityProvider(cfg)
	identity, err := provider.ExchangeCode(context.Background(), "valid-code")
	if err != nil {
		t.Fatalf("exchange code: %v", err)
	}
	if identity.AppID != "wx-app-1" || identity.OpenID != "openid-1" || identity.SessionKeyHash == "" {
		t.Fatalf("unexpected identity: %+v", identity)
	}
	phone, err := provider.ResolvePhone(context.Background(), "phone-code")
	if err != nil || phone != "13900000001" {
		t.Fatalf("resolve phone: phone=%s err=%v", phone, err)
	}
}

// TestMockIdentityProviderRejectsNonTestCode 验证模拟身份服务商拒绝非测试登录码。
func TestMockIdentityProviderRejectsNonTestCode(t *testing.T) {
	cfg := config.Load().WeChat
	cfg.AuthMockEnabled = true
	provider := NewIdentityProvider(cfg)
	if _, err := provider.ExchangeCode(context.Background(), "production-looking-code"); err == nil {
		t.Fatal("expected mock provider to require explicit test code")
	}
}
