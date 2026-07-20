package config

import (
	"strings"
	"testing"
	"time"
)

// TestProductionConfigRejectsMocksAndDefaultSecrets 验证Production 配置 Rejects Mocks And 默认项 Secrets的预期行为。
func TestProductionConfigRejectsMocksAndDefaultSecrets(t *testing.T) {
	cfg := validProductionConfig()
	cfg.Feature.PaymentMockEnabled = true
	cfg.JWT.AccessSecret = "local_access_secret_change_me"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected unsafe production config to fail")
	}
	for _, expected := range []string{"mock payment", "JWT secrets"} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("expected error to contain %q, got %v", expected, err)
		}
	}
}

// TestProductionConfigAcceptsExplicitSafeValues 验证Production 配置 Accepts Explicit Safe Values的预期行为。
func TestProductionConfigAcceptsExplicitSafeValues(t *testing.T) {
	if err := validProductionConfig().Validate(); err != nil {
		t.Fatalf("expected production config to pass: %v", err)
	}
}

func TestProductionPaymentAndRefundWorkersAreMandatory(t *testing.T) {
	t.Run("payment reconciliation worker", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.Order.ExpiryWorkerEnabled = false
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "JXE_ORDER_EXPIRY_WORKER_ENABLED=true") {
			t.Fatalf("expected disabled payment worker to fail, got %v", err)
		}
	})
	t.Run("refund worker", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.AfterSale.Enabled = true
		cfg.AfterSale.RefundExecutionEnabled = true
		cfg.AfterSale.WorkerEnabled = false
		cfg.WeChat.RefundNotifyURL = "https://api.example.com/api/v1/refunds/wechat/callbacks"
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "JXE_REFUND_WORKER_ENABLED=true") {
			t.Fatalf("expected disabled refund worker to fail, got %v", err)
		}
	})
	t.Run("daily bill reconciliation worker", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.Reconciliation.WorkerEnabled = false
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "JXE_WECHAT_BILL_RECONCILIATION_WORKER_ENABLED=true") {
			t.Fatalf("expected disabled bill reconciliation worker to fail, got %v", err)
		}
	})
}

func TestWechatBillBackfillConfigurationIsBounded(t *testing.T) {
	t.Run("enabled reconciliation requires an explicit start date", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.Reconciliation.StartDate = ""
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "JXE_WECHAT_BILL_RECONCILIATION_START_DATE") {
			t.Fatalf("expected missing start date to fail, got %v", err)
		}
	})
	t.Run("start date uses the official calendar format", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.Reconciliation.StartDate = "2026/07/01"
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "must use YYYY-MM-DD") {
			t.Fatalf("expected invalid start date to fail, got %v", err)
		}
	})
	t.Run("start date cannot leave production reconciliation dormant", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.Reconciliation.StartDate = time.Now().In(time.FixedZone("CST", 8*60*60)).AddDate(0, 0, 1).Format("2006-01-02")
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "cannot be in the future") {
			t.Fatalf("expected future start date to fail, got %v", err)
		}
	})
	t.Run("per-cycle backfill is bounded", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.Reconciliation.BackfillDaysPerCycle = 31
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "invalid WeChat bill reconciliation configuration") {
			t.Fatalf("expected excessive backfill batch to fail, got %v", err)
		}
	})
	t.Run("worker cannot run before official bill generation hour", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.Reconciliation.RunHour = 9
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "invalid WeChat bill reconciliation configuration") {
			t.Fatalf("expected early run hour to fail, got %v", err)
		}
	})
	t.Run("run lease must outlive the provider request", func(t *testing.T) {
		cfg := validProductionConfig()
		cfg.Reconciliation.RunningTimeout = cfg.Reconciliation.RequestTimeout
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "invalid WeChat bill reconciliation configuration") {
			t.Fatalf("expected short run lease to fail, got %v", err)
		}
	})
}

func TestProductionConfigRequiresTencentCloudSMS(t *testing.T) {
	cfg := validProductionConfig()
	cfg.SMS.SecretKey = ""
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "Tencent Cloud SMS requires") {
		t.Fatalf("expected missing Tencent Cloud SMS credentials to fail, got %v", err)
	}
}

// TestConfigRejectsInvalidSnowflakeNode 验证配置 Rejects 无效雪花 ID节点的预期行为。
func TestConfigRejectsInvalidSnowflakeNode(t *testing.T) {
	cfg := Load()
	cfg.App.SnowflakeNodeID = 1024
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid snowflake node to fail")
	}
}

// TestAssetFeatureDependencies 验证资产 Feature Dependencies的预期行为。
func TestAssetFeatureDependencies(t *testing.T) {
	cfg := Load()
	cfg.Asset.WriteEnabled = false
	cfg.Asset.CompensationIssueEnabled = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "compensation issuance") {
		t.Fatalf("expected compensation dependency error, got %v", err)
	}
	cfg.Asset.CompensationIssueEnabled = false
	cfg.Asset.ExpiryEnabled = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "asset expiry") {
		t.Fatalf("expected expiry dependency error, got %v", err)
	}
}

func TestProductionCustomerLBSEnforceRejectsUnsafeProvider(t *testing.T) {
	cfg := validProductionConfig()
	cfg.CustomerLBS.Mode = "enforce"
	cfg.CustomerLBS.Provider = "fake"
	cfg.CustomerLBS.RegeocodeEnabled = true
	cfg.CustomerLBS.CacheHMACSecret = "production-customer-lbs-hmac-secret-123456789"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "official HTTPS endpoint") {
		t.Fatalf("expected unsafe customer LBS provider rejection, got %v", err)
	}
}

