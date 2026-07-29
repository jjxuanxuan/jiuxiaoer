package ops

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/integrity"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

const (
	ExceptionStatusInvestigating        = "investigating"
	ExceptionStatusAwaitingExternalFact = "awaiting_external_fact"
	ExceptionStatusPendingReview        = "pending_review"
	ExceptionStatusResolved             = "resolved"

	ExceptionActionRetryClosure            = "retry_idempotent_closure"
	ExceptionActionConfirmProviderSuccess  = "confirm_provider_success"
	ExceptionActionProviderFailureRestore  = "confirm_provider_failure_restore"
	ExceptionActionRestoreQuantity         = "restore_verified_quantity"
	ExceptionActionAuditedAdjustment       = "create_audited_adjustment"
	ExceptionActionCloseWithoutAssetChange = "close_without_asset_change"
)

type ExceptionAdminFilter struct {
	Status   string
	Severity string
}

type ExceptionResolutionRequest struct {
	ResolutionAction string `json:"resolution_action"`
	Reason           string `json:"reason"`
	ReviewTicketNo   string `json:"review_ticket_no"`
	ExpectedVersion  uint   `json:"expected_version"`
}

// ExceptionAdminDTO 是管理端 API 返回的唯一异常表示。
// 完整性检查快照按不可信证据处理，脱敏后才会放入该投影。
type ExceptionAdminDTO struct {
	ExceptionNo      string          `json:"exception_no"`
	ExceptionType    string          `json:"exception_type"`
	BizType          string          `json:"biz_type"`
	BizID            string          `json:"biz_id"`
	BizNo            *string         `json:"biz_no"`
	IssuerMerchantID *string         `json:"issuer_merchant_id,omitempty"`
	SourceType       string          `json:"source_type"`
	SourceID         *string         `json:"source_id,omitempty"`
	CorrelationID    *string         `json:"correlation_id,omitempty"`
	Severity         string          `json:"severity"`
	Status           string          `json:"status"`
	ExpectedSnapshot json.RawMessage `json:"expected_snapshot,omitempty"`
	ActualSnapshot   json.RawMessage `json:"actual_snapshot,omitempty"`
	ProposedAction   *string         `json:"proposed_action,omitempty"`
	ProposedReason   *string         `json:"proposed_reason,omitempty"`
	ReviewTicketNo   *string         `json:"review_ticket_no,omitempty"`
	ProposedBy       *string         `json:"proposed_by,omitempty"`
	ProposedAt       *string         `json:"proposed_at,omitempty"`
	ReviewDecision   *string         `json:"review_decision,omitempty"`
	ReviewNote       *string         `json:"review_note,omitempty"`
	ReviewedBy       *string         `json:"reviewed_by,omitempty"`
	ReviewedAt       *string         `json:"reviewed_at,omitempty"`
	ResolutionResult json.RawMessage `json:"resolution_result,omitempty"`
	OccurrenceCount  uint            `json:"occurrence_count"`
	FirstDetectedAt  string          `json:"first_detected_at"`
	LastDetectedAt   string          `json:"last_detected_at"`
	ResolvedAt       *string         `json:"resolved_at,omitempty"`
	Version          uint            `json:"version"`
	CreatedAt        string          `json:"created_at"`
	UpdatedAt        string          `json:"updated_at"`
}

type ExceptionResolutionCommand struct {
	Exception      integrity.Exception
	Action         string
	Reason         string
	ReviewTicketNo string
	ActorID        uint64
	ResolutionTime time.Time
}

// ExceptionResolutionExecutor 被有意设计为失败关闭。
// 业务闭环代码必须校验支付机构和资产事实，并在调用方事务中执行；
// 管理端快照仅是证据，绝不能作为调账指令。
type ExceptionResolutionExecutor interface {
	ExecuteWineTicketExceptionResolution(
		ctx context.Context,
		tx *gorm.DB,
		command ExceptionResolutionCommand,
	) (datatypes.JSON, error)
}

type safeExceptionResolutionExecutor struct{}

func (safeExceptionResolutionExecutor) ExecuteWineTicketExceptionResolution(
	_ context.Context,
	_ *gorm.DB,
	command ExceptionResolutionCommand,
) (datatypes.JSON, error) {
	if command.Action != ExceptionActionCloseWithoutAssetChange {
		return nil, problem.Conflict(
			"WT_EXCEPTION_ACTION_UNAVAILABLE",
			"the selected resolution requires a registered verified business closure",
		)
	}
	return jsonData(map[string]any{
		"action":           command.Action,
		"asset_changed":    false,
		"review_ticket_no": command.ReviewTicketNo,
		"resolved_at":      formatShanghai(command.ResolutionTime),
	}), nil
}

func normalizeExceptionResolution(
	request ExceptionResolutionRequest,
) (ExceptionResolutionRequest, error) {
	request.ResolutionAction = strings.TrimSpace(request.ResolutionAction)
	request.Reason = strings.TrimSpace(request.Reason)
	request.ReviewTicketNo = strings.TrimSpace(request.ReviewTicketNo)
	if !validExceptionResolutionAction(request.ResolutionAction) {
		return ExceptionResolutionRequest{}, problem.InvalidArgument(
			"VALIDATION_FAILED",
			"invalid resolution_action",
		)
	}
	if request.Reason == "" || len(request.Reason) > 500 {
		return ExceptionResolutionRequest{}, problem.InvalidArgument(
			"VALIDATION_FAILED",
			"reason must contain between 1 and 500 characters",
		)
	}
	if request.ReviewTicketNo == "" ||
		len(request.ReviewTicketNo) > 64 ||
		!businessNoPattern.MatchString(request.ReviewTicketNo) {
		return ExceptionResolutionRequest{}, problem.InvalidArgument(
			"VALIDATION_FAILED",
			"review_ticket_no must be a valid business number",
		)
	}
	if request.ExpectedVersion == 0 {
		return ExceptionResolutionRequest{}, problem.InvalidArgument(
			"VALIDATION_FAILED",
			"expected_version must be at least 1",
		)
	}
	return request, nil
}

func validExceptionResolutionAction(value string) bool {
	switch value {
	case ExceptionActionRetryClosure,
		ExceptionActionConfirmProviderSuccess,
		ExceptionActionProviderFailureRestore,
		ExceptionActionRestoreQuantity,
		ExceptionActionAuditedAdjustment,
		ExceptionActionCloseWithoutAssetChange:
		return true
	default:
		return false
	}
}

func validExceptionStatus(value string) bool {
	switch value {
	case ExceptionStatusInvestigating,
		ExceptionStatusAwaitingExternalFact,
		ExceptionStatusPendingReview,
		ExceptionStatusResolved:
		return true
	default:
		return false
	}
}

func validExceptionSeverity(value string) bool {
	switch value {
	case "P0", "P1", "P2", "P3":
		return true
	default:
		return false
	}
}
