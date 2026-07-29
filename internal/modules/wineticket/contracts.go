package wineticket

import (
	"context"

	"jiuxiaoer-admin/backend-go/internal/modules/delivery"
	"jiuxiaoer-admin/backend-go/internal/modules/deliveryreturn"
	"jiuxiaoer-admin/backend-go/internal/modules/dispatch"
	"jiuxiaoer-admin/backend-go/internal/modules/order"
	sharedrefund "jiuxiaoer-admin/backend-go/internal/modules/refund"
	refunddomain "jiuxiaoer-admin/backend-go/internal/modules/wineticket/refund"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/renewal"
)

// RefundOriginalPaymentQuerier 提供共享退款模块向支付机构提交退款前
// 所需的最小 APIv3 原支付事实查询能力。
type RefundOriginalPaymentQuerier interface {
	Query(context.Context, string) (order.ProviderPaymentState, error)
}

// FulfillmentSettlement 是零现金配送的完整集成端口。
// 具体核销实现仅在 Module 内部可见。
type FulfillmentSettlement interface {
	delivery.FulfillmentSettlementHandler
	dispatch.AssignmentSettlementHandler
}

// RefundSettlement 是共享退款模块接入酒票业务的集成端口。
type RefundSettlement interface {
	sharedrefund.RefundSettlementHandler
	sharedrefund.RefundSettlementFailureHandler
}

// ReturnSettlement 是共享配送退回模块的集成端口。
// 具体实现还可以实现可选的收货准备接口。
type ReturnSettlement interface {
	deliveryreturn.ReturnSettlementHandler
	deliveryreturn.ReturnSettlementReceivePreparer
}

// BackgroundWorker 是应用装配层管理无需额外调参的后台任务时
// 唯一需要依赖的生命周期契约。
type BackgroundWorker interface {
	Run(context.Context)
}

// NewVerifiedWineTicketRefundProvider 在保持共享 refund.Provider 契约的同时，
// 增加酒票原支付校验防线。
func NewVerifiedWineTicketRefundProvider(
	payments RefundOriginalPaymentQuerier,
	refunds sharedrefund.Provider,
) sharedrefund.Provider {
	return refunddomain.NewVerifiedWineTicketRefundProvider(
		payments,
		refunds,
		refunddomain.WineTicketPurchaseRefundBusiness,
		renewal.RenewalCompensationRefundBusiness,
	)
}
