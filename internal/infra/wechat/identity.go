package wechat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
)

// NewIdentityProvider 创建并初始化身份提供器。
func NewIdentityProvider(cfg config.WeChatConfig) auth.WeChatProvider {
	if !cfg.AuthEnabled {
		return nil
	}
	if cfg.AuthMockEnabled {
		return &mockIdentityProvider{appID: cfg.MiniAppID}
	}
	return &identityClient{
		appID:     cfg.MiniAppID,
		appSecret: cfg.MiniAppSecret,
		baseURL:   strings.TrimRight(cfg.APIBaseURL, "/"),
		http:      &http.Client{Timeout: cfg.HTTPTimeout},
	}
}

type mockIdentityProvider struct {
	appID string
}

// ExchangeCode 返回Exchange 代码。
func (m *mockIdentityProvider) ExchangeCode(_ context.Context, code string) (auth.WeChatIdentityResult, error) {
	if !strings.HasPrefix(code, "test-code-") {
		return auth.WeChatIdentityResult{}, auth.ErrWeChatCodeInvalid
	}
	sum := sha256.Sum256([]byte(code))
	return auth.WeChatIdentityResult{AppID: m.appID, OpenID: "test-openid-" + hex.EncodeToString(sum[:16])}, nil
}

// ResolvePhone 返回Resolve 手机号。
func (m *mockIdentityProvider) ResolvePhone(_ context.Context, phoneCode string) (string, error) {
	phone := strings.TrimPrefix(phoneCode, "test-phone-")
	if phone == phoneCode || len(phone) != 11 {
		return "", auth.ErrWeChatCodeInvalid
	}
	return phone, nil
}

type identityClient struct {
	appID     string
	appSecret string
	baseURL   string
	http      *http.Client
	mu        sync.Mutex
	token     string
	tokenExp  time.Time
}

type wechatError struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

// ExchangeCode 返回Exchange 代码。
func (c *identityClient) ExchangeCode(ctx context.Context, code string) (auth.WeChatIdentityResult, error) {
	query := url.Values{
		"appid":      []string{c.appID},
		"secret":     []string{c.appSecret},
		"js_code":    []string{code},
		"grant_type": []string{"authorization_code"},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/sns/jscode2session?"+query.Encode(), nil)
	if err != nil {
		return auth.WeChatIdentityResult{}, fmt.Errorf("%w: build session request", auth.ErrWeChatProviderUnavailable)
	}
	var response struct {
		wechatError
		OpenID     string `json:"openid"`
		UnionID    string `json:"unionid"`
		SessionKey string `json:"session_key"`
	}
	if err := c.doJSON(request, &response); err != nil {
		return auth.WeChatIdentityResult{}, err
	}
	if response.ErrCode == 40029 || response.ErrCode == 40163 || response.OpenID == "" {
		return auth.WeChatIdentityResult{}, auth.ErrWeChatCodeInvalid
	}
	if response.ErrCode != 0 {
		return auth.WeChatIdentityResult{}, fmt.Errorf("%w: code exchange failed", auth.ErrWeChatProviderUnavailable)
	}
	sessionHash := sha256.Sum256([]byte(response.SessionKey))
	return auth.WeChatIdentityResult{
		AppID:          c.appID,
		OpenID:         response.OpenID,
		UnionID:        response.UnionID,
		SessionKeyHash: hex.EncodeToString(sessionHash[:]),
	}, nil
}

// ResolvePhone 返回Resolve 手机号。
func (c *identityClient) ResolvePhone(ctx context.Context, phoneCode string) (string, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]string{"code": phoneCode})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/wxa/business/getuserphonenumber?access_token="+url.QueryEscape(token), bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("%w: build phone request", auth.ErrWeChatProviderUnavailable)
	}
	request.Header.Set("Content-Type", "application/json")
	var response struct {
		wechatError
		PhoneInfo struct {
			PhoneNumber     string `json:"phoneNumber"`
			PurePhoneNumber string `json:"purePhoneNumber"`
		} `json:"phone_info"`
	}
	if err := c.doJSON(request, &response); err != nil {
		return "", err
	}
	if response.ErrCode == 40029 || response.ErrCode == 40163 {
		return "", auth.ErrWeChatCodeInvalid
	}
	if response.ErrCode != 0 || response.PhoneInfo.PurePhoneNumber == "" {
		return "", fmt.Errorf("%w: phone authorization failed", auth.ErrWeChatProviderUnavailable)
	}
	return response.PhoneInfo.PurePhoneNumber, nil
}

// accessToken 获取微信访问令牌。
func (c *identityClient) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenExp) {
		return c.token, nil
	}
	query := url.Values{
		"grant_type": []string{"client_credential"},
		"appid":      []string{c.appID},
		"secret":     []string{c.appSecret},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/cgi-bin/token?"+query.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("%w: build token request", auth.ErrWeChatProviderUnavailable)
	}
	var response struct {
		wechatError
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := c.doJSON(request, &response); err != nil {
		return "", err
	}
	if response.ErrCode != 0 || response.AccessToken == "" {
		return "", fmt.Errorf("%w: access token failed", auth.ErrWeChatProviderUnavailable)
	}
	ttl := time.Duration(response.ExpiresIn) * time.Second
	if ttl > 5*time.Minute {
		ttl -= 5 * time.Minute
	}
	c.token = response.AccessToken
	c.tokenExp = time.Now().Add(ttl)
	return c.token, nil
}

// doJSON 发送 JSON 请求并解析响应。
func (c *identityClient) doJSON(request *http.Request, target any) error {
	response, err := c.http.Do(request)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%w: request timeout", auth.ErrWeChatProviderUnavailable)
		}
		return fmt.Errorf("%w: request failed", auth.ErrWeChatProviderUnavailable)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return fmt.Errorf("%w: HTTP %d", auth.ErrWeChatProviderUnavailable, response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: invalid response", auth.ErrWeChatProviderUnavailable)
	}
	return nil
}
