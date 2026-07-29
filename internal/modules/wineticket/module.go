package wineticket

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/aftersale"
	"jiuxiaoer-admin/backend-go/internal/modules/dispatch"
	"jiuxiaoer-admin/backend-go/internal/modules/notification"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/cabinet"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/catalog"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/gift"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/integrity"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/ops"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/purchase"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/redemption"
	refunddomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/reminder"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/renewal"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type ModuleOptions struct {
	GiftTokenPepper  string
	QuoteTokenSecret string
	WeChatAppID      string
	InstanceID       string
}

// Module 是酒票业务唯一的应用装配入口。
// 各子包拥有自己的用例，且不得反向导入父包。
type Module struct {
	db      *gorm.DB
	ids     *snowflake.Generator
	options ModuleOptions

	catalogService *catalog.Service
	catalogHandler *catalog.Handler

	purchaseService *purchase.Service
	purchaseHandler *purchase.Handler

	cabinetService *cabinet.Service
	cabinetHandler *cabinet.Handler

	giftService *gift.GiftService
	giftHandler *gift.GiftHandler

	redemptionService *redemption.RedemptionService
	redemptionHandler *redemption.RedemptionHandler

	renewalService *renewal.RenewalService
	renewalHandler *renewal.RenewalHandler

	refundService *refunddomain.RefundService
	refundHandler *refunddomain.RefundHandler

	reminderService *reminder.ReminderService
	reminderHandler *reminder.ReminderHandler

	fulfillmentSettlement *redemption.WineTicketFulfillmentSettlement

	opsService     *ops.Service
	opsHandler     *ops.Handler
	slotOpsHandler *ops.SlotAdminHandler
}

func NewModule(
	db *gorm.DB,
	ids *snowflake.Generator,
	options ModuleOptions,
) *Module {
	catalogService := catalog.NewService(db, ids).WithInstance(options.InstanceID)
	purchaseService := purchase.NewService(db, ids).
		WithWeChatAppID(options.WeChatAppID)
	cabinetService := cabinet.NewService(db)
	giftService := gift.NewGiftService(db, ids, options.GiftTokenPepper)
	redemptionService := redemption.NewRedemptionService(db, ids)
	renewalService := renewal.NewRenewalService(
		db,
		ids,
		options.QuoteTokenSecret,
	).WithWeChatAppID(options.WeChatAppID)
	refundService := refunddomain.NewRefundService(
		db,
		ids,
		options.QuoteTokenSecret,
	)
	reminderService := reminder.NewReminderService(db, ids)
	opsService := ops.NewService(db, ids, purchaseService).WithInstance(options.InstanceID)

	return &Module{
		db:                db,
		ids:               ids,
		options:           options,
		catalogService:    catalogService,
		catalogHandler:    catalog.NewHandler(catalogService),
		purchaseService:   purchaseService,
		purchaseHandler:   purchase.NewHandler(purchaseService),
		cabinetService:    cabinetService,
		cabinetHandler:    cabinet.NewHandler(cabinetService),
		giftService:       giftService,
		giftHandler:       gift.NewGiftHandler(giftService),
		redemptionService: redemptionService,
		redemptionHandler: redemption.NewRedemptionHandler(redemptionService),
		renewalService:    renewalService,
		renewalHandler:    renewal.NewRenewalHandler(renewalService),
		refundService:     refundService,
		refundHandler:     refunddomain.NewRefundHandler(refundService),
		reminderService:   reminderService,
		reminderHandler:   reminder.NewReminderHandler(reminderService),
		fulfillmentSettlement: redemption.NewWineTicketFulfillmentSettlement(
			db,
			ids,
		),
		opsService:     opsService,
		opsHandler:     ops.NewHandler(opsService),
		slotOpsHandler: ops.NewSlotAdminHandler(ops.NewSlotAdminService(opsService)),
	}
}

func (m *Module) WithPaymentService(service *order.Service) *Module {
	m.purchaseService.WithPaymentService(service)
	m.renewalService.WithPaymentService(service)
	return m
}

func (m *Module) WithDispatchService(service *dispatch.Service) *Module {
	m.redemptionService.WithDispatch(redemption.NewFulfillmentDispatch(service))
	return m
}

func (m *Module) PurchasePaymentSettlement() order.PaymentSettlementHandler {
	return m.purchaseService
}

