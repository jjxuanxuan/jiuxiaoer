package ops

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/integrity"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
)

func (s *Service) ListAdminExceptions(
	ctx context.Context,
	claims *auth.Claims,
	query pagination.Query,
	filter ExceptionAdminFilter,
) ([]ExceptionAdminDTO, string, error) {
	if _, err := adminIDWithPermission(claims, "wine_ticket_exception:list"); err != nil {
		return nil, "", err
	}
	filter.Status = strings.TrimSpace(filter.Status)
	filter.Severity = strings.TrimSpace(filter.Severity)
	if filter.Status != "" && !validExceptionStatus(filter.Status) {
		return nil, "", problem.InvalidArgument(
			"VALIDATION_FAILED",
			"invalid exception status",
		)
	}
	if filter.Severity != "" && !validExceptionSeverity(filter.Severity) {
		return nil, "", problem.InvalidArgument(
			"VALIDATION_FAILED",
			"invalid exception severity",
		)
	}
	rows, err := s.repo.ListAdminExceptions(ctx, query, filter)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > query.PageSize {
		rows = rows[:query.PageSize]
		next = pagination.NextPageTokenWithCursor(
			query,
			idString(rows[len(rows)-1].ID),
		)
	}
	items := make([]ExceptionAdminDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, exceptionAdminDTO(row))
	}
	return items, next, nil
}

func (s *Service) AdminException(
	ctx context.Context,
	claims *auth.Claims,
	exceptionNo string,
) (ExceptionAdminDTO, error) {
	if _, err := adminIDWithPermission(claims, "wine_ticket_exception:view"); err != nil {
		return ExceptionAdminDTO{}, err
	}
	exceptionNo = strings.TrimSpace(exceptionNo)
	if err := validateBusinessNo(exceptionNo, "exception_no"); err != nil {
		return ExceptionAdminDTO{}, err
	}
	row, err := s.repo.AdminExceptionByNo(ctx, nil, exceptionNo)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ExceptionAdminDTO{}, exceptionNotFound()
	}
	if err != nil {
		return ExceptionAdminDTO{}, err
	}
	return exceptionAdminDTO(row), nil
}

