package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
)

type Config struct {
	App              AppConfig
	HTTP             HTTPConfig
	MySQL            MySQLConfig
	Redis            RedisConfig
	RabbitMQ         RabbitMQConfig
	MQ               MQConfig
	JWT              JWTConfig
	Order            OrderConfig
	Reconciliation   ReconciliationConfig
	AfterSale        AfterSaleConfig
	DeliveryIncident DeliveryIncidentConfig
	DeliveryReturn   DeliveryReturnConfig
	Asset            AssetConfig
	Dispatch         DispatchConfig
	Realtime         RealtimeConfig
	MapRoute         MapRouteConfig
	CustomerLBS      CustomerLBSConfig
	Search           SearchConfig
	RiderApplication RiderApplicationConfig
	Service          ServiceAreaConfig
	SMS              SMSConfig
	WeChat           WeChatConfig
	Feature          FeatureConfig
	WineTicket       WineTicketConfig
	Security         SecurityConfig
	Metrics          MetricsConfig
	CP1              CP1Config
}

type AppConfig struct {
	Name            string
	Env             string
	InstanceID      string
	SnowflakeNodeID int64
	NodeLeaseTTL    time.Duration
}

type HTTPConfig struct {
	Addr              string
	TrustedProxies    []string
	ShutdownTimeout   time.Duration
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	RequestTimeout    time.Duration
	MaxHeaderBytes    int
	MaxBodyBytes      int64
}

type MySQLConfig struct {
	DSN                            string
	Required                       bool
	MaxOpenConns                   int
	MaxIdleConns                   int
	ConnMaxLifetime                time.Duration
	PingTimeout                    time.Duration
	RequiredTimeZone               string
	RequireWineTicketSchema        bool
	RequireWineTicketMoneyContract bool
}

type RedisConfig struct {
	Addr        string
	Password    string
	DB          int
	Required    bool
	PingTimeout time.Duration
}

type RabbitMQConfig struct {
	URL           string
	ManagementURL string
	Required      bool
	DialTimeout   time.Duration
	ReconnectMin  time.Duration
	ReconnectMax  time.Duration
}

type MQConfig struct {
	TopologyVersion             string
	ConsumerNotificationEnabled bool
	ConsumerPrintEnabled        bool
	ConsumerCacheEnabled        bool
	ConsumerSecurityEnabled     bool
	ConsumerDispatchEnabled     bool
	ConsumerRealtimeEnabled     bool
	DBFallbackEnabled           bool
	FailOnTopologyDrift         bool
	PublisherBatchSize          int
}

type DispatchConfig struct {
	Enabled                  bool
	WorkerEnabled            bool
	ModeOverride             string
	WorkerInterval           time.Duration
	WorkerBatchSize          int
	LeaseDuration            time.Duration
	JobTimeout               time.Duration
	MaxAttempts              int
	HeartbeatPersistInterval time.Duration
}

// RealtimeConfig 控制骑手 WebSocket 投递平面。所有开关默认关闭，
// 确保仅部署二进制文件不会启用实时副作用。
type RealtimeConfig struct {
	Enabled                  bool
	RelayEnabled             bool
	TicketTTL                time.Duration
	HandshakeTimeout         time.Duration
	HeartbeatInterval        time.Duration
	PongTimeout              time.Duration
	SessionCheckInterval     time.Duration
	ShutdownDrainTimeout     time.Duration
	RelayInterval            time.Duration
	DeliveryRetention        time.Duration
	AcknowledgementRetention time.Duration
	MaxConnectionsPerRider   int
	SendQueueSize            int
	MaxFrameBytes            int64
	ResumeLimit              int
	RelayBatchSize           int
	AllowedOrigins           []string
	CanaryRiderIDs           []string
	TicketRiderRatePerMinute int
	TicketIPRatePerMinute    int
	HandshakeIPRatePerMinute int
	ACKRatePerMinute         int
	ResumeRatePerMinute      int
}

// MapRouteConfig 控制只读配送路线规划能力。该功能默认关闭，
// 并使用结果确定的模拟服务商。
type MapRouteConfig struct {
	Enabled              bool
	Provider             string
	AmapBaseURL          string
	AmapKey              string
	Mode                 string
	Strategy             string
	Timeout              time.Duration
	CacheTTL             time.Duration
	StaleTTL             time.Duration
	LocationFreshness    time.Duration
	MaxAccuracyM         float64
	RiderRatePerMinute   int
	AccountRatePerMinute int
	IPRatePerMinute      int
	MaxConcurrency       int
	CanaryRiderIDs       []string
	CacheHMACSecret      string
}

// CustomerLBSConfig 控制面向客户的位置上下文流程。它刻意与骑手路线规划分离，
// 使两类工作负载可以使用独立的高德密钥、额度和灰度开关。
type CustomerLBSConfig struct {
	Mode                   string
	Provider               string
	AmapBaseURL            string
	AmapKey                string
	RegeocodeEnabled       bool
	RouteRefineEnabled     bool
	ContextTTL             time.Duration
	RegeocodeTimeout       time.Duration
	RouteTimeout           time.Duration
	ResolveTimeout         time.Duration
	RegeocodeFreshTTL      time.Duration
	RegeocodeStaleTTL      time.Duration
	RouteFreshTTL          time.Duration
	RouteStaleTTL          time.Duration
	MaxRouteCandidates     int
	MaxAccuracyM           float64
	MaxConcurrency         int
	AnonymousIPRate        int
	CustomerRate           int
	SessionRate            int
	AllowedOrigins         []string
	CacheHMACSecret        string
	AnonymousSessionHeader string
}

// SearchConfig 控制客户搜索历史、热词排行和保留策略。
// MySQL 始终是权威数据源，Redis 仅作为短期排行缓存。
type SearchConfig struct {
	HistoryMax         int
	HistoryRetention   time.Duration
	HotWindowDays      int
	StatsRetentionDays int
	HotCacheTTL        time.Duration
	EventRatePerMinute int
	CleanupEnabled     bool
	CleanupInterval    time.Duration
	CleanupBatchSize   int
}

// RiderApplicationConfig 控制骑手正式入驻前的申请流程。该功能刻意默认关闭，
// 只有 MySQL 和 Redis 都可用时才能开放申请或审核流量。
type RiderApplicationConfig struct {
	Enabled                    bool
	TokenTTL                   time.Duration
	HMACSecret                 string
	MaxShops                   int
	SubmitIPRatePerHour        int
	SubmitPhoneRatePerDay      int
	LoginIPRatePerMinute       int
	LoginPhoneRatePer15Minutes int
	WriteAccountRatePerMinute  int
	ResubmitAccountRatePerDay  int
	ReviewAdminRatePerMinute   int
}

type JWTConfig struct {
	AccessSecret  string
	RefreshSecret string
	AccessTTL     time.Duration
	RefreshTTL    time.Duration
}

type OrderConfig struct {
	PaymentTTL           time.Duration
	CreatingReconcileAge time.Duration
	ExpiryWorkerEnabled  bool
	ExpiryScanInterval   time.Duration
	ExpiryBatchSize      int
}

// ReconciliationConfig 控制 T+1 微信交易账单和资金账单闭环。
// 本地默认关闭，生产支付必须启用。
type ReconciliationConfig struct {
	Enabled              bool
	WorkerEnabled        bool
	WorkerInterval       time.Duration
	RunHour              int
	LagDays              int
	StartDate            string
	BackfillDaysPerCycle int
	RequestTimeout       time.Duration
	InsertBatchSize      int
	RunningTimeout       time.Duration
}

type AfterSaleConfig struct {
	Enabled                 bool
	RefundExecutionEnabled  bool
	StandardWindow          time.Duration
	UnopenedReturnWindow    time.Duration
	PlatformReviewThreshold int64
	WorkerEnabled           bool
	WorkerInterval          time.Duration
	WorkerBatchSize         int
	EvidenceTokenSecret     string
}

// DeliveryIncidentConfig 使新的写入路径及其两个副作用在灰度期间可以独立开关。
type DeliveryIncidentConfig struct {
	Enabled               bool
	AutoResolveEnabled    bool
	NotificationEnabled   bool
	RiderAllowlist        []string
	ShopAllowlist         []string
	CreateRatePerHour     int
	EvidenceRatePerHour   int
	CreateIPRatePerHour   int
	EvidenceIPRatePerHour int
	EvidenceViewBaseURL   string
	EvidenceViewSecret    string
	EvidenceViewTTL       time.Duration
}

// DeliveryReturnConfig 使每个高风险分支都能独立开关。
// 仅打开总开关不会启用骑手写入、批准、收货或通知。
type DeliveryReturnConfig struct {
	Enabled                bool
	RiderWriteEnabled      bool
	ApprovalEnabled        bool
	ReceiptEnabled         bool
	SystemAfterSaleEnabled bool
	NotificationEnabled    bool
	PaidCancelStockEnabled bool
	RiderAllowlist         []string
	ShopAllowlist          []string
	HandoffTTL             time.Duration
	RiderRatePer10Minutes  int
	SLAWorkerEnabled       bool
	SLAWorkerInterval      time.Duration
	SLAWorkerBatchSize     int
	ReceiptReminderAfter   time.Duration
	ReceiptDeadlineAfter   time.Duration
}

type AssetConfig struct {
	MemberEnabled            bool
	ReadEnabled              bool
	WriteEnabled             bool
	CompensationIssueEnabled bool
	ExpiryEnabled            bool
	RepairEnabled            bool
	WorkerEnabled            bool
	WorkerInterval           time.Duration
	WorkerBatchSize          int
}

type ServiceAreaConfig struct {
	EnforcementMode string
	ResolveCacheTTL time.Duration
	HomeCacheTTL    time.Duration
	QueryTimeout    time.Duration
}

// SMSConfig 控制登录验证短信。本地开发可继续使用结果确定的模拟实现，
// 非模拟环境则使用带已审核签名和模板的腾讯云短信 API 3.0。
type SMSConfig struct {
	Enabled     bool
	Provider    string
	Region      string
	SecretID    string
	SecretKey   string
	SDKAppID    string
	SignName    string
	TemplateID  string
	Endpoint    string
	HTTPTimeout time.Duration
}

type WeChatConfig struct {
	AuthEnabled       bool
	AuthMockEnabled   bool
	MiniAppID         string
	MiniAppSecret     string
	APIBaseURL        string
	HTTPTimeout       time.Duration
	PayEnabled        bool
	PayMockEnabled    bool
	PayMchID          string
	PayCertSerial     string
	PayPrivateKeyPath string
	PayPublicKeyID    string
	PayPublicKeyPath  string
	PayAPIv3Key       string
	PayNotifyURL      string
	RefundNotifyURL   string
	PayDescription    string
}

type FeatureConfig struct {
	PaymentMockEnabled      bool
	SMSMockEnabled          bool
	OrderIdempotencyEnabled bool
	StockReserveEnabled     bool
	MQPublisherEnabled      bool
}

// WineTicketConfig 让所有酒票入口保持失败关闭。
// 总开关只会暴露已通过数据库结构和时区门禁的基础设施；
// 每个业务分支仍需单独显式开启。
type WineTicketConfig struct {
	Enabled                              bool
	MaintenanceOwner                     string
	PackageReadEnabled                   bool
	AdminEnabled                         bool
	PurchaseEnabled                      bool
	RedemptionEnabled                    bool
	GiftEnabled                          bool
	ReminderEnabled                      bool
	WeChatReminderEnabled                bool
	WeChatReminderProviderEnabled        bool
	WeChatReminderProductNameField       string
	WeChatReminderRemainingQuantityField string
	WeChatReminderExpiryDateField        string
	WeChatReminderPage                   string
	RenewalEnabled                       bool
	RefundEnabled                        bool
	ReconciliationEnabled                bool
	ReconciliationBatchSize              int
	ReconciliationBatchInterval          time.Duration
	ReconciliationSweepInterval          time.Duration
	ReconciliationDailyStart             time.Duration
	ReconciliationLeaseDuration          time.Duration
	GiftTokenPepper                      string
	QuoteTokenSecret                     string
}

const (
	WineTicketMaintenanceOwnerAPI    = "api"
	WineTicketMaintenanceOwnerWorker = "worker"
)

type SecurityConfig struct {
	AdminBootstrapPassword    string
	MerchantBootstrapPassword string
}

type MetricsConfig struct {
	Enabled bool
	Token   string
}

