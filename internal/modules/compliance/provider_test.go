package compliance

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"
)

const testIdentityCallbackSecret = "test-identity-callback-secret-123456789"

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
