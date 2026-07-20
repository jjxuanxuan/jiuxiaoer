package auth

import (
	"context"
	"time"
)

// SMSProvider delivers a verification code. Code generation, expiry,
// rate-limiting, and consumption remain owned by the auth service.
type SMSProvider interface {
	SendVerificationCode(ctx context.Context, phone, code string, ttl time.Duration) error
}

type mockSMSProvider struct{}

func (mockSMSProvider) SendVerificationCode(context.Context, string, string, time.Duration) error {
	return nil
}
