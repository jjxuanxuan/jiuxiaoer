package config

import (
	"strings"
	"testing"
	"time"
)

// TestProductionConfigRejectsMocksAndDefaultSecrets 验证生产配置拒绝模拟实现和默认密钥。
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

// TestProductionConfigAcceptsExplicitSafeValues 验证生产配置接受显式提供的安全值。
func TestProductionConfigAcceptsExplicitSafeValues(t *testing.T) {
	if err := validProductionConfig().Validate(); err != nil {
		t.Fatalf("expected production config to pass: %v", err)
	}
}

func TestRepurchaseConfigRejectsUnsafeBounds(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(cfg *Config) { cfg.Repurchase.LookbackDays = 0 },
		func(cfg *Config) { cfg.Repurchase.LookbackDays = 3651 },
		func(cfg *Config) { cfg.Repurchase.MaxItems = 0 },
		func(cfg *Config) { cfg.Repurchase.MaxItems = 21 },
	} {
		cfg := Load()
		mutate(&cfg)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "invalid repurchase lookback or item limit configuration") {
			t.Fatalf("expected invalid repurchase configuration to fail, got %v", err)
		}
	}
}

func TestCP1ReleaseProfileRequiresEnforcedDeliveryVerification(t *testing.T) {
	for _, mode := range []string{"off", "observe"} {
		t.Run("pickup-"+mode, func(t *testing.T) {
			cfg := validCP1ReleaseConfig()
			cfg.CP1.PickupVerificationMode = mode
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), "JXE_CP1_PICKUP_VERIFICATION_MODE=enforce") {
				t.Fatalf("expected unsafe pickup verification mode to fail, got %v", err)
			}
		})
		t.Run("delivery-"+mode, func(t *testing.T) {
			cfg := validCP1ReleaseConfig()
			cfg.CP1.DeliveryVerificationMode = mode
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), "JXE_CP1_DELIVERY_VERIFICATION_MODE=enforce") {
				t.Fatalf("expected unsafe delivery verification mode to fail, got %v", err)
			}
		})
	}
}

func TestProductionWithoutCP1ReleaseProfileAllowsApprovedDegradation(t *testing.T) {
	cfg := validProductionConfig()
	cfg.CP1.ReleaseProfile = CP1ReleaseProfileOff
	cfg.CP1.PrintEnabled = false
	cfg.CP1.PrintProvider = "fake"
	cfg.CP1.NotificationEnabled = false
	cfg.CP1.NotificationProvider = "fake"
	cfg.CP1.ComplianceMode = "off"
	cfg.CP1.IdentityProvider = "fake"
	cfg.CP1.PickupVerificationMode = "observe"
	cfg.CP1.DeliveryVerificationMode = "observe"
	cfg.Realtime.Enabled = false
	cfg.Realtime.RelayEnabled = false
	cfg.Feature.MQPublisherEnabled = false
	cfg.MQ.ConsumerNotificationEnabled = false
	cfg.MQ.ConsumerPrintEnabled = false
	cfg.MQ.ConsumerCacheEnabled = false
	cfg.MQ.ConsumerSecurityEnabled = false
	cfg.MQ.ConsumerDispatchEnabled = false
	cfg.MQ.ConsumerRealtimeEnabled = false
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected ordinary production degradation config to pass: %v", err)
	}
}

