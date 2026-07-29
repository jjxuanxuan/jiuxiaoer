package wechat

import (
	"bytes"
	"context"
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
	"jiuxiaoer-admin/backend-go/internal/modules/notification"
)

const (
	subscriptionSendPath = "/cgi-bin/message/subscribe/send"
	accessTokenPath      = "/cgi-bin/token"
	maxWeChatResponse    = 1 << 20
)

// SubscriptionMessageProvider 是微信小程序一次性订阅消息的真实适配器。
// 应用访问令牌只保存在内存中，错误和结果均不会暴露令牌或接收方。
type SubscriptionMessageProvider struct {
	appID                  string
	appSecret              string
	baseURL                string
	page                   string
	productNameField       string
	remainingQuantityField string
	expiryDateField        string
	http                   *http.Client
	now                    func() time.Time

	tokenMu        sync.Mutex
	accessToken    string
	accessTokenExp time.Time
}

type subscriptionReminderPayload struct {
	ProductShortName string `json:"product_short_name"`
	RemainingBottles uint   `json:"remaining_bottles"`
	ExpiryDate       string `json:"expiry_date"`
}

type wechatAPIResponse struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

// NewSubscriptionMessageProvider 仅在专用服务商开关开启时构造实现。
// 客户通道在后台任务中还有独立开关，只有两个开关同时开启才会发起外部调用。
func NewSubscriptionMessageProvider(
	wechatConfig config.WeChatConfig,
	wineTicketConfig config.WineTicketConfig,
) (notification.Provider, error) {
	if !wineTicketConfig.WeChatReminderProviderEnabled {
		return &notification.UnavailableProvider{}, nil
	}
	if strings.TrimSpace(wechatConfig.MiniAppID) == "" ||
		strings.TrimSpace(wechatConfig.MiniAppSecret) == "" {
		return nil, errors.New("WeChat subscription provider requires Mini Program credentials")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(wechatConfig.APIBaseURL), "/")
	parsedBaseURL, err := url.Parse(baseURL)
	if err != nil || parsedBaseURL.Scheme == "" || parsedBaseURL.Host == "" {
		return nil, errors.New("WeChat subscription provider requires a valid API base URL")
	}
	if wechatConfig.HTTPTimeout <= 0 {
		return nil, errors.New("WeChat subscription provider requires a positive HTTP timeout")
	}
	if err := validateSubscriptionMessageMapping(wineTicketConfig); err != nil {
		return nil, err
	}
	return &SubscriptionMessageProvider{
		appID:                  strings.TrimSpace(wechatConfig.MiniAppID),
		appSecret:              strings.TrimSpace(wechatConfig.MiniAppSecret),
		baseURL:                baseURL,
		page:                   strings.TrimSpace(wineTicketConfig.WeChatReminderPage),
		productNameField:       strings.TrimSpace(wineTicketConfig.WeChatReminderProductNameField),
		remainingQuantityField: strings.TrimSpace(wineTicketConfig.WeChatReminderRemainingQuantityField),
		expiryDateField:        strings.TrimSpace(wineTicketConfig.WeChatReminderExpiryDateField),
		http:                   &http.Client{Timeout: wechatConfig.HTTPTimeout},
		now:                    time.Now,
	}, nil
}

func validateSubscriptionMessageMapping(cfg config.WineTicketConfig) error {
	page := strings.TrimSpace(cfg.WeChatReminderPage)
	if !validMiniProgramPage(page) {
		return errors.New("WeChat subscription provider requires a safe Mini Program landing page")
	}
	fields := []string{
		strings.TrimSpace(cfg.WeChatReminderProductNameField),
		strings.TrimSpace(cfg.WeChatReminderRemainingQuantityField),
		strings.TrimSpace(cfg.WeChatReminderExpiryDateField),
	}
	seen := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		if !validTemplateField(field) {
			return errors.New("WeChat subscription provider requires valid template field mappings")
		}
		if _, duplicate := seen[field]; duplicate {
			return errors.New("WeChat subscription provider template field mappings must be unique")
		}
		seen[field] = struct{}{}
	}
	return nil
}

func validMiniProgramPage(page string) bool {
	if !strings.HasPrefix(page, "pages/") || len(page) > 256 {
		return false
	}
	for _, character := range page {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '/', character == '_', character == '-':
		default:
			return false
		}
	}
	return true
}