func (s *Service) ResolveException(
	ctx context.Context,
	claims *auth.Claims,
	method string,
	path string,
	key string,
	exceptionNo string,
	request ExceptionResolutionRequest,
) (response ExceptionAdminDTO, resultErr error) {
	actorID, err := adminIDWithPermission(
		claims,
		"wine_ticket_exception:resolve",
	)
	if err != nil {
		return ExceptionAdminDTO{}, err
	}
	exceptionNo = strings.TrimSpace(exceptionNo)
	if err := validateBusinessNo(exceptionNo, "exception_no"); err != nil {
		return ExceptionAdminDTO{}, err
	}
	request, err = normalizeExceptionResolution(request)
	if err != nil {
		return ExceptionAdminDTO{}, err
	}
	requestHash := idempotency.ResourceRequestHash(
		"wine_ticket.exception.resolve",
		exceptionNo,
		request,
	)

	resultErr = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		allowed, authErr := auth.ActiveAdminHasPermission(
			ctx,
			tx,
			actorID,
			"wine_ticket_exception:resolve",
		)
		if authErr != nil {
			return authErr
		}
		if !allowed {
			return problem.Forbidden(
				"PERM_FORBIDDEN",
				"administrator is no longer active or authorized",
			)
		}
		started, startErr := s.claimIdempotency(
			ctx,
			tx,
			claims.AccountType,
			actorID,
			method,
			path,
			key,
			requestHash,
		)
		if startErr != nil {
			return startErr
		}
		if !started {
			return s.cachedResponse(
				ctx,
				tx,
				claims.AccountType,
				actorID,
				path,
				key,
				&response,
			)
		}
		row, lockErr := s.repo.AdminExceptionByNoLocked(ctx, tx, exceptionNo)
		if errors.Is(lockErr, gorm.ErrRecordNotFound) {
			return exceptionNotFound()
		}
		if lockErr != nil {
			return lockErr
		}
		if row.Version != request.ExpectedVersion {
			return exceptionVersionConflict()
		}
		if row.Status == ExceptionStatusResolved {
			return problem.Conflict(
				"WT_CONCURRENT_MODIFICATION",
				"resolved exception cannot receive another resolution",
			)
		}
		if s.exceptionResolutionExecutor == nil {
			return problem.Internal(
				"wine ticket exception executor is unavailable",
			)
		}

		before := exceptionAdminDTO(row)
		now := s.nowShanghai()
		resolutionResult, executeErr := s.exceptionResolutionExecutor.
			ExecuteWineTicketExceptionResolution(
				ctx,
				tx,
				ExceptionResolutionCommand{
					Exception:      row,
					Action:         request.ResolutionAction,
					Reason:         request.Reason,
					ReviewTicketNo: request.ReviewTicketNo,
					ActorID:        actorID,
					ResolutionTime: now,
				},
			)
		if executeErr != nil {
			return executeErr
		}
		nextVersion := row.Version + 1
		updated, updateErr := s.repo.ResolveException(
			ctx,
			tx,
			row,
			request.ResolutionAction,
			request.Reason,
			request.ReviewTicketNo,
			actorID,
			resolutionResult,
			now,
		)
		if updateErr != nil {
			return updateErr
		}
		if !updated {
			return exceptionVersionConflict()
		}
		row.Status = ExceptionStatusResolved
		row.ProposedAction = stringPointer(request.ResolutionAction)
		row.ProposedReason = stringPointer(request.Reason)
		row.ReviewTicketNo = stringPointer(request.ReviewTicketNo)
		row.ProposedBy = uint64Ptr(actorID)
		row.ProposedAt = &now
		row.ReviewDecision = nil
		row.ReviewNote = nil
		row.ReviewedBy = nil
		row.ReviewedAt = nil
		row.ResolutionResult = resolutionResult
		row.ResolvedAt = &now
		row.Version = nextVersion
		row.UpdatedAt = now
		response = exceptionAdminDTO(row)
		requestID := requestctx.RequestID(ctx)
		correlationID := requestID
		if response.CorrelationID != nil &&
			strings.TrimSpace(*response.CorrelationID) != "" {
			correlationID = strings.TrimSpace(*response.CorrelationID)
		}
		if auditErr := s.createExceptionAudit(
			ctx,
			tx,
			actorID,
			"wine_ticket_exception.resolution_executed",
			row.ID,
			before,
			response,
			map[string]any{
				"permission":             "wine_ticket_exception:resolve",
				"request_id":             requestID,
				"correlation_id":         correlationID,
				"request_correlation_id": requestID,
				"idempotency_key_hash":   idempotency.KeyHash(key),
				"service_instance":       nonEmptyExceptionAuditInstance(s.instance),
			},
		); auditErr != nil {
			return auditErr
		}
		return s.idStore.Succeed(
			ctx,
			tx,
			claims.AccountType,
			actorID,
			path,
			key,
			response,
		)
	})
	return response, resultErr
}

func exceptionAdminDTO(row integrity.Exception) ExceptionAdminDTO {
	item := ExceptionAdminDTO{
		ExceptionNo:      row.ExceptionNo,
		ExceptionType:    row.ExceptionType,
		BizType:          row.BizType,
		BizID:            idString(row.BizID),
		BizNo:            row.BizNo,
		SourceType:       row.SourceType,
		CorrelationID:    row.CorrelationID,
		Severity:         row.Severity,
		Status:           row.Status,
		ExpectedSnapshot: safeExceptionSnapshot(row.ExpectedSnapshot),
		ActualSnapshot:   safeExceptionSnapshot(row.ActualSnapshot),
		ProposedAction:   row.ProposedAction,
		ProposedReason:   row.ProposedReason,
		ReviewTicketNo:   row.ReviewTicketNo,
		ReviewDecision:   row.ReviewDecision,
		ReviewNote:       row.ReviewNote,
		ResolutionResult: safeExceptionSnapshot(row.ResolutionResult),
		OccurrenceCount:  row.OccurrenceCount,
		FirstDetectedAt:  formatShanghai(row.FirstDetectedAt),
		LastDetectedAt:   formatShanghai(row.LastDetectedAt),
		Version:          row.Version,
		CreatedAt:        formatShanghai(row.CreatedAt),
		UpdatedAt:        formatShanghai(row.UpdatedAt),
	}
	if row.IssuerMerchantID != nil {
		value := idString(*row.IssuerMerchantID)
		item.IssuerMerchantID = &value
	}
	if row.SourceID != nil {
		value := idString(*row.SourceID)
		item.SourceID = &value
	}
	if row.ProposedBy != nil {
		value := idString(*row.ProposedBy)
		item.ProposedBy = &value
	}
	if row.ProposedAt != nil {
		value := formatShanghai(*row.ProposedAt)
		item.ProposedAt = &value
	}
	if row.ReviewedBy != nil {
		value := idString(*row.ReviewedBy)
		item.ReviewedBy = &value
	}
	if row.ReviewedAt != nil {
		value := formatShanghai(*row.ReviewedAt)
		item.ReviewedAt = &value
	}
	if row.ResolvedAt != nil {
		value := formatShanghai(*row.ResolvedAt)
		item.ResolvedAt = &value
	}
	return item
}