func TestCP1ReleaseProfileRequiresCompleteRuntime(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*Config)
		expected string
	}{
		{name: "production baseline", mutate: func(cfg *Config) { cfg.App.Env = "preprod" }, expected: "JXE_APP_ENV=production"},
		{name: "printing", mutate: func(cfg *Config) { cfg.CP1.PrintEnabled = false }, expected: "JXE_CP1_PRINT_ENABLED=true"},
		{name: "real print provider", mutate: func(cfg *Config) { cfg.CP1.PrintProvider = "fake" }, expected: "non-fake JXE_CP1_PRINT_PROVIDER"},
		{name: "cp1 worker", mutate: func(cfg *Config) { cfg.CP1.WorkerEnabled = false }, expected: "JXE_CP1_WORKER_ENABLED=true"},
		{name: "order idempotency", mutate: func(cfg *Config) { cfg.Feature.OrderIdempotencyEnabled = false }, expected: "JXE_ORDER_IDEMPOTENCY_ENABLED=true"},
		{name: "stock reservation", mutate: func(cfg *Config) { cfg.Feature.StockReserveEnabled = false }, expected: "JXE_STOCK_RESERVE_ENABLED=true"},
		{name: "realtime", mutate: func(cfg *Config) { cfg.Realtime.Enabled = false }, expected: "JXE_REALTIME_ENABLED=true"},
		{name: "realtime relay", mutate: func(cfg *Config) { cfg.Realtime.RelayEnabled = false }, expected: "JXE_REALTIME_RELAY_ENABLED=true"},
		{name: "publisher", mutate: func(cfg *Config) { cfg.Feature.MQPublisherEnabled = false }, expected: "JXE_MQ_PUBLISH_ENABLED=true"},
		{name: "notification consumer", mutate: func(cfg *Config) { cfg.MQ.ConsumerNotificationEnabled = false }, expected: "JXE_MQ_CONSUMER_NOTIFICATION_ENABLED=true"},
		{name: "print consumer", mutate: func(cfg *Config) { cfg.MQ.ConsumerPrintEnabled = false }, expected: "JXE_MQ_CONSUMER_PRINT_ENABLED=true"},
		{name: "cache consumer", mutate: func(cfg *Config) { cfg.MQ.ConsumerCacheEnabled = false }, expected: "JXE_MQ_CONSUMER_CACHE_ENABLED=true"},
		{name: "security consumer", mutate: func(cfg *Config) { cfg.MQ.ConsumerSecurityEnabled = false }, expected: "JXE_MQ_CONSUMER_SECURITY_ENABLED=true"},
		{name: "dispatch consumer", mutate: func(cfg *Config) { cfg.MQ.ConsumerDispatchEnabled = false }, expected: "JXE_MQ_CONSUMER_DISPATCH_ENABLED=true"},
		{name: "realtime consumer", mutate: func(cfg *Config) { cfg.MQ.ConsumerRealtimeEnabled = false }, expected: "JXE_MQ_CONSUMER_REALTIME_ENABLED=true"},
		{name: "database fallback", mutate: func(cfg *Config) { cfg.MQ.DBFallbackEnabled = false }, expected: "JXE_MQ_DB_FALLBACK_ENABLED=true"},
		{name: "topology gate", mutate: func(cfg *Config) { cfg.MQ.FailOnTopologyDrift = false }, expected: "JXE_MQ_FAIL_ON_TOPOLOGY_DRIFT=true"},
		{name: "rabbitmq required", mutate: func(cfg *Config) { cfg.RabbitMQ.Required = false }, expected: "JXE_RABBITMQ_REQUIRED=true"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validCP1ReleaseConfig()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.expected) {
				t.Fatalf("expected release gate error containing %q, got %v", tt.expected, err)
			}
		})
	}
}

func TestCP1ReleaseProfileAcceptsCompleteRuntime(t *testing.T) {
	if err := validCP1ReleaseConfig().Validate(); err != nil {
		t.Fatalf("expected complete CP1 release profile to pass: %v", err)
	}
}

func TestCP1ReleaseProfileLoadsFromEnvironment(t *testing.T) {
	t.Setenv("JXE_CP1_RELEASE_PROFILE", CP1ReleaseProfilePhaseOne)
	if got := Load().CP1.ReleaseProfile; got != CP1ReleaseProfilePhaseOne {
		t.Fatalf("release profile=%q", got)
	}
}

func TestCP1CoreOrderGatesLoadFromDocumentedEnvironment(t *testing.T) {
	t.Setenv("JXE_ORDER_IDEMPOTENCY_ENABLED", "false")
	t.Setenv("JXE_STOCK_RESERVE_ENABLED", "false")
	// 这些相似名称刻意不受支持，设置它们绝不能覆盖上方长期使用的运行时配置键。
	t.Setenv("JXE_FEATURE_ORDER_IDEMPOTENCY_ENABLED", "true")
	t.Setenv("JXE_FEATURE_STOCK_RESERVE_ENABLED", "true")
	cfg := Load()
	if cfg.Feature.OrderIdempotencyEnabled || cfg.Feature.StockReserveEnabled {
		t.Fatalf("core order gates ignored documented env: %+v", cfg.Feature)
	}
	cfg.CP1.ReleaseProfile = CP1ReleaseProfilePhaseOne
	cfg.App.Env = "production"
	problems := strings.Join(cfg.CP1ReleaseProfileProblems(), "; ")
	for _, expected := range []string{"JXE_ORDER_IDEMPOTENCY_ENABLED=true", "JXE_STOCK_RESERVE_ENABLED=true"} {
		if !strings.Contains(problems, expected) {
			t.Fatalf("release problems %q do not name documented key %s", problems, expected)
		}
	}
}