func validTemplateField(field string) bool {
	if len(field) < 2 || len(field) > 64 {
		return false
	}
	for index, character := range field {
		if index == 0 {
			if (character < 'a' || character > 'z') &&
				(character < 'A' || character > 'Z') {
				return false
			}
			continue
		}
		if (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') &&
			character != '_' {
			return false
		}
	}
	return true
}

// Send 使用用户一次性授权中记录的 OpenID 和准确模板 ID。
// 请求发出后的传输失败属于终态或未知状态；
// 由于微信 API 没有幂等键，本适配器不会重试。
func (p *SubscriptionMessageProvider) Send(
	ctx context.Context,
	req notification.SendRequest,
) (notification.SendResult, error) {
	recipient := strings.TrimSpace(req.Recipient)
	templateID := strings.TrimSpace(req.TemplateID)
	if recipient == "" || templateID == "" {
		return notification.SendResult{}, &notification.ProviderError{
			Code: "wechat_subscription_request_invalid", Retryable: false,
		}
	}
	payload, err := decodeSubscriptionReminderPayload(req.Payload)
	if err != nil {
		return notification.SendResult{}, &notification.ProviderError{
			Code: "wechat_subscription_payload_invalid", Retryable: false,
		}
	}
	token, err := p.token(ctx)
	if err != nil {
		return notification.SendResult{}, err
	}
	result, providerErr := p.sendWithToken(ctx, token, req.ProviderRequestID, recipient, templateID, payload)
	if providerErr == nil {
		return result, nil
	}
	if isAccessTokenError(providerErr.Code) {
		p.invalidateToken(token)
		refreshedToken, refreshErr := p.token(ctx)
		if refreshErr != nil {
			return notification.SendResult{}, refreshErr
		}
		// 令牌错误属于明确的服务商拒绝，消息并未被接收，
		// 因此可以安全刷新并重试一次；其他失败均不重试。
		retryResult, retryErr := p.sendWithToken(
			ctx, refreshedToken, req.ProviderRequestID, recipient, templateID, payload,
		)
		if retryErr != nil {
			return notification.SendResult{}, retryErr
		}
		return retryResult, nil
	}
	return notification.SendResult{}, providerErr
}

func decodeSubscriptionReminderPayload(raw []byte) (subscriptionReminderPayload, error) {
	var payload subscriptionReminderPayload
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return payload, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return payload, errors.New("trailing payload data")
	}
	payload.ProductShortName = strings.TrimSpace(payload.ProductShortName)
	payload.ExpiryDate = strings.TrimSpace(payload.ExpiryDate)
	if payload.ProductShortName == "" || payload.RemainingBottles == 0 ||
		payload.ExpiryDate == "" {
		return payload, errors.New("missing reminder payload field")
	}
	if _, err := time.Parse("2006-01-02", payload.ExpiryDate); err != nil {
		return payload, errors.New("invalid expiry date")
	}
	return payload, nil
}

func (p *SubscriptionMessageProvider) sendWithToken(
	ctx context.Context,
	token, providerRequestID, recipient, templateID string,
	payload subscriptionReminderPayload,
) (notification.SendResult, *notification.ProviderError) {
	body, err := json.Marshal(map[string]any{
		"touser":      recipient,
		"template_id": templateID,
		"page":        p.page,
		"data": map[string]any{
			p.productNameField: map[string]string{
				"value": payload.ProductShortName,
			},
			p.remainingQuantityField: map[string]string{
				"value": fmt.Sprintf("%d", payload.RemainingBottles),
			},
			p.expiryDateField: map[string]string{
				"value": payload.ExpiryDate,
			},
		},
	})
	if err != nil {
		return notification.SendResult{}, &notification.ProviderError{
			Code: "wechat_subscription_request_invalid", Retryable: false,
		}
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		p.baseURL+subscriptionSendPath+"?access_token="+url.QueryEscape(token),
		bytes.NewReader(body),
	)
	if err != nil {
		return notification.SendResult{}, &notification.ProviderError{
			Code: "wechat_subscription_request_invalid", Retryable: false,
		}
	}
	request.Header.Set("Content-Type", "application/json")
	var response wechatAPIResponse
	if providerErr := p.doProviderJSON(request, &response, true); providerErr != nil {
		return notification.SendResult{}, providerErr
	}
	if response.ErrCode != 0 {
		return notification.SendResult{}, mapSubscriptionProviderError(response.ErrCode)
	}
	return notification.SendResult{
		ProviderRequestID: strings.TrimSpace(providerRequestID),
		Status:            "succeeded",
	}, nil
}

