package auth

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type recordingSMSProvider struct {
	phone string
	code  string
	ttl   time.Duration
	err   error
}

func (p *recordingSMSProvider) SendVerificationCode(_ context.Context, phone, code string, ttl time.Duration) error {
	p.phone, p.code, p.ttl = phone, code, ttl
	return p.err
}

func TestPasswordLoginRejectsRiderAccounts(t *testing.T) {
	service := NewService(config.Load(), nil, nil, snowflake.New(991))

	_, err := service.PasswordLogin(context.Background(), "rider", PasswordLoginReq{
		Username: "legacy_rider",
		Password: "legacy-password",
	})
	if err == nil {
		t.Fatal("rider password login unexpectedly succeeded")
	}
	if got := problem.FromError(err).ErrorCode; got != "AUTH_LOGIN_METHOD_NOT_ALLOWED" {
		t.Fatalf("error_code=%s, want AUTH_LOGIN_METHOD_NOT_ALLOWED", got)
	}
}

func TestRiderAndCustomerSMSCodesUseSeparateNamespaces(t *testing.T) {
	phone := "13800138000"
	if customer, rider := smsLoginKey("customer", phone), smsLoginKey("rider", phone); customer == rider {
		t.Fatalf("customer and rider sms keys must differ: %s", customer)
	}
}

func TestSendCustomerCodeUsesProviderAndConsumesCodeOnce(t *testing.T) {
	mini := miniredis.RunT(t)
	redisClient := goredis.NewClient(&goredis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	cfg := config.Load()
	cfg.Feature.SMSMockEnabled = false
	cfg.SMS.Enabled = true
	provider := &recordingSMSProvider{}
	service := NewService(cfg, nil, redisClient, snowflake.New(992)).WithSMSProvider(provider)

	phone := "13800138000"
	if err := service.SendCustomerCode(context.Background(), SendCodeReq{Phone: phone}); err != nil {
		t.Fatalf("send customer code: %v", err)
	}
	if provider.phone != phone || provider.ttl != smsCodeTTL || !regexp.MustCompile(`^[0-9]{6}$`).MatchString(provider.code) {
		t.Fatalf("unexpected provider call: phone=%q code=%q ttl=%s", provider.phone, provider.code, provider.ttl)
	}
	if err := service.verifySMSCode(context.Background(), "customer", phone, provider.code); err != nil {
		t.Fatalf("verify generated code: %v", err)
	}
	if err := service.verifySMSCode(context.Background(), "customer", phone, provider.code); problem.FromError(err).ErrorCode != "AUTH_INVALID_CODE" {
		t.Fatalf("code must be single-use, got %v", err)
	}
}

func TestSendCustomerCodeRemovesCodeWhenProviderFails(t *testing.T) {
	mini := miniredis.RunT(t)
	redisClient := goredis.NewClient(&goredis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	cfg := config.Load()
	cfg.Feature.SMSMockEnabled = false
	cfg.SMS.Enabled = true
	provider := &recordingSMSProvider{err: errors.New("provider down")}
	service := NewService(cfg, nil, redisClient, snowflake.New(993)).WithSMSProvider(provider)

	phone := "13900139000"
	err := service.SendCustomerCode(context.Background(), SendCodeReq{Phone: phone})
	if problem.FromError(err).ErrorCode != "SYSTEM_DEPENDENCY_UNAVAILABLE" {
		t.Fatalf("unexpected send error: %v", err)
	}
	if mini.Exists(smsLoginKey("customer", phone)) {
		t.Fatal("failed delivery left a usable verification code")
	}
}