func TestDeliveryVerificationTTLIsAtLeastTwoHours(t *testing.T) {
	cfg := Load()
	cfg.CP1.DeliveryVerificationTTL = 119 * time.Minute
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "invalid CP1 worker or verification configuration") {
		t.Fatalf("expected short delivery verification TTL to fail, got %v", err)
	}
}

func TestDeliveryVerificationMaxTTLIsConfigurableAndNotBelowFloor(t *testing.T) {
	t.Setenv("JXE_CP1_DELIVERY_VERIFICATION_MAX_TTL", "7h")
	cfg := Load()
	if cfg.CP1.DeliveryVerificationMaxTTL != 7*time.Hour {
		t.Fatalf("delivery verification max ttl=%s", cfg.CP1.DeliveryVerificationMaxTTL)
	}
	cfg.CP1.DeliveryVerificationTTL = 3 * time.Hour
	cfg.CP1.DeliveryVerificationMaxTTL = 2 * time.Hour
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "invalid CP1 worker or verification configuration") {
		t.Fatalf("expected max ttl below floor to fail, got %v", err)
	}
}

func TestEnabledPrintingRequiresConfiguredProvider(t *testing.T) {
	cfg := Load()
	cfg.CP1.PrintEnabled = true
	cfg.CP1.PrintProvider = ""
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "JXE_CP1_PRINT_PROVIDER") {
		t.Fatalf("expected enabled printing without a provider to fail, got %v", err)
	}
}

func TestPrintConsumerRequiresPrintingEnabled(t *testing.T) {
	cfg := Load()
	cfg.CP1.PrintEnabled = false
	cfg.MQ.ConsumerPrintEnabled = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "JXE_CP1_PRINT_ENABLED=true") {
		t.Fatalf("expected print consumer without printing to fail, got %v", err)
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

// TestConfigRejectsInvalidSnowflakeNode 验证配置拒绝无效的雪花节点编号。
func TestConfigRejectsInvalidSnowflakeNode(t *testing.T) {
	cfg := Load()
	cfg.App.SnowflakeNodeID = 1024
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid snowflake node to fail")
	}
}

// TestAssetFeatureDependencies 验证资产功能的依赖关系。
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
	cfg.CP1.PickupVerificationMode = "enforce"
	cfg.CP1.DeliveryVerificationMode = "enforce"
	cfg.CP1.VerificationPepper = "production-verification-pepper-123456789"
	cfg.CP1.IdentityCallbackSecret = "production-identity-callback-secret-123456789"
	cfg.CP1.DataEncryptionKey = "production-data-encryption-key-123456789"
	cfg.MapRoute.Enabled = false
	cfg.MapRoute.Provider = "fake"
	cfg.MapRoute.AmapKey = ""
	return cfg
}

func validCP1ReleaseConfig() Config {
	cfg := validProductionConfig()
	cfg.CP1.ReleaseProfile = CP1ReleaseProfilePhaseOne
	cfg.CP1.PrintEnabled = true
	cfg.CP1.WorkerEnabled = true
	cfg.CP1.ComplianceMode = "enforce"
	cfg.CP1.PickupVerificationMode = "enforce"
	cfg.CP1.DeliveryVerificationMode = "enforce"
	cfg.Realtime.Enabled = true
	cfg.Realtime.RelayEnabled = true
	cfg.Realtime.AllowedOrigins = []string{"https://merchant.example.com", "https://rider.example.com"}
	cfg.Feature.MQPublisherEnabled = true
	cfg.MQ.ConsumerNotificationEnabled = true
	cfg.MQ.ConsumerPrintEnabled = true
	cfg.MQ.ConsumerCacheEnabled = true
	cfg.MQ.ConsumerSecurityEnabled = true
	cfg.MQ.ConsumerDispatchEnabled = true
	cfg.MQ.ConsumerRealtimeEnabled = true
	cfg.MQ.DBFallbackEnabled = true
	cfg.MQ.FailOnTopologyDrift = true
	cfg.Dispatch.Enabled = true
	cfg.Dispatch.WorkerEnabled = true
	return cfg
}
