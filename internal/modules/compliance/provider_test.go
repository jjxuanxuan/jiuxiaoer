package compliance

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/config"
)

const testIdentityCallbackSecret = "test-identity-callback-secret-123456789"

type registryProvider struct{ code string }

func (p *registryProvider) Code() string { return p.code }

func (*registryProvider) CreateSession(context.Context, ProviderSessionRequest) (ProviderSession, error) {
	return ProviderSession{}, nil
}

func (*registryProvider) ParseCallback(context.Context, http.Header, []byte) (ProviderCallback, error) {
	return ProviderCallback{}, nil
}

func (*registryProvider) Query(context.Context, string) (ProviderResult, error) {
	return ProviderResult{}, nil
}

// TestFakeProviderSessionCallbackAndQuery 验证Fake 提供器会话回调 And 查询的预期行为。
func TestFakeProviderSessionCallbackAndQuery(t *testing.T) {
	provider := NewFakeProvider(testIdentityCallbackSecret)
	session, err := provider.CreateSession(context.Background(), ProviderSessionRequest{
		VerificationID: "1", SubjectReference: "opaque-customer", Purpose: "alcohol_purchase",
		VerificationLevel: "identity_and_liveness", State: "state-1",
	})
	if err != nil || session.RequestID == "" || session.URL == "" {
		t.Fatalf("create session: session=%+v err=%v", session, err)
	}
	body, _ := json.Marshal(fakeCallbackPayload{
		EventID: "event-1", ProviderRequestID: session.RequestID, State: "state-1", Status: StatusVerified,
		AdultResult: AdultAdult, Subject: "provider-subject", VerificationLevel: "identity_and_liveness", ResultReference: "result-1",
	})
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	headers := make(http.Header)
	headers.Set(IdentityTimestampHeader, timestamp)
	headers.Set(IdentitySignatureHeader, SignFakeCallback(testIdentityCallbackSecret, timestamp, body))
	event, err := provider.ParseCallback(context.Background(), headers, body)
	if err != nil || event.ProviderRequestID != session.RequestID {
		t.Fatalf("parse callback: event=%+v err=%v", event, err)
	}
	result, err := provider.Query(context.Background(), session.RequestID)
	if err != nil || result.Status != StatusVerified || result.AdultResult != AdultAdult {
		t.Fatalf("query result: result=%+v err=%v", result, err)
	}
}

// TestFakeProviderRejectsInvalidCallback 验证Fake 提供器 Rejects 无效回调的预期行为。
func TestFakeProviderRejectsInvalidCallback(t *testing.T) {
	provider := NewFakeProvider(testIdentityCallbackSecret)
	session, err := provider.CreateSession(context.Background(), ProviderSessionRequest{
		VerificationID: "2", SubjectReference: "opaque-customer", Purpose: "alcohol_purchase",
		VerificationLevel: "identity", State: "state-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(fakeCallbackPayload{
		EventID: "event-2", ProviderRequestID: session.RequestID, State: "wrong-state", Status: StatusVerified,
		AdultResult: AdultAdult, Subject: "provider-subject", VerificationLevel: "identity", ResultReference: "result-2",
	})
	headers := make(http.Header)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	headers.Set(IdentityTimestampHeader, timestamp)
	headers.Set(IdentitySignatureHeader, SignFakeCallback(testIdentityCallbackSecret, timestamp, body))
	if _, err := provider.ParseCallback(context.Background(), headers, body); err == nil {
		t.Fatal("expected state mismatch to fail")
	}
	headers.Set(IdentitySignatureHeader, "00")
	if _, err := provider.ParseCallback(context.Background(), headers, body); err == nil {
		t.Fatal("expected invalid signature to fail")
	}
}

// TestProviderAdultResultContract 验证提供器 Adult 结果 Contract的预期行为。
func TestProviderAdultResultContract(t *testing.T) {
	for _, adultResult := range []string{AdultAdult, AdultMinor, AdultUnknown} {
		result := ProviderResult{RequestID: "provider-request", Subject: "subject", Status: StatusVerified, AdultResult: adultResult, VerificationLevel: "identity"}
		if !validProviderResult(result) {
			t.Fatalf("expected explicit provider age group %q to be accepted", adultResult)
		}
	}
	if validProviderResult(ProviderResult{RequestID: "provider-request", Status: StatusVerified, AdultResult: "18", VerificationLevel: "identity"}) {
		t.Fatal("backend must not infer age from an unrecognized provider value")
	}
}

func TestProviderRegistryBuildsOnlyMatchingAdapter(t *testing.T) {
	registry := NewProviderRegistry()
	provider := &registryProvider{code: "approved-provider"}
	if err := registry.Register("approved-provider", func(context.Context, config.Config) (Provider, error) {
		return provider, nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{CP1: config.CP1Config{IdentityProvider: "approved-provider"}}
	built, err := registry.Build(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if built != provider {
		t.Fatal("registry returned a different provider instance")
	}

	if err := registry.Register("mismatched-provider", func(context.Context, config.Config) (Provider, error) {
		return &registryProvider{code: "different-provider"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg.CP1.IdentityProvider = "mismatched-provider"
	if _, err := registry.Build(context.Background(), cfg); !errors.Is(err, ErrProviderCodeMismatch) {
		t.Fatalf("mismatched provider error=%v want ErrProviderCodeMismatch", err)
	}
}

func TestProviderRegistryRejectsMissingDuplicateAndProductionFake(t *testing.T) {
	registry := NewProviderRegistry()
	cfg := config.Config{CP1: config.CP1Config{IdentityProvider: "missing-provider"}}
	if _, err := registry.Build(context.Background(), cfg); !errors.Is(err, ErrProviderNotRegistered) {
		t.Fatalf("missing provider error=%v want ErrProviderNotRegistered", err)
	}
	if err := registry.Register(FakeProviderCode, func(_ context.Context, cfg config.Config) (Provider, error) {
		return NewFakeProvider(cfg.CP1.IdentityCallbackSecret), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(FakeProviderCode, func(context.Context, config.Config) (Provider, error) {
		return &registryProvider{code: FakeProviderCode}, nil
	}); err == nil {
		t.Fatal("duplicate provider registration must fail")
	}
	cfg.App.Env = "production"
	cfg.CP1.IdentityProvider = FakeProviderCode
	if _, err := registry.Build(context.Background(), cfg); err == nil {
		t.Fatal("fake identity compliance provider must not build in production")
	}
}
