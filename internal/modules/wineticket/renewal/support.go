package renewal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/core"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

const maxRequestBodyBytes = 64 << 10

type serviceCore struct {
	db          *gorm.DB
	idStore     *idempotency.Store
	ids         *snowflake.Generator
	wechatAppID string
	now         func() time.Time
	effects     *serviceCoreRepository
}

func newServiceCore(db *gorm.DB, ids *snowflake.Generator) *serviceCore {
	return &serviceCore{
		db:      db,
		idStore: idempotency.NewStore(db),
		ids:     ids,
		now:     time.Now,
		effects: &serviceCoreRepository{},
	}
}

func (c *serviceCore) setClock(now func() time.Time) {
	if now != nil {
		c.now = now
	}
}

func (c *serviceCore) claimIdempotencyWithID(
	ctx context.Context,
	tx *gorm.DB,
	claimID uint64,
	actorType string,
	actorID uint64,
	method, path, key, requestHash string,
) (bool, error) {
	return c.idStore.StartAt(
		ctx,
		tx,
		claimID,
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

func (c *serviceCore) releaseCustomerIdempotencyOwned(
	ctx context.Context,
	claimID uint64,
	actorType string,
	actorID uint64,
	path, key string,
) {
	cleanupCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		2*time.Second,
	)
	defer cancel()
	_ = c.db.WithContext(cleanupCtx).Transaction(func(tx *gorm.DB) error {
		return c.idStore.FailOwned(
			cleanupCtx,
			tx,
			claimID,
			actorType,
			actorID,
			path,
			key,
		)
	})
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
	action, resourceType string,
	resourceID uint64,
	before, after any,
) error {
	return c.createAudit(ctx, tx, map[string]any{
		"id":            c.ids.Next(),
		"actor_type":    "customer",
		"actor_id":      actorID,
		"action":        action,
		"resource_type": resourceType,
		"resource_id":   resourceID,
		"before_data":   jsonData(before),
		"after_data":    jsonData(after),
		"result":        "success",
		"request_id":    requestctx.RequestIDPtr(ctx),
		"ip_hash":       requestctx.IPHashPtr(ctx),
		"user_agent":    requestctx.UserAgentPtr(ctx),
	})
}

func (c *serviceCore) createWineTicketOutbox(
	ctx context.Context,
	tx *gorm.DB,
	eventType, aggregateType string,
	aggregateID uint64,
	payload any,
) error {
	return c.effects.createOutbox(ctx, tx, map[string]any{
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
		"created_at":     core.NowShanghai(c.now),
	})
}

func customerIDWithPermission(
	claims *auth.Claims,
	permission string,
) (uint64, error) {
	if claims == nil || claims.AccountType != "customer" {
		return 0, problem.Unauthorized(
			"AUTH_UNAUTHORIZED",
			"customer authentication required",
		)
	}
	customerID, err := strconv.ParseUint(claims.CustomerID, 10, 64)
	if err != nil || customerID == 0 {
		return 0, problem.Unauthorized(
			"AUTH_UNAUTHORIZED",
			"invalid customer identity",
		)
	}
	for _, granted := range claims.Permissions {
		if granted == permission {
			return customerID, nil
		}
	}
	return 0, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
}

func customerClaims(c *gin.Context) (*auth.Claims, bool) {
	c.Header("Cache-Control", "private, no-store")
	claims, ok := auth.ClaimsFromContext(c)
	if !ok || claims.AccountType != "customer" {
		response.Error(c, problem.Unauthorized(
			"AUTH_UNAUTHORIZED",
			"customer authentication required",
		))
		return nil, false
	}
	return claims, true
}

func rejectUnknownPackageQuery(c *gin.Context, allowed ...string) error {
	allow := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allow[key] = struct{}{}
	}
	for key := range c.Request.URL.Query() {
		if _, ok := allow[key]; !ok {
			return problem.InvalidArgument(
				"VALIDATION_INVALID_QUERY",
				"unknown query parameter: "+key,
			)
		}
	}
	return nil
}

func decodeStrictJSON(c *gin.Context, out any) error {
	reader := http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodyBytes)
	payload, err := io.ReadAll(reader)
	if err != nil {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"request body is too large or unreadable",
		)
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"request body must be a JSON object",
		)
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return problem.InvalidArgument("VALIDATION_FAILED", safeJSONError(err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return problem.InvalidArgument(
			"VALIDATION_FAILED",
			"request body must contain exactly one JSON object",
		)
	}
	return nil
}

func safeJSONError(err error) string {
	message := err.Error()
	if strings.Contains(message, "unknown field") ||
		strings.Contains(message, "cannot unmarshal") ||
		strings.Contains(message, "invalid character") ||
		strings.Contains(message, "unexpected EOF") {
		return message
	}
	return "invalid JSON request body"
}

func idString(value uint64) string { return core.IDString(value) }

func formatShanghai(value time.Time) string {
	return core.FormatShanghai(value)
}

func validateBusinessNo(value, field string) error {
	return core.ValidateBusinessNo(strings.TrimSpace(value), field)
}

func cloneJSON(value datatypes.JSON) datatypes.JSON {
	return core.CloneJSON(value)
}

func jsonData(value any) datatypes.JSON { return core.JSONData(value) }

func decodePolicyJSON(raw []byte, out any, required ...string) error {
	return core.DecodePolicyJSON(raw, out, required...)
}

func renewalPolicySummary(policy core.RenewalPolicy) string {
	return core.RenewalPolicySummary(policy)
}

func decodePaymentParameters(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var parameters map[string]any
	if err := json.Unmarshal(raw, &parameters); err != nil ||
		len(parameters) == 0 {
		return nil
	}
	return parameters
}

func stringPointer(value string) *string { return &value }

func timePtr(value time.Time) *time.Time { return &value }
