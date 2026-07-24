package printjob

import (
	"context"
	"testing"
	"time"
)

// TestValidPrintEvents 验证有效打印 Events的预期行为。
func TestValidPrintEvents(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []string
		want   bool
	}{
		{"accepted", []string{"order_accepted"}, true},
		{"both", []string{"order_accepted", "order_prepared"}, true},
		{"empty", nil, false},
		{"unknown", []string{"order_paid"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validPrintEvents(tc.events); got != tc.want {
				t.Fatalf("validPrintEvents() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPrintBackoffIsBounded 验证打印退避时间有上限。
func TestPrintBackoffIsBounded(t *testing.T) {
	if got := backoff(0); got != 10*time.Second {
		t.Fatalf("zero attempt backoff = %v", got)
	}
	if got := backoff(99); got != 30*time.Minute {
		t.Fatalf("bounded backoff = %v", got)
	}
}

// TestFakeProvider 验证模拟打印服务商。
func TestFakeProvider(t *testing.T) {
	p := &FakeProvider{}
	result, err := p.Submit(context.Background(), PrintRequest{ProviderRequestID: "print-1"})
	if err != nil || result.Status != "succeeded" || result.ProviderRequestID != "print-1" {
		t.Fatalf("unexpected fake provider result: %#v, %v", result, err)
	}
	queried, err := p.Query(context.Background(), "print-1")
	if err != nil || queried.Status != "succeeded" {
		t.Fatalf("fake provider reconciliation failed: %#v, %v", queried, err)
	}
	p.Failure = &ProviderError{Code: "paper_out", Retryable: true}
	if _, err := p.Submit(context.Background(), PrintRequest{ProviderRequestID: "print-2"}); err == nil {
		t.Fatal("expected injected provider failure")
	}
}