func (p *SubscriptionMessageProvider) token(ctx context.Context) (string, error) {
	p.tokenMu.Lock()
	defer p.tokenMu.Unlock()
	now := p.now()
	if p.accessToken != "" && now.Before(p.accessTokenExp) {
		return p.accessToken, nil
	}
	query := url.Values{
		"grant_type": {"client_credential"},
		"appid":      {p.appID},
		"secret":     {p.appSecret},
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, p.baseURL+accessTokenPath+"?"+query.Encode(), nil,
	)
	if err != nil {
		return "", &notification.ProviderError{
			Code: "wechat_access_token_request_invalid", Retryable: false,
		}
	}
	var response struct {
		wechatAPIResponse
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if providerErr := p.doProviderJSON(request, &response, false); providerErr != nil {
		return "", providerErr
	}
	if response.ErrCode != 0 || strings.TrimSpace(response.AccessToken) == "" ||
		response.ExpiresIn <= 0 {
		return "", mapAccessTokenProviderError(response.ErrCode)
	}
	ttl := time.Duration(response.ExpiresIn) * time.Second
	skew := ttl / 10
	if skew > 5*time.Minute {
		skew = 5 * time.Minute
	}
	if skew < time.Second {
		skew = time.Second
	}
	cacheTTL := ttl - skew
	if cacheTTL <= 0 {
		cacheTTL = ttl / 2
	}
	p.accessToken = strings.TrimSpace(response.AccessToken)
	p.accessTokenExp = now.Add(cacheTTL)
	return p.accessToken, nil
}

func (p *SubscriptionMessageProvider) invalidateToken(token string) {
	p.tokenMu.Lock()
	defer p.tokenMu.Unlock()
	if p.accessToken == token {
		p.accessToken = ""
		p.accessTokenExp = time.Time{}
	}
}

func (p *SubscriptionMessageProvider) doProviderJSON(
	request *http.Request,
	target any,
	sendAttempt bool,
) *notification.ProviderError {
	response, err := p.http.Do(request)
	if err != nil {
		return &notification.ProviderError{
			Code:      providerTransportCode(sendAttempt),
			Retryable: !sendAttempt,
			Unknown:   sendAttempt,
		}
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		code := "wechat_provider_http_error"
		if !sendAttempt {
			code = "wechat_access_token_http_error"
		}
		return &notification.ProviderError{
			Code:      code,
			Retryable: true,
			Unknown:   sendAttempt,
		}
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxWeChatResponse))
	if err := decoder.Decode(target); err != nil {
		code := "wechat_provider_response_invalid"
		if !sendAttempt {
			code = "wechat_access_token_response_invalid"
		}
		return &notification.ProviderError{
			Code:      code,
			Retryable: true,
			Unknown:   sendAttempt,
		}
	}
	return nil
}

func providerTransportCode(sendAttempt bool) string {
	if sendAttempt {
		return "wechat_subscription_result_unknown"
	}
	return "wechat_access_token_unavailable"
}

func mapAccessTokenProviderError(code int) *notification.ProviderError {
	return &notification.ProviderError{
		Code:      fmt.Sprintf("wechat_access_token_error_%d", safeWeChatCode(code)),
		Retryable: code == -1,
	}
}

func mapSubscriptionProviderError(code int) *notification.ProviderError {
	switch code {
	case 43101:
		return &notification.ProviderError{
			Code: "wechat_subscription_quota_exhausted", Retryable: false,
		}
	case 40037:
		return &notification.ProviderError{
			Code: "wechat_subscription_template_invalid", Retryable: false,
		}
	case 47003:
		return &notification.ProviderError{
			Code: "wechat_subscription_payload_rejected", Retryable: false,
		}
	case 40003:
		return &notification.ProviderError{
			Code: "wechat_subscription_recipient_invalid", Retryable: false,
		}
	case -1, 45009:
		return &notification.ProviderError{
			Code: "wechat_subscription_result_unknown", Retryable: true, Unknown: true,
		}
	default:
		return &notification.ProviderError{
			Code: fmt.Sprintf("wechat_subscription_error_%d", safeWeChatCode(code)),
		}
	}
}

func isAccessTokenError(code string) bool {
	switch code {
	case "wechat_subscription_error_40001",
		"wechat_subscription_error_40014",
		"wechat_subscription_error_41001",
		"wechat_subscription_error_42001":
		return true
	default:
		return false
	}
}

func safeWeChatCode(code int) int {
	if code == 0 {
		return -99999
	}
	return code
}

// Query 被有意设计为不支持：
// 小程序订阅消息 API 不提供消息状态查询接口。
func (*SubscriptionMessageProvider) Query(
	context.Context,
	string,
) (notification.SendResult, error) {
	return notification.SendResult{}, &notification.ProviderError{
		Code: "wechat_subscription_query_unsupported", Retryable: false, Unknown: true,
	}
}
