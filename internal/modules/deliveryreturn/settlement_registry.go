package deliveryreturn

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/aftersale"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

const (
	SettlementRetailCashRefund  = "retail_cash_refund"
	SettlementWineTicketRestore = "wine_ticket_restore"
)

type ReturnSettlementKey struct {
	OrderType      string
	SettlementMode string
}

type ReturnSettlementBinding struct {
	SettlementType  string
	SettlementBizID *uint64
}

type ReturnSettlementApproval struct {
	AfterSaleID     uint64
	SettlementBizID *uint64
	OrderStatus     string
	AfterSaleStatus string
}

// ReturnSettlementHandler 负责配送退回中的业务特有部分。
// 配送退回服务会在一个事务中处理共享物流、收货、库存、审计和闭环事实。
type ReturnSettlementHandler interface {
	RoutingKey() ReturnSettlementKey
	SettlementType() string
	InitialBinding(context.Context, *gorm.DB, OrderRef) (ReturnSettlementBinding, error)
	Approve(context.Context, *gorm.DB, Return, DeliveryOrder, OrderRef, uint64, string) (ReturnSettlementApproval, error)
	SettleReceived(context.Context, *gorm.DB, Return, AfterSale, OrderRef) (bool, error)
}

// ReturnSettlementReceivePlan 是可选的事务级结算计划，
// 在配送退回锁定实物库存前准备。
// 同一事务后续调用 ApplyReceived 时，实现必须复用准备阶段锁定的业务记录。
type ReturnSettlementReceivePlan interface {
	ApplyReceived(context.Context, *gorm.DB, Return, AfterSale, OrderRef) (bool, error)
}

// ReturnSettlementReceivePreparer 允许结算路由在共享收货流程锁定库存前，
// 建立自身业务锁前缀。未实现该接口的处理器继续使用旧结算路径。
type ReturnSettlementReceivePreparer interface {
	PrepareReceived(context.Context, *gorm.DB, Return, AfterSale, OrderRef) (ReturnSettlementReceivePlan, error)
}

type returnSettlementRegistry struct {
	mu     sync.RWMutex
	byKey  map[ReturnSettlementKey]ReturnSettlementHandler
	byType map[string]ReturnSettlementHandler
}

func newReturnSettlementRegistry() *returnSettlementRegistry {
	return &returnSettlementRegistry{
		byKey:  make(map[ReturnSettlementKey]ReturnSettlementHandler),
		byType: make(map[string]ReturnSettlementHandler),
	}
}

func normalizeReturnSettlementKey(key ReturnSettlementKey) ReturnSettlementKey {
	return ReturnSettlementKey{
		OrderType:      strings.TrimSpace(key.OrderType),
		SettlementMode: strings.TrimSpace(key.SettlementMode),
	}
}

func (r *returnSettlementRegistry) register(handler ReturnSettlementHandler) error {
	if handler == nil {
		return fmt.Errorf("return settlement handler is required")
	}
	key := normalizeReturnSettlementKey(handler.RoutingKey())
	settlementType := strings.TrimSpace(handler.SettlementType())
	if key.OrderType == "" || key.SettlementMode == "" || settlementType == "" {
		return fmt.Errorf("invalid return settlement handler route")
	}
	if settlementType != SettlementRetailCashRefund && settlementType != SettlementWineTicketRestore {
		return fmt.Errorf("invalid return settlement type %q", settlementType)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byKey[key]; exists {
		return fmt.Errorf("return settlement handler already registered for %q/%q", key.OrderType, key.SettlementMode)
	}
	if _, exists := r.byType[settlementType]; exists {
		return fmt.Errorf("return settlement handler already registered for type %q", settlementType)
	}
	r.byKey[key] = handler
	r.byType[settlementType] = handler
	return nil
}

func (r *returnSettlementRegistry) resolve(key ReturnSettlementKey) (ReturnSettlementHandler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	handler, ok := r.byKey[normalizeReturnSettlementKey(key)]
	return handler, ok
}

func (r *returnSettlementRegistry) resolveType(settlementType string) (ReturnSettlementHandler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	handler, ok := r.byType[strings.TrimSpace(settlementType)]
	return handler, ok
}

// WithReturnSettlementHandler 注册应用启动期外部结算实现。
// 重复或格式错误的注册属于编程错误，启动时必须明确失败。
func (s *Service) WithReturnSettlementHandler(handler ReturnSettlementHandler) *Service {
	if s.settlements == nil {
		s.settlements = newReturnSettlementRegistry()
	}
	if err := s.settlements.register(handler); err != nil {
		panic(err)
	}
	return s
}

func (s *Service) settlementHandler(order OrderRef) (ReturnSettlementHandler, error) {
	if s.settlements == nil {
		return nil, problem.Internal("delivery return settlement registry is not initialized")
	}
	handler, ok := s.settlements.resolve(ReturnSettlementKey{
		OrderType: order.OrderType, SettlementMode: order.SettlementMode,
	})
	if !ok {
		return nil, problem.New(
			503,
			"RETURN_SETTLEMENT_HANDLER_NOT_FOUND",
			"Service Unavailable",
			"delivery return settlement handler is not registered",
		)
	}
	return handler, nil
}

func validateReturnSettlementBinding(handler ReturnSettlementHandler, binding ReturnSettlementBinding) error {
	if strings.TrimSpace(binding.SettlementType) != strings.TrimSpace(handler.SettlementType()) {
		return problem.Internal("delivery return settlement binding type mismatch")
	}
	if binding.SettlementType == SettlementRetailCashRefund && binding.SettlementBizID != nil {
		return problem.Internal("retail delivery return cannot bind a business before approval")
	}
	if binding.SettlementType == SettlementWineTicketRestore &&
		(binding.SettlementBizID == nil || *binding.SettlementBizID == 0) {
		return problem.Internal("wine-ticket delivery return business binding is missing")
	}
	return nil
}

func validateReturnSettlementRoute(handler ReturnSettlementHandler, row Return) error {
	if row.SettlementType == nil || strings.TrimSpace(*row.SettlementType) != strings.TrimSpace(handler.SettlementType()) {
		return problem.Internal("delivery return settlement route changed")
	}
	if *row.SettlementType == SettlementWineTicketRestore &&
		(row.SettlementBizID == nil || *row.SettlementBizID == 0) {
		return problem.Internal("wine-ticket delivery return business binding is missing")
	}
	return nil
}

func prepareReturnSettlementReceived(
	ctx context.Context,
	tx *gorm.DB,
	handler ReturnSettlementHandler,
	row Return,
	afterSale AfterSale,
	order OrderRef,
) (ReturnSettlementReceivePlan, error) {
	preparer, ok := handler.(ReturnSettlementReceivePreparer)
	if !ok {
		return nil, nil
	}
	plan, err := preparer.PrepareReceived(ctx, tx, row, afterSale, order)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, problem.Internal("delivery return settlement preparation is incomplete")
	}
	return plan, nil
}

