package paygateway

import (
	"errors"
	"testing"
)

func TestErrorMetadataSurvivesWrapping(t *testing.T) {
	cause := errors.New("transport")
	err := &ProviderError{Operation: "refund.query", HTTPStatus: 404, Code: "RESOURCE_NOT_EXISTS", RequestID: "wx-request", Retryable: false, Cause: cause}
	wrapped := errors.Join(errors.New("context"), err)
	if !IsCode(wrapped, "RESOURCE_NOT_EXISTS") || Retryable(wrapped) || RequestID(wrapped) != "wx-request" {
		t.Fatalf("unexpected metadata: %v", wrapped)
	}
	if !errors.Is(wrapped, cause) {
		t.Fatal("provider error must retain the original cause")
	}
}

func TestUnknownErrorsAreRetryable(t *testing.T) {
	if !Retryable(errors.New("network unavailable")) {
		t.Fatal("unknown transport failures should be retryable")
	}
	if Code(errors.New("network unavailable"), "PROVIDER_UNAVAILABLE") != "PROVIDER_UNAVAILABLE" {
		t.Fatal("fallback code was not returned")
	}
}
