package app

import (
	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/infra/amap"
	"jiuxiaoer-admin/backend-go/internal/modules/address"
	"jiuxiaoer-admin/backend-go/internal/modules/admin"
	"jiuxiaoer-admin/backend-go/internal/modules/aftersale"
	"jiuxiaoer-admin/backend-go/internal/modules/asset"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/cart"
	"jiuxiaoer-admin/backend-go/internal/modules/compliance"
	"jiuxiaoer-admin/backend-go/internal/modules/cp1metrics"
	"jiuxiaoer-admin/backend-go/internal/modules/customerlocation"
	"jiuxiaoer-admin/backend-go/internal/modules/delivery"
	"jiuxiaoer-admin/backend-go/internal/modules/deliveryincident"
	"jiuxiaoer-admin/backend-go/internal/modules/deliveryreturn"
	"jiuxiaoer-admin/backend-go/internal/modules/deliveryverification"
	"jiuxiaoer-admin/backend-go/internal/modules/dispatch"
	"jiuxiaoer-admin/backend-go/internal/modules/docs"
	"jiuxiaoer-admin/backend-go/internal/modules/health"
	"jiuxiaoer-admin/backend-go/internal/modules/home"
	"jiuxiaoer-admin/backend-go/internal/modules/member"
	"jiuxiaoer-admin/backend-go/internal/modules/mq"
	"jiuxiaoer-admin/backend-go/internal/modules/notification"
	"jiuxiaoer-admin/backend-go/internal/modules/ops"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	"jiuxiaoer-admin/backend-go/internal/modules/printjob"
	"jiuxiaoer-admin/backend-go/internal/modules/product"
	"jiuxiaoer-admin/backend-go/internal/modules/provisioning"
	"jiuxiaoer-admin/backend-go/internal/modules/realtime"
	"jiuxiaoer-admin/backend-go/internal/modules/reconciliation"
	"jiuxiaoer-admin/backend-go/internal/modules/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/riderapplication"
	"jiuxiaoer-admin/backend-go/internal/modules/routeplanning"
	"jiuxiaoer-admin/backend-go/internal/modules/search"
	"jiuxiaoer-admin/backend-go/internal/modules/servicearea"
	"jiuxiaoer-admin/backend-go/internal/modules/shop"
	"jiuxiaoer-admin/backend-go/internal/modules/store"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// NewRouter 创建并初始化路由器。
func NewRouter(deps Dependencies) *gin.Engine {
	if deps.Config.App.Env == "prod" || deps.Config.App.Env == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	router.Use(requestIDMiddleware())
	router.Use(accessLogMiddleware(deps.Log, deps.Metrics))
	router.Use(recoveryMiddleware(deps.Log))
	router.Use(requestLimitsMiddleware(deps.Config.HTTP.RequestTimeout, deps.Config.HTTP.MaxBodyBytes))
	if deps.Config.Metrics.Enabled && deps.Metrics != nil {
		router.GET("/metrics", deps.Metrics.Handler)
	}

	// 公开路由只保留健康检查、认证、商品目录和门店。
	// protected 下的写接口仍会在 service 层校验角色和对象范围。
	api := router.Group("/api/v1")
	health.RegisterRoutes(router, api, deps.Config, deps.DB, deps.Redis, deps.RabbitMQ, deps.NodeLease, deps.Metrics)
	docs.RegisterRoutes(api)
	cp1metrics.Register(deps.DB, deps.Metrics)
	mq.RegisterMetrics(deps.DB, deps.RabbitMQ, deps.Metrics)
	dispatch.RegisterMetrics(deps.DB, deps.Metrics)
	deliveryreturn.RegisterMetrics(deps.DB, deps.Metrics)

	idGen := deps.IDGen
	if idGen == nil {
		idGen = snowflake.New(deps.Config.App.SnowflakeNodeID)
	}
	if deps.Metrics != nil {
		deps.Metrics.AddCollector(func() []metrics.Sample {
			return []metrics.Sample{{
				Name: "jxe_snowflake_clock_rollbacks_total", Help: "Observed wall-clock rollbacks handled by the Snowflake logical clock.",
				Type: "counter", Value: float64(idGen.ClockRollbackCount()),
			}}
		})
	}
	authService := auth.NewService(deps.Config, deps.DB, deps.Redis, idGen, deps.WeChatAuth).WithSMSProvider(deps.SMSProvider)
	authHandler := auth.NewHandler(authService)
	auth.RegisterRoutes(api.Group("/auth"), authHandler)
	riderApplicationService := riderapplication.NewService(deps.Config, deps.DB, deps.Redis, idGen, deps.Metrics).WithSMSVerifier(authService)
	riderApplicationHandler := riderapplication.NewHandler(riderApplicationService)
	riderapplication.RegisterPublicRoutes(api, riderApplicationHandler)
	riderapplication.RegisterApplicantRoutes(api, riderApplicationHandler)

	serviceAreaService := servicearea.NewService(deps.Config.Service, deps.DB, deps.Redis, deps.Metrics)
	servicearea.RegisterRoutes(api, servicearea.NewHandler(serviceAreaService))
	lbsProvider := deps.CustomerLBSProvider
	if lbsProvider == nil {
		switch deps.Config.CustomerLBS.Provider {
		case "fake":
			lbsProvider = amap.NewFakeProvider()
		case "amap":
			provider, err := amap.NewClient(deps.Config.CustomerLBS.AmapBaseURL, deps.Config.CustomerLBS.AmapKey, deps.Config.CustomerLBS.RegeocodeTimeout, deps.Config.CustomerLBS.RouteTimeout)
			if err != nil {
				lbsProvider = &amap.UnavailableProvider{}
			} else {
				lbsProvider = provider
			}
		default:
			lbsProvider = &amap.UnavailableProvider{}
		}
	}
	customerLocationService := customerlocation.NewService(deps.Config.CustomerLBS, deps.DB, deps.Redis, lbsProvider, serviceAreaService, deps.Metrics, deps.Log, idGen)
	authService.WithCustomerLogoutHook(customerLocationService.RevokeCustomer)
	customerLocationHandler := customerlocation.NewHandler(customerLocationService)
	if deps.Config.CustomerLBS.Mode != "off" {
		customerlocation.RegisterRoutes(api, customerLocationHandler, authHandler.OptionalAuth())
	}
	publicReads := api.Group("")
	publicReads.Use(authHandler.OptionalAuth())
	homeService := home.NewService(deps.Config.Service, deps.DB, deps.Redis, serviceAreaService, idGen).WithLocationContexts(deps.Config.CustomerLBS.Mode, customerLocationService)
	homeHandler := home.NewHandler(homeService)
	home.RegisterPublicRoutes(publicReads, homeHandler)

	productService := product.NewService(deps.DB, deps.Redis, serviceAreaService).WithLocationContexts(deps.Config.CustomerLBS.Mode, customerLocationService)
	product.RegisterRoutes(publicReads, product.NewHandler(productService))
	searchService := search.NewService(deps.Config.Search, deps.DB, deps.Redis, idGen, deps.Metrics, deps.Log).WithLocations(customerLocationService)
	searchHandler := search.NewHandler(searchService)
	search.RegisterPublicRoutes(publicReads, searchHandler)

	shopService := shop.NewService(deps.DB, deps.Redis)
	shop.RegisterRoutes(publicReads, shop.NewHandler(shopService).WithLocationContexts(deps.Config.CustomerLBS.Mode, customerLocationService))

	protected := api.Group("")
	protected.Use(authHandler.AuthRequired())
	search.RegisterCustomerRoutes(protected, searchHandler)
	customerlocation.RegisterAdminRoutes(protected.Group("/admin"), customerLocationHandler)
	if deps.Realtime == nil {
		deps.Realtime = realtime.NewRuntime(deps.Config, deps.DB, deps.Redis, idGen, deps.Metrics, deps.Log)
	}
	realtime.RegisterRoutes(api, protected, deps.Realtime.Handler)

	// JWT 只能证明账号身份；service 方法仍需校验 C 端数据归属、
	// 商户授权门店、骑手配送单归属和 admin 权限点。
	cartService := cart.NewService(deps.DB, idGen)
	cart.RegisterRoutes(protected.Group("/cart"), cart.NewHandler(cartService))

	addressService := address.NewService(deps.DB, idGen).WithLocationVerification(deps.Config.CustomerLBS.Mode, customerLocationService)
	address.RegisterRoutes(protected.Group("/addresses"), address.NewHandler(addressService))

	assetService := asset.NewService(deps.Config, deps.DB, idGen)
	asset.RegisterMetrics(deps.DB, deps.Metrics)
	asset.RegisterCustomerRoutes(protected.Group("/assets"), asset.NewHandler(assetService))
	memberService := member.NewService(deps.Config, deps.DB, idGen)
	member.RegisterCustomerRoutes(protected.Group("/member"), member.NewHandler(memberService))

	dispatchService := dispatch.NewService(deps.Config, deps.DB, deps.Redis, idGen, deps.Metrics, deps.Log)
	dispatchHandler := dispatch.NewHandler(dispatchService)
	incidentService := deliveryincident.NewService(deps.Config, deps.DB, idGen, deps.Metrics, deps.Redis)
	incidentHandler := deliveryincident.NewHandler(incidentService)
	orderService := order.NewService(deps.Config, deps.DB, idGen).WithLogger(deps.Log).WithServiceArea(serviceAreaService).WithCustomerLocation(customerLocationService).WithPaymentProvider(deps.PaymentProvider, deps.Metrics).WithDispatch(dispatchService).WithIncidentResolver(incidentService)
	orderHandler := order.NewHandler(orderService)
	order.RegisterCallbackRoute(api, orderHandler)
	order.RegisterRoutes(protected.Group("/orders"), orderHandler)

	afterSaleService := aftersale.NewService(deps.Config, deps.DB, idGen)
	afterSaleHandler := aftersale.NewHandler(afterSaleService)
	aftersale.RegisterCustomerRoutes(protected.Group("/after-sales"), afterSaleHandler)
	aftersale.RegisterStoreRoutes(protected.Group("/store/after-sales"), afterSaleHandler)
	aftersale.RegisterAdminRoutes(protected.Group("/admin/after-sales"), afterSaleHandler)

	refundService := refund.NewService(deps.Config, deps.DB, idGen, deps.RefundProvider)
	refund.RegisterMetrics(deps.DB, deps.Metrics)
	refundHandler := refund.NewHandler(refundService)
	refund.RegisterAdminRoutes(protected.Group("/admin/refunds"), refundHandler)
	if deps.RefundProvider != nil {
		refund.RegisterCallbackRoute(api, refundHandler)
	}
	if deps.DB != nil {
		reconciliationService := reconciliation.NewService(deps.Config, deps.DB, idGen, deps.BillProvider, deps.Log)
		reconciliation.RegisterAdminRoutes(protected.Group("/admin/reconciliation"), reconciliation.NewHandler(reconciliationService))
	}

	storeService := store.NewService(deps.DB, deps.Redis, idGen).WithCP1(deps.Config.CP1).WithDispatch(dispatchService)
	store.RegisterRoutes(protected.Group("/store"), store.NewHandler(storeService))

	returnService := deliveryreturn.NewService(deps.Config, deps.DB, deps.Redis, idGen).WithAfterSale(afterSaleService)
	incidentService.WithReturnOrchestrator(returnService)
	refundService.WithDeliveryReturnClosure(returnService)
	returnHandler := deliveryreturn.NewHandler(returnService)
	deliveryService := delivery.NewService(deps.DB, idGen).WithCP1(deps.Config.CP1).WithDispatch(dispatchService).WithIncidentResolver(incidentService).WithReturnGuard(returnService)
	delivery.RegisterRoutes(protected.Group("/delivery"), delivery.NewHandler(deliveryService))
	deliveryreturn.RegisterRiderRoutes(protected.Group("/delivery"), returnHandler)
	deliveryreturn.RegisterStoreRoutes(protected.Group("/store"), returnHandler)
	deliveryreturn.RegisterAdminRoutes(protected.Group("/admin"), returnHandler)
	deliveryincident.RegisterRiderRoutes(protected.Group("/delivery"), incidentHandler)
	deliveryincident.RegisterStoreRoutes(protected.Group("/store"), incidentHandler)
	deliveryincident.RegisterAdminRoutes(protected.Group("/admin"), incidentHandler)
	dispatch.RegisterRiderRoutes(protected.Group("/delivery"), dispatchHandler)
	routeProvider := deps.RouteProvider
	if routeProvider == nil {
		switch deps.Config.MapRoute.Provider {
		case "amap":
			amapProvider, err := routeplanning.NewAmapProvider(deps.Config.MapRoute.AmapBaseURL, deps.Config.MapRoute.AmapKey, deps.Config.MapRoute.Timeout)
			if err != nil {
				routeProvider = &routeplanning.UnavailableProvider{}
			} else {
				routeProvider = amapProvider
			}
		default:
			routeProvider = routeplanning.NewFakeProvider()
		}
	}
	routeService := routeplanning.NewService(deps.Config.MapRoute, deps.DB, deps.Redis, routeProvider, deps.Metrics, deps.Log)
	routeplanning.RegisterRoutes(protected.Group("/delivery"), routeplanning.NewHandler(routeService))
	dispatch.RegisterAdminRoutes(protected.Group("/admin/dispatch"), dispatchHandler)

	printService := printjob.NewService(deps.Config.CP1, deps.DB, idGen)
	printHandler := printjob.NewHandler(printService)
	printjob.RegisterStoreRoutes(protected.Group("/store"), printHandler)
	printjob.RegisterAdminRoutes(protected.Group("/admin"), printHandler)

	notificationService := notification.NewService(deps.DB, idGen)
	notificationHandler := notification.NewHandler(notificationService)
	notification.RegisterCustomerRoutes(protected.Group("/messages"), notificationHandler)
	notification.RegisterAdminRoutes(protected.Group("/admin"), notificationHandler)
	mq.RegisterAdminRoutes(protected.Group("/admin/mq"), mq.NewAdminHandler(mq.NewAdminService(deps.DB, deps.RabbitMQ, mq.MustDefaultEventRegistry(), idGen)))

	verificationService := deliveryverification.NewService(deps.Config.CP1, deps.DB, idGen)
	verificationHandler := deliveryverification.NewHandler(verificationService)
	deliveryverification.RegisterStoreRoutes(protected.Group("/store"), verificationHandler)
	deliveryverification.RegisterCustomerRoutes(protected.Group("/orders"), verificationHandler)
	deliveryverification.RegisterAdminRoutes(protected.Group("/admin"), verificationHandler)

	var identityProvider compliance.Provider = &compliance.UnavailableProvider{}
	if deps.Config.CP1.IdentityProvider == "fake" {
		identityProvider = compliance.NewFakeProvider(deps.Config.CP1.IdentityCallbackSecret)
	}
	complianceService := compliance.NewService(deps.Config.CP1, deps.DB, idGen, identityProvider)
	complianceHandler := compliance.NewHandler(complianceService)
	compliance.RegisterCallbackRoute(api, complianceHandler)
	compliance.RegisterCustomerRoutes(protected.Group("/identity-verifications"), complianceHandler)
	compliance.RegisterAdminRoutes(protected.Group("/admin"), complianceHandler)

	provisioning.RegisterRoutes(protected.Group("/admin"), provisioning.NewHandler(provisioning.NewService(deps.Config.CP1, deps.DB, idGen)))
	riderapplication.RegisterAdminRoutes(protected.Group("/admin"), riderApplicationHandler)
	opsService := ops.NewService(deps.Config, deps.DB, idGen, orderService).WithDispatch(dispatchService).WithIncidentResolver(incidentService).WithReturnGuard(returnService)
	ops.RegisterRoutes(protected.Group("/admin"), ops.NewHandler(opsService))

	adminService := admin.NewService(deps.DB, deps.Redis, idGen)
	admin.RegisterRoutes(protected.Group("/admin"), admin.NewHandler(adminService))
	asset.RegisterAdminRoutes(protected.Group("/admin"), asset.NewHandler(assetService))
	member.RegisterAdminRoutes(protected.Group("/admin"), member.NewHandler(memberService))
	home.RegisterAdminRoutes(protected.Group("/admin"), homeHandler)

	return router
}
