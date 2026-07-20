package refund

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/paygateway"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
)

const storedRepairQueryTimeout = 8 * time.Second

type RepairResult struct {
	RefundNo       string `json:"refund_no"`
	BeforeStatus   string `json:"before_status"`
	AfterStatus    string `json:"after_status"`
	ProviderStatus string `json:"provider_status,omitempty"`
	ProviderCode   string `json:"provider_code,omitempty"`
	ProviderReqID  string `json:"provider_request_id,omitempty"`
	Action         string `json:"action"`
	Result         string `json:"result"`
	Error          string `json:"error,omitempty"`
}

type RepairCandidatePage struct {
	Items       []DTO  `json:"items"`
	NextAfterID string `json:"next_after_id,omitempty"`
}

// RepairCandidates performs the read-only local scan required before a stored
// refund repair. It deliberately does not call or mutate the provider.
func (s *Service) RepairCandidates(ctx context.Context, claims *auth.Claims, afterID uint64, size int) (RepairCandidatePage, error) {
	if _, err := adminPermission(claims, "refund:view"); err != nil {
		return RepairCandidatePage{}, err
	}
	if size < 1 || size > 100 {
		return RepairCandidatePage{}, problem.InvalidArgument("VALIDATION_FAILED", "page_size must be between 1 and 100")
	}
	rows, err := s.repo.RepairCandidates(ctx, afterID, size)
	if err != nil {
		return RepairCandidatePage{}, err
	}
	page := RepairCandidatePage{Items: make([]DTO, 0, min(size, len(rows)))}
	if len(rows) > size {
		page.NextAfterID = id(rows[size-1].ID)
		rows = rows[:size]
	}
	for _, row := range rows {
		page.Items = append(page.Items, dto(row))
	}
	return page, nil
}

