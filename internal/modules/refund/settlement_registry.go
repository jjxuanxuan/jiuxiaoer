package refund

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

const RetailAfterSaleRefundBusiness = "retail_after_sale"

// RefundSettlementCommand 是传给外部退款业务的不可变支付机构观测。
// Lookup 有意不加锁：处理器应用状态前，必须按业务特有锁计划
// 重新锁定并校验每条相关记录。
type RefundSettlementCommand struct {
	Lookup         Row
	State          State
	ClaimedVersion *uint32
}

// RefundSettlementResult 区分可持久化的支付机构拒绝与事务失败。
// Reject 非空且处理器未返回错误时，LockAndApply 写入的全部状态会被提交，
// 回调也会被持久标记为失败。
type RefundSettlementResult struct {
	Reject            error
	CallbackErrorCode string
}

// RefundSettlementHandler 负责一个外部业务的完整锁计划和状态应用。
// LockAndApply 在退款服务事务中运行。
//
// 不同酒票退款类型需要不同锁顺序，因此公共退款模块不得强制使用
// 全局统一的业务和退款锁顺序。实现必须：
//   - 锁定并重新读取公共退款记录；
//   - 校验其 ID、biz_type、biz_id、支付 ID 和已认领版本；
//   - 建立 PRD 为该业务规定的锁顺序；
//   - 校验支付机构事实，并原子应用公共与业务台账；
//   - 确保 SUCCESS、终态和异常观测均具备幂等性。
type RefundSettlementHandler interface {
	BusinessType() string
	LockAndApply(context.Context, *gorm.DB, RefundSettlementCommand) (RefundSettlementResult, error)
}

// RefundSettlementFailureCommand 是可信本地观测，表示支付机构调用
// 未产生可用退款状态。它与 RefundSettlementCommand 分离：
// 伪造支付机构 UNKNOWN/CLOSED 状态会模糊传输事实，
// 并可能错误释放业务冻结。
type RefundSettlementFailureCommand struct {
	Lookup         Row
	ClaimedVersion uint32
	Code           string
	Detail         string
	Retryable      bool
	OccurredAt     time.Time
	NextRetryAt    *time.Time
}

// RefundSettlementFailureHandler 允许外部业务在后台任务遇到传输错误
// 或永久性提交、查询错误时保持自身锁顺序。
// 实现必须重新锁定并校验 Lookup 和 ClaimedVersion。
// 可重试失败会保留全部业务冻结；永久失败会把业务转入异常状态，
// 但仍不得释放权益。
//
// 外部处理器必须实现本接口，才能使用支付机构错误路径；
// 否则公共服务会失败关闭。
type RefundSettlementFailureHandler interface {
	LockAndApplyFailure(context.Context, *gorm.DB, RefundSettlementFailureCommand) error
}

type settlementRegistry struct {
	mu       sync.RWMutex
	handlers map[string]RefundSettlementHandler
}

func newSettlementRegistry() *settlementRegistry {
	return &settlementRegistry{handlers: make(map[string]RefundSettlementHandler)}
}

