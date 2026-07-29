package dispatch

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

type AssignmentSettlementKey struct {
	OrderType      string
	SettlementMode string
}

// AssignmentSettlementFact 在调度分配事务内产生，
// 此时 delivery_order 和 order 均已锁定，骑手资格校验也已全部通过。
type AssignmentSettlementFact struct {
	Delivery       DeliveryOrder
	OrderID        uint64
	OrderType      string
	SettlementMode string
	PayStatus      string
	PaidAmount     int64
	OccurredAt     time.Time
	ActorType      string
	ActorID        uint64
}

type AssignmentSettlementHandler interface {
	AssignmentRoutingKey() AssignmentSettlementKey
	ApplyAssignment(context.Context, *gorm.DB, AssignmentSettlementFact) error
}

type assignmentSettlementRegistry struct {
	mu       sync.RWMutex
	handlers map[AssignmentSettlementKey]AssignmentSettlementHandler
}

func newAssignmentSettlementRegistry() *assignmentSettlementRegistry {
	registry := &assignmentSettlementRegistry{
		handlers: make(map[AssignmentSettlementKey]AssignmentSettlementHandler),
	}
	if err := registry.register(retailCashAssignmentSettlement{}); err != nil {
		panic(err)
	}
	return registry
}

func normalizeAssignmentSettlementKey(key AssignmentSettlementKey) AssignmentSettlementKey {
	key.OrderType = strings.TrimSpace(key.OrderType)
	key.SettlementMode = strings.TrimSpace(key.SettlementMode)
	if key.OrderType == "" {
		key.OrderType = "retail"
	}
	if key.SettlementMode == "" {
		key.SettlementMode = "cash"
	}
	return key
}

func (r *assignmentSettlementRegistry) register(handler AssignmentSettlementHandler) error {
	if handler == nil {
		return fmt.Errorf("assignment settlement handler is required")
	}
	key := normalizeAssignmentSettlementKey(handler.AssignmentRoutingKey())
	if key.OrderType == "" || key.SettlementMode == "" {
		return fmt.Errorf("invalid assignment settlement handler route")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.handlers[key]; exists {
		return fmt.Errorf(
			"assignment settlement handler already registered for %q/%q",
			key.OrderType,
			key.SettlementMode,
		)
	}
	r.handlers[key] = handler
	return nil
}

func (r *assignmentSettlementRegistry) resolve(
	key AssignmentSettlementKey,
) (AssignmentSettlementHandler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	handler, ok := r.handlers[normalizeAssignmentSettlementKey(key)]
	return handler, ok
}

func (s *Service) WithAssignmentSettlementHandler(
	handler AssignmentSettlementHandler,
) *Service {
	if s.assignmentSettlements == nil {
		s.assignmentSettlements = newAssignmentSettlementRegistry()
	}
	if err := s.assignmentSettlements.register(handler); err != nil {
		panic(err)
	}
	return s
}

func (s *Service) applyAssignmentSettlement(
	ctx context.Context,
	tx *gorm.DB,
	delivery DeliveryOrder,
	order domainOrder,
	occurredAt time.Time,
	actorType string,
	actorID uint64,
) error {
	if s.assignmentSettlements == nil {
		return problem.Internal("assignment settlement registry is not initialized")
	}
	handler, ok := s.assignmentSettlements.resolve(AssignmentSettlementKey{
		OrderType:      order.OrderType,
		SettlementMode: order.SettlementMode,
	})
	if !ok {
		return problem.New(
			503,
			"FULFILLMENT_SETTLEMENT_HANDLER_NOT_FOUND",
			"Service Unavailable",
			"fulfillment settlement handler is not registered",
		)
	}
	return handler.ApplyAssignment(ctx, tx, AssignmentSettlementFact{
		Delivery:       delivery,
		OrderID:        order.ID,
		OrderType:      order.OrderType,
		SettlementMode: order.SettlementMode,
		PayStatus:      order.PayStatus,
		PaidAmount:     order.PaidAmount,
		OccurredAt:     occurredAt,
		ActorType:      actorType,
		ActorID:        actorID,
	})
}

type retailCashAssignmentSettlement struct{}

func (retailCashAssignmentSettlement) AssignmentRoutingKey() AssignmentSettlementKey {
	return AssignmentSettlementKey{OrderType: "retail", SettlementMode: "cash"}
}

func (retailCashAssignmentSettlement) ApplyAssignment(
	context.Context,
	*gorm.DB,
	AssignmentSettlementFact,
) error {
	return nil
}