// RepairStored previews or applies one controlled stored-refund repair. The
// provider is always queried first. It never submits a refund or accepts an
// administrator-supplied success state.
func (s *Service) RepairStored(ctx context.Context, claims *auth.Claims, method, path, key, refundNo string, apply bool) (RepairResult, error) {
	permission := "refund:view"
	if apply {
		permission = "refund:exception"
	}
	actorID, err := adminPermission(claims, permission)
	if err != nil {
		return RepairResult{}, err
	}
	if s.provider == nil {
		return RepairResult{}, problem.New(http.StatusServiceUnavailable, "REFUND_PROVIDER_UNAVAILABLE", "Service Unavailable", "refund provider is unavailable")
	}
	row, err := s.repo.ByNo(ctx, refundNo)
	if isNotFound(err) {
		return RepairResult{}, problem.NotFound("REFUND_NOT_FOUND", "refund not found")
	}
	if err != nil {
		return RepairResult{}, err
	}
	if row.Provider != s.provider.Code() {
		return RepairResult{}, problem.Conflict("REFUND_PROVIDER_MISMATCH", "refund provider does not match configured provider")
	}
	if !storedRepairEligible(row.Status) {
		return RepairResult{}, problem.Conflict("REFUND_REPAIR_NOT_ALLOWED", "refund is not eligible for stored repair")
	}

	requestHash := idempotency.RequestHash(map[string]any{"refund_no": refundNo, "apply": apply})
	if apply {
		if len(key) < 8 || len(key) > 128 {
			if key == "" {
				return RepairResult{}, problem.InvalidArgument("IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key is required")
			}
			return RepairResult{}, problem.InvalidArgument("IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must be between 8 and 128 characters")
		}
		var cached RepairResult
		ok, replayErr := s.idem.ReplayCompleted(ctx, s.repo.DB(), claims.AccountType, actorID, path, key, requestHash, &cached)
		if replayErr != nil {
			return RepairResult{}, replayErr
		}
		if ok {
			return cached, nil
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, storedRepairQueryTimeout)
	state, queryErr := s.provider.QueryRefund(callCtx, refundNo)
	cancel()
	result := repairDecision(row, state, queryErr)
	if !apply {
		result.Result = "preview"
		return result, nil
	}

	err = s.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, startErr := s.idem.Start(ctx, tx, s.ids.Next(), claims.AccountType, actorID, method, path, key, requestHash)
		if startErr != nil {
			return startErr
		}
		if !started {
			var cached RepairResult
			ok, cachedErr := s.idem.CachedResponse(ctx, tx, claims.AccountType, actorID, path, key, &cached)
			if cachedErr != nil {
				return cachedErr
			}
			if !ok {
				return problem.Conflict("IDEMPOTENCY_IN_PROGRESS", "request is still processing")
			}
			result = cached
			return nil
		}

		current, lockErr := s.repo.LockByNo(ctx, tx, refundNo)
		if lockErr != nil {
			return lockErr
		}
		result.BeforeStatus = current.Status
		if current.Version != row.Version {
			result.AfterStatus = current.Status
			result.Action = "state_changed_concurrently"
			result.Result = "skipped"
			if auditErr := s.auditResult(ctx, tx, actorID, "refund.stored_repair", current, result, "success"); auditErr != nil {
				return auditErr
			}
			return s.idem.Succeed(ctx, tx, claims.AccountType, actorID, path, key, result)
		}
		if !storedRepairEligible(current.Status) {
			result.AfterStatus = current.Status
			result.Action = "state_changed_concurrently"
			result.Result = "skipped"
			if auditErr := s.auditResult(ctx, tx, actorID, "refund.stored_repair", current, result, "success"); auditErr != nil {
				return auditErr
			}
			return s.idem.Succeed(ctx, tx, claims.AccountType, actorID, path, key, result)
		}

		if queryErr != nil {
			if paygateway.IsCode(queryErr, "RESOURCE_NOT_EXISTS") {
				if !storedRepairResubmissionAllowed(current) {
					result.AfterStatus = current.Status
					result.Action = "manual_investigation_required"
					result.Result = "failure"
					if auditErr := s.auditResult(ctx, tx, actorID, "refund.stored_repair", current, result, "failure"); auditErr != nil {
						return auditErr
					}
					return s.idem.Succeed(ctx, tx, claims.AccountType, actorID, path, key, result)
				}
				now := time.Now()
				// RESOURCE_NOT_EXISTS is the result of the mandatory query, not a
				// permanent error from the original submission. Clear the old
				// failure code so the worker is allowed to query once more and then
				// resubmit the exact immutable request.
				values := map[string]any{"status": "submission_unknown", "next_retry_at": now, "locked_by": nil, "locked_until": nil, "failure_code": nil, "failure_detail": "stored repair query confirmed provider record does not exist"}
				if updateErr := s.repo.Update(ctx, tx, current.ID, values); updateErr != nil {
					return updateErr
				}
				result.AfterStatus = "submission_unknown"
				result.Action = "schedule_original_refund_resubmission"
				result.Result = "success"
				if outboxErr := s.outbox(ctx, tx, "refund.stored_repair_applied", current.ID, result); outboxErr != nil {
					return outboxErr
				}
				if auditErr := s.auditResult(ctx, tx, actorID, "refund.stored_repair", current, result, "success"); auditErr != nil {
					return auditErr
				}
				return s.idem.Succeed(ctx, tx, claims.AccountType, actorID, path, key, result)
			}

			// A failed provider query is itself an auditable repair result. The
			// local financial state remains unchanged and the operator may retry.
			result.AfterStatus = current.Status
			result.Result = "failure"
			if auditErr := s.auditResult(ctx, tx, actorID, "refund.stored_repair", current, result, "failure"); auditErr != nil {
				return auditErr
			}
			return s.idem.Succeed(ctx, tx, claims.AccountType, actorID, path, key, result)
		}

		applyErr := s.applyState(ctx, tx, current, state)
		if applyErr != nil && isStateMismatch(applyErr) {
			detail := applyErr.Error()
			if len(detail) > 500 {
				detail = detail[:500]
			}
			if updateErr := s.repo.Update(ctx, tx, current.ID, map[string]any{"status": "exception", "failure_code": "PROVIDER_DATA_MISMATCH", "failure_detail": detail, "next_retry_at": nil, "locked_by": nil, "locked_until": nil}); updateErr != nil {
				return updateErr
			}
			if s.deliveryReturnClosure != nil {
				if closureErr := s.deliveryReturnClosure.ReconcileRefundWithTx(ctx, tx, current.AfterSaleID); closureErr != nil {
					return closureErr
				}
			}
			result.AfterStatus = "exception"
			result.Action = "provider_data_mismatch"
			result.Result = "failure"
			result.Error = applyErr.Error()
			if auditErr := s.auditResult(ctx, tx, actorID, "refund.stored_repair", current, result, "failure"); auditErr != nil {
				return auditErr
			}
			return s.idem.Succeed(ctx, tx, claims.AccountType, actorID, path, key, result)
		}
		if applyErr != nil && problem.FromError(applyErr).ErrorCode == "REFUND_PROVIDER_STATUS_INVALID" {
			result.AfterStatus = current.Status
			result.Action = "reject_unsupported_provider_status"
			result.Result = "failure"
			result.Error = applyErr.Error()
			if auditErr := s.auditResult(ctx, tx, actorID, "refund.stored_repair", current, result, "failure"); auditErr != nil {
				return auditErr
			}
			return s.idem.Succeed(ctx, tx, claims.AccountType, actorID, path, key, result)
		}
		if applyErr != nil {
			return applyErr
		}
		result.AfterStatus = localStatusForProvider(state.Status)
		result.Result = "success"
		if auditErr := s.auditResult(ctx, tx, actorID, "refund.stored_repair", current, result, "success"); auditErr != nil {
			return auditErr
		}
		return s.idem.Succeed(ctx, tx, claims.AccountType, actorID, path, key, result)
	})
	return result, err
}

