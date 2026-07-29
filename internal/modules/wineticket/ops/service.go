package ops

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/purchase"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// serviceCore 包含运营命令所需的共享基础设施，
// 不会注册为业务结算处理器。
type serviceCore struct {
	repo     *Repository
	idStore  *idempotency.Store
	ids      *snowflake.Generator
	now      func() time.Time
	instance string
}

func newServiceCore(db *gorm.DB, ids *snowflake.Generator) *serviceCore {
	return &serviceCore{
		repo:    NewRepository(db),
		idStore: idempotency.NewStore(db),
		ids:     ids,
		now:     time.Now,
	}
}

// Service 负责运营查询和异常处置。
// 购买读取投影通过注入提供，使本包只依赖下层购买契约，
// 而无需在构造函数中创建同级服务。
type Service struct {
	*serviceCore
	purchaseProjector           PurchaseProjector
	exceptionResolutionExecutor ExceptionResolutionExecutor
}

func NewService(
	db *gorm.DB,
	ids *snowflake.Generator,
	projector PurchaseProjector,
) *Service {
	return &Service{
		serviceCore:                 newServiceCore(db, ids),
		purchaseProjector:           projector,
		exceptionResolutionExecutor: safeExceptionResolutionExecutor{},
	}
}

func (s *Service) WithClock(now func() time.Time) *Service {
	if now != nil {
		s.now = now
	}
	return s
}

func (s *Service) WithInstance(instance string) *Service {
	s.instance = strings.TrimSpace(instance)
	return s
}

func (s *Service) WithExceptionResolutionExecutor(
	executor ExceptionResolutionExecutor,
) *Service {
	if executor != nil {
		s.exceptionResolutionExecutor = executor
	}
	return s
}

func (s *Service) purchaseDTOs(
	ctx context.Context,
	rows []purchase.PurchaseRecord,
) ([]purchase.PurchaseDTO, error) {
	if s.purchaseProjector == nil {
		return nil, problem.Internal(
			"wine ticket purchase projection is unavailable",
		)
	}
	return s.purchaseProjector.DTOsFromRecords(ctx, rows)
}

func (c *serviceCore) claimIdempotency(
	ctx context.Context,
	tx *gorm.DB,
	actorType string,
	actorID uint64,
	method, path, key, requestHash string,
) (bool, error) {
	return c.idStore.StartAt(
		ctx,
		tx,
		c.ids.Next(),
		actorType,
		actorID,
		method,
		path,
		key,
		requestHash,
		c.now(),
	)
}

func (c *serviceCore) cachedResponse(
	ctx context.Context,
	tx *gorm.DB,
	actorType string,
	actorID uint64,
	path, key string,
	out any,
) error {
	found, err := c.idStore.CachedResponse(
		ctx,
		tx,
		actorType,
		actorID,
		path,
		key,
		out,
	)
	if err != nil {
		return err
	}
	if !found {
		return problem.Conflict(
			"IDEMPOTENCY_IN_PROGRESS",
			"request with the same idempotency key is still processing",
		)
	}
	return nil
}

func (c *serviceCore) nowShanghai() time.Time {
	return c.now().In(shanghaiLocation).Truncate(time.Millisecond)
}

func (c *serviceCore) createWineTicketOutbox(
	ctx context.Context,
	tx *gorm.DB,
	eventType string,
	aggregateType string,
	aggregateID uint64,
	payload any,
) error {
	return c.repo.CreateOutbox(ctx, tx, map[string]any{
		"id":             c.ids.Next(),
		"event_id":       uuid.NewString(),
		"event_type":     eventType,
		"event_version":  1,
		"spec_version":   "1.0",
		"aggregate_type": aggregateType,
		"aggregate_id":   aggregateID,
		"producer":       "wine-ticket",
		"payload":        jsonData(payload),
		"status":         "pending",
		"retry_count":    0,
		"request_id":     requestctx.RequestIDPtr(ctx),
		"created_at":     c.nowShanghai(),
	})
}
