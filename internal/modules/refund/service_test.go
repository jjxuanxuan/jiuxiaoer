package refund

import (
	"errors"
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

// TestIsStateMismatch 验证Is 状态 Mismatch的预期行为。
func TestIsStateMismatch(t *testing.T) {
	for _, code := range []string{"REFUND_AMOUNT_MISMATCH", "REFUND_PROVIDER_ID_MISMATCH", "REFUND_PAYMENT_MISMATCH", "REFUND_AMOUNT_EXCEEDED", "REFUND_ITEM_AMOUNT_EXCEEDED"} {
		if !isStateMismatch(problem.Conflict(code, "mismatch")) {
			t.Fatalf("expected %s to be terminal", code)
		}
	}
	if isStateMismatch(problem.Conflict("REFUND_RETRY_NOT_ALLOWED", "state")) {
		t.Fatal("retry conflict must not be classified as provider mismatch")
	}
	if isStateMismatch(errors.New("network timeout")) {
		t.Fatal("transport errors must remain retryable")
	}
}

func TestRefundPollDelayFollowsOfficialBackoff(t *testing.T) {
	tests := []struct {
		attempts uint32
		want     time.Duration
	}{
		{attempts: 1, want: time.Minute},
		{attempts: 5, want: time.Minute},
		{attempts: 6, want: 5 * time.Minute},
		{attempts: 7, want: 10 * time.Minute},
		{attempts: 8, want: 20 * time.Minute},
		{attempts: 9, want: 30 * time.Minute},
		{attempts: 100, want: 30 * time.Minute},
	}
	for _, test := range tests {
		if got := refundPollDelay(test.attempts); got != test.want {
			t.Fatalf("attempts=%d got=%s want=%s", test.attempts, got, test.want)
		}
	}
}

func TestSubmissionRetryClassification(t *testing.T) {
	for _, code := range []string{"INVALID_REQUEST", "SIGN_ERROR", "MCH_NOT_EXISTS", "RESOURCE_NOT_EXISTS", "USER_ACCOUNT_ABNORMAL", "NOT_ENOUGH", "UNKNOWN_PROVIDER_ERROR"} {
		if submissionRetryAllowed(code) {
			t.Fatalf("%s must stop after query confirms the refund does not exist", code)
		}
	}
	for _, code := range []string{"", "PROVIDER_UNAVAILABLE", "SYSTEM_ERROR", "FREQUENCY_LIMITED", "NOT_ENOUGH_MANUAL_RETRY"} {
		if !submissionRetryAllowed(code) {
			t.Fatalf("%s must allow original-number resubmission", code)
		}
	}
}

func TestStoredRepairResubmissionClassification(t *testing.T) {
	for _, status := range []string{"creating", "submission_unknown", "pending"} {
		if !storedRepairResubmissionAllowed(Row{Status: status}) {
			t.Fatalf("%s must allow controlled original-number resubmission", status)
		}
	}
	for _, code := range []string{"PROVIDER_DATA_MISMATCH", "PROVIDER_UNAVAILABLE", "SYSTEM_ERROR", "FREQUENCY_LIMITED"} {
		code := code
		if !storedRepairResubmissionAllowed(Row{Status: "exception", FailureCode: &code}) {
			t.Fatalf("legacy exception %s must remain repairable", code)
		}
	}
	for _, code := range []string{"INVALID_REQUEST", "SIGN_ERROR", "MCH_NOT_EXISTS", "USER_ACCOUNT_ABNORMAL", "NOT_ENOUGH"} {
		code := code
		if storedRepairResubmissionAllowed(Row{Status: "exception", FailureCode: &code}) {
			t.Fatalf("permanent exception %s must not be resubmitted by stored repair", code)
		}
	}
	abnormal := "ABNORMAL"
	mismatch := "PROVIDER_DATA_MISMATCH"
	if storedRepairResubmissionAllowed(Row{Status: "exception", ProviderStatus: &abnormal, FailureCode: &mismatch}) {
		t.Fatal("ABNORMAL must remain a merchant-platform manual recovery")
	}
}

func TestGuardTerminalRefundTransition(t *testing.T) {
	closed := "CLOSED"
	tests := []struct {
		name       string
		row        Row
		incoming   string
		idempotent bool
		errorCode  string
	}{
		{name: "success duplicate", row: Row{Status: "succeeded"}, incoming: "SUCCESS", idempotent: true},
		{name: "success regression", row: Row{Status: "succeeded"}, incoming: "ABNORMAL", errorCode: "REFUND_STATUS_REGRESSION"},
		{name: "closed duplicate", row: Row{Status: "failed", ProviderStatus: &closed}, incoming: "CLOSED", idempotent: true},
		{name: "closed cannot process again", row: Row{Status: "failed", ProviderStatus: &closed}, incoming: "PROCESSING", errorCode: "REFUND_STATUS_REGRESSION"},
		{name: "closed cannot succeed", row: Row{Status: "failed", ProviderStatus: &closed}, incoming: "SUCCESS", errorCode: "REFUND_STATUS_REGRESSION"},
		{name: "abnormal may recover", row: Row{Status: "exception"}, incoming: "SUCCESS"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			idempotent, err := guardTerminalRefundTransition(test.row, test.incoming)
			if idempotent != test.idempotent || refundErrorCode(err) != test.errorCode {
				t.Fatalf("idempotent=%v error=%v", idempotent, err)
			}
		})
	}
}

func TestWorkerResultApplicableRequiresExactActiveClaim(t *testing.T) {
	if !workerResultApplicable(Row{Status: "pending", Version: 8}, 8) {
		t.Fatal("current pending claim must accept its worker result")
	}
	for _, row := range []Row{
		{Status: "pending", Version: 9},
		{Status: "failed", Version: 8},
		{Status: "exception", Version: 8},
		{Status: "succeeded", Version: 8},
	} {
		if workerResultApplicable(row, 8) {
			t.Fatalf("stale or terminal row accepted worker result: %+v", row)
		}
	}
}