// CP1Config 包含一期闭环的灰度控制项和密钥。模式包括 off、observe 和 enforce。
// 在对应服务商与灰度门禁获批前，外部副作用默认保持关闭。
type CP1Config struct {
	ReleaseProfile             string
	PrintEnabled               bool
	NotificationEnabled        bool
	ProvisioningEnabled        bool
	ForceActionEnabled         bool
	PickupVerificationMode     string
	DeliveryVerificationMode   string
	ComplianceMode             string
	PrintProvider              string
	NotificationProvider       string
	IdentityProvider           string
	WorkerEnabled              bool
	WorkerInterval             time.Duration
	WorkerBatchSize            int
	VerificationTTL            time.Duration
	DeliveryVerificationTTL    time.Duration
	DeliveryVerificationMaxTTL time.Duration
	VerificationMaxAttempts    int
	VerificationLockDuration   time.Duration
	VerificationPepper         string
	IdentityCallbackSecret     string
	DataEncryptionKey          string
}

const (
	CP1ReleaseProfileOff      = "off"
	CP1ReleaseProfilePhaseOne = "phase_one"
)

// Load 读取环境变量并提供本地开发默认值。
// 依赖是否必需由 infra 包负责校验，不放在配置解析阶段处理。
func Load() Config {
	appEnv := env("JXE_APP_ENV", "local")
	wineTicketEnabled := boolEnv("JXE_WINE_TICKET_ENABLED", false)
	wineTicketMoneyEnabled := boolEnv("JXE_WINE_TICKET_PURCHASE_ENABLED", false) ||
		boolEnv("JXE_WINE_TICKET_RENEWAL_ENABLED", false) ||
		boolEnv("JXE_WINE_TICKET_REFUND_ENABLED", false)
	requiredMySQLTimeZone := env("JXE_MYSQL_REQUIRED_TIME_ZONE", "")
	if wineTicketEnabled && requiredMySQLTimeZone == "" {
		requiredMySQLTimeZone = "+08:00"
	}
	return Config{
		App: AppConfig{
			Name:            "jiuxiaoer-api",
			Env:             appEnv,
			InstanceID:      env("JXE_INSTANCE_ID", defaultInstanceID()),
			SnowflakeNodeID: int64Env("JXE_SNOWFLAKE_NODE_ID", 1),
			NodeLeaseTTL:    durationEnv("JXE_SNOWFLAKE_LEASE_TTL", 30*time.Second),
		},
		HTTP: HTTPConfig{
			Addr:              env("JXE_HTTP_ADDR", ":8080"),
			TrustedProxies:    csvEnv("JXE_HTTP_TRUSTED_PROXIES"),
			ShutdownTimeout:   durationEnv("JXE_HTTP_SHUTDOWN_TIMEOUT", 10*time.Second),
			ReadHeaderTimeout: durationEnv("JXE_HTTP_READ_HEADER_TIMEOUT", 5*time.Second),
			ReadTimeout:       durationEnv("JXE_HTTP_READ_TIMEOUT", 15*time.Second),
			WriteTimeout:      durationEnv("JXE_HTTP_WRITE_TIMEOUT", 30*time.Second),
			IdleTimeout:       durationEnv("JXE_HTTP_IDLE_TIMEOUT", 60*time.Second),
			RequestTimeout:    durationEnv("JXE_HTTP_REQUEST_TIMEOUT", 10*time.Second),
			MaxHeaderBytes:    intEnv("JXE_HTTP_MAX_HEADER_BYTES", 1<<20),
			MaxBodyBytes:      int64Env("JXE_HTTP_MAX_BODY_BYTES", 1<<20),
		},
		MySQL: MySQLConfig{
			DSN:                            env("JXE_MYSQL_DSN", ""),
			Required:                       boolEnv("JXE_MYSQL_REQUIRED", false),
			MaxOpenConns:                   intEnv("JXE_MYSQL_MAX_OPEN_CONNS", 50),
			MaxIdleConns:                   intEnv("JXE_MYSQL_MAX_IDLE_CONNS", 10),
			ConnMaxLifetime:                durationEnv("JXE_MYSQL_CONN_MAX_LIFETIME", 30*time.Minute),
			PingTimeout:                    durationEnv("JXE_MYSQL_PING_TIMEOUT", 3*time.Second),
			RequiredTimeZone:               requiredMySQLTimeZone,
			RequireWineTicketSchema:        wineTicketEnabled,
			RequireWineTicketMoneyContract: wineTicketEnabled && wineTicketMoneyEnabled,
		},
		Redis: RedisConfig{
			Addr:        env("JXE_REDIS_ADDR", ""),
			Password:    env("JXE_REDIS_PASSWORD", ""),
			DB:          intEnv("JXE_REDIS_DB", 0),
			Required:    boolEnv("JXE_REDIS_REQUIRED", false),
			PingTimeout: durationEnv("JXE_REDIS_PING_TIMEOUT", 500*time.Millisecond),
		},
		RabbitMQ: RabbitMQConfig{
			URL:           env("JXE_RABBITMQ_URL", ""),
			ManagementURL: env("JXE_RABBITMQ_MANAGEMENT_URL", ""),
			Required:      boolEnv("JXE_RABBITMQ_REQUIRED", false),
			DialTimeout:   durationEnv("JXE_RABBITMQ_DIAL_TIMEOUT", 5*time.Second),
			ReconnectMin:  durationEnv("JXE_RABBITMQ_RECONNECT_MIN", time.Second),
			ReconnectMax:  durationEnv("JXE_RABBITMQ_RECONNECT_MAX", 30*time.Second),
		},
		MQ: MQConfig{
			TopologyVersion:             env("JXE_MQ_TOPOLOGY_VERSION", "v1"),
			ConsumerNotificationEnabled: boolEnv("JXE_MQ_CONSUMER_NOTIFICATION_ENABLED", false),
			ConsumerPrintEnabled:        boolEnv("JXE_MQ_CONSUMER_PRINT_ENABLED", false),
			ConsumerCacheEnabled:        boolEnv("JXE_MQ_CONSUMER_CACHE_ENABLED", true),
			ConsumerSecurityEnabled:     boolEnv("JXE_MQ_CONSUMER_SECURITY_ENABLED", false),
			ConsumerDispatchEnabled:     boolEnv("JXE_MQ_CONSUMER_DISPATCH_ENABLED", true),
			ConsumerRealtimeEnabled:     boolEnv("JXE_MQ_CONSUMER_REALTIME_ENABLED", false),
			DBFallbackEnabled:           boolEnv("JXE_MQ_DB_FALLBACK_ENABLED", true),
			FailOnTopologyDrift:         boolEnv("JXE_MQ_FAIL_ON_TOPOLOGY_DRIFT", appEnv == "preprod" || isProduction(appEnv)),
			PublisherBatchSize:          intEnv("JXE_MQ_PUBLISHER_BATCH_SIZE", 50),
		},
		JWT: JWTConfig{
			AccessSecret:  env("JXE_JWT_ACCESS_SECRET", "local_access_secret_change_me"),
			RefreshSecret: env("JXE_JWT_REFRESH_SECRET", "local_refresh_secret_change_me"),
			AccessTTL:     durationEnv("JXE_JWT_ACCESS_TTL", 2*time.Hour),
			RefreshTTL:    durationEnv("JXE_JWT_REFRESH_TTL", 168*time.Hour),
		},
		Order: OrderConfig{
			PaymentTTL:           durationEnv("JXE_ORDER_PAYMENT_TTL", 15*time.Minute),
			CreatingReconcileAge: durationEnv("JXE_PAYMENT_CREATING_RECONCILE_AGE", 30*time.Second),
			ExpiryWorkerEnabled:  boolEnv("JXE_ORDER_EXPIRY_WORKER_ENABLED", true),
			ExpiryScanInterval:   durationEnv("JXE_ORDER_EXPIRY_SCAN_INTERVAL", 10*time.Second),
			ExpiryBatchSize:      intEnv("JXE_ORDER_EXPIRY_BATCH_SIZE", 100),
		},
		Reconciliation: ReconciliationConfig{
			Enabled:              boolEnv("JXE_WECHAT_BILL_RECONCILIATION_ENABLED", false),
			WorkerEnabled:        boolEnv("JXE_WECHAT_BILL_RECONCILIATION_WORKER_ENABLED", true),
			WorkerInterval:       durationEnv("JXE_WECHAT_BILL_RECONCILIATION_INTERVAL", 30*time.Minute),
			RunHour:              intEnv("JXE_WECHAT_BILL_RECONCILIATION_RUN_HOUR", 10),
			LagDays:              intEnv("JXE_WECHAT_BILL_RECONCILIATION_LAG_DAYS", 1),
			StartDate:            env("JXE_WECHAT_BILL_RECONCILIATION_START_DATE", ""),
			BackfillDaysPerCycle: intEnv("JXE_WECHAT_BILL_RECONCILIATION_BACKFILL_DAYS_PER_CYCLE", 3),
			RequestTimeout:       durationEnv("JXE_WECHAT_BILL_RECONCILIATION_REQUEST_TIMEOUT", 5*time.Minute),
			InsertBatchSize:      intEnv("JXE_WECHAT_BILL_RECONCILIATION_INSERT_BATCH_SIZE", 200),
			RunningTimeout:       durationEnv("JXE_WECHAT_BILL_RECONCILIATION_RUNNING_TIMEOUT", 30*time.Minute),
		},
		AfterSale: AfterSaleConfig{
			Enabled:                 boolEnv("JXE_AFTERSALE_ENABLED", false),
			RefundExecutionEnabled:  boolEnv("JXE_REFUND_EXECUTION_ENABLED", false),
			StandardWindow:          durationEnv("JXE_AFTERSALE_STANDARD_WINDOW", 48*time.Hour),
			UnopenedReturnWindow:    durationEnv("JXE_AFTERSALE_UNOPENED_WINDOW", 7*24*time.Hour),
			PlatformReviewThreshold: int64Env("JXE_AFTERSALE_PLATFORM_REVIEW_THRESHOLD", 50000),
			WorkerEnabled:           boolEnv("JXE_REFUND_WORKER_ENABLED", true),
			WorkerInterval:          durationEnv("JXE_REFUND_WORKER_INTERVAL", 10*time.Second),
			WorkerBatchSize:         intEnv("JXE_REFUND_WORKER_BATCH_SIZE", 50),
			EvidenceTokenSecret:     env("JXE_EVIDENCE_TOKEN_SECRET", "local_evidence_token_secret_change_me"),
		},
		DeliveryIncident: DeliveryIncidentConfig{
			Enabled:               boolEnv("JXE_DELIVERY_INCIDENT_ENABLED", false),
			AutoResolveEnabled:    boolEnv("JXE_DELIVERY_INCIDENT_AUTO_RESOLVE_ENABLED", false),
			NotificationEnabled:   boolEnv("JXE_DELIVERY_INCIDENT_NOTIFICATION_ENABLED", false),
			RiderAllowlist:        csvEnv("JXE_DELIVERY_INCIDENT_RIDER_ALLOWLIST"),
			ShopAllowlist:         csvEnv("JXE_DELIVERY_INCIDENT_SHOP_ALLOWLIST"),
			CreateRatePerHour:     intEnv("JXE_DELIVERY_INCIDENT_CREATE_RATE_PER_HOUR", 20),
			EvidenceRatePerHour:   intEnv("JXE_DELIVERY_INCIDENT_EVIDENCE_RATE_PER_HOUR", 30),
			CreateIPRatePerHour:   intEnv("JXE_DELIVERY_INCIDENT_CREATE_IP_RATE_PER_HOUR", 100),
			EvidenceIPRatePerHour: intEnv("JXE_DELIVERY_INCIDENT_EVIDENCE_IP_RATE_PER_HOUR", 150),
			EvidenceViewBaseURL:   strings.TrimSpace(os.Getenv("JXE_EVIDENCE_VIEW_BASE_URL")),
			EvidenceViewSecret:    strings.TrimSpace(os.Getenv("JXE_EVIDENCE_VIEW_SECRET")),
			EvidenceViewTTL:       durationEnv("JXE_EVIDENCE_VIEW_TTL", 5*time.Minute),
		},
		DeliveryReturn: DeliveryReturnConfig{
			Enabled:                boolEnv("JXE_DELIVERY_RETURN_ENABLED", false),
			RiderWriteEnabled:      boolEnv("JXE_DELIVERY_RETURN_RIDER_WRITE_ENABLED", false),
			ApprovalEnabled:        boolEnv("JXE_DELIVERY_RETURN_APPROVAL_ENABLED", false),
			ReceiptEnabled:         boolEnv("JXE_DELIVERY_RETURN_RECEIPT_ENABLED", false),
			SystemAfterSaleEnabled: boolEnv("JXE_DELIVERY_RETURN_SYSTEM_AFTERSALE_ENABLED", false),
			NotificationEnabled:    boolEnv("JXE_DELIVERY_RETURN_NOTIFICATION_ENABLED", false),
			PaidCancelStockEnabled: boolEnv("JXE_PAID_CANCEL_STOCK_DISPOSITION_ENABLED", false),
			RiderAllowlist:         csvEnv("JXE_DELIVERY_RETURN_RIDER_ALLOWLIST"),
			ShopAllowlist:          csvEnv("JXE_DELIVERY_RETURN_SHOP_ALLOWLIST"),
			HandoffTTL:             durationEnv("JXE_DELIVERY_RETURN_HANDOFF_TTL", 10*time.Minute),
			RiderRatePer10Minutes:  intEnv("JXE_DELIVERY_RETURN_RIDER_RATE_PER_10_MINUTES", 10),
			SLAWorkerEnabled:       boolEnv("JXE_DELIVERY_RETURN_SLA_WORKER_ENABLED", true),
			SLAWorkerInterval:      durationEnv("JXE_DELIVERY_RETURN_SLA_WORKER_INTERVAL", time.Minute),
			SLAWorkerBatchSize:     intEnv("JXE_DELIVERY_RETURN_SLA_WORKER_BATCH_SIZE", 100),
			ReceiptReminderAfter:   durationEnv("JXE_DELIVERY_RETURN_RECEIPT_REMINDER_AFTER", 2*time.Hour),
			ReceiptDeadlineAfter:   durationEnv("JXE_DELIVERY_RETURN_RECEIPT_DEADLINE_AFTER", 24*time.Hour),
		},
		Asset: AssetConfig{
			MemberEnabled:            boolEnv("JXE_MEMBER_ENABLED", false),
			ReadEnabled:              boolEnv("JXE_ASSET_READ_ENABLED", false),
			WriteEnabled:             boolEnv("JXE_ASSET_WRITE_ENABLED", false),
			CompensationIssueEnabled: boolEnv("JXE_COMPENSATION_ISSUE_ENABLED", false),
			ExpiryEnabled:            boolEnv("JXE_ASSET_EXPIRY_ENABLED", false),
			RepairEnabled:            boolEnv("JXE_ASSET_REPAIR_ENABLED", false),
			WorkerEnabled:            boolEnv("JXE_ASSET_WORKER_ENABLED", true),
			WorkerInterval:           durationEnv("JXE_ASSET_WORKER_INTERVAL", 10*time.Second),
			WorkerBatchSize:          intEnv("JXE_ASSET_WORKER_BATCH_SIZE", 100),
		},
		Dispatch: DispatchConfig{
			Enabled:                  boolEnv("JXE_DISPATCH_ENABLED", true),
			WorkerEnabled:            boolEnv("JXE_DISPATCH_WORKER_ENABLED", true),
			ModeOverride:             env("JXE_DISPATCH_MODE_OVERRIDE", ""),
			WorkerInterval:           durationEnv("JXE_DISPATCH_WORKER_INTERVAL", time.Second),
			WorkerBatchSize:          intEnv("JXE_DISPATCH_WORKER_BATCH_SIZE", 100),
			LeaseDuration:            durationEnv("JXE_DISPATCH_LEASE_DURATION", 15*time.Second),
			JobTimeout:               durationEnv("JXE_DISPATCH_JOB_TIMEOUT", 5*time.Second),
			MaxAttempts:              intEnv("JXE_DISPATCH_MAX_ATTEMPTS", 10),
			HeartbeatPersistInterval: durationEnv("JXE_DISPATCH_HEARTBEAT_PERSIST_INTERVAL", 30*time.Second),
		},
		Realtime: RealtimeConfig{
			Enabled:                  boolEnv("JXE_REALTIME_ENABLED", false),
			RelayEnabled:             boolEnv("JXE_REALTIME_RELAY_ENABLED", false),
			TicketTTL:                durationEnv("JXE_REALTIME_TICKET_TTL", time.Minute),
			HandshakeTimeout:         durationEnv("JXE_REALTIME_HANDSHAKE_TIMEOUT", 5*time.Second),
			HeartbeatInterval:        durationEnv("JXE_REALTIME_HEARTBEAT_INTERVAL", 25*time.Second),
			PongTimeout:              durationEnv("JXE_REALTIME_PONG_TIMEOUT", 60*time.Second),
			SessionCheckInterval:     durationEnv("JXE_REALTIME_SESSION_CHECK_INTERVAL", 60*time.Second),
			ShutdownDrainTimeout:     durationEnv("JXE_REALTIME_SHUTDOWN_DRAIN_TIMEOUT", 5*time.Second),
			RelayInterval:            durationEnv("JXE_REALTIME_RELAY_INTERVAL", time.Second),
			DeliveryRetention:        durationEnv("JXE_REALTIME_DELIVERY_RETENTION", 7*24*time.Hour),
			AcknowledgementRetention: durationEnv("JXE_REALTIME_ACK_RETENTION", 30*24*time.Hour),
			MaxConnectionsPerRider:   intEnv("JXE_REALTIME_MAX_CONNECTIONS_PER_RIDER", 3),
			SendQueueSize:            intEnv("JXE_REALTIME_SEND_QUEUE_SIZE", 32),
			MaxFrameBytes:            int64Env("JXE_REALTIME_MAX_FRAME_BYTES", 8<<10),
			ResumeLimit:              intEnv("JXE_REALTIME_RESUME_LIMIT", 100),
			RelayBatchSize:           intEnv("JXE_REALTIME_RELAY_BATCH_SIZE", 100),
			AllowedOrigins:           csvEnv("JXE_REALTIME_ALLOWED_ORIGINS"),
			CanaryRiderIDs:           csvEnv("JXE_REALTIME_CANARY_RIDER_IDS"),
			TicketRiderRatePerMinute: intEnv("JXE_REALTIME_TICKET_RIDER_RATE_PER_MINUTE", 10),
			TicketIPRatePerMinute:    intEnv("JXE_REALTIME_TICKET_IP_RATE_PER_MINUTE", 30),
			HandshakeIPRatePerMinute: intEnv("JXE_REALTIME_HANDSHAKE_IP_RATE_PER_MINUTE", 60),
			ACKRatePerMinute:         intEnv("JXE_REALTIME_ACK_RATE_PER_MINUTE", 120),
			ResumeRatePerMinute:      intEnv("JXE_REALTIME_RESUME_RATE_PER_MINUTE", 6),
		},
		MapRoute: MapRouteConfig{
			Enabled:              boolEnv("JXE_MAP_ROUTE_ENABLED", false),
			Provider:             env("JXE_MAP_ROUTE_PROVIDER", "fake"),
			AmapBaseURL:          env("JXE_MAP_ROUTE_AMAP_BASE_URL", "https://restapi.amap.com"),
			AmapKey:              env("JXE_MAP_ROUTE_AMAP_KEY", ""),
			Mode:                 env("JXE_MAP_ROUTE_MODE", "electric_bicycle"),
			Strategy:             env("JXE_MAP_ROUTE_STRATEGY", "default"),
			Timeout:              durationEnv("JXE_MAP_ROUTE_TIMEOUT", 1500*time.Millisecond),
			CacheTTL:             durationEnv("JXE_MAP_ROUTE_CACHE_TTL", time.Minute),
			StaleTTL:             durationEnv("JXE_MAP_ROUTE_STALE_TTL", 10*time.Minute),
			LocationFreshness:    durationEnv("JXE_MAP_ROUTE_LOCATION_FRESHNESS", 120*time.Second),
			MaxAccuracyM:         float64Env("JXE_MAP_ROUTE_MAX_ACCURACY_M", 200),
			RiderRatePerMinute:   intEnv("JXE_MAP_ROUTE_RIDER_RATE_PER_MINUTE", 20),
			AccountRatePerMinute: intEnv("JXE_MAP_ROUTE_ACCOUNT_RATE_PER_MINUTE", 30),
			IPRatePerMinute:      intEnv("JXE_MAP_ROUTE_IP_RATE_PER_MINUTE", 60),
			MaxConcurrency:       intEnv("JXE_MAP_ROUTE_MAX_CONCURRENCY", 50),
			CanaryRiderIDs:       csvEnv("JXE_MAP_ROUTE_CANARY_RIDER_IDS"),
			CacheHMACSecret:      env("JXE_MAP_ROUTE_CACHE_HMAC_SECRET", "local_map_route_hmac_secret_change_me"),
		},
		CustomerLBS: CustomerLBSConfig{
			Mode:                   env("JXE_C_LBS_MODE", "off"),
			Provider:               env("JXE_C_LBS_PROVIDER", "amap"),
			AmapBaseURL:            env("JXE_C_LBS_AMAP_BASE_URL", "https://restapi.amap.com"),
			AmapKey:                env("JXE_C_LBS_AMAP_KEY", ""),
			RegeocodeEnabled:       boolEnv("JXE_C_LBS_REGEOCODE_ENABLED", false),
			RouteRefineEnabled:     boolEnv("JXE_C_LBS_ROUTE_REFINE_ENABLED", false),
			ContextTTL:             durationEnv("JXE_C_LBS_CONTEXT_TTL", 10*time.Minute),
			RegeocodeTimeout:       durationEnv("JXE_C_LBS_REGEOCODE_TIMEOUT", 2*time.Second),
			RouteTimeout:           durationEnv("JXE_C_LBS_ROUTE_TIMEOUT", 2*time.Second),
			ResolveTimeout:         durationEnv("JXE_C_LBS_RESOLVE_TIMEOUT", 3*time.Second),
			RegeocodeFreshTTL:      durationEnv("JXE_C_LBS_REGEOCODE_FRESH_TTL", 6*time.Hour),
			RegeocodeStaleTTL:      durationEnv("JXE_C_LBS_REGEOCODE_STALE_TTL", 7*24*time.Hour),
			RouteFreshTTL:          durationEnv("JXE_C_LBS_ROUTE_FRESH_TTL", 5*time.Minute),
			RouteStaleTTL:          durationEnv("JXE_C_LBS_ROUTE_STALE_TTL", 30*time.Minute),
			MaxRouteCandidates:     intEnv("JXE_C_LBS_MAX_ROUTE_CANDIDATES", 3),
			MaxAccuracyM:           float64Env("JXE_C_LBS_MAX_ACCURACY_M", 200),
			MaxConcurrency:         intEnv("JXE_C_LBS_MAX_CONCURRENCY", 50),
			AnonymousIPRate:        intEnv("JXE_C_LBS_ANONYMOUS_IP_RATE", 30),
			CustomerRate:           intEnv("JXE_C_LBS_CUSTOMER_RATE", 20),
			SessionRate:            intEnv("JXE_C_LBS_SESSION_RATE", 10),
			AllowedOrigins:         csvEnv("JXE_C_LBS_ALLOWED_ORIGINS"),
			CacheHMACSecret:        env("JXE_C_LBS_CACHE_HMAC_SECRET", "local_customer_lbs_hmac_secret_change_me"),
			AnonymousSessionHeader: env("JXE_C_LBS_SESSION_HEADER", "X-Session-ID"),
		},
		Search: SearchConfig{
			HistoryMax:         intEnv("JXE_SEARCH_HISTORY_MAX", 20),
			HistoryRetention:   durationEnv("JXE_SEARCH_HISTORY_RETENTION", 180*24*time.Hour),
			HotWindowDays:      intEnv("JXE_SEARCH_HOT_WINDOW_DAYS", 7),
			StatsRetentionDays: intEnv("JXE_SEARCH_STATS_RETENTION_DAYS", 30),
			HotCacheTTL:        durationEnv("JXE_SEARCH_HOT_CACHE_TTL", 5*time.Minute),
			EventRatePerMinute: intEnv("JXE_SEARCH_EVENT_RATE_LIMIT", 30),
			CleanupEnabled:     boolEnv("JXE_SEARCH_CLEANUP_ENABLED", true),
			CleanupInterval:    durationEnv("JXE_SEARCH_CLEANUP_INTERVAL", 24*time.Hour),
			CleanupBatchSize:   intEnv("JXE_SEARCH_CLEANUP_BATCH_SIZE", 1000),
		},
		RiderApplication: RiderApplicationConfig{
			Enabled:                    boolEnv("JXE_RIDER_APPLICATION_ENABLED", false),
			TokenTTL:                   durationEnv("JXE_RIDER_APPLICATION_TOKEN_TTL", 30*time.Minute),
			HMACSecret:                 env("JXE_RIDER_APPLICATION_HMAC_SECRET", "local_rider_application_hmac_secret_change_me"),
			MaxShops:                   intEnv("JXE_RIDER_APPLICATION_MAX_SHOPS", 50),
			SubmitIPRatePerHour:        intEnv("JXE_RIDER_APPLICATION_SUBMIT_IP_RATE_PER_HOUR", 10),
			SubmitPhoneRatePerDay:      intEnv("JXE_RIDER_APPLICATION_SUBMIT_PHONE_RATE_PER_DAY", 3),
			LoginIPRatePerMinute:       intEnv("JXE_RIDER_APPLICATION_LOGIN_IP_RATE_PER_MINUTE", 30),
			LoginPhoneRatePer15Minutes: intEnv("JXE_RIDER_APPLICATION_LOGIN_PHONE_RATE_PER_15_MINUTES", 10),
			WriteAccountRatePerMinute:  intEnv("JXE_RIDER_APPLICATION_WRITE_ACCOUNT_RATE_PER_MINUTE", 10),
			ResubmitAccountRatePerDay:  intEnv("JXE_RIDER_APPLICATION_RESUBMIT_ACCOUNT_RATE_PER_DAY", 3),
			ReviewAdminRatePerMinute:   intEnv("JXE_RIDER_APPLICATION_REVIEW_ADMIN_RATE_PER_MINUTE", 60),
		},
		Service: ServiceAreaConfig{
			EnforcementMode: env("JXE_ORDER_SERVICE_AREA_ENFORCEMENT", "off"),
			ResolveCacheTTL: durationEnv("JXE_SERVICE_AREA_CACHE_TTL", 10*time.Second),
			HomeCacheTTL:    durationEnv("JXE_HOME_CACHE_TTL", time.Minute),
			QueryTimeout:    durationEnv("JXE_SERVICE_AREA_QUERY_TIMEOUT", 200*time.Millisecond),
		},
		SMS: SMSConfig{
			Enabled:     boolEnv("JXE_SMS_ENABLED", true),
			Provider:    env("JXE_SMS_PROVIDER", "tencentcloud"),
			Region:      env("JXE_SMS_TENCENTCLOUD_REGION", "ap-guangzhou"),
			SecretID:    env("JXE_SMS_TENCENTCLOUD_SECRET_ID", ""),
			SecretKey:   env("JXE_SMS_TENCENTCLOUD_SECRET_KEY", ""),
			SDKAppID:    env("JXE_SMS_TENCENTCLOUD_SDK_APP_ID", ""),
			SignName:    env("JXE_SMS_TENCENTCLOUD_SIGN_NAME", ""),
			TemplateID:  env("JXE_SMS_TENCENTCLOUD_TEMPLATE_ID", ""),
			Endpoint:    env("JXE_SMS_TENCENTCLOUD_ENDPOINT", "sms.tencentcloudapi.com"),
			HTTPTimeout: durationEnv("JXE_SMS_HTTP_TIMEOUT", 5*time.Second),
		},
		WeChat: WeChatConfig{
			AuthEnabled:       boolEnv("JXE_WECHAT_AUTH_ENABLED", true),
			AuthMockEnabled:   boolEnv("JXE_WECHAT_AUTH_MOCK_ENABLED", true),
			MiniAppID:         env("JXE_WECHAT_MINIAPP_ID", "local-miniapp"),
			MiniAppSecret:     env("JXE_WECHAT_MINIAPP_SECRET", ""),
			APIBaseURL:        env("JXE_WECHAT_API_BASE_URL", "https://api.weixin.qq.com"),
			HTTPTimeout:       durationEnv("JXE_WECHAT_HTTP_TIMEOUT", 5*time.Second),
			PayEnabled:        boolEnv("JXE_WECHAT_PAY_ENABLED", true),
			PayMockEnabled:    boolEnv("JXE_WECHAT_PAY_MOCK_ENABLED", true),
			PayMchID:          env("JXE_WECHAT_PAY_MCH_ID", ""),
			PayCertSerial:     env("JXE_WECHAT_PAY_CERT_SERIAL", ""),
			PayPrivateKeyPath: env("JXE_WECHAT_PAY_PRIVATE_KEY_PATH", ""),
			PayPublicKeyID:    env("JXE_WECHAT_PAY_PUBLIC_KEY_ID", ""),
			PayPublicKeyPath:  env("JXE_WECHAT_PAY_PUBLIC_KEY_PATH", ""),
			PayAPIv3Key:       env("JXE_WECHAT_PAY_API_V3_KEY", ""),
			PayNotifyURL:      env("JXE_WECHAT_PAY_NOTIFY_URL", ""),
			RefundNotifyURL:   env("JXE_WECHAT_REFUND_NOTIFY_URL", ""),
			PayDescription:    env("JXE_WECHAT_PAY_DESCRIPTION", "酒小二订单"),
		},
		Feature: FeatureConfig{
			PaymentMockEnabled:      boolEnv("JXE_PAYMENT_MOCK_ENABLED", true),
			SMSMockEnabled:          boolEnv("JXE_SMS_MOCK_ENABLED", true),
			OrderIdempotencyEnabled: boolEnv("JXE_ORDER_IDEMPOTENCY_ENABLED", true),
			StockReserveEnabled:     boolEnv("JXE_STOCK_RESERVE_ENABLED", true),
			MQPublisherEnabled:      boolEnv("JXE_MQ_PUBLISH_ENABLED", boolEnv("JXE_MQ_PUBLISHER_ENABLED", false)),
		},
		WineTicket: WineTicketConfig{
			Enabled:            wineTicketEnabled,
			MaintenanceOwner:   env("JXE_WINE_TICKET_MAINTENANCE_OWNER", WineTicketMaintenanceOwnerAPI),
			PackageReadEnabled: boolEnv("JXE_WINE_TICKET_PACKAGE_READ_ENABLED", false),
			AdminEnabled:       boolEnv("JXE_WINE_TICKET_ADMIN_ENABLED", false),
			PurchaseEnabled:    boolEnv("JXE_WINE_TICKET_PURCHASE_ENABLED", false),
			RedemptionEnabled:  boolEnv("JXE_WINE_TICKET_REDEMPTION_ENABLED", false),
			GiftEnabled:        boolEnv("JXE_WINE_TICKET_GIFT_ENABLED", false),
			ReminderEnabled:    boolEnv("JXE_WINE_TICKET_REMINDER_ENABLED", false),
			WeChatReminderEnabled: boolEnv(
				"JXE_WINE_TICKET_WECHAT_REMINDER_ENABLED",
				false,
			),
			WeChatReminderProviderEnabled: boolEnv(
				"JXE_WINE_TICKET_WECHAT_REMINDER_PROVIDER_ENABLED",
				false,
			),
			WeChatReminderProductNameField: env(
				"JXE_WINE_TICKET_WECHAT_REMINDER_PRODUCT_NAME_FIELD",
				"",
			),
			WeChatReminderRemainingQuantityField: env(
				"JXE_WINE_TICKET_WECHAT_REMINDER_REMAINING_QUANTITY_FIELD",
				"",
			),
			WeChatReminderExpiryDateField: env(
				"JXE_WINE_TICKET_WECHAT_REMINDER_EXPIRY_DATE_FIELD",
				"",
			),
			WeChatReminderPage: env(
				"JXE_WINE_TICKET_WECHAT_REMINDER_PAGE",
				"",
			),
			RenewalEnabled: boolEnv("JXE_WINE_TICKET_RENEWAL_ENABLED", false),
			RefundEnabled:  boolEnv("JXE_WINE_TICKET_REFUND_ENABLED", false),
			ReconciliationEnabled: boolEnv(
				"JXE_WINE_TICKET_RECONCILIATION_ENABLED",
				false,
			),
			ReconciliationBatchSize: intEnv(
				"JXE_WINE_TICKET_RECONCILIATION_BATCH_SIZE",
				250,
			),
			ReconciliationBatchInterval: durationEnv(
				"JXE_WINE_TICKET_RECONCILIATION_BATCH_INTERVAL",
				100*time.Millisecond,
			),
			ReconciliationSweepInterval: durationEnv(
				"JXE_WINE_TICKET_RECONCILIATION_SWEEP_INTERVAL",
				15*time.Minute,
			),
			ReconciliationDailyStart: durationEnv(
				"JXE_WINE_TICKET_RECONCILIATION_DAILY_START",
				5*time.Minute,
			),
			ReconciliationLeaseDuration: durationEnv(
				"JXE_WINE_TICKET_RECONCILIATION_LEASE_DURATION",
				5*time.Minute,
			),
			GiftTokenPepper:  env("JXE_WINE_TICKET_GIFT_TOKEN_PEPPER", "local_wine_ticket_gift_token_pepper_change_me"),
			QuoteTokenSecret: env("JXE_WINE_TICKET_QUOTE_TOKEN_SECRET", "local_wine_ticket_quote_token_secret_change_me"),
		},
		Security: SecurityConfig{
			AdminBootstrapPassword:    env("JXE_ADMIN_BOOTSTRAP_PASSWORD", "admin123"),
			MerchantBootstrapPassword: env("JXE_MERCHANT_BOOTSTRAP_PASSWORD", "merchant123"),
		},
		Metrics: MetricsConfig{
			Enabled: boolEnv("JXE_METRICS_ENABLED", true),
			Token:   env("JXE_METRICS_TOKEN", ""),
		},
		CP1: CP1Config{
			ReleaseProfile:             env("JXE_CP1_RELEASE_PROFILE", CP1ReleaseProfileOff),
			PrintEnabled:               boolEnv("JXE_CP1_PRINT_ENABLED", false),
			NotificationEnabled:        boolEnv("JXE_CP1_NOTIFICATION_ENABLED", false),
			ProvisioningEnabled:        boolEnv("JXE_CP1_PROVISIONING_ENABLED", true),
			ForceActionEnabled:         boolEnv("JXE_CP1_FORCE_ACTION_ENABLED", false),
			PickupVerificationMode:     env("JXE_CP1_PICKUP_VERIFICATION_MODE", "observe"),
			DeliveryVerificationMode:   env("JXE_CP1_DELIVERY_VERIFICATION_MODE", "observe"),
			ComplianceMode:             env("JXE_CP1_COMPLIANCE_MODE", "observe"),
			PrintProvider:              env("JXE_CP1_PRINT_PROVIDER", "fake"),
			NotificationProvider:       env("JXE_CP1_NOTIFICATION_PROVIDER", "fake"),
			IdentityProvider:           env("JXE_CP1_IDENTITY_PROVIDER", "fake"),
			WorkerEnabled:              boolEnv("JXE_CP1_WORKER_ENABLED", true),
			WorkerInterval:             durationEnv("JXE_CP1_WORKER_INTERVAL", 2*time.Second),
			WorkerBatchSize:            intEnv("JXE_CP1_WORKER_BATCH_SIZE", 50),
			VerificationTTL:            durationEnv("JXE_CP1_VERIFICATION_TTL", 30*time.Minute),
			DeliveryVerificationTTL:    durationEnv("JXE_CP1_DELIVERY_VERIFICATION_TTL", 2*time.Hour),
			DeliveryVerificationMaxTTL: durationEnv("JXE_CP1_DELIVERY_VERIFICATION_MAX_TTL", 6*time.Hour),
			VerificationMaxAttempts:    intEnv("JXE_CP1_VERIFICATION_MAX_ATTEMPTS", 5),
			VerificationLockDuration:   durationEnv("JXE_CP1_VERIFICATION_LOCK_DURATION", 15*time.Minute),
			VerificationPepper:         env("JXE_CP1_VERIFICATION_PEPPER", "local_verification_pepper_change_me"),
			IdentityCallbackSecret:     env("JXE_CP1_IDENTITY_CALLBACK_SECRET", "local_identity_callback_secret_change_me"),
			DataEncryptionKey:          env("JXE_CP1_DATA_ENCRYPTION_KEY", "local_data_encryption_key_change_me"),
		},
	}
}

