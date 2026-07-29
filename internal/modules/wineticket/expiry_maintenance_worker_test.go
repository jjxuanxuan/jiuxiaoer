package wineticket

import (
	"context"
	"errors"
	"testing"
)

func TestExpiryMaintenanceRunOnceAlwaysRunsCoreExpiryAndJoinsErrors(
	t *testing.T,
) {
	reminderErr := errors.New("reminder unavailable")
	expiryErr := errors.New("expiry failed")
	reminderCalls := 0
	expiryCalls := 0

	err := runExpiryMaintenanceOnce(
		context.Background(),
		func(context.Context) error {
			reminderCalls++
			return reminderErr
		},
		func(context.Context) (int, error) {
			expiryCalls++
			return 0, expiryErr
		},
	)

	if reminderCalls != 1 || expiryCalls != 1 {
		t.Fatalf(
			"reminder_calls=%d expiry_calls=%d",
			reminderCalls,
			expiryCalls,
		)
	}
	if !errors.Is(err, reminderErr) || !errors.Is(err, expiryErr) {
		t.Fatalf("joined error=%v", err)
	}
}
