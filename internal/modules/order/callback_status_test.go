package order

import (
	"errors"
	"net/http"
	"testing"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestPaymentCallbackFailureStatus(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{problem.InvalidArgument("CALLBACK_INVALID", "bad payload"), http.StatusBadRequest},
		{problem.Unauthorized("CALLBACK_INVALID", "bad signature"), http.StatusUnauthorized},
		{problem.NotFound("PAYMENT_PROVIDER_NOT_FOUND", "provider"), http.StatusBadRequest},
		{problem.Conflict("PAYMENT_PROVIDER_DATA_MISMATCH", "mismatch"), http.StatusInternalServerError},
		{errors.New("database unavailable"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		if got := paymentCallbackFailureStatus(tc.err); got != tc.want {
			t.Fatalf("%v: got %d want %d", tc.err, got, tc.want)
		}
	}
}
