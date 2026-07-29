package delivery

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

const (
	FulfillmentAssigned  = "assigned"
	FulfillmentPickedUp  = "picked_up"
	FulfillmentDelivered = "delivered"
)

type FulfillmentSettlementKey struct {
	OrderType      string
	SettlementMode string
}

// FulfillmentSettlementFact 仅在调用方事务已经锁定 delivery_order 和 order 后传入。
// 处理器可以获取自身业务锁，但不得提交或开启其他事务。
type FulfillmentSettlementFact struct {
	Action     string
	Delivery   DeliveryOrder
	Order      Order
	OccurredAt time.Time
	ActorType  string
	ActorID    uint64
}

type FulfillmentSettlementHandler interface {
	RoutingKey() FulfillmentSettlementKey
	ApplyFulfillment(context.Context, *gorm.DB, FulfillmentSettlementFact) error
}

type fulfillmentSettlementRegistry struct {
	mu       sync.RWMutex
	handlers map[FulfillmentSettlementKey]FulfillmentSettlementHandler
}

func newFulfillmentSettlementRegistry() *fulfillmentSettlementRegistry {
	registry := &fulfillmentSettlementRegistry{
		handlers: make(map[FulfillmentSettlementKey]FulfillmentSettlementHandler),
	}
	if err := registry.register(retailCashFulfillmentSettlement{}); err != nil {
		panic(err)
	}
	return registry
}

func normalizeFulfillmentSettlementKey(key FulfillmentSettlementKey) FulfillmentSettlementKey {
	key.OrderType = strings.TrimSpace(key.OrderType)
	key.SettlementMode = strings.TrimSpace(key.SettlementMode)
	// 区分字段迁移前创建的记录属于普通零售订单。
	// 在此统一处理可以保持其原有行为。
	if key.OrderType == "" {
		key.OrderType = "retail"
	}
	if key.SettlementMode == "" {
		key.SettlementMode = "cash"
	}
	return key
}

func (r *fulfillmentSettlementRegistry) register(handler FulfillmentSettlementHandler) error {
	if handler == nil {
		return fmt.Errorf("fulfillment settlement handler is required")
	}
	key := normalizeFulfillmentSettlementKey(handler.RoutingKey())
	if key.OrderType == "" || key.SettlementMode == "" {
		return fmt.Errorf("invalid fulfillment settlement handler route")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.handlers[key]; exists {
		return fmt.Errorf(
			"fulfillment settlement handler already registered for %q/%q",
			key.OrderType,
			key.SettlementMode,
		)
	}
	r.handlers[key] = handler
	return nil
}

func (r *fulfillmentSettlementRegistry) resolve(
	key FulfillmentSettlementKey,
) (FulfillmentSettlementHandler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	handler, ok := r.handlers[normalizeFulfillmentSettlementKey(key)]
	return handler, ok
}

// WithFulfillmentSettlementHandler 注册应用启动期业务处理器。
// 重复或格式错误的注册属于装配错误，必须快速失败。
func (s *Service) WithFulfillmentSettlementHandler(
	handler FulfillmentSettlementHandler,
) *Service {
	if s.settlements == nil {
		s.settlements = newFulfillmentSettlementRegistry()
	}
	if err := s.settlements.register(handler); err != nil {
		panic(err)
	}
	return s
}

func (s *Service) applyFulfillmentSettlement(
	ctx context.Context,
	tx *gorm.DB,
	action string,
	deliveryRow DeliveryOrder,
	orderRow Order,
	occurredAt time.Time,
	actorType string,
	actorID uint64,
) error {
	if s.settlements == nil {
		return problem.Internal("fulfillment settlement registry is not initialized")
	}
	handler, ok := s.settlements.resolve(FulfillmentSettlementKey{
		OrderType:      orderRow.OrderType,
		SettlementMode: orderRow.SettlementMode,
	})
	if !ok {
		return problem.New(
			503,
			"FULFILLMENT_SETTLEMENT_HANDLER_NOT_FOUND",
			"Service Unavailable",
			"fulfillment settlement handler is not registered",
		)
	}
	return handler.ApplyFulfillment(ctx, tx, FulfillmentSettlementFact{
		Action:     action,
		Delivery:   deliveryRow,
		Order:      orderRow,
		OccurredAt: occurredAt,
		ActorType:  actorType,
		ActorID:    actorID,
	})
}

type retailCashFulfillmentSettlement struct{}

func (retailCashFulfillmentSettlement) RoutingKey() FulfillmentSettlementKey {
	return FulfillmentSettlementKey{OrderType: "retail", SettlementMode: "cash"}
}

func (retailCashFulfillmentSettlement) ApplyFulfillment(
	context.Context,
	*gorm.DB,
	FulfillmentSettlementFact,
) error {
	return nil
}
