package auth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

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

func TestCustomerPermissionsCoverPhaseOneProtectedCapabilities(t *testing.T) {
	permissions := customerPermissions()
	for _, required := range []string{
		"cart:view", "cart:update",
		"order:create", "order:list", "order:view", "order:cancel",
		"payment:create", "payment:view",
		"delivery_verification:view_customer",
	} {
		found := false
		for _, permission := range permissions {
			if permission == required {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("customer token permissions missing %q", required)
		}
	}
}

func TestCustomerSMSLoginRequiresBoundCurrentWeChatIdentity(t *testing.T) {
	tests := []struct {
		name            string
		seedAccount     bool
		accountStatus   string
		customerStatus  string
		identityAppID   string
		identityStatus  string
		identityDeleted bool
		wantErrorCode   string
		wantHTTPStatus  int
	}{
		{name: "unknown phone", wantErrorCode: "AUTH_WECHAT_LOGIN_REQUIRED", wantHTTPStatus: 403},
		{name: "legacy sms only account", seedAccount: true, wantErrorCode: "AUTH_WECHAT_LOGIN_REQUIRED", wantHTTPStatus: 403},
		{name: "identity belongs to another miniapp", seedAccount: true, identityAppID: "another-miniapp", identityStatus: "active", wantErrorCode: "AUTH_WECHAT_LOGIN_REQUIRED", wantHTTPStatus: 403},
		{name: "identity is inactive", seedAccount: true, identityAppID: "current-miniapp", identityStatus: "inactive", wantErrorCode: "AUTH_WECHAT_LOGIN_REQUIRED", wantHTTPStatus: 403},
		{name: "identity is soft deleted", seedAccount: true, identityAppID: "current-miniapp", identityStatus: "active", identityDeleted: true, wantErrorCode: "AUTH_WECHAT_LOGIN_REQUIRED", wantHTTPStatus: 403},
		{name: "account is disabled", seedAccount: true, accountStatus: "disabled", identityAppID: "current-miniapp", identityStatus: "active", wantErrorCode: "AUTH_ACCOUNT_DISABLED", wantHTTPStatus: 403},
		{name: "customer is disabled", seedAccount: true, customerStatus: "disabled", identityAppID: "current-miniapp", identityStatus: "active", wantErrorCode: "AUTH_ACCOUNT_DISABLED", wantHTTPStatus: 403},
		{name: "bound current miniapp identity", seedAccount: true, identityAppID: "current-miniapp", identityStatus: "active"},
	}

	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, db := newCustomerSMSLoginTestService(t, int64(100+index))
			phone := fmt.Sprintf("13800138%03d", index)
			if tt.seedAccount {
				seedCustomerSMSLoginFixture(t, db, phone, tt.accountStatus, tt.customerStatus, tt.identityAppID, tt.identityStatus, tt.identityDeleted)
			}

			before := customerSMSLoginRowCounts(t, db)
			if err := service.SendCustomerCode(context.Background(), SendCodeReq{Phone: phone}); err != nil {
				t.Fatalf("send code: %v", err)
			}
			resp, err := service.CustomerSMSLogin(context.Background(), SmsLoginReq{Phone: phone, Code: "123456"})
			after := customerSMSLoginRowCounts(t, db)
			if before != after {
				t.Fatalf("sms login mutated registration rows: before=%+v after=%+v", before, after)
			}

			if tt.wantErrorCode != "" {
				if err == nil {
					t.Fatal("sms login unexpectedly succeeded")
				}
				details := problem.FromError(err)
				if details.ErrorCode != tt.wantErrorCode || details.Status != tt.wantHTTPStatus {
					t.Fatalf("problem=%+v, want status=%d code=%s", details, tt.wantHTTPStatus, tt.wantErrorCode)
				}
				return
			}

			if err != nil {
				t.Fatalf("sms login: %v", err)
			}
			if resp.AccountID != "101" {
				t.Fatalf("account_id=%s, want 101", resp.AccountID)
			}
			profile, ok := resp.Profile.(map[string]any)
			if !ok || profile["customer_id"] != "201" || profile["phone"] != phone {
				t.Fatalf("unexpected profile: %#v", resp.Profile)
			}
		})
	}
}

func newCustomerSMSLoginTestService(t *testing.T, node int64) (*Service, *gorm.DB) {
	t.Helper()
	dsn := fmt.Sprintf("file:auth-sms-login-%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&Account{}, &Customer{}, &CustomerIdentity{}, &Cart{}, &AuditLog{}); err != nil {
		t.Fatalf("migrate auth fixtures: %v", err)
	}
	if err := db.Exec("ALTER TABLE customers ADD COLUMN deleted_at DATETIME").Error; err != nil {
		t.Fatalf("add customers.deleted_at: %v", err)
	}
	if err := db.Exec("ALTER TABLE customer_identities ADD COLUMN deleted_at DATETIME").Error; err != nil {
		t.Fatalf("add customer_identities.deleted_at: %v", err)
	}

	mini := miniredis.RunT(t)
	redisClient := goredis.NewClient(&goredis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	cfg := config.Load()
	cfg.SMS.Enabled = true
	cfg.Feature.SMSMockEnabled = true
	cfg.WeChat.MiniAppID = "current-miniapp"
	return NewService(cfg, db, redisClient, snowflake.New(node)), db
}

func seedCustomerSMSLoginFixture(t *testing.T, db *gorm.DB, phone, accountStatus, customerStatus, identityAppID, identityStatus string, identityDeleted bool) {
	t.Helper()
	if accountStatus == "" {
		accountStatus = "active"
	}
	if customerStatus == "" {
		customerStatus = "active"
	}
	phoneCopy := phone
	if err := db.Create(&Account{ID: 101, AccountType: "customer", Phone: &phoneCopy, Status: accountStatus, CredentialVersion: 1}).Error; err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := db.Create(&Customer{ID: 201, AccountID: 101, Phone: phone, Status: customerStatus}).Error; err != nil {
		t.Fatalf("create customer: %v", err)
	}
	if err := db.Create(&Cart{ID: 301, CustomerID: 201}).Error; err != nil {
		t.Fatalf("create cart: %v", err)
	}
	if identityAppID == "" {
		return
	}
	identity := CustomerIdentity{
		ID: 401, CustomerID: 201, Provider: "wechat_miniapp", AppID: identityAppID,
		ProviderSubject: "openid-201", Status: identityStatus,
	}
	if err := db.Create(&identity).Error; err != nil {
		t.Fatalf("create identity: %v", err)
	}
	if identityDeleted {
		if err := db.Table("customer_identities").Where("id = ?", identity.ID).Update("deleted_at", time.Now()).Error; err != nil {
			t.Fatalf("soft delete identity: %v", err)
		}
	}
}

type customerSMSLoginCounts struct {
	accounts  int64
	customers int64
	carts     int64
}

func customerSMSLoginRowCounts(t *testing.T, db *gorm.DB) customerSMSLoginCounts {
	t.Helper()
	var result customerSMSLoginCounts
	if err := db.Table("accounts").Count(&result.accounts).Error; err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if err := db.Table("customers").Count(&result.customers).Error; err != nil {
		t.Fatalf("count customers: %v", err)
	}
	if err := db.Table("carts").Count(&result.carts).Error; err != nil {
		t.Fatalf("count carts: %v", err)
	}
	return result
}