func TestProductionCustomerLBSEnforceAcceptsExplicitSafeValues(t *testing.T) {
	cfg := validProductionConfig()
	cfg.CustomerLBS.Mode = "enforce"
	cfg.CustomerLBS.Provider = "amap"
	cfg.CustomerLBS.AmapBaseURL = "https://restapi.amap.com"
	cfg.CustomerLBS.AmapKey = "production-amap-key"
	cfg.CustomerLBS.RegeocodeEnabled = true
	cfg.CustomerLBS.RouteRefineEnabled = true
	cfg.CustomerLBS.AllowedOrigins = []string{"https://app.example.com"}
	cfg.CustomerLBS.CacheHMACSecret = "production-customer-lbs-hmac-secret-123456789"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected safe customer LBS config to pass: %v", err)
	}
}

func TestCustomerLBSDefaultsAllowObservedColdAmapLatency(t *testing.T) {
	for _, key := range []string{
		"JXE_C_LBS_REGEOCODE_TIMEOUT",
		"JXE_C_LBS_ROUTE_TIMEOUT",
		"JXE_C_LBS_RESOLVE_TIMEOUT",
	} {
		t.Setenv(key, "")
	}
	cfg := Load().CustomerLBS
	if cfg.RegeocodeTimeout != 2*time.Second || cfg.RouteTimeout != 2*time.Second || cfg.ResolveTimeout != 3*time.Second {
		t.Fatalf("unsafe customer LBS defaults: regeo=%s route=%s resolve=%s", cfg.RegeocodeTimeout, cfg.RouteTimeout, cfg.ResolveTimeout)
	}
}

func TestSearchDiscoveryDefaultsAndValidation(t *testing.T) {
	cfg := Load()
	if cfg.Search.HistoryMax != 20 || cfg.Search.HistoryRetention != 180*24*time.Hour || cfg.Search.HotWindowDays != 7 || cfg.Search.StatsRetentionDays != 30 {
		t.Fatalf("unexpected search defaults: %#v", cfg.Search)
	}
	cfg.Search.StatsRetentionDays = 6
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "search hot window") {
		t.Fatalf("expected inconsistent search retention to fail, got %v", err)
	}
}

// validProductionConfig 返回有效 Production 配置。
func validProductionConfig() Config {
	cfg := Load()
	cfg.App.Env = "production"
	cfg.MySQL.DSN = "user:password@tcp(mysql:3306)/jxe"
	cfg.MySQL.Required = true
	cfg.Redis.Addr = "redis:6379"
	cfg.Redis.Required = true
	cfg.RabbitMQ.URL = "amqp://rabbitmq:5672/"
	cfg.RabbitMQ.Required = true
	cfg.Feature.PaymentMockEnabled = false
	cfg.Feature.SMSMockEnabled = false
	cfg.SMS.Enabled = true
	cfg.SMS.Provider = "tencentcloud"
	cfg.SMS.Region = "ap-guangzhou"
	cfg.SMS.SecretID = "production-tencent-secret-id"
	cfg.SMS.SecretKey = "production-tencent-secret-key"
	cfg.SMS.SDKAppID = "1400006666"
	cfg.SMS.SignName = "酒小二"
	cfg.SMS.TemplateID = "1234567"
	cfg.SMS.Endpoint = "sms.tencentcloudapi.com"
	cfg.SMS.HTTPTimeout = 5 * time.Second
	cfg.WeChat.AuthMockEnabled = false
	cfg.WeChat.PayMockEnabled = false
	cfg.WeChat.MiniAppID = "wx-production-app"
	cfg.WeChat.MiniAppSecret = "production-miniapp-secret"
	cfg.WeChat.PayMchID = "1900000001"
	cfg.WeChat.PayCertSerial = "SERIAL123"
	cfg.WeChat.PayPrivateKeyPath = "/run/secrets/wechat-pay-private-key.pem"
	cfg.WeChat.PayAPIv3Key = "0123456789abcdef0123456789abcdef"
	cfg.WeChat.PayNotifyURL = "https://api.example.com/api/v1/payments/wechat/callbacks"
	cfg.Reconciliation.Enabled = true
	cfg.Reconciliation.WorkerEnabled = true
	cfg.Reconciliation.StartDate = "2026-07-01"
	cfg.AfterSale.EvidenceTokenSecret = "production-evidence-token-secret-123456789"
	cfg.JWT.AccessSecret = "access-secret-that-is-long-enough-123456"
	cfg.JWT.RefreshSecret = "refresh-secret-that-is-long-enough-654321"
	cfg.Metrics.Token = "metrics-token-long-enough"
	cfg.Service.EnforcementMode = "enforce"
	cfg.CP1.PrintProvider = "approved-print-provider"
	cfg.CP1.NotificationProvider = "approved-notification-provider"
	cfg.CP1.IdentityProvider = "approved-identity-provider"
	cfg.CP1.ComplianceMode = "enforce"
	cfg.CP1.VerificationPepper = "production-verification-pepper-123456789"
	cfg.CP1.IdentityCallbackSecret = "production-identity-callback-secret-123456789"
	cfg.CP1.DataEncryptionKey = "production-data-encryption-key-123456789"
	cfg.MapRoute.Enabled = false
	cfg.MapRoute.Provider = "fake"
	cfg.MapRoute.AmapKey = ""
	return cfg
}