func (m *Module) RenewalPaymentSettlement() order.PaymentSettlementHandler {
	return renewal.NewRenewalPaymentSettlementHandler(m.renewalService)
}

func (m *Module) FulfillmentSettlement() FulfillmentSettlement {
	return m.fulfillmentSettlement
}

func (m *Module) PurchaseRefundSettlement() RefundSettlement {
	return refunddomain.NewWineTicketRefundSettlement(m.db, m.ids)
}

func (m *Module) RenewalRefundSettlement() RefundSettlement {
	return renewal.NewRenewalRefundSettlementHandler(m.renewalService)
}

func (m *Module) ReturnSettlement(
	afterSales *aftersale.Service,
) ReturnSettlement {
	return redemption.NewWineTicketReturnSettlement(m.db, m.ids, afterSales)
}

func (m *Module) RegisterCatalogRoutes(router *gin.RouterGroup) {
	catalog.RegisterPublicRoutes(router, m.catalogHandler)
}

func (m *Module) RegisterGiftPublicRoutes(router *gin.RouterGroup) {
	gift.RegisterGiftPublicRoutes(router, m.giftHandler)
}

func (m *Module) RegisterGiftContinuityRoutes(router *gin.RouterGroup) {
	gift.RegisterGiftContinuityRoutes(router, m.giftHandler)
}

func (m *Module) RegisterGiftCreationRoutes(router *gin.RouterGroup) {
	gift.RegisterGiftCreationRoutes(router, m.giftHandler)
}

func (m *Module) RegisterReminderRoutes(router *gin.RouterGroup) {
	reminder.RegisterReminderCustomerRoutes(router, m.reminderHandler)
}

func (m *Module) RegisterPurchaseContinuityRoutes(router *gin.RouterGroup) {
	purchase.RegisterContinuityRoutes(router, m.purchaseHandler)
}

func (m *Module) RegisterPurchaseCreationRoutes(router *gin.RouterGroup) {
	purchase.RegisterCreationRoutes(router, m.purchaseHandler)
}

func (m *Module) RegisterRedemptionContinuityRoutes(router *gin.RouterGroup) {
	redemption.RegisterRedemptionContinuityRoutes(router, m.redemptionHandler)
}

func (m *Module) RegisterRedemptionCreationRoutes(router *gin.RouterGroup) {
	redemption.RegisterRedemptionCreationRoutes(router, m.redemptionHandler)
}

func (m *Module) RegisterRenewalContinuityRoutes(router *gin.RouterGroup) {
	renewal.RegisterRenewalContinuityRoutes(router, m.renewalHandler)
}

func (m *Module) RegisterRenewalCreationRoutes(router *gin.RouterGroup) {
	renewal.RegisterRenewalCreationRoutes(router, m.renewalHandler)
}

func (m *Module) RegisterCabinetRoutes(router *gin.RouterGroup) {
	cabinet.RegisterCustomerRoutes(router, m.cabinetHandler)
}

func (m *Module) RegisterRefundContinuityRoutes(router *gin.RouterGroup) {
	refunddomain.RegisterRefundContinuityRoutes(router, m.refundHandler)
}

func (m *Module) RegisterRefundCreationRoutes(router *gin.RouterGroup) {
	refunddomain.RegisterRefundCreationRoutes(router, m.refundHandler)
}

func (m *Module) RegisterAdminRoutes(router *gin.RouterGroup) {
	catalog.RegisterAdminRoutes(router, m.catalogHandler)
	ops.RegisterAdminRoutes(router, m.opsHandler)
}

func (m *Module) RegisterSlotAdminRoutes(router *gin.RouterGroup) {
	ops.RegisterSlotAdminRoutes(router, m.slotOpsHandler)
}

func (m *Module) RegisterMetrics(registry *metrics.Registry) {
	ops.RegisterMetrics(m.db, registry)
}

func (m *Module) NewGiftExpiryWorker(log *slog.Logger) BackgroundWorker {
	return gift.NewGiftExpiryWorker(m.giftService, log)
}

func (m *Module) NewExpiryMaintenanceWorker(
	provider notification.Provider,
	instance string,
	log *slog.Logger,
) *ExpiryMaintenanceWorker {
	if log == nil {
		log = slog.Default()
	}
	instance = strings.TrimSpace(instance)
	if instance == "" {
		instance = "wine-ticket-expiry-reminder"
	}
	return &ExpiryMaintenanceWorker{
		reminder: reminder.NewExpiryReminderWorker(
			m.db,
			m.ids,
			provider,
			instance,
			log,
		).WithWeChatAppID(m.options.WeChatAppID),
		expiry:   core.NewExpiryWorker(m.db, m.ids),
		instance: instance,
		log:      log,
		interval: time.Minute,
	}
}

