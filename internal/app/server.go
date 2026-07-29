package app

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/amap"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/infra/nodelease"
	"jiuxiaoer-admin/backend-go/internal/infra/rabbitmq"
	redisinfra "jiuxiaoer-admin/backend-go/internal/infra/redis"
	"jiuxiaoer-admin/backend-go/internal/infra/tencentcloudsms"
	"jiuxiaoer-admin/backend-go/internal/infra/wechat"
	wechatpayinfra "jiuxiaoer-admin/backend-go/internal/infra/wechatpay"
	"jiuxiaoer-admin/backend-go/internal/modules/aftersale"
	"jiuxiaoer-admin/backend-go/internal/modules/asset"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/compliance"
	"jiuxiaoer-admin/backend-go/internal/modules/deliveryreturn"
	"jiuxiaoer-admin/backend-go/internal/modules/dispatch"
	"jiuxiaoer-admin/backend-go/internal/modules/mq"
	"jiuxiaoer-admin/backend-go/internal/modules/notification"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/modules/printjob"
	"jiuxiaoer-admin/backend-go/internal/modules/realtime"
	"jiuxiaoer-admin/backend-go/internal/modules/reconciliation"
	"jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/routeplanning"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type Dependencies struct {
	Config              config.Config
	Log                 *slog.Logger
	DB                  *gorm.DB
	Redis               *goredis.Client
	RabbitMQ            *rabbitmq.Manager
	NodeLease           *nodelease.Lease
	Metrics             *metrics.Registry
	IDGen               *snowflake.Generator
	WeChatAuth          auth.WeChatProvider
	SMSProvider         auth.SMSProvider
	PaymentProvider     order.PaymentProvider
	RefundProvider      refund.Provider
	BillProvider        reconciliation.Provider
	PrintProvider       printjob.Provider
	ComplianceProvider  compliance.Provider
	Realtime            *realtime.Runtime
	RouteProvider       routeplanning.Provider
	CustomerLBSProvider amap.Provider
}

func complianceProviderRequired(cfg config.Config) bool {
	return cfg.CP1.ComplianceMode != "off"
}