func applyReturnSettlementReceived(
	ctx context.Context,
	tx *gorm.DB,
	handler ReturnSettlementHandler,
	plan ReturnSettlementReceivePlan,
	row Return,
	afterSale AfterSale,
	order OrderRef,
) (bool, error) {
	if _, prepared := handler.(ReturnSettlementReceivePreparer); prepared {
		if plan == nil {
			return false, problem.Internal("delivery return settlement preparation is missing")
		}
		return plan.ApplyReceived(ctx, tx, row, afterSale, order)
	}
	if plan != nil {
		return false, problem.Internal("delivery return settlement preparation route changed")
	}
	return handler.SettleReceived(ctx, tx, row, afterSale, order)
}

type retailCashReturnSettlement struct {
	afterSales *aftersale.Service
	repo       *Repository
}

func (h *retailCashReturnSettlement) RoutingKey() ReturnSettlementKey {
	return ReturnSettlementKey{OrderType: "retail", SettlementMode: "cash"}
}

func (h *retailCashReturnSettlement) SettlementType() string {
	return SettlementRetailCashRefund
}

func (h *retailCashReturnSettlement) InitialBinding(context.Context, *gorm.DB, OrderRef) (ReturnSettlementBinding, error) {
	return ReturnSettlementBinding{SettlementType: SettlementRetailCashRefund}, nil
}

func (h *retailCashReturnSettlement) Approve(
	ctx context.Context,
	tx *gorm.DB,
	row Return,
	_ DeliveryOrder,
	_ OrderRef,
	actorID uint64,
	note string,
) (ReturnSettlementApproval, error) {
	if h.afterSales == nil {
		return ReturnSettlementApproval{}, problem.New(
			503,
			"DELIVERY_RETURN_DEPENDENCY_UNAVAILABLE",
			"Service Unavailable",
			"system after-sale service is unavailable",
		)
	}
	result, err := h.afterSales.CreateSystemDeliveryReturnWithTx(ctx, tx, aftersale.SystemDeliveryReturnRequest{
		DeliveryReturnID: row.ID,
		OrderID:          row.OrderID,
		ApprovedBy:       actorID,
		ReasonCode:       row.ReasonCode,
		Description:      note,
	})
	if err != nil {
		return ReturnSettlementApproval{}, err
	}
	bizID := result.AfterSaleID
	return ReturnSettlementApproval{
		AfterSaleID:     result.AfterSaleID,
		SettlementBizID: &bizID,
		OrderStatus:     "refunding",
		AfterSaleStatus: "processing",
	}, nil
}

func (h *retailCashReturnSettlement) SettleReceived(
	ctx context.Context,
	tx *gorm.DB,
	row Return,
	afterSale AfterSale,
	_ OrderRef,
) (bool, error) {
	status, err := h.repo.RefundStatus(ctx, tx, afterSale.ID)
	if err != nil {
		return false, err
	}
	if status != "succeeded" {
		return false, nil
	}
	return h.repo.ClosureComplete(ctx, tx, afterSale.ID, row.ID)
}
