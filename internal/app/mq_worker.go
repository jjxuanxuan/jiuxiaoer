package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/wechat"
	"jiuxiaoer-admin/backend-go/internal/modules/aftersale"
	"jiuxiaoer-admin/backend-go/internal/modules/deliveryreturn"
	"jiuxiaoer-admin/backend-go/internal/modules/dispatch"
	"jiuxiaoer-admin/backend-go/internal/modules/mq"
	"jiuxiaoer-admin/backend-go/internal/modules/notification"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/modules/printjob"
	"jiuxiaoer-admin/backend-go/internal/modules/realtime"
	"jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/search"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket"
)

var mqWorkerRoles = map[string]bool{
	"all":                      true,
	"outbox-publisher":         true,
	"mq-consumer-notification": true,
	"mq-consumer-print":        true,
	"mq-consumer-cache":        true,
	"mq-consumer-security":     true,
	"mq-consumer-dispatch":     true,
	"mq-consumer-realtime":     true,
	"realtime-relay":           true,
	"mq-dead-sink":             true,
	"search-retention":         true,
	"wine-ticket-maintenance":  true,
}

// RunMQWorker 在不打开 API 监听器的情况下运行选定后台角色。
// MQ 角色运行 RabbitMQ 骨干；search-retention 和
// wine-ticket-maintenance 是不要求 RabbitMQ 的数据库任务角色，且只在
// JXE_WINE_TICKET_MAINTENANCE_OWNER=worker 时取得维护任务所有权。
// 部署时每个进程使用不同的雪花节点 ID；本地开发可继续使用
// Server.Run 和进程内工作任务。
func (s *Server) RunMQWorker(ctx context.Context, role string) error {
	if !mqWorkerRoles[role] {
		return fmt.Errorf("unknown MQ worker role %q", role)
	}
	selected := func(candidate string) bool { return role == "all" || role == candidate }
	if role == "wine-ticket-maintenance" &&
		s.cfg.WineTicket.MaintenanceOwner != config.WineTicketMaintenanceOwnerWorker {
		return fmt.Errorf(
			"wine-ticket maintenance worker requires JXE_WINE_TICKET_MAINTENANCE_OWNER=worker (got %q)",
			s.cfg.WineTicket.MaintenanceOwner,
		)
	}
	if role == "wine-ticket-maintenance" && !s.cfg.WineTicket.Enabled {
		return fmt.Errorf("wine-ticket maintenance worker requires JXE_WINE_TICKET_ENABLED=true")
	}
	wineTicketMaintenanceSelected := selected("wine-ticket-maintenance") &&
		workerOwnsWineTicketMaintenance(s.cfg)
	if selected("mq-consumer-print") && s.cfg.MQ.ConsumerPrintEnabled {
		if err := s.ensurePrintProvider(ctx); err != nil {
			return err
		}
	}
	if s.deps.DB == nil {
		return fmt.Errorf("worker requires MySQL")
	}
	var printWorker *printjob.Worker
	if selected("mq-consumer-print") && s.cfg.MQ.ConsumerPrintEnabled {
		var err error
		printWorker, err = s.newPrintWorker(ctx)
		if err != nil {
			return err
		}
	}
	needsMQ := role != "search-retention" && role != "wine-ticket-maintenance"
	if needsMQ && (s.deps.RabbitMQ == nil || s.deps.IDGen == nil) {
		return fmt.Errorf("MQ worker requires RabbitMQ and Snowflake ID generation")
	}
	if wineTicketMaintenanceSelected &&
		s.deps.IDGen == nil {
		return fmt.Errorf("wine-ticket maintenance worker requires Snowflake ID generation")
	}
	if selected("mq-consumer-cache") && s.cfg.MQ.ConsumerCacheEnabled && s.deps.Redis == nil {
		return fmt.Errorf("cache MQ worker requires Redis")
	}
	if (selected("mq-consumer-realtime") || selected("realtime-relay")) && s.cfg.Realtime.Enabled && s.deps.Redis == nil {
		return fmt.Errorf("realtime worker requires Redis")
	}
	if needsMQ {
		connection, err := s.deps.RabbitMQ.Connection(ctx)
		if err != nil {
			return err
		}
		channel, err := connection.Channel()
		if err != nil {
			return err
		}
		err = mq.DeclareTopology(channel, mq.DefaultTopology())
		_ = channel.Close()
		if err != nil {
			return fmt.Errorf("MQ topology declaration failed: %w", err)
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 1)
	var waitGroup sync.WaitGroup
	started := 0
	spawn := func(run func()) {
		started++
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			run()
		}()
	}
	if s.deps.NodeLease != nil {
		spawn(func() {
			if runErr := s.deps.NodeLease.Run(runCtx); runErr != nil && runCtx.Err() == nil {
				select {
				case errCh <- runErr:
				default:
				}
			}
		})
	}
	registry := mq.MustDefaultEventRegistry()
	dispatchService := dispatch.NewService(s.cfg, s.deps.DB, s.deps.Redis, s.deps.IDGen, s.deps.Metrics, s.log)
	if selected("search-retention") && s.cfg.Search.CleanupEnabled {
		worker := search.NewWorker(s.cfg.Search, s.deps.DB, s.deps.Metrics, s.cfg.App.InstanceID+":search-retention", s.log)
		spawn(func() { worker.Run(runCtx) })
	}
	if wineTicketMaintenanceSelected {
		wineTicketModule := wineticket.NewModule(
			s.deps.DB,
			s.deps.IDGen,
			wineticket.ModuleOptions{
				GiftTokenPepper:  s.cfg.WineTicket.GiftTokenPepper,
				QuoteTokenSecret: s.cfg.WineTicket.QuoteTokenSecret,
				WeChatAppID:      s.cfg.WeChat.MiniAppID,
				InstanceID:       s.cfg.App.InstanceID,
			},
		)

		s.startWineTicketOrderExpiryWorker(
			runCtx,
			wineTicketModule,
			spawn,
		)

		giftWorker := wineTicketModule.NewGiftExpiryWorker(s.log)
		spawn(func() { giftWorker.Run(runCtx) })

		reminderProvider, providerErr := wechat.NewSubscriptionMessageProvider(
			s.cfg.WeChat,
			s.cfg.WineTicket,
		)
		if providerErr != nil {
			cancel()
			waitGroup.Wait()
			return providerErr
		}
		expiryWorker := wineTicketModule.NewExpiryMaintenanceWorker(
			reminderProvider,
			s.cfg.App.InstanceID+":wine-ticket-expiry",
			s.log,
		).
			WithRemindersEnabled(s.cfg.WineTicket.ReminderEnabled).
			WithWeChatEnabled(s.cfg.WineTicket.WeChatReminderEnabled).
			WithSendLease(4*s.cfg.WeChat.HTTPTimeout + time.Minute)
		spawn(func() { expiryWorker.Run(runCtx) })

		if s.cfg.WineTicket.ReconciliationEnabled {
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

		if s.cfg.AfterSale.WorkerEnabled &&
			s.cfg.AfterSale.RefundExecutionEnabled &&
			s.deps.RefundProvider != nil {
			afterSaleService := aftersale.NewService(
				s.cfg, s.deps.DB, s.deps.IDGen,
			)
			returnService := deliveryreturn.NewService(
				s.cfg, s.deps.DB, s.deps.Redis, s.deps.IDGen,
			).WithAfterSale(afterSaleService)
			returnService.WithReturnSettlementHandler(
				wineTicketModule.ReturnSettlement(afterSaleService),
			)
			service := refund.NewService(
				s.cfg,
				s.deps.DB,
				s.deps.IDGen,
				s.deps.RefundProvider,
			).
				WithRefundSettlementHandler(
					wineTicketModule.PurchaseRefundSettlement(),
				).
				WithRefundSettlementHandler(
					wineTicketModule.RenewalRefundSettlement(),
				).
				WithDeliveryReturnClosure(returnService)
			worker := refund.NewWorker(
				s.cfg, service, s.deps.RefundProvider, s.log,
			)
			spawn(func() { worker.Run(runCtx) })
		}
	}

	if selected("outbox-publisher") && s.cfg.Feature.MQPublisherEnabled {
		publisher := mq.NewPublisher(s.deps.DB, s.deps.RabbitMQ, s.deps.Metrics, s.cfg.App.InstanceID, s.log,
			mq.WithPublisherRegistry(registry), mq.WithPublisherEnvironment(s.cfg.App.Env),
			mq.WithPublisherIDs(s.deps.IDGen), mq.WithPublisherBatchSize(s.cfg.MQ.PublisherBatchSize))
		spawn(func() { publisher.Run(runCtx) })
	}

	var notificationWorker *notification.Worker
	if selected("mq-consumer-notification") && s.cfg.MQ.ConsumerNotificationEnabled {
		var provider notification.Provider = &notification.UnavailableProvider{}
		if s.cfg.CP1.NotificationProvider == "fake" {
			provider = &notification.FakeProvider{}
		}
		notificationWorker = notification.NewWorker(s.cfg.CP1, s.deps.DB, s.deps.IDGen, provider, s.cfg.App.InstanceID, s.log).
			WithDeliveryIncidentNotifications(s.cfg.DeliveryIncident.NotificationEnabled).
			WithDeliveryReturnNotifications(s.cfg.DeliveryReturn.NotificationEnabled)
		spec, _ := mq.DefaultConsumerSpec("notification")
		runtime := mq.NewConsumerRuntime(spec, s.deps.DB, s.deps.RabbitMQ, registry, notification.NewMQHandler(notificationWorker), s.deps.IDGen, s.deps.Metrics, s.cfg.App.InstanceID, s.log)
		spawn(func() { runtime.Run(runCtx) })
		if s.cfg.CP1.WorkerEnabled {
			spawn(func() { notificationWorker.RunWithFallback(runCtx, s.cfg.MQ.DBFallbackEnabled) })
		}
	}

	if selected("mq-consumer-print") && s.cfg.MQ.ConsumerPrintEnabled {
		spec, _ := mq.DefaultConsumerSpec("print")
		runtime := mq.NewConsumerRuntime(spec, s.deps.DB, s.deps.RabbitMQ, registry, printjob.NewMQHandler(printWorker), s.deps.IDGen, s.deps.Metrics, s.cfg.App.InstanceID, s.log)
		spawn(func() { runtime.Run(runCtx) })
		if s.cfg.CP1.WorkerEnabled && s.cfg.MQ.DBFallbackEnabled {
			spawn(func() { printWorker.Run(runCtx) })
		}
	}

	if selected("mq-consumer-cache") && s.cfg.MQ.ConsumerCacheEnabled {
		handler := mq.NewCacheInvalidationHandler(s.deps.Redis)
		spec, _ := mq.DefaultConsumerSpec("cache")
		runtime := mq.NewConsumerRuntime(spec, s.deps.DB, s.deps.RabbitMQ, registry, handler, s.deps.IDGen, s.deps.Metrics, s.cfg.App.InstanceID, s.log)
		spawn(func() { runtime.Run(runCtx) })
		if s.cfg.MQ.DBFallbackEnabled {
			fallback := mq.NewCacheFallbackWorker(s.deps.DB, registry, handler, s.deps.IDGen, s.deps.Metrics, s.cfg.App.InstanceID, s.log)
			spawn(func() { fallback.Run(runCtx) })
		}
	}

	if selected("mq-consumer-security") && s.cfg.MQ.ConsumerSecurityEnabled {
		spec, _ := mq.DefaultConsumerSpec("security")
		handler := mq.ConsumerHandlerFunc(func(context.Context, *gorm.DB, mq.EventEnvelope) (mq.ConsumerResult, error) {
			return mq.ConsumerResult{RefType: "security_observation"}, nil
		})
		runtime := mq.NewConsumerRuntime(spec, s.deps.DB, s.deps.RabbitMQ, registry, handler, s.deps.IDGen, s.deps.Metrics, s.cfg.App.InstanceID, s.log)
		spawn(func() { runtime.Run(runCtx) })
	}

	if selected("mq-consumer-dispatch") && s.cfg.MQ.ConsumerDispatchEnabled {
		spec, _ := mq.DefaultConsumerSpec("dispatch")
		runtime := mq.NewConsumerRuntime(spec, s.deps.DB, s.deps.RabbitMQ, registry, dispatch.NewMQHandler(dispatchService), s.deps.IDGen, s.deps.Metrics, s.cfg.App.InstanceID, s.log)
		spawn(func() { runtime.Run(runCtx) })
		if s.cfg.Dispatch.WorkerEnabled {
			worker := dispatch.NewWorker(dispatchService, s.cfg.App.InstanceID+":dispatch-sweeper", s.log)
			spawn(func() { worker.Run(runCtx) })
		}
	}

	if selected("mq-consumer-realtime") && s.cfg.MQ.ConsumerRealtimeEnabled && s.deps.Realtime != nil {
		spec, _ := mq.DefaultConsumerSpec("realtime")
		runtime := mq.NewConsumerRuntime(spec, s.deps.DB, s.deps.RabbitMQ, registry, realtime.NewMQHandler(s.deps.Realtime.Service), s.deps.IDGen, s.deps.Metrics, s.cfg.App.InstanceID, s.log)
		spawn(func() { runtime.Run(runCtx) })
	}

	if selected("realtime-relay") && s.cfg.Realtime.Enabled && s.cfg.Realtime.RelayEnabled && s.deps.Realtime != nil {
		spawn(func() { s.deps.Realtime.Relay.Run(runCtx) })
	}

	if selected("mq-dead-sink") {
		for _, consumer := range []string{"notification", "print", "cache", "security", "dispatch", "realtime", "unrouted"} {
			sink := mq.NewDeadSink(s.deps.DB, s.deps.RabbitMQ, s.deps.IDGen, registry, s.deps.Metrics, consumer, s.log)
			spawn(func() { sink.Run(runCtx) })
		}
	}

	// 单独的节点租约只是基础设施，并不是有实际用途的工作角色。
	minimum := 1
	if s.deps.NodeLease != nil {
		minimum = 2
	}
	if started < minimum {
		cancel()
		waitGroup.Wait()
		return fmt.Errorf("MQ worker role %q is disabled by configuration", role)
	}
	s.log.Info("worker process started", "role", role)
	select {
	case <-ctx.Done():
		cancel()
		waitGroup.Wait()
		s.closeInfra()
		return nil
	case runErr := <-errCh:
		cancel()
		waitGroup.Wait()
		s.closeInfra()
		return runErr
	}
}

// startWineTicketOrderExpiryWorker 确保即使未配置远程支付机构，
// 本地订单过期仍归选定的维护进程负责。
// 支付机构对账可以关闭，但释放本地已过期未支付订单不能关闭。
func (s *Server) startWineTicketOrderExpiryWorker(
	ctx context.Context,
	module *wineticket.Module,
	spawn func(func()),
) {
	if !s.cfg.Order.ExpiryWorkerEnabled {
		return
	}
	providers := make([]order.PaymentProvider, 0, 1)
	if s.deps.PaymentProvider != nil {
		providers = append(providers, s.deps.PaymentProvider)
	}
	worker := order.NewExpiryWorker(
		s.cfg,
		s.deps.DB,
		s.deps.IDGen,
		s.deps.Metrics,
		s.log,
		providers...,
	).
		WithPaymentSettlementHandler(module.PurchasePaymentSettlement()).
		WithPaymentSettlementHandler(module.RenewalPaymentSettlement())
	spawn(func() { worker.Run(ctx) })
}