// buildConfiguredComplianceProvider 是生产环境的装配边界。选定的供应商适配器
// 必须由其集成包注册；此函数绝不会替换为不可用的占位实现。
func buildConfiguredComplianceProvider(ctx context.Context, cfg config.Config) (compliance.Provider, error) {
	if !complianceProviderRequired(cfg) {
		return nil, nil
	}
	provider, err := compliance.BuildProvider(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return provider, nil
}

// routerComplianceProvider 支持在测试中直接构造路由，
// 同时执行与 NewServer 相同的服务商代码不变量。
func routerComplianceProvider(deps Dependencies) compliance.Provider {
	if deps.ComplianceProvider != nil {
		if err := compliance.ValidateConfiguredProvider(deps.Config, deps.ComplianceProvider); err != nil {
			panic(err)
		}
		return deps.ComplianceProvider
	}
	if !complianceProviderRequired(deps.Config) {
		return &compliance.UnavailableProvider{}
	}
	provider, err := buildConfiguredComplianceProvider(context.Background(), deps.Config)
	if err != nil {
		panic(err)
	}
	return provider
}

// Server 持有 HTTP Server 以及所有长期存在的基础设施连接。
// 统一放在这里关闭，避免业务模块误关共享的 DB/Redis/MQ 客户端。
type Server struct {
	cfg             config.Config
	log             *slog.Logger
	httpServer      *http.Server
	deps            Dependencies
	printProviderMu sync.Mutex
}

// NewServer 先初始化基础设施，再把共享依赖注入路由。
// MySQL/Redis/RabbitMQ 是否必需由配置控制，便于本地 P0 分阶段启动。
func NewServer(ctx context.Context, cfg config.Config, log *slog.Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	printProvider, err := buildConfiguredPrintProvider(ctx, cfg)
	if err != nil {
		return nil, err
	}
	complianceProvider, err := buildConfiguredComplianceProvider(ctx, cfg)
	if err != nil {
		return nil, err
	}
	var smsProvider auth.SMSProvider
	if cfg.SMS.Enabled && !cfg.Feature.SMSMockEnabled {
		var err error
		smsProvider, err = tencentcloudsms.New(cfg.SMS)
		if err != nil {
			return nil, err
		}
	}
	db, err := mysql.Open(ctx, cfg.MySQL, log)
	if err != nil {
		return nil, err
	}

	redisClient, err := redisinfra.Open(ctx, cfg.Redis, log)
	if err != nil {
		return nil, err
	}

	rabbitManager, err := rabbitmq.Open(ctx, cfg.RabbitMQ, log)
	if err != nil {
		return nil, err
	}
	lease, err := nodelease.Acquire(ctx, redisClient, cfg.App.SnowflakeNodeID, cfg.App.InstanceID, cfg.App.NodeLeaseTTL)
	if err != nil {
		if rabbitManager != nil {
			_ = rabbitManager.Close()
		}
		if redisClient != nil {
			_ = redisClient.Close()
		}
		if db != nil {
			if sqlDB, dbErr := db.DB(); dbErr == nil {
				_ = sqlDB.Close()
			}
		}
		return nil, err
	}
	metricRegistry := metrics.New(cfg.App.InstanceID, cfg.Metrics.Token)                           //监控
	idGen := snowflake.New(cfg.App.SnowflakeNodeID)                                                //雪花ID
	wechatAuth := wechat.NewIdentityProvider(cfg.WeChat)                                           //商户
	paymentProvider, err := wechatpayinfra.New(ctx, cfg.WeChat, cfg.Reconciliation.RequestTimeout) //微信支付
	if err != nil {
		if lease != nil {
			_ = lease.Release(ctx)
		}
		if rabbitManager != nil {
			_ = rabbitManager.Close()
		}
		if redisClient != nil {
			_ = redisClient.Close()
		}
		if db != nil {
			if sqlDB, dbErr := db.DB(); dbErr == nil {
				_ = sqlDB.Close()
			}
		}
		return nil, err
	}

	var refundProvider refund.Provider
	if provider, ok := paymentProvider.(refund.Provider); ok {
		refundProvider = applicationRefundProvider(
			cfg.WineTicket.Enabled,
			paymentProvider,
			provider,
		)
	}
	var billProvider reconciliation.Provider
	if provider, ok := paymentProvider.(reconciliation.Provider); ok {
		billProvider = provider
	}
	deps := Dependencies{
		Config:             cfg,
		Log:                log,
		DB:                 db,
		Redis:              redisClient,
		RabbitMQ:           rabbitManager,
		NodeLease:          lease,
		Metrics:            metricRegistry,
		IDGen:              idGen,
		WeChatAuth:         wechatAuth,
		SMSProvider:        smsProvider,
		PaymentProvider:    paymentProvider,
		RefundProvider:     refundProvider,
		BillProvider:       billProvider,
		PrintProvider:      printProvider,
		ComplianceProvider: complianceProvider,
	}
	deps.Realtime = realtime.NewRuntime(cfg, db, redisClient, idGen, metricRegistry, log)

	router := NewRouter(deps)
	return &Server{
		cfg: cfg,
		log: log,
		httpServer: &http.Server{
			Addr:              cfg.HTTP.Addr,
			Handler:           router,
			ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
			ReadTimeout:       cfg.HTTP.ReadTimeout,
			WriteTimeout:      cfg.HTTP.WriteTimeout,
			IdleTimeout:       cfg.HTTP.IdleTimeout,
			MaxHeaderBytes:    cfg.HTTP.MaxHeaderBytes,
		},
		deps: deps,
	}, nil
}

func applicationRefundProvider(
	wineTicketEnabled bool,
	payments wineticket.RefundOriginalPaymentQuerier,
	refunds refund.Provider,
) refund.Provider {
	if refunds == nil || !wineTicketEnabled {
		return refunds
	}
	return wineticket.NewVerifiedWineTicketRefundProvider(payments, refunds)
}

// Run 在同一个父 context 下启动 HTTP 和可选的 outbox publisher。
// 取消 context 会触发 HTTP 优雅关闭，并释放所有基础设施连接。
func (s *Server) Run(ctx context.Context) error {
	if err := s.ensurePrintProvider(ctx); err != nil {
		return err
	}
	apiWineTicketMaintenance := apiOwnsWineTicketMaintenance(s.cfg)
	apiSharedMaintenance := apiOwnsSharedMaintenance(s.cfg)
	var printWorker *printjob.Worker
	if s.deps.DB != nil && s.cfg.CP1.PrintEnabled {
		var err error
		printWorker, err = s.newPrintWorker(ctx)
		if err != nil {
			return err
		}
	}
	runCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()
	errCh := make(chan error, 4)
	var workerWG sync.WaitGroup
	spawn := func(run func()) {
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			run()
		}()
	}
	if s.deps.NodeLease != nil {
		spawn(func() {
			if err := s.deps.NodeLease.Run(runCtx); err != nil {
				errCh <- err
			}
		})
	}

	registry := mq.MustDefaultEventRegistry()
	if s.deps.Realtime != nil && s.cfg.Realtime.Enabled {
		spawn(func() { s.deps.Realtime.RunSubscriber(runCtx) })
		if s.cfg.Realtime.RelayEnabled {
			spawn(func() { s.deps.Realtime.Relay.Run(runCtx) })
		}
	}
	var dispatchService *dispatch.Service
	var deliveryReturnService *deliveryreturn.Service
	var wineTicketModule *wineticket.Module
	if s.deps.DB != nil {
		wineTicketModule = wineticket.NewModule(
			s.deps.DB,
			s.deps.IDGen,
			wineticket.ModuleOptions{
				GiftTokenPepper:  s.cfg.WineTicket.GiftTokenPepper,
				QuoteTokenSecret: s.cfg.WineTicket.QuoteTokenSecret,
				WeChatAppID:      s.cfg.WeChat.MiniAppID,
				InstanceID:       s.cfg.App.InstanceID,
			},
		)
		afterSaleService := aftersale.NewService(s.cfg, s.deps.DB, s.deps.IDGen)
		deliveryReturnService = deliveryreturn.NewService(s.cfg, s.deps.DB, s.deps.Redis, s.deps.IDGen).WithAfterSale(afterSaleService)
		deliveryReturnService.WithReturnSettlementHandler(
			wineTicketModule.ReturnSettlement(afterSaleService),
		)
		if s.cfg.DeliveryReturn.Enabled && s.cfg.DeliveryReturn.SLAWorkerEnabled {
			worker := deliveryreturn.NewSLAWorker(deliveryReturnService, s.log)
			spawn(func() { worker.Run(runCtx) })
		}
		dispatchService = dispatch.NewService(s.cfg, s.deps.DB, s.deps.Redis, s.deps.IDGen, s.deps.Metrics, s.log)
		if s.cfg.Dispatch.WorkerEnabled {
			worker := dispatch.NewWorker(dispatchService, s.cfg.App.InstanceID+":dispatch-sweeper", s.log)
			spawn(func() { worker.Run(runCtx) })
		}
	}
	var notificationProvider notification.Provider = &notification.UnavailableProvider{}
	if s.cfg.CP1.NotificationProvider == "fake" {
		notificationProvider = &notification.FakeProvider{}
	}
	var wineTicketReminderProvider notification.Provider = &notification.UnavailableProvider{}
	if apiWineTicketMaintenance {
		provider, err := wechat.NewSubscriptionMessageProvider(
			s.cfg.WeChat,
			s.cfg.WineTicket,
		)
		if err != nil {
			return err
		}
		wineTicketReminderProvider = provider
	}
	var notificationWorker *notification.Worker
	if s.deps.DB != nil {
		notificationWorker = notification.NewWorker(s.cfg.CP1, s.deps.DB, s.deps.IDGen, notificationProvider, s.cfg.App.InstanceID, s.log).
			WithDeliveryIncidentNotifications(s.cfg.DeliveryIncident.NotificationEnabled).
			WithDeliveryReturnNotifications(s.cfg.DeliveryReturn.NotificationEnabled)
	}
	var cacheHandler *mq.CacheInvalidationHandler
	if s.deps.Redis != nil {
		cacheHandler = mq.NewCacheInvalidationHandler(s.deps.Redis)
	}
	if s.cfg.MQ.DBFallbackEnabled && s.deps.DB != nil && cacheHandler != nil {
		fallback := mq.NewCacheFallbackWorker(s.deps.DB, registry, cacheHandler, s.deps.IDGen, s.deps.Metrics, s.cfg.App.InstanceID, s.log)
		spawn(func() { fallback.Run(runCtx) })
	}

	if s.deps.RabbitMQ != nil {
		if s.cfg.Feature.MQPublisherEnabled {
			publisher := mq.NewPublisher(s.deps.DB, s.deps.RabbitMQ, s.deps.Metrics, s.cfg.App.InstanceID, s.log,
				mq.WithPublisherRegistry(registry), mq.WithPublisherEnvironment(s.cfg.App.Env),
				mq.WithPublisherIDs(s.deps.IDGen), mq.WithPublisherBatchSize(s.cfg.MQ.PublisherBatchSize))
			spawn(func() { publisher.Run(runCtx) })
		}
		if s.cfg.MQ.ConsumerCacheEnabled && cacheHandler != nil {
			spec, _ := mq.DefaultConsumerSpec("cache")
			runtime := mq.NewConsumerRuntime(spec, s.deps.DB, s.deps.RabbitMQ, registry, cacheHandler, s.deps.IDGen, s.deps.Metrics, s.cfg.App.InstanceID, s.log)
			spawn(func() { runtime.Run(runCtx) })
		}
		if s.cfg.MQ.ConsumerNotificationEnabled && notificationWorker != nil {
			spec, _ := mq.DefaultConsumerSpec("notification")
			runtime := mq.NewConsumerRuntime(spec, s.deps.DB, s.deps.RabbitMQ, registry, notification.NewMQHandler(notificationWorker), s.deps.IDGen, s.deps.Metrics, s.cfg.App.InstanceID, s.log)
			spawn(func() { runtime.Run(runCtx) })
		}
		if s.cfg.MQ.ConsumerPrintEnabled && printWorker != nil {
			spec, _ := mq.DefaultConsumerSpec("print")
			runtime := mq.NewConsumerRuntime(spec, s.deps.DB, s.deps.RabbitMQ, registry, printjob.NewMQHandler(printWorker), s.deps.IDGen, s.deps.Metrics, s.cfg.App.InstanceID, s.log)
			spawn(func() { runtime.Run(runCtx) })
		}
		if s.cfg.MQ.ConsumerSecurityEnabled {
			spec, _ := mq.DefaultConsumerSpec("security")
			handler := mq.ConsumerHandlerFunc(func(context.Context, *gorm.DB, mq.EventEnvelope) (mq.ConsumerResult, error) {
				return mq.ConsumerResult{RefType: "security_observation"}, nil
			})
			runtime := mq.NewConsumerRuntime(spec, s.deps.DB, s.deps.RabbitMQ, registry, handler, s.deps.IDGen, s.deps.Metrics, s.cfg.App.InstanceID, s.log)
			spawn(func() { runtime.Run(runCtx) })
		}
		if s.cfg.MQ.ConsumerDispatchEnabled && dispatchService != nil {
			spec, _ := mq.DefaultConsumerSpec("dispatch")
			runtime := mq.NewConsumerRuntime(spec, s.deps.DB, s.deps.RabbitMQ, registry, dispatch.NewMQHandler(dispatchService), s.deps.IDGen, s.deps.Metrics, s.cfg.App.InstanceID, s.log)
			spawn(func() { runtime.Run(runCtx) })
		}
		if s.cfg.MQ.ConsumerRealtimeEnabled && s.deps.Realtime != nil {
			spec, _ := mq.DefaultConsumerSpec("realtime")
			runtime := mq.NewConsumerRuntime(spec, s.deps.DB, s.deps.RabbitMQ, registry, realtime.NewMQHandler(s.deps.Realtime.Service), s.deps.IDGen, s.deps.Metrics, s.cfg.App.InstanceID, s.log)
			spawn(func() { runtime.Run(runCtx) })
		}
		for _, consumer := range []string{"notification", "print", "cache", "security", "dispatch", "realtime", "unrouted"} {
			sink := mq.NewDeadSink(s.deps.DB, s.deps.RabbitMQ, s.deps.IDGen, registry, s.deps.Metrics, consumer, s.log)
			spawn(func() { sink.Run(runCtx) })
		}
	}
	if apiSharedMaintenance && s.cfg.Order.ExpiryWorkerEnabled && s.deps.DB != nil {
		worker := order.NewExpiryWorker(s.cfg, s.deps.DB, s.deps.IDGen, s.deps.Metrics, s.log, s.deps.PaymentProvider)
		if wineTicketModule != nil {
			worker.
				WithPaymentSettlementHandler(
					wineTicketModule.PurchasePaymentSettlement(),
				).
				WithPaymentSettlementHandler(
					wineTicketModule.RenewalPaymentSettlement(),
				)
		}
		spawn(func() { worker.Run(runCtx) })
	}
	if apiWineTicketMaintenance && wineTicketModule != nil {
		worker := wineTicketModule.NewGiftExpiryWorker(s.log)
		spawn(func() { worker.Run(runCtx) })
	}
	if apiWineTicketMaintenance && s.deps.DB != nil {
		worker := wineTicketModule.NewExpiryMaintenanceWorker(
			wineTicketReminderProvider,
			s.cfg.App.InstanceID+":wine-ticket-expiry",
			s.log,
		).
			WithRemindersEnabled(s.cfg.WineTicket.ReminderEnabled).
			WithWeChatEnabled(s.cfg.WineTicket.WeChatReminderEnabled).
			WithSendLease(4*s.cfg.WeChat.HTTPTimeout + time.Minute)
		spawn(func() { worker.Run(runCtx) })
	}
	if apiWineTicketMaintenance &&
		s.cfg.WineTicket.ReconciliationEnabled &&
		s.deps.DB != nil {
		worker := wineTicketModule.NewIntegrityWorker(s.log).
			ConfigureSchedule(
				s.cfg.App.InstanceID,
				s.cfg.WineTicket.ReconciliationDailyStart,
				s.cfg.WineTicket.ReconciliationLeaseDuration,
			).
			ConfigureBounds(
				s.cfg.WineTicket.ReconciliationBatchSize,
				s.cfg.WineTicket.ReconciliationBatchInterval,
				s.cfg.WineTicket.ReconciliationSweepInterval,
			)
		spawn(func() { worker.Run(runCtx) })
	}
	if apiSharedMaintenance && s.cfg.AfterSale.WorkerEnabled && s.cfg.AfterSale.RefundExecutionEnabled && s.deps.DB != nil && s.deps.RefundProvider != nil {
		service := refund.NewService(s.cfg, s.deps.DB, s.deps.IDGen, s.deps.RefundProvider).
			WithRefundSettlementHandler(
				wineTicketModule.PurchaseRefundSettlement(),
			).
			WithRefundSettlementHandler(
				wineTicketModule.RenewalRefundSettlement(),
			)
		if deliveryReturnService != nil {
			service.WithDeliveryReturnClosure(deliveryReturnService)
		}
		worker := refund.NewWorker(s.cfg, service, s.deps.RefundProvider, s.log)
		spawn(func() { worker.Run(runCtx) })
	}
	if s.cfg.Reconciliation.Enabled && s.cfg.Reconciliation.WorkerEnabled && s.deps.DB != nil && s.deps.BillProvider != nil {
		service := reconciliation.NewService(s.cfg, s.deps.DB, s.deps.IDGen, s.deps.BillProvider, s.log)
		worker := reconciliation.NewWorker(s.cfg, service, s.deps.Metrics, s.log)
		spawn(func() { worker.Run(runCtx) })
	}
	if s.cfg.Asset.WorkerEnabled && s.deps.DB != nil && (s.cfg.Asset.CompensationIssueEnabled || s.cfg.Asset.ExpiryEnabled) {
		service := asset.NewService(s.cfg, s.deps.DB, s.deps.IDGen)
		worker := asset.NewWorker(s.cfg, service, s.log)
		spawn(func() { worker.Run(runCtx) })
	}
	if s.cfg.CP1.WorkerEnabled && s.deps.DB != nil {
		if printWorker != nil && s.cfg.MQ.DBFallbackEnabled {
			spawn(func() { printWorker.Run(runCtx) })
		}
		if notificationWorker != nil {
			spawn(func() { notificationWorker.RunWithFallback(runCtx, s.cfg.MQ.DBFallbackEnabled) })
		}
	}
	go func() {
		s.log.Info("http server listening", slog.String("addr", s.cfg.HTTP.Addr))
		errCh <- s.httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		cancelWorkers()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.HTTP.ShutdownTimeout)
		defer cancel()
		if s.deps.Realtime != nil && s.cfg.Realtime.Enabled {
			s.deps.Realtime.Shutdown(shutdownCtx)
		}
		err := s.httpServer.Shutdown(shutdownCtx)
		workerWG.Wait()
		s.closeInfra()
		return err
	case err := <-errCh:
		cancelWorkers()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.Realtime.ShutdownDrainTimeout)
		if s.deps.Realtime != nil && s.cfg.Realtime.Enabled {
			s.deps.Realtime.Shutdown(shutdownCtx)
		}
		cancel()
		workerWG.Wait()
		s.closeInfra()
		return err
	}
}

// closeInfra 关闭Infra并释放相关资源。
func (s *Server) closeInfra() {
	if s.deps.NodeLease != nil {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.deps.NodeLease.Release(releaseCtx)
		cancel()
	}
	if s.deps.Redis != nil {
		_ = s.deps.Redis.Close()
	}
	if s.deps.RabbitMQ != nil {
		_ = s.deps.RabbitMQ.Close()
	}
	if s.deps.PaymentProvider != nil {
		s.deps.PaymentProvider.Shutdown()
	}
	if s.deps.DB != nil {
		if sqlDB, err := s.deps.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}
}