func safeExceptionSnapshot(raw datatypes.JSON) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if len(raw) > 64*1024 {
		return json.RawMessage(`{"redacted":"snapshot_too_large"}`)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return json.RawMessage(`{"redacted":"invalid_snapshot"}`)
	}
	value = redactExceptionValue(value, 0)
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{"redacted":"invalid_snapshot"}`)
	}
	return json.RawMessage(encoded)
}

func redactExceptionValue(value any, depth int) any {
	if depth >= 8 {
		return "[REDACTED_DEPTH]"
	}
	switch typed := value.(type) {
	case map[string]any:
		safe := make(map[string]any, len(typed))
		for key, child := range typed {
			if exceptionSnapshotSensitiveKey(key) {
				safe[key] = "[REDACTED]"
				continue
			}
			safe[key] = redactExceptionValue(child, depth+1)
		}
		return safe
	case []any:
		limit := len(typed)
		if limit > 100 {
			limit = 100
		}
		safe := make([]any, 0, limit)
		for _, child := range typed[:limit] {
			safe = append(safe, redactExceptionValue(child, depth+1))
		}
		return safe
	default:
		return value
	}
}

func exceptionSnapshotSensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	for _, fragment := range []string{
		"token",
		"openid",
		"open_id",
		"phone",
		"mobile",
		"address",
		"realname",
		"real_name",
		"id_card",
		"certificate",
		"client_payload",
		"raw_payload",
		"provider_payload",
		"ciphertext",
		"private_key",
		"secret",
		"password",
	} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}

func (s *Service) createExceptionAudit(
	ctx context.Context,
	tx *gorm.DB,
	actorID uint64,
	action string,
	resourceID uint64,
	before ExceptionAdminDTO,
	after ExceptionAdminDTO,
	metadata map[string]any,
) error {
	beforeStatus := before.Status
	afterStatus := after.Status
	values := map[string]any{
		"id":            s.ids.Next(),
		"event_id":      uuid.NewString(),
		"actor_type":    "admin",
		"actor_id":      actorID,
		"action":        action,
		"resource_type": "wine_ticket_exception",
		"resource_id":   resourceID,
		"before_data":   jsonData(before),
		"after_data":    exceptionAuditData(after, metadata),
		"result":        "success",
		"before_status": beforeStatus,
		"after_status":  afterStatus,
		"version":       uint64(after.Version),
		"request_id":    requestctx.RequestIDPtr(ctx),
		"ip_hash":       requestctx.IPHashPtr(ctx),
		"user_agent":    requestctx.UserAgentPtr(ctx),
		"created_at":    s.nowShanghai(),
	}
	if accountID := requestctx.AccountID(ctx); accountID != 0 {
		values["account_id"] = accountID
	}
	return s.repo.CreateAudit(ctx, tx, values)
}

func exceptionAuditData(after ExceptionAdminDTO, metadata map[string]any) datatypes.JSON {
	if len(metadata) == 0 {
		return jsonData(after)
	}
	payload := map[string]any{}
	raw := jsonData(after)
	if err := json.Unmarshal(raw, &payload); err != nil {
		payload = map[string]any{}
	}
	for key, value := range metadata {
		payload[key] = value
	}
	return jsonData(payload)
}

func nonEmptyExceptionAuditInstance(instance string) string {
	if instance = strings.TrimSpace(instance); instance != "" {
		return instance
	}
	return "unknown"
}

func exceptionNotFound() error {
	return problem.NotFound(
		"WT_EXCEPTION_NOT_FOUND",
		"wine ticket exception not found",
	)
}

func exceptionVersionConflict() error {
	return problem.Conflict(
		"WT_CONCURRENT_MODIFICATION",
		"wine ticket exception changed concurrently",
	)
}