func (m *Module) NewIntegrityWorker(
	log *slog.Logger,
) *IntegrityMaintenanceWorker {
	return &IntegrityMaintenanceWorker{worker: integrity.NewIntegrityWorker(
		integrity.NewIntegrityService(m.db, m.ids),
		log,
	)}
}

// IntegrityMaintenanceWorker 将运维参数留在模块边界内，
// 避免应用装配层依赖完整性检查的具体实现。
type IntegrityMaintenanceWorker struct {
	worker *integrity.IntegrityWorker
}

func (w *IntegrityMaintenanceWorker) ConfigureSchedule(
	owner string,
	dailyStart time.Duration,
	leaseDuration time.Duration,
) *IntegrityMaintenanceWorker {
	if w != nil && w.worker != nil {
		w.worker.ConfigureSchedule(owner, dailyStart, leaseDuration)
	}
	return w
}

func (w *IntegrityMaintenanceWorker) ConfigureBounds(
	batchSize int,
	batchInterval time.Duration,
	sweepInterval time.Duration,
) *IntegrityMaintenanceWorker {
	if w != nil && w.worker != nil {
		w.worker.ConfigureBounds(batchSize, batchInterval, sweepInterval)
	}
	return w
}

func (w *IntegrityMaintenanceWorker) Run(ctx context.Context) {
	if w != nil && w.worker != nil {
		w.worker.Run(ctx)
	}
}

// ExpiryMaintenanceWorker 组合客户提醒循环与权益过期循环。
// 两种行为仍归各自子域所有，只有应用装配入口知道它们共用调度周期。
type ExpiryMaintenanceWorker struct {
	reminder *reminder.ExpiryReminderWorker
	expiry   *core.ExpiryWorker
	instance string
	log      *slog.Logger
	interval time.Duration
}

func (w *ExpiryMaintenanceWorker) WithNow(
	now func() time.Time,
) *ExpiryMaintenanceWorker {
	w.reminder.WithNow(now)
	w.expiry.WithNow(now)
	return w
}

func (w *ExpiryMaintenanceWorker) WithBatchSize(
	batch int,
) *ExpiryMaintenanceWorker {
	w.reminder.WithBatchSize(batch)
	w.expiry.WithBatchSize(batch)
	return w
}

func (w *ExpiryMaintenanceWorker) WithInterval(
	interval time.Duration,
) *ExpiryMaintenanceWorker {
	if interval > 0 {
		w.interval = interval
	}
	w.reminder.WithInterval(interval)
	return w
}

func (w *ExpiryMaintenanceWorker) WithRemindersEnabled(
	enabled bool,
) *ExpiryMaintenanceWorker {
	w.reminder.WithRemindersEnabled(enabled)
	return w
}

func (w *ExpiryMaintenanceWorker) WithWeChatEnabled(
	enabled bool,
) *ExpiryMaintenanceWorker {
	w.reminder.WithWeChatEnabled(enabled)
	return w
}

func (w *ExpiryMaintenanceWorker) WithSendLease(
	lease time.Duration,
) *ExpiryMaintenanceWorker {
	w.reminder.WithSendLease(lease)
	return w
}

func (w *ExpiryMaintenanceWorker) RunOnce(ctx context.Context) error {
	return runExpiryMaintenanceOnce(
		ctx,
		w.reminder.RunOnce,
		w.expiry.ExpireLotsOnce,
	)
}

func runExpiryMaintenanceOnce(
	ctx context.Context,
	runReminder func(context.Context) error,
	runExpiry func(context.Context) (int, error),
) error {
	reminderErr := runReminder(ctx)
	_, expiryErr := runExpiry(ctx)
	return errors.Join(reminderErr, expiryErr)
}

func (w *ExpiryMaintenanceWorker) Run(ctx context.Context) {
	w.runAndLog(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runAndLog(ctx)
		}
	}
}

func (w *ExpiryMaintenanceWorker) runAndLog(ctx context.Context) {
	if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		w.log.Error(
			"wine ticket expiry maintenance worker failed",
			slog.String("instance", w.instance),
			slog.Any("error", err),
		)
	}
}