func (r *settlementRegistry) register(handler RefundSettlementHandler) error {
	if handler == nil {
		return fmt.Errorf("refund settlement handler is required")
	}
	bizType := strings.TrimSpace(handler.BusinessType())
	if bizType == "" || bizType == RetailAfterSaleRefundBusiness {
		return fmt.Errorf("invalid external refund business type %q", bizType)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.handlers[bizType]; exists {
		return fmt.Errorf("refund settlement handler already registered for %q", bizType)
	}
	r.handlers[bizType] = handler
	return nil
}

func (r *settlementRegistry) resolve(bizType string) (RefundSettlementHandler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	handler, ok := r.handlers[bizType]
	return handler, ok
}

// WithRefundSettlementHandler 注册应用启动期业务处理器。
// 无效或重复注册属于编程错误，启动时必须明确失败。
func (s *Service) WithRefundSettlementHandler(handler RefundSettlementHandler) *Service {
	if s.settlements == nil {
		s.settlements = newSettlementRegistry()
	}
	if err := s.settlements.register(handler); err != nil {
		panic(err)
	}
	return s
}

// RefundBusiness 返回标准化的不可变路由键。
// 发布期间缺少 biz_type/biz_id 的旧记录仍按零售售后退款处理。
func RefundBusiness(row Row) (string, uint64) {
	if row.BizType == nil || strings.TrimSpace(*row.BizType) == "" {
		if row.AfterSaleID == nil {
			return RetailAfterSaleRefundBusiness, 0
		}
		return RetailAfterSaleRefundBusiness, *row.AfterSaleID
	}
	bizID := uint64(0)
	if row.BizID != nil {
		bizID = *row.BizID
	}
	return strings.TrimSpace(*row.BizType), bizID
}

// SameRefundSettlementRoute 允许外部处理器在建立完整锁计划后，
// 校验此前无锁读取的路由快照。
func SameRefundSettlementRoute(lookup, locked Row) bool {
	lookupType, lookupID := RefundBusiness(lookup)
	lockedType, lockedID := RefundBusiness(locked)
	return lookup.ID == locked.ID &&
		lookup.RefundNo == locked.RefundNo &&
		lookup.PaymentID == locked.PaymentID &&
		lookupType == lockedType &&
		lookupID == lockedID
}

func retailRefundLinks(row Row) (uint64, uint64, error) {
	bizType, bizID := RefundBusiness(row)
	if bizType != RetailAfterSaleRefundBusiness ||
		row.AfterSaleID == nil || *row.AfterSaleID == 0 ||
		row.OrderID == nil || *row.OrderID == 0 ||
		bizID != *row.AfterSaleID {
		return 0, 0, settlementRoutingFailure("REFUND_BUSINESS_LINK_INVALID", "retail refund business registry is incomplete")
	}
	return *row.AfterSaleID, *row.OrderID, nil
}

func requireRetailAdminRefund(row Row) error {
	bizType, _ := RefundBusiness(row)
	if bizType != RetailAfterSaleRefundBusiness {
		return problem.Conflict(
			"REFUND_TYPED_ADMIN_ACTION_REQUIRED",
			"non-retail refunds must be handled through their typed business exception workflow",
		)
	}
	if _, _, err := retailRefundLinks(row); err != nil {
		return problem.Conflict(
			"REFUND_BUSINESS_LINK_INVALID",
			"retail refund business registry is incomplete",
		)
	}
	return nil
}

func (s *Service) externalSettlementHandler(row Row) (RefundSettlementHandler, error) {
	bizType, bizID := RefundBusiness(row)
	if bizType == RetailAfterSaleRefundBusiness {
		return nil, nil
	}
	if bizID == 0 {
		return nil, settlementRoutingFailure("REFUND_BUSINESS_ID_MISSING", "refund business registry is incomplete")
	}
	if s.settlements == nil {
		return nil, settlementRoutingFailure("REFUND_SETTLEMENT_HANDLER_NOT_FOUND", "refund settlement registry is not initialized")
	}
	handler, ok := s.settlements.resolve(bizType)
	if !ok {
		return nil, settlementRoutingFailure("REFUND_SETTLEMENT_HANDLER_NOT_FOUND", "refund settlement handler is not registered")
	}
	return handler, nil
}

type settlementRoutingError struct {
	callbackCode string
	cause        error
}

func (e *settlementRoutingError) Error() string { return e.cause.Error() }
func (e *settlementRoutingError) Unwrap() error { return e.cause }

func settlementRoutingFailure(code, detail string) error {
	return &settlementRoutingError{
		callbackCode: code,
		cause:        problem.New(500, code, "Internal Error", detail),
	}
}

func callbackRoutingFailure(err error) (string, bool) {
	var routeErr *settlementRoutingError
	if !errors.As(err, &routeErr) {
		return "", false
	}
	return routeErr.callbackCode, true
}