func repairDecision(row Row, state State, err error) RepairResult {
	result := RepairResult{RefundNo: row.RefundNo, BeforeStatus: row.Status, AfterStatus: row.Status, Action: "query_failed", Result: "failure"}
	if err != nil {
		result.ProviderCode = paygateway.Code(err, "PROVIDER_UNAVAILABLE")
		result.Error = err.Error()
		if providerErr, ok := paygateway.As(err); ok {
			result.ProviderReqID = providerErr.RequestID
		}
		if paygateway.IsCode(err, "RESOURCE_NOT_EXISTS") && storedRepairResubmissionAllowed(row) {
			result.AfterStatus = "submission_unknown"
			result.Action = "schedule_original_refund_resubmission"
		} else if paygateway.IsCode(err, "RESOURCE_NOT_EXISTS") {
			result.Action = "manual_investigation_required"
		}
		return result
	}
	result.ProviderStatus = strings.ToUpper(strings.TrimSpace(state.Status))
	result.ProviderReqID = state.RequestID
	result.AfterStatus = localStatusForProvider(state.Status)
	switch result.ProviderStatus {
	case "SUCCESS":
		result.Action = "apply_success"
	case "PROCESSING":
		result.Action = "restore_pending"
	case "CLOSED":
		result.Action = "apply_closed_recovery"
	case "ABNORMAL":
		result.Action = "apply_abnormal_recovery"
	default:
		result.Action = "reject_unsupported_provider_status"
	}
	return result
}

func localStatusForProvider(status string) string {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "SUCCESS":
		return "succeeded"
	case "PROCESSING":
		return "pending"
	case "CLOSED":
		return "failed"
	case "ABNORMAL":
		return "exception"
	default:
		return "exception"
	}
}

func storedRepairEligible(status string) bool {
	switch status {
	case "creating", "submission_unknown", "pending", "exception":
		return true
	default:
		return false
	}
}

func storedRepairResubmissionAllowed(row Row) bool {
	switch row.Status {
	case "creating", "submission_unknown", "pending":
		return true
	case "exception":
		if row.ProviderStatus != nil {
			switch strings.ToUpper(strings.TrimSpace(*row.ProviderStatus)) {
			case "ABNORMAL", "CLOSED":
				return false
			}
		}
		if row.FailureCode == nil {
			return false
		}
		switch strings.ToUpper(strings.TrimSpace(*row.FailureCode)) {
		case "PROVIDER_DATA_MISMATCH", "PROVIDER_UNAVAILABLE", "SYSTEM_ERROR", "FREQUENCY_LIMITED":
			return true
		}
	}
	return false
}

func (s *Service) auditResult(ctx context.Context, tx *gorm.DB, actorID uint64, action string, before Row, after any, result string) error {
	if result == "" {
		result = "success"
	}
	beforeJSON, _ := jsonMarshal(before)
	afterJSON, _ := jsonMarshal(after)
	return tx.WithContext(ctx).Create(&Audit{ID: s.ids.Next(), ActorType: "admin", ActorID: actorID, Action: action, ResourceType: "refund", ResourceID: before.ID, BeforeData: datatypes.JSON(beforeJSON), AfterData: datatypes.JSON(afterJSON), Result: result, RequestID: requestctx.RequestIDPtr(ctx), IP: requestctx.IPPtr(ctx), UserAgent: requestctx.UserAgentPtr(ctx)}).Error
}

func jsonMarshal(value any) ([]byte, error) { return json.Marshal(value) }
