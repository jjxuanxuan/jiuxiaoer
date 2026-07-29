package redemption

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
)

type IDGenerator interface {
	Next() uint64
}

type serviceCore struct {
	db      *gorm.DB
	idStore *idempotency.Store
	ids     IDGenerator
	now     func() time.Time
	assets  *core.AssetService
	effects *serviceCoreRepository
}

func newServiceCore(db *gorm.DB, ids IDGenerator) *serviceCore {
	return &serviceCore{
		db:      db,
		idStore: idempotency.NewStore(db),
		ids:     ids,
		now:     time.Now,
		assets:  core.NewAssetService(ids),
		effects: &serviceCoreRepository{},
	}
}

func newSettlementCore(db *gorm.DB, ids IDGenerator) *serviceCore {
	return newServiceCore(db, ids)
}

func (c *serviceCore) setClock(now func() time.Time) {
	if now != nil {
		c.now = now
		c.assets.WithClock(now)
	}
}

func (c *serviceCore) nowShanghai() time.Time {
	return coreNowShanghai(c.now)
}

func (c *serviceCore) claimIdempotency(
	ctx context.Context,
	tx *gorm.DB,
	actorType string,
	actorID uint64,
	method string,
	path string,
	key string,
	requestHash string,
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
	path string,
	key string,
	out any,
) error {
	found, err := c.idStore.CachedResponse(ctx, tx, actorType, actorID, path, key, out)
	if err != nil {
		return err
	}
	if !found {
		return problem.Conflict("IDEMPOTENCY_IN_PROGRESS", "request with the same idempotency key is still processing")
	}
	return nil
}

func (c *serviceCore) createAudit(
	ctx context.Context,
	tx *gorm.DB,
	values map[string]any,
) error {
	return c.effects.createAudit(ctx, tx, values)
}

func (c *serviceCore) createCustomerAudit(
	ctx context.Context,
	tx *gorm.DB,
	actorID uint64,
	action string,
	resourceType string,
	resourceID uint64,
	before any,
	after any,
) error {
	return c.createAudit(ctx, tx, map[string]any{
		"id": c.ids.Next(), "actor_type": "customer", "actor_id": actorID,
		"action": action, "resource_type": resourceType, "resource_id": resourceID,
		"before_data": jsonData(before), "after_data": jsonData(after), "result": "success",
		"request_id": requestctx.RequestIDPtr(ctx), "ip_hash": requestctx.IPHashPtr(ctx),
		"user_agent": requestctx.UserAgentPtr(ctx),
	})
}

func (c *serviceCore) createWineTicketOutbox(
	ctx context.Context,
	tx *gorm.DB,
	eventType string,
	aggregateType string,
	aggregateID uint64,
	payload any,
) error {
	return c.effects.createOutbox(ctx, tx, map[string]any{
		"id": c.ids.Next(), "event_id": uuid.NewString(), "event_type": eventType,
		"event_version": 1, "spec_version": "1.0", "aggregate_type": aggregateType,
		"aggregate_id": aggregateID, "producer": "wine-ticket",
		"payload": jsonData(payload), "status": "pending", "retry_count": 0,
		"request_id": requestctx.RequestIDPtr(ctx), "created_at": c.nowShanghai(),
	})
}

func coreNowShanghai(now func() time.Time) time.Time {
	return now().In(shanghaiLocation).Truncate(time.Millisecond)
}
