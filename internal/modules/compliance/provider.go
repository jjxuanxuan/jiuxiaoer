package compliance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	IdentitySignatureHeader = "X-JXE-Identity-Signature"
	IdentityTimestampHeader = "X-JXE-Identity-Timestamp"
)

type Provider interface {
	Code() string
	CreateSession(context.Context, ProviderSessionRequest) (ProviderSession, error)
	ParseCallback(context.Context, http.Header, []byte) (ProviderCallback, error)
	Query(context.Context, string) (ProviderResult, error)
}

type fakeSession struct {
	state  string
	result ProviderResult
}

// FakeProvider 为本地和集成环境模拟服务商托管会话。
// 只有此模拟实现会用回调内容填充查询结果；生产适配器必须独立查询服务商 API。
type FakeProvider struct {
	secret   string
	mu       sync.RWMutex
	sessions map[string]fakeSession
}

// NewFakeProvider 创建并初始化Fake 提供器。
func NewFakeProvider(secret string) *FakeProvider {
	return &FakeProvider{secret: secret, sessions: make(map[string]fakeSession)}
}

// Code 返回代码。
func (*FakeProvider) Code() string { return "fake" }

// CreateSession 创建会话。
func (p *FakeProvider) CreateSession(_ context.Context, req ProviderSessionRequest) (ProviderSession, error) {
	if req.VerificationID == "" || req.SubjectReference == "" || req.State == "" {
		return ProviderSession{}, fmt.Errorf("identity session request is incomplete")
	}
	providerRequestID := "fake-iv-" + uuid.NewString()
	expires := time.Now().Add(15 * time.Minute)
	p.mu.Lock()
	p.sessions[providerRequestID] = fakeSession{state: req.State, result: ProviderResult{
		RequestID: providerRequestID, Status: StatusPending, AdultResult: AdultUnknown,
		VerificationLevel: req.VerificationLevel,
	}}
	p.mu.Unlock()
	query := url.Values{"state": []string{req.State}}
	return ProviderSession{RequestID: providerRequestID, URL: "https://fake.identity.local/session/" + providerRequestID + "?" + query.Encode(), ExpiresAt: expires}, nil
}

type fakeCallbackPayload struct {
	EventID           string `json:"event_id"`
	ProviderRequestID string `json:"provider_request_id"`
	State             string `json:"state"`
	Status            string `json:"status"`
	AdultResult       string `json:"adult_result"`
	Subject           string `json:"provider_subject_id"`
	VerificationLevel string `json:"verification_level"`
	ResultReference   string `json:"result_reference"`
	ValidUntil        string `json:"valid_until,omitempty"`
}

// ParseCallback 解析回调。
func (p *FakeProvider) ParseCallback(_ context.Context, headers http.Header, body []byte) (ProviderCallback, error) {
	timestamp := headers.Get(IdentityTimestampHeader)
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || absDuration(time.Since(time.Unix(seconds, 0))) > 5*time.Minute {
		return ProviderCallback{}, fmt.Errorf("invalid callback timestamp")
	}
	provided, err := hex.DecodeString(headers.Get(IdentitySignatureHeader))
	if err != nil || !hmac.Equal(provided, callbackMAC(p.secret, timestamp, body)) {
		return ProviderCallback{}, fmt.Errorf("invalid callback signature")
	}
	var payload fakeCallbackPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return ProviderCallback{}, fmt.Errorf("invalid callback body")
	}
	if payload.EventID == "" || payload.ProviderRequestID == "" || payload.State == "" || payload.Subject == "" {
		return ProviderCallback{}, fmt.Errorf("incomplete callback payload")
	}
	if payload.Status != StatusVerified && payload.Status != StatusRejected && payload.Status != StatusRevoked {
		return ProviderCallback{}, fmt.Errorf("invalid callback status")
	}
	if payload.AdultResult != AdultAdult && payload.AdultResult != AdultMinor && payload.AdultResult != AdultUnknown {
		return ProviderCallback{}, fmt.Errorf("invalid adult result")
	}
	p.mu.Lock()
	session, ok := p.sessions[payload.ProviderRequestID]
	if !ok || session.state != payload.State {
		p.mu.Unlock()
		return ProviderCallback{}, fmt.Errorf("unknown identity session")
	}
	var validUntil *time.Time
	if payload.ValidUntil != "" {
		parsed, parseErr := time.Parse(time.RFC3339, payload.ValidUntil)
		if parseErr != nil {
			p.mu.Unlock()
			return ProviderCallback{}, fmt.Errorf("invalid valid_until")
		}
		validUntil = &parsed
	}
	session.result = ProviderResult{
		RequestID: payload.ProviderRequestID, Subject: payload.Subject, Status: payload.Status,
		AdultResult: payload.AdultResult, VerificationLevel: payload.VerificationLevel,
		ValidUntil: validUntil, ResultReference: payload.ResultReference,
	}
	p.sessions[payload.ProviderRequestID] = session
	p.mu.Unlock()
	return ProviderCallback{EventID: payload.EventID, ProviderRequestID: payload.ProviderRequestID, State: payload.State}, nil
}

// Query 查询提供器结果。
func (p *FakeProvider) Query(_ context.Context, providerRequestID string) (ProviderResult, error) {
	p.mu.RLock()
	session, ok := p.sessions[providerRequestID]
	p.mu.RUnlock()
	if !ok {
		return ProviderResult{}, fmt.Errorf("identity session not found")
	}
	return session.result, nil
}

// SignFakeCallback 为Fake 回调生成签名。
// SignFakeCallback 刻意仅限本地模拟服务商使用，
// 供集成客户端模拟服务商回调。
func SignFakeCallback(secret, timestamp string, body []byte) string {
	return hex.EncodeToString(callbackMAC(secret, timestamp, body))
}

// callbackMAC 返回回调 MAC。
func callbackMAC(secret, timestamp string, body []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte{'\n'})
	_, _ = mac.Write(body)
	return mac.Sum(nil)
}

// absDuration 返回耗时的绝对值。
func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

type UnavailableProvider struct{}

// Code 返回代码。
func (*UnavailableProvider) Code() string { return "unavailable" }

// CreateSession 创建会话。
func (*UnavailableProvider) CreateSession(context.Context, ProviderSessionRequest) (ProviderSession, error) {
	return ProviderSession{}, fmt.Errorf("identity provider adapter is not configured")
}

// ParseCallback 解析回调。
func (*UnavailableProvider) ParseCallback(context.Context, http.Header, []byte) (ProviderCallback, error) {
	return ProviderCallback{}, fmt.Errorf("identity provider adapter is not configured")
}

// Query 查询提供器结果。
func (*UnavailableProvider) Query(context.Context, string) (ProviderResult, error) {
	return ProviderResult{}, fmt.Errorf("identity provider adapter is not configured")
}