// Validate 校验配置是否合法。
// Validate 拒绝不安全或内部不一致的运行时配置。
func (c Config) Validate() error {
	var problems []string
	if c.App.SnowflakeNodeID < 0 || c.App.SnowflakeNodeID > 1023 {
		problems = append(problems, "JXE_SNOWFLAKE_NODE_ID must be between 0 and 1023")
	}
	if c.App.InstanceID == "" {
		problems = append(problems, "JXE_INSTANCE_ID must not be empty")
	}
	if c.App.NodeLeaseTTL < 3*time.Second {
		problems = append(problems, "JXE_SNOWFLAKE_LEASE_TTL must be at least 3s")
	}
	if c.HTTP.ReadHeaderTimeout <= 0 || c.HTTP.ReadTimeout <= 0 || c.HTTP.WriteTimeout <= 0 || c.HTTP.IdleTimeout <= 0 || c.HTTP.RequestTimeout <= 0 {
		problems = append(problems, "all HTTP timeouts must be positive")
	}
	if c.HTTP.MaxHeaderBytes < 1024 || c.HTTP.MaxBodyBytes < 1024 {
		problems = append(problems, "HTTP header and body limits must be at least 1024 bytes")
	}
	if c.MySQL.MaxOpenConns <= 0 || c.MySQL.MaxIdleConns < 0 || c.MySQL.MaxIdleConns > c.MySQL.MaxOpenConns {
		problems = append(problems, "invalid MySQL connection pool limits")
	}
	problems = append(problems, c.wechatPayRuntimeProblems()...)
	if c.MySQL.PingTimeout <= 0 || c.Redis.PingTimeout <= 0 {
		problems = append(problems, "dependency ping timeouts must be positive")
	}
	problems = append(problems, c.wineTicketRuntimeProblems()...)
	if c.RabbitMQ.DialTimeout <= 0 || c.RabbitMQ.ReconnectMin <= 0 || c.RabbitMQ.ReconnectMax < c.RabbitMQ.ReconnectMin {
		problems = append(problems, "invalid RabbitMQ timeout or reconnect limits")
	}
	if c.MQ.TopologyVersion != "v1" {
		problems = append(problems, "JXE_MQ_TOPOLOGY_VERSION must be v1")
	}
	if c.MQ.PublisherBatchSize < 10 || c.MQ.PublisherBatchSize > 500 {
		problems = append(problems, "JXE_MQ_PUBLISHER_BATCH_SIZE must be between 10 and 500")
	}
	if c.Order.PaymentTTL < time.Minute || c.Order.CreatingReconcileAge <= 0 || c.Order.ExpiryScanInterval <= 0 || c.Order.ExpiryBatchSize <= 0 || c.Order.ExpiryBatchSize > 1000 {
		problems = append(problems, "invalid order payment expiry configuration")
	}
	if c.Reconciliation.WorkerInterval <= 0 || c.Reconciliation.RunHour < 10 || c.Reconciliation.RunHour > 23 || c.Reconciliation.LagDays < 1 || c.Reconciliation.LagDays > 90 || c.Reconciliation.BackfillDaysPerCycle < 1 || c.Reconciliation.BackfillDaysPerCycle > 30 || c.Reconciliation.RequestTimeout <= 0 || c.Reconciliation.InsertBatchSize < 1 || c.Reconciliation.InsertBatchSize > 1000 || c.Reconciliation.RunningTimeout <= c.Reconciliation.RequestTimeout {
		problems = append(problems, "invalid WeChat bill reconciliation configuration")
	}
	if c.Reconciliation.StartDate != "" {
		startDate, err := time.Parse("2006-01-02", c.Reconciliation.StartDate)
		if err != nil {
			problems = append(problems, "JXE_WECHAT_BILL_RECONCILIATION_START_DATE must use YYYY-MM-DD")
		} else {
			chinaTime := time.Now().In(time.FixedZone("CST", 8*60*60))
			today, _ := time.Parse("2006-01-02", chinaTime.Format("2006-01-02"))
			if startDate.After(today) {
				problems = append(problems, "JXE_WECHAT_BILL_RECONCILIATION_START_DATE cannot be in the future")
			}
		}
	} else if c.Reconciliation.Enabled {
		problems = append(problems, "enabled WeChat bill reconciliation requires JXE_WECHAT_BILL_RECONCILIATION_START_DATE")
	}
	if c.Reconciliation.Enabled && (!c.WeChat.PayEnabled || c.MySQL.DSN == "") {
		problems = append(problems, "WeChat bill reconciliation requires enabled WeChat Pay and MySQL")
	}
	if c.AfterSale.StandardWindow <= 0 || c.AfterSale.UnopenedReturnWindow <= 0 || c.AfterSale.PlatformReviewThreshold <= 0 || c.AfterSale.WorkerInterval <= 0 || c.AfterSale.WorkerBatchSize <= 0 || c.AfterSale.WorkerBatchSize > 1000 {
		problems = append(problems, "invalid after-sale or refund worker configuration")
	}
	if c.AfterSale.RefundExecutionEnabled && !c.AfterSale.Enabled {
		problems = append(problems, "refund execution requires JXE_AFTERSALE_ENABLED=true")
	}
	if c.DeliveryIncident.CreateRatePerHour < 1 || c.DeliveryIncident.CreateRatePerHour > 1000 ||
		c.DeliveryIncident.EvidenceRatePerHour < 1 || c.DeliveryIncident.EvidenceRatePerHour > 1000 ||
		c.DeliveryIncident.CreateIPRatePerHour < 1 || c.DeliveryIncident.CreateIPRatePerHour > 10000 ||
		c.DeliveryIncident.EvidenceIPRatePerHour < 1 || c.DeliveryIncident.EvidenceIPRatePerHour > 10000 {
		problems = append(problems, "invalid delivery incident rate limit configuration")
	}
	viewURL, viewSecret := strings.TrimSpace(c.DeliveryIncident.EvidenceViewBaseURL), strings.TrimSpace(c.DeliveryIncident.EvidenceViewSecret)
	if c.DeliveryIncident.Enabled && (c.App.Env == "preprod" || isProduction(c.App.Env)) && viewURL == "" {
		problems = append(problems, "enabled delivery incidents require JXE_EVIDENCE_VIEW_BASE_URL and JXE_EVIDENCE_VIEW_SECRET in preprod/production")
	}
	if (viewURL == "") != (viewSecret == "") {
		problems = append(problems, "JXE_EVIDENCE_VIEW_BASE_URL and JXE_EVIDENCE_VIEW_SECRET must be configured together")
	}
	if viewURL != "" {
		parsed, err := url.Parse(viewURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
			problems = append(problems, "JXE_EVIDENCE_VIEW_BASE_URL must be an absolute HTTP(S) URL without credentials or fragment")
		} else if (c.App.Env == "preprod" || isProduction(c.App.Env)) && parsed.Scheme != "https" {
			problems = append(problems, "JXE_EVIDENCE_VIEW_BASE_URL must use HTTPS outside local development")
		}
		if len(viewSecret) < 32 {
			problems = append(problems, "JXE_EVIDENCE_VIEW_SECRET must contain at least 32 characters")
		}
		if c.DeliveryIncident.EvidenceViewTTL <= 0 || c.DeliveryIncident.EvidenceViewTTL > 5*time.Minute {
			problems = append(problems, "JXE_EVIDENCE_VIEW_TTL must be between 1ns and 5m")
		}
	}
	if c.DeliveryIncident.AutoResolveEnabled && !c.DeliveryIncident.Enabled {
		problems = append(problems, "JXE_DELIVERY_INCIDENT_AUTO_RESOLVE_ENABLED requires JXE_DELIVERY_INCIDENT_ENABLED=true")
	}
	if c.DeliveryReturn.RiderRatePer10Minutes < 1 || c.DeliveryReturn.RiderRatePer10Minutes > 100 ||
		c.DeliveryReturn.HandoffTTL < time.Minute || c.DeliveryReturn.HandoffTTL > time.Hour {
		problems = append(problems, "invalid delivery return rate or handoff TTL configuration")
	}
	if c.DeliveryReturn.SLAWorkerInterval <= 0 || c.DeliveryReturn.SLAWorkerBatchSize < 1 || c.DeliveryReturn.SLAWorkerBatchSize > 1000 ||
		c.DeliveryReturn.ReceiptReminderAfter <= 0 || c.DeliveryReturn.ReceiptDeadlineAfter <= c.DeliveryReturn.ReceiptReminderAfter {
		problems = append(problems, "invalid delivery return SLA worker or receipt deadlines")
	}
	if (c.DeliveryReturn.RiderWriteEnabled || c.DeliveryReturn.ApprovalEnabled || c.DeliveryReturn.ReceiptEnabled ||
		c.DeliveryReturn.SystemAfterSaleEnabled || c.DeliveryReturn.NotificationEnabled) && !c.DeliveryReturn.Enabled {
		problems = append(problems, "delivery return branch switches require JXE_DELIVERY_RETURN_ENABLED=true")
	}
	if c.DeliveryReturn.ApprovalEnabled && (!c.DeliveryReturn.SystemAfterSaleEnabled || !c.AfterSale.Enabled || !c.AfterSale.RefundExecutionEnabled) {
		problems = append(problems, "delivery return approval requires system after-sale and refund execution")
	}
	for envName, values := range map[string][]string{
		"JXE_DELIVERY_RETURN_RIDER_ALLOWLIST": c.DeliveryReturn.RiderAllowlist,
		"JXE_DELIVERY_RETURN_SHOP_ALLOWLIST":  c.DeliveryReturn.ShopAllowlist,
	} {
		if containsDeliveryIncidentFullRollout(values) {
			if len(values) != 1 {
				problems = append(problems, envName+" full rollout marker '*' or 'all' must be the only value")
			}
			continue
		}
		seen := make(map[uint64]bool, len(values))
		for _, raw := range values {
			id, err := strconv.ParseUint(raw, 10, 64)
			if err != nil || id == 0 || seen[id] {
				problems = append(problems, envName+" must contain unique positive IDs")
				break
			}
			seen[id] = true
		}
	}
	if c.DeliveryReturn.Enabled && (c.App.Env == "preprod" || isProduction(c.App.Env)) {
		if c.MySQL.DSN == "" || c.Redis.Addr == "" || c.RabbitMQ.URL == "" {
			problems = append(problems, "enabled delivery returns require MySQL, Redis, and RabbitMQ in preprod/production")
		}
		if len(c.DeliveryReturn.RiderAllowlist) == 0 || len(c.DeliveryReturn.ShopAllowlist) == 0 {
			problems = append(problems, "enabled delivery returns require explicit rider and shop allowlists in preprod/production")
		}
	}
	for envName, values := range map[string][]string{
		"JXE_DELIVERY_INCIDENT_RIDER_ALLOWLIST": c.DeliveryIncident.RiderAllowlist,
		"JXE_DELIVERY_INCIDENT_SHOP_ALLOWLIST":  c.DeliveryIncident.ShopAllowlist,
	} {
		if containsDeliveryIncidentFullRollout(values) {
			if len(values) != 1 {
				problems = append(problems, envName+" full rollout marker '*' or 'all' must be the only value")
			}
			continue
		}
		seen := make(map[uint64]bool, len(values))
		for _, raw := range values {
			id, err := strconv.ParseUint(raw, 10, 64)
			if err != nil || id == 0 || seen[id] {
				problems = append(problems, envName+" must contain unique positive IDs")
				break
			}
			seen[id] = true
		}
	}
	if c.Asset.WorkerInterval <= 0 || c.Asset.WorkerBatchSize <= 0 || c.Asset.WorkerBatchSize > 1000 {
		problems = append(problems, "invalid asset worker configuration")
	}
	if c.Asset.CompensationIssueEnabled && !c.Asset.WriteEnabled {
		problems = append(problems, "compensation issuance requires JXE_ASSET_WRITE_ENABLED=true")
	}
	if c.Dispatch.ModeOverride != "" && c.Dispatch.ModeOverride != "hybrid" && c.Dispatch.ModeOverride != "auto" && c.Dispatch.ModeOverride != "grab" && c.Dispatch.ModeOverride != "manual" {
		problems = append(problems, "JXE_DISPATCH_MODE_OVERRIDE must be empty, hybrid, auto, grab, or manual")
	}
	if c.Dispatch.WorkerInterval <= 0 || c.Dispatch.WorkerBatchSize <= 0 || c.Dispatch.WorkerBatchSize > 500 || c.Dispatch.LeaseDuration <= 0 || c.Dispatch.JobTimeout <= 0 || c.Dispatch.MaxAttempts < 1 || c.Dispatch.MaxAttempts > 100 || c.Dispatch.HeartbeatPersistInterval <= 0 {
		problems = append(problems, "invalid dispatch worker configuration")
	}
	if c.Realtime.TicketTTL < 10*time.Second || c.Realtime.TicketTTL > 5*time.Minute || c.Realtime.HandshakeTimeout <= 0 || c.Realtime.HandshakeTimeout > 30*time.Second || c.Realtime.HeartbeatInterval < 5*time.Second || c.Realtime.PongTimeout <= c.Realtime.HeartbeatInterval || c.Realtime.SessionCheckInterval < 5*time.Second || c.Realtime.ShutdownDrainTimeout <= 0 || c.Realtime.RelayInterval <= 0 || c.Realtime.DeliveryRetention < time.Hour || c.Realtime.AcknowledgementRetention < time.Hour {
		problems = append(problems, "invalid realtime timeout or retention configuration")
	}
	if c.Realtime.MaxConnectionsPerRider < 1 || c.Realtime.MaxConnectionsPerRider > 10 || c.Realtime.SendQueueSize < 1 || c.Realtime.SendQueueSize > 256 || c.Realtime.MaxFrameBytes < 1024 || c.Realtime.MaxFrameBytes > 64<<10 || c.Realtime.ResumeLimit < 1 || c.Realtime.ResumeLimit > 500 || c.Realtime.RelayBatchSize < 1 || c.Realtime.RelayBatchSize > 500 {
		problems = append(problems, "invalid realtime connection, frame, resume, or relay limits")
	}
	if c.Realtime.TicketRiderRatePerMinute < 1 || c.Realtime.TicketRiderRatePerMinute > 100 || c.Realtime.TicketIPRatePerMinute < 1 || c.Realtime.TicketIPRatePerMinute > 300 || c.Realtime.HandshakeIPRatePerMinute < 1 || c.Realtime.HandshakeIPRatePerMinute > 600 || c.Realtime.ACKRatePerMinute < 1 || c.Realtime.ACKRatePerMinute > 1000 || c.Realtime.ResumeRatePerMinute < 1 || c.Realtime.ResumeRatePerMinute > 60 {
		problems = append(problems, "invalid realtime rate limit configuration")
	}
	seenCanaryRiders := make(map[uint64]bool, len(c.Realtime.CanaryRiderIDs))
	for _, raw := range c.Realtime.CanaryRiderIDs {
		id, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || id == 0 || seenCanaryRiders[id] {
			problems = append(problems, "JXE_REALTIME_CANARY_RIDER_IDS must contain unique positive rider IDs")
			break
		}
		seenCanaryRiders[id] = true
	}
	if c.Realtime.RelayEnabled && !c.Realtime.Enabled {
		problems = append(problems, "JXE_REALTIME_RELAY_ENABLED requires JXE_REALTIME_ENABLED=true")
	}
	if c.MQ.ConsumerRealtimeEnabled && !c.Realtime.Enabled {
		problems = append(problems, "JXE_MQ_CONSUMER_REALTIME_ENABLED requires JXE_REALTIME_ENABLED=true")
	}
	if c.Realtime.Enabled && (c.MySQL.DSN == "" || c.Redis.Addr == "") {
		problems = append(problems, "realtime requires configured MySQL and Redis")
	}
	if c.MapRoute.Provider != "fake" && c.MapRoute.Provider != "amap" {
		problems = append(problems, "JXE_MAP_ROUTE_PROVIDER must be fake or amap")
	}
	if c.MapRoute.Mode != "electric_bicycle" && c.MapRoute.Mode != "driving" {
		problems = append(problems, "JXE_MAP_ROUTE_MODE must be electric_bicycle or driving")
	}
	if !validMapRouteStrategy(c.MapRoute.Mode, c.MapRoute.Strategy) {
		problems = append(problems, "JXE_MAP_ROUTE_STRATEGY is not allowed for the configured mode")
	}
	if c.MapRoute.Timeout < 200*time.Millisecond || c.MapRoute.Timeout > 5*time.Second || c.MapRoute.CacheTTL <= 0 || c.MapRoute.StaleTTL <= c.MapRoute.CacheTTL || c.MapRoute.LocationFreshness <= 0 || c.MapRoute.MaxAccuracyM <= 0 {
		problems = append(problems, "invalid map route timeout, cache, or rider location configuration")
	}
	if c.MapRoute.RiderRatePerMinute < 1 || c.MapRoute.AccountRatePerMinute < 1 || c.MapRoute.IPRatePerMinute < 1 || c.MapRoute.MaxConcurrency < 1 || c.MapRoute.MaxConcurrency > 1000 {
		problems = append(problems, "invalid map route rate or concurrency limits")
	}
	if len(c.MapRoute.CacheHMACSecret) < 32 {
		problems = append(problems, "JXE_MAP_ROUTE_CACHE_HMAC_SECRET must be at least 32 characters")
	}
	seenRouteCanaries := make(map[uint64]bool, len(c.MapRoute.CanaryRiderIDs))
	for _, raw := range c.MapRoute.CanaryRiderIDs {
		id, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || id == 0 || seenRouteCanaries[id] {
			problems = append(problems, "JXE_MAP_ROUTE_CANARY_RIDER_IDS must contain unique positive rider IDs")
			break
		}
		seenRouteCanaries[id] = true
	}
	if c.MapRoute.Enabled && (c.MySQL.DSN == "" || c.Redis.Addr == "") {
		problems = append(problems, "map route requires configured MySQL and Redis")
	}
	if c.MapRoute.Enabled && c.MapRoute.Provider == "amap" && (c.MapRoute.AmapKey == "" || c.MapRoute.AmapBaseURL != "https://restapi.amap.com") {
		problems = append(problems, "enabled Amap route provider requires a key and the official HTTPS endpoint")
	}
	if c.CustomerLBS.Mode != "off" && c.CustomerLBS.Mode != "observe" && c.CustomerLBS.Mode != "enforce" {
		problems = append(problems, "JXE_C_LBS_MODE must be off, observe, or enforce")
	}
	if c.CustomerLBS.Provider != "amap" && c.CustomerLBS.Provider != "fake" {
		problems = append(problems, "JXE_C_LBS_PROVIDER must be amap or fake")
	}
	if c.CustomerLBS.ContextTTL < 5*time.Minute || c.CustomerLBS.ContextTTL > 30*time.Minute ||
		c.CustomerLBS.RegeocodeTimeout <= 0 || c.CustomerLBS.RegeocodeTimeout > 2*time.Second ||
		c.CustomerLBS.RouteTimeout <= 0 || c.CustomerLBS.RouteTimeout > 2*time.Second ||
		c.CustomerLBS.ResolveTimeout <= 0 || c.CustomerLBS.ResolveTimeout > 3*time.Second ||
		c.CustomerLBS.RegeocodeFreshTTL <= 0 || c.CustomerLBS.RegeocodeStaleTTL <= c.CustomerLBS.RegeocodeFreshTTL ||
		c.CustomerLBS.RouteFreshTTL <= 0 || c.CustomerLBS.RouteStaleTTL <= c.CustomerLBS.RouteFreshTTL {
		problems = append(problems, "invalid customer LBS timeout, context TTL, or cache TTL configuration")
	}
	if c.CustomerLBS.MaxRouteCandidates < 1 || c.CustomerLBS.MaxRouteCandidates > 3 || c.CustomerLBS.MaxAccuracyM < 20 || c.CustomerLBS.MaxAccuracyM > 1000 ||
		c.CustomerLBS.MaxConcurrency < 1 || c.CustomerLBS.MaxConcurrency > 1000 || c.CustomerLBS.AnonymousIPRate < 1 || c.CustomerLBS.CustomerRate < 1 || c.CustomerLBS.SessionRate < 1 {
		problems = append(problems, "invalid customer LBS candidate, accuracy, concurrency, or rate limit configuration")
	}
	if len(c.CustomerLBS.CacheHMACSecret) < 32 {
		problems = append(problems, "JXE_C_LBS_CACHE_HMAC_SECRET must be at least 32 characters")
	}
	if strings.TrimSpace(c.CustomerLBS.AnonymousSessionHeader) == "" || strings.ContainsAny(c.CustomerLBS.AnonymousSessionHeader, "\r\n") {
		problems = append(problems, "JXE_C_LBS_SESSION_HEADER must be a valid header name")
	}
	if c.CustomerLBS.Mode != "off" && (c.MySQL.DSN == "" || c.Redis.Addr == "") {
		problems = append(problems, "customer LBS requires configured MySQL and Redis")
	}
	if c.CustomerLBS.Mode != "off" && c.CustomerLBS.Provider == "amap" &&
		(c.CustomerLBS.AmapKey == "" || c.CustomerLBS.AmapBaseURL == "") {
		problems = append(problems, "enabled customer LBS Amap provider requires a key and base URL")
	}
	if c.CustomerLBS.RouteRefineEnabled && !c.CustomerLBS.RegeocodeEnabled {
		problems = append(problems, "JXE_C_LBS_ROUTE_REFINE_ENABLED requires JXE_C_LBS_REGEOCODE_ENABLED=true")
	}
	if c.Search.HistoryMax < 1 || c.Search.HistoryMax > 100 ||
		c.Search.HistoryRetention < 24*time.Hour || c.Search.HistoryRetention > 730*24*time.Hour {
		problems = append(problems, "invalid search history maximum or retention configuration")
	}
	if c.Search.HotWindowDays < 1 || c.Search.HotWindowDays > 30 ||
		c.Search.StatsRetentionDays < c.Search.HotWindowDays || c.Search.StatsRetentionDays > 365 {
		problems = append(problems, "invalid search hot window or statistics retention configuration")
	}
	if c.Search.HotCacheTTL < 30*time.Second || c.Search.HotCacheTTL > time.Hour ||
		c.Search.EventRatePerMinute < 1 || c.Search.EventRatePerMinute > 300 ||
		c.Search.CleanupInterval < time.Minute || c.Search.CleanupBatchSize < 100 || c.Search.CleanupBatchSize > 5000 {
		problems = append(problems, "invalid search cache, rate limit, or cleanup configuration")
	}
	if c.RiderApplication.TokenTTL < 5*time.Minute || c.RiderApplication.TokenTTL > time.Hour {
		problems = append(problems, "JXE_RIDER_APPLICATION_TOKEN_TTL must be between 5m and 1h")
	}
	if len(c.RiderApplication.HMACSecret) < 32 {
		problems = append(problems, "JXE_RIDER_APPLICATION_HMAC_SECRET must be at least 32 characters")
	}
	if c.RiderApplication.MaxShops < 1 || c.RiderApplication.MaxShops > 200 ||
		c.RiderApplication.SubmitIPRatePerHour < 1 || c.RiderApplication.SubmitPhoneRatePerDay < 1 ||
		c.RiderApplication.LoginIPRatePerMinute < 1 || c.RiderApplication.LoginPhoneRatePer15Minutes < 1 ||
		c.RiderApplication.WriteAccountRatePerMinute < 1 || c.RiderApplication.ResubmitAccountRatePerDay < 1 ||
		c.RiderApplication.ReviewAdminRatePerMinute < 1 {
		problems = append(problems, "invalid rider application limit configuration")
	}
	if c.RiderApplication.Enabled && (c.MySQL.DSN == "" || c.Redis.Addr == "") {
		problems = append(problems, "rider application requires configured MySQL and Redis")
	}
	if c.MQ.ConsumerRealtimeEnabled && c.RabbitMQ.URL == "" {
		problems = append(problems, "realtime MQ consumer requires configured RabbitMQ")
	}
	if isProduction(c.App.Env) && c.Realtime.Enabled {
		for _, origin := range c.Realtime.AllowedOrigins {
			if strings.TrimSpace(origin) == "*" {
				problems = append(problems, "production realtime origin allowlist cannot contain wildcard '*'")
				break
			}
		}
	}
	if c.Asset.ExpiryEnabled && !c.Asset.WriteEnabled {
		problems = append(problems, "asset expiry requires JXE_ASSET_WRITE_ENABLED=true")
	}
	if c.Asset.RepairEnabled && !c.Asset.ReadEnabled {
		problems = append(problems, "asset repair requires JXE_ASSET_READ_ENABLED=true")
	}
	if len(c.AfterSale.EvidenceTokenSecret) < 32 {
		problems = append(problems, "JXE_EVIDENCE_TOKEN_SECRET must be at least 32 characters")
	}
	if c.Service.EnforcementMode != "off" && c.Service.EnforcementMode != "observe" && c.Service.EnforcementMode != "enforce" {
		problems = append(problems, "JXE_ORDER_SERVICE_AREA_ENFORCEMENT must be off, observe, or enforce")
	}
	if c.Service.ResolveCacheTTL <= 0 || c.Service.HomeCacheTTL <= 0 || c.Service.QueryTimeout <= 0 {
		problems = append(problems, "service area and home cache/query timeouts must be positive")
	}
	if c.WeChat.HTTPTimeout <= 0 {
		problems = append(problems, "JXE_WECHAT_HTTP_TIMEOUT must be positive")
	}
	for name, mode := range map[string]string{
		"JXE_CP1_PICKUP_VERIFICATION_MODE":   c.CP1.PickupVerificationMode,
		"JXE_CP1_DELIVERY_VERIFICATION_MODE": c.CP1.DeliveryVerificationMode,
		"JXE_CP1_COMPLIANCE_MODE":            c.CP1.ComplianceMode,
	} {
		if mode != "off" && mode != "observe" && mode != "enforce" {
			problems = append(problems, name+" must be off, observe, or enforce")
		}
	}
	if c.CP1.ReleaseProfile != CP1ReleaseProfileOff && c.CP1.ReleaseProfile != CP1ReleaseProfilePhaseOne {
		problems = append(problems, "JXE_CP1_RELEASE_PROFILE must be off or phase_one")
	}
	problems = append(problems, c.CP1ReleaseProfileProblems()...)
	if c.CP1.WorkerInterval <= 0 || c.CP1.WorkerBatchSize <= 0 || c.CP1.WorkerBatchSize > 200 || c.CP1.VerificationTTL <= 0 || c.CP1.DeliveryVerificationTTL < 2*time.Hour || c.CP1.DeliveryVerificationMaxTTL < c.CP1.DeliveryVerificationTTL || c.CP1.VerificationMaxAttempts < 1 || c.CP1.VerificationMaxAttempts > 20 || c.CP1.VerificationLockDuration <= 0 {
		problems = append(problems, "invalid CP1 worker or verification configuration")
	}
	if (c.CP1.PrintEnabled || c.MQ.ConsumerPrintEnabled) && strings.TrimSpace(c.CP1.PrintProvider) == "" {
		problems = append(problems, "enabled printing requires JXE_CP1_PRINT_PROVIDER")
	}
	if c.MQ.ConsumerPrintEnabled && !c.CP1.PrintEnabled {
		problems = append(problems, "JXE_MQ_CONSUMER_PRINT_ENABLED requires JXE_CP1_PRINT_ENABLED=true")
	}
	if len(c.CP1.VerificationPepper) < 32 || len(c.CP1.IdentityCallbackSecret) < 32 || len(c.CP1.DataEncryptionKey) < 32 {
		problems = append(problems, "CP1 verification pepper, identity callback secret, and data encryption key must be at least 32 characters")
	}
	if c.SMS.Enabled && !c.Feature.SMSMockEnabled {
		if c.SMS.Provider != "tencentcloud" {
			problems = append(problems, "enabled SMS requires provider tencentcloud")
		}
		if c.SMS.Region == "" || c.SMS.SecretID == "" || c.SMS.SecretKey == "" || c.SMS.SDKAppID == "" || c.SMS.SignName == "" || c.SMS.TemplateID == "" {
			problems = append(problems, "Tencent Cloud SMS requires region, SecretID, SecretKey, SDK AppID, sign name, and template ID")
		}
		if c.SMS.HTTPTimeout <= 0 {
			problems = append(problems, "Tencent Cloud SMS HTTP timeout must be positive")
		}
	}
	if isProduction(c.App.Env) {
		if c.Service.EnforcementMode != "enforce" {
			problems = append(problems, "production requires JXE_ORDER_SERVICE_AREA_ENFORCEMENT=enforce")
		}
		if c.MySQL.DSN == "" || !c.MySQL.Required {
			problems = append(problems, "production requires MySQL and JXE_MYSQL_REQUIRED=true")
		}
		if c.Redis.Addr == "" || !c.Redis.Required {
			problems = append(problems, "production requires Redis and JXE_REDIS_REQUIRED=true")
		}
		if c.Feature.MQPublisherEnabled && (c.RabbitMQ.URL == "" || !c.RabbitMQ.Required) {
			problems = append(problems, "production MQ publisher requires RabbitMQ and JXE_RABBITMQ_REQUIRED=true")
		}
		if c.Feature.PaymentMockEnabled || c.Feature.SMSMockEnabled || c.WeChat.AuthMockEnabled || c.WeChat.PayMockEnabled {
			problems = append(problems, "mock payment, SMS, and WeChat providers must be disabled in production")
		}
		if !c.SMS.Enabled || c.SMS.Provider != "tencentcloud" {
			problems = append(problems, "production requires Tencent Cloud SMS")
		}
		if c.SMS.Endpoint != "sms.tencentcloudapi.com" {
			problems = append(problems, "production Tencent Cloud SMS endpoint must use the official endpoint")
		}
		if !c.WeChat.AuthEnabled || c.WeChat.MiniAppID == "" || c.WeChat.MiniAppSecret == "" {
			problems = append(problems, "production requires real WeChat miniapp authentication credentials")
		}
		if c.WeChat.APIBaseURL != "https://api.weixin.qq.com" {
			problems = append(problems, "production WeChat API base URL must use the official endpoint")
		}
		if !c.WeChat.PayEnabled || c.WeChat.PayMchID == "" || c.WeChat.PayCertSerial == "" || c.WeChat.PayPrivateKeyPath == "" || len(c.WeChat.PayAPIv3Key) != 32 || c.WeChat.PayNotifyURL == "" {
			problems = append(problems, "production requires complete WeChat Pay API v3 credentials and callback URL")
		}
		if c.WeChat.PayEnabled && !c.Order.ExpiryWorkerEnabled {
			problems = append(problems, "production WeChat Pay requires JXE_ORDER_EXPIRY_WORKER_ENABLED=true")
		}
		if c.WeChat.PayEnabled && (!c.Reconciliation.Enabled || !c.Reconciliation.WorkerEnabled) {
			problems = append(problems, "production WeChat Pay requires JXE_WECHAT_BILL_RECONCILIATION_ENABLED=true and JXE_WECHAT_BILL_RECONCILIATION_WORKER_ENABLED=true")
		}
		if c.AfterSale.RefundExecutionEnabled && c.WeChat.RefundNotifyURL == "" {
			problems = append(problems, "production refund execution requires JXE_WECHAT_REFUND_NOTIFY_URL")
		}
		if c.AfterSale.RefundExecutionEnabled && !c.AfterSale.WorkerEnabled {
			problems = append(problems, "production refund execution requires JXE_REFUND_WORKER_ENABLED=true")
		}
		if strings.Contains(c.AfterSale.EvidenceTokenSecret, "change_me") {
			problems = append(problems, "production requires a non-default evidence token secret")
		}
		if len(c.JWT.AccessSecret) < 32 || len(c.JWT.RefreshSecret) < 32 || c.JWT.AccessSecret == c.JWT.RefreshSecret || strings.Contains(c.JWT.AccessSecret, "change_me") || strings.Contains(c.JWT.RefreshSecret, "change_me") {
			problems = append(problems, "production JWT secrets must be distinct, non-default, and at least 32 characters")
		}
		if c.Metrics.Enabled && len(c.Metrics.Token) < 16 {
			problems = append(problems, "production metrics endpoint requires JXE_METRICS_TOKEN with at least 16 characters")
		}
		if (c.CP1.PrintEnabled || c.MQ.ConsumerPrintEnabled) && strings.EqualFold(strings.TrimSpace(c.CP1.PrintProvider), "fake") {
			problems = append(problems, "production enabled printing must not use the fake CP1 provider")
		}
		if c.CP1.NotificationEnabled && strings.EqualFold(strings.TrimSpace(c.CP1.NotificationProvider), "fake") {
			problems = append(problems, "production enabled notifications must not use the fake CP1 provider")
		}
		if c.CP1.ComplianceMode != "off" && strings.EqualFold(strings.TrimSpace(c.CP1.IdentityProvider), "fake") {
			problems = append(problems, "production enabled compliance must not use the fake CP1 provider")
		}
		if c.Realtime.Enabled && len(c.Realtime.AllowedOrigins) == 0 {
			problems = append(problems, "production realtime requires JXE_REALTIME_ALLOWED_ORIGINS")
		}
		if strings.Contains(c.CP1.VerificationPepper, "change_me") || strings.Contains(c.CP1.IdentityCallbackSecret, "change_me") || strings.Contains(c.CP1.DataEncryptionKey, "change_me") {
			problems = append(problems, "production requires non-default CP1 security secrets")
		}
		if c.RiderApplication.Enabled && strings.Contains(c.RiderApplication.HMACSecret, "change_me") {
			problems = append(problems, "production rider application requires a non-default HMAC secret")
		}
		if c.MapRoute.Enabled && c.MapRoute.Provider == "fake" {
			problems = append(problems, "production must not use the fake map route provider")
		}
		if c.MapRoute.Enabled && strings.Contains(c.MapRoute.CacheHMACSecret, "change_me") {
			problems = append(problems, "production map route requires a non-default cache HMAC secret")
		}
		if c.CustomerLBS.Mode != "off" && c.CustomerLBS.Provider == "fake" {
			problems = append(problems, "production must not use the fake customer LBS provider")
		}
		if c.CustomerLBS.Mode == "enforce" {
			if c.CustomerLBS.Provider != "amap" || !c.CustomerLBS.RegeocodeEnabled || c.CustomerLBS.AmapKey == "" || c.CustomerLBS.AmapBaseURL != "https://restapi.amap.com" {
				problems = append(problems, "production customer LBS enforce requires enabled Amap regeo with a key and the official HTTPS endpoint")
			}
			if len(c.CustomerLBS.AllowedOrigins) == 0 {
				problems = append(problems, "production customer LBS enforce requires JXE_C_LBS_ALLOWED_ORIGINS")
			}
			for _, origin := range c.CustomerLBS.AllowedOrigins {
				if strings.TrimSpace(origin) == "*" {
					problems = append(problems, "production customer LBS origin allowlist cannot contain wildcard '*'")
					break
				}
			}
			if strings.Contains(c.CustomerLBS.CacheHMACSecret, "change_me") {
				problems = append(problems, "production customer LBS requires a non-default cache HMAC secret")
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration: %s", strings.Join(problems, "; "))
	}
	return nil
}

func (c Config) wineTicketRuntimeProblems() []string {
	var problems []string
	if c.WineTicket.MaintenanceOwner != WineTicketMaintenanceOwnerAPI &&
		c.WineTicket.MaintenanceOwner != WineTicketMaintenanceOwnerWorker {
		problems = append(
			problems,
			"JXE_WINE_TICKET_MAINTENANCE_OWNER must be api or worker",
		)
	}
	branchesEnabled := c.WineTicket.PackageReadEnabled ||
		c.WineTicket.AdminEnabled ||
		c.WineTicket.PurchaseEnabled ||
		c.WineTicket.RedemptionEnabled ||
		c.WineTicket.GiftEnabled ||
		c.WineTicket.ReminderEnabled ||
		c.WineTicket.WeChatReminderEnabled ||
		c.WineTicket.WeChatReminderProviderEnabled ||
		c.WineTicket.RenewalEnabled ||
		c.WineTicket.RefundEnabled ||
		c.WineTicket.ReconciliationEnabled
	if !c.WineTicket.Enabled {
		if branchesEnabled {
			problems = append(problems, "wine-ticket branch switches require JXE_WINE_TICKET_ENABLED=true")
		}
		return problems
	}

	if c.MySQL.DSN == "" || !c.MySQL.Required {
		problems = append(problems, "enabled wine tickets require MySQL and JXE_MYSQL_REQUIRED=true")
	} else {
		dsn, err := mysqldriver.ParseDSN(c.MySQL.DSN)
		if err != nil {
			problems = append(problems, "enabled wine tickets require a valid JXE_MYSQL_DSN")
		} else {
			if !dsn.ParseTime {
				problems = append(problems, "enabled wine tickets require JXE_MYSQL_DSN parseTime=true")
			}
			if dsn.Loc != time.Local {
				problems = append(problems, "enabled wine tickets require JXE_MYSQL_DSN loc=Local")
			}
		}
	}
	if c.MySQL.RequiredTimeZone != "+08:00" {
		problems = append(problems, "enabled wine tickets require JXE_MYSQL_REQUIRED_TIME_ZONE=+08:00")
	}
	if time.Local.String() != "Asia/Shanghai" {
		problems = append(problems, "enabled wine tickets require process TZ=Asia/Shanghai with zoneinfo available")
	}
	if !c.MySQL.RequireWineTicketSchema {
		problems = append(problems, "enabled wine tickets require wine-ticket schema verification")
	}
	if len(c.WineTicket.GiftTokenPepper) < 32 {
		problems = append(problems, "JXE_WINE_TICKET_GIFT_TOKEN_PEPPER must contain at least 32 characters")
	}
	if len(c.WineTicket.QuoteTokenSecret) < 32 {
		problems = append(problems, "JXE_WINE_TICKET_QUOTE_TOKEN_SECRET must contain at least 32 characters")
	}
	if c.WineTicket.WeChatReminderEnabled && !c.WineTicket.ReminderEnabled {
		problems = append(
			problems,
			"JXE_WINE_TICKET_WECHAT_REMINDER_ENABLED=true requires JXE_WINE_TICKET_REMINDER_ENABLED=true",
		)
	}
	if c.WineTicket.WeChatReminderEnabled &&
		!c.WineTicket.WeChatReminderProviderEnabled {
		problems = append(
			problems,
			"JXE_WINE_TICKET_WECHAT_REMINDER_ENABLED=true requires JXE_WINE_TICKET_WECHAT_REMINDER_PROVIDER_ENABLED=true",
		)
	}
	if c.WineTicket.WeChatReminderProviderEnabled {
		if strings.TrimSpace(c.WeChat.MiniAppID) == "" ||
			strings.TrimSpace(c.WeChat.MiniAppSecret) == "" {
			problems = append(
				problems,
				"enabled wine-ticket WeChat reminder provider requires Mini Program AppID and secret",
			)
		}
		if !validWineTicketWeChatPage(c.WineTicket.WeChatReminderPage) {
			problems = append(
				problems,
				"JXE_WINE_TICKET_WECHAT_REMINDER_PAGE must be a safe pages/... Mini Program path",
			)
		}
		fields := []string{
			c.WineTicket.WeChatReminderProductNameField,
			c.WineTicket.WeChatReminderRemainingQuantityField,
			c.WineTicket.WeChatReminderExpiryDateField,
		}
		seenFields := make(map[string]bool, len(fields))
		for _, field := range fields {
			field = strings.TrimSpace(field)
			if !validWineTicketWeChatTemplateField(field) || seenFields[field] {
				problems = append(
					problems,
					"wine-ticket WeChat reminder template field mappings must be valid and unique",
				)
				break
			}
			seenFields[field] = true
		}
		if isProduction(c.App.Env) &&
			c.WeChat.APIBaseURL != "https://api.weixin.qq.com" {
			problems = append(
				problems,
				"production wine-ticket WeChat reminder provider requires the official HTTPS WeChat API endpoint",
			)
		}
	}
	if c.WineTicket.ReconciliationEnabled {
		if c.WineTicket.ReconciliationBatchSize < 1 ||
			c.WineTicket.ReconciliationBatchSize > 2000 {
			problems = append(
				problems,
				"JXE_WINE_TICKET_RECONCILIATION_BATCH_SIZE must be between 1 and 2000",
			)
		}
		if c.WineTicket.ReconciliationBatchInterval <= 0 {
			problems = append(
				problems,
				"JXE_WINE_TICKET_RECONCILIATION_BATCH_INTERVAL must be positive",
			)
		}
		if c.WineTicket.ReconciliationSweepInterval <= 0 {
			problems = append(
				problems,
				"JXE_WINE_TICKET_RECONCILIATION_SWEEP_INTERVAL must be positive",
			)
		}
		if c.WineTicket.ReconciliationDailyStart < 0 ||
			c.WineTicket.ReconciliationDailyStart >= 6*time.Hour {
			problems = append(
				problems,
				"JXE_WINE_TICKET_RECONCILIATION_DAILY_START must be between 0 and 6h",
			)
		}
		if c.WineTicket.ReconciliationLeaseDuration <= 0 {
			problems = append(
				problems,
				"JXE_WINE_TICKET_RECONCILIATION_LEASE_DURATION must be positive",
			)
		}
	}
	if c.WineTicket.PurchaseEnabled || c.WineTicket.RenewalEnabled || c.WineTicket.RefundEnabled {
		if !c.MySQL.RequireWineTicketMoneyContract {
			problems = append(problems, "wine-ticket money branches require the schema-only money registry CONTRACT gate")
		}
		if !c.WeChat.PayEnabled {
			problems = append(problems, "wine-ticket money branches require JXE_WECHAT_PAY_ENABLED=true")
		}
		if !c.Feature.OrderIdempotencyEnabled {
			problems = append(problems, "wine-ticket money branches require JXE_ORDER_IDEMPOTENCY_ENABLED=true")
		}
		// 购买和续期都可能在支付机构确认支付后创建补偿退款。
		// 因此即使客户退款创建开关关闭，这些后台任务仍属于资金契约的一部分。
		if !c.Order.ExpiryWorkerEnabled {
			problems = append(problems, "wine-ticket money branches require JXE_ORDER_EXPIRY_WORKER_ENABLED=true")
		}
		if !c.AfterSale.RefundExecutionEnabled {
			problems = append(problems, "wine-ticket money branches require JXE_REFUND_EXECUTION_ENABLED=true")
		}
		if !c.AfterSale.WorkerEnabled {
			problems = append(problems, "wine-ticket money branches require JXE_REFUND_WORKER_ENABLED=true")
		}
	}
	if isProduction(c.App.Env) {
		if strings.Contains(c.WineTicket.GiftTokenPepper, "change_me") || strings.Contains(c.WineTicket.QuoteTokenSecret, "change_me") {
			problems = append(problems, "production wine tickets require non-default gift and quote secrets")
		}
		if c.WineTicket.PurchaseEnabled || c.WineTicket.RenewalEnabled || c.WineTicket.RefundEnabled {
			if c.WeChat.PayMockEnabled {
				problems = append(problems, "production wine-ticket money branches must not use mock WeChat Pay")
			}
			if strings.TrimSpace(c.WeChat.PayPublicKeyID) == "" || strings.TrimSpace(c.WeChat.PayPublicKeyPath) == "" {
				problems = append(problems, "production wine-ticket money branches require JXE_WECHAT_PAY_PUBLIC_KEY_ID and JXE_WECHAT_PAY_PUBLIC_KEY_PATH")
			}
			if c.WeChat.RefundNotifyURL == "" {
				problems = append(problems, "production wine-ticket money branches require JXE_WECHAT_REFUND_NOTIFY_URL")
			}
			if !c.WineTicket.ReconciliationEnabled {
				problems = append(problems, "production wine-ticket money branches require JXE_WINE_TICKET_RECONCILIATION_ENABLED=true")
			}
		}
	}
	return problems
}

func validWineTicketWeChatPage(page string) bool {
	page = strings.TrimSpace(page)
	if !strings.HasPrefix(page, "pages/") || len(page) > 256 {
		return false
	}
	for _, character := range page {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '/' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func validWineTicketWeChatTemplateField(field string) bool {
	field = strings.TrimSpace(field)
	if len(field) < 2 || len(field) > 64 {
		return false
	}
	for index, character := range field {
		if index == 0 {
			if (character < 'a' || character > 'z') &&
				(character < 'A' || character > 'Z') {
				return false
			}
			continue
		}
		if (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') &&
			character != '_' {
			return false
		}
	}
	return true
}

func (c Config) wechatPayRuntimeProblems() []string {
	publicKeyID := strings.TrimSpace(c.WeChat.PayPublicKeyID)
	publicKeyPath := strings.TrimSpace(c.WeChat.PayPublicKeyPath)
	if (publicKeyID == "") != (publicKeyPath == "") {
		return []string{"JXE_WECHAT_PAY_PUBLIC_KEY_ID and JXE_WECHAT_PAY_PUBLIC_KEY_PATH must be configured together"}
	}
	return nil
}

// CP1ReleaseProfileProblems 返回一期候选版本按失败关闭原则检查出的能力缺口。
// 普通生产实例保持此配置关闭，以便经批准的事故响应仍能停用可选副作用。
func (c Config) CP1ReleaseProfileProblems() []string {
	if c.CP1.ReleaseProfile != CP1ReleaseProfilePhaseOne {
		return nil
	}
	var problems []string
	require := func(ok bool, message string) {
		if !ok {
			problems = append(problems, "JXE_CP1_RELEASE_PROFILE=phase_one requires "+message)
		}
	}
	require(isProduction(c.App.Env), "JXE_APP_ENV=production")
	require(c.CP1.PrintEnabled, "JXE_CP1_PRINT_ENABLED=true")
	printProvider := strings.TrimSpace(c.CP1.PrintProvider)
	require(printProvider != "" && !strings.EqualFold(printProvider, "fake"), "a configured non-fake JXE_CP1_PRINT_PROVIDER")
	require(c.CP1.WorkerEnabled, "JXE_CP1_WORKER_ENABLED=true")
	require(c.CP1.ComplianceMode == "enforce", "JXE_CP1_COMPLIANCE_MODE=enforce")
	require(c.CP1.PickupVerificationMode == "enforce", "JXE_CP1_PICKUP_VERIFICATION_MODE=enforce")
	require(c.CP1.DeliveryVerificationMode == "enforce", "JXE_CP1_DELIVERY_VERIFICATION_MODE=enforce")
	require(c.Feature.OrderIdempotencyEnabled, "JXE_ORDER_IDEMPOTENCY_ENABLED=true")
	require(c.Feature.StockReserveEnabled, "JXE_STOCK_RESERVE_ENABLED=true")
	require(c.Realtime.Enabled, "JXE_REALTIME_ENABLED=true")
	require(c.Realtime.RelayEnabled, "JXE_REALTIME_RELAY_ENABLED=true")
	require(c.Feature.MQPublisherEnabled, "JXE_MQ_PUBLISH_ENABLED=true")
	require(c.MQ.ConsumerNotificationEnabled, "JXE_MQ_CONSUMER_NOTIFICATION_ENABLED=true")
	require(c.MQ.ConsumerPrintEnabled, "JXE_MQ_CONSUMER_PRINT_ENABLED=true")
	require(c.MQ.ConsumerCacheEnabled, "JXE_MQ_CONSUMER_CACHE_ENABLED=true")
	require(c.MQ.ConsumerSecurityEnabled, "JXE_MQ_CONSUMER_SECURITY_ENABLED=true")
	require(c.MQ.ConsumerDispatchEnabled, "JXE_MQ_CONSUMER_DISPATCH_ENABLED=true")
	require(c.MQ.ConsumerRealtimeEnabled, "JXE_MQ_CONSUMER_REALTIME_ENABLED=true")
	require(c.MQ.DBFallbackEnabled, "JXE_MQ_DB_FALLBACK_ENABLED=true")
	require(c.MQ.FailOnTopologyDrift, "JXE_MQ_FAIL_ON_TOPOLOGY_DRIFT=true")
	require(c.Dispatch.Enabled && c.Dispatch.WorkerEnabled, "JXE_DISPATCH_ENABLED=true and JXE_DISPATCH_WORKER_ENABLED=true")
	require(c.RabbitMQ.URL != "" && c.RabbitMQ.Required, "RabbitMQ URL and JXE_RABBITMQ_REQUIRED=true")
	return problems
}

func containsDeliveryIncidentFullRollout(values []string) bool {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "*" || strings.EqualFold(value, "all") {
			return true
		}
	}
	return false
}

func validMapRouteStrategy(mode, strategy string) bool {
	if strategy == "default" {
		return true
	}
	if mode != "driving" {
		return false
	}
	switch strategy {
	case "32", "33", "34", "35", "36", "37", "38", "39", "40", "41", "42":
		return true
	default:
		return false
	}
}

// isProduction 判断Production是否成立。
func isProduction(envName string) bool {
	return envName == "prod" || envName == "production"
}

// defaultInstanceID 返回默认实例 ID。
func defaultInstanceID() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown-host"
	}
	return fmt.Sprintf("%s-%d", hostname, os.Getpid())
}

// env 返回环境变量值。
func env(key string, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

// boolEnv 判断布尔值 Env。
func boolEnv(key string, fallback bool) bool {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return value
}

// csvEnv 解析英文逗号分隔的环境变量。
func csvEnv(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			result = append(result, value)
		}
	}
	return result
}

// intEnv 解析整数环境变量。
func intEnv(key string, fallback int) int {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

// int64Env 解析 64 位整数环境变量。
func int64Env(key string, fallback int64) int64 {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return value
}

func float64Env(key string, fallback float64) float64 {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}
	return value
}

// durationEnv 返回耗时 Env。
func durationEnv(key string, fallback time.Duration) time.Duration {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return value
}
