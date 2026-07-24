package auth

import (
	"context"
	"time"
)

// SMSProvider 发送验证码。代码生成、过期、
// 速率限制和消费仍然归身份验证服务所有。
type SMSProvider interface {
	SendVerificationCode(ctx context.Context, phone, code string, ttl time.Duration) error
}

type mockSMSProvider struct{}

func (mockSMSProvider) SendVerificationCode(context.Context, string, string, time.Duration) error {
	return nil
}
