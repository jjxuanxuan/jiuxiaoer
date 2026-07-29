package integrity

import (
	"fmt"
	"strings"
	"time"
)

// IntegrityPhase 是酒票三类台账扫描器的稳定游标分区。
// 游标不包含偏移量：每个阶段均按不可变主键推进，
// 因此插入和删除不会导致分页漂移。
type IntegrityPhase string

const (
	IntegrityPhasePayments    IntegrityPhase = "payments"
	IntegrityPhasePurchases   IntegrityPhase = "purchases"
	IntegrityPhaseLots        IntegrityPhase = "lots"
	IntegrityPhaseRedemptions IntegrityPhase = "redemptions"
	IntegrityPhaseGifts       IntegrityPhase = "gifts"
	IntegrityPhaseRenewals    IntegrityPhase = "renewals"
	IntegrityPhaseRefunds     IntegrityPhase = "refunds"
	IntegrityPhaseSlots       IntegrityPhase = "slots"
	IntegrityPhaseReminders   IntegrityPhase = "reminders"
)

var reconciliationPhases = []IntegrityPhase{
	IntegrityPhasePayments,
	IntegrityPhasePurchases,
	IntegrityPhaseLots,
	IntegrityPhaseRedemptions,
	IntegrityPhaseGifts,
	IntegrityPhaseRenewals,
	IntegrityPhaseRefunds,
	IntegrityPhaseSlots,
	IntegrityPhaseReminders,
}

const (
	reconciliationRulePaymentSettlement = "REC-WT-001"
	reconciliationRulePurchaseIssue     = "REC-WT-002"
	reconciliationRuleLotReplay         = "REC-WT-003"
	reconciliationRuleAllocationView    = "REC-WT-003A"
	reconciliationRuleRedemptionLedger  = "REC-WT-004"
	reconciliationRuleFulfillment       = "REC-WT-004A"
	reconciliationRuleGift              = "REC-WT-005"
	reconciliationRuleRenewal           = "REC-WT-006"
	reconciliationRuleRefund            = "REC-WT-007"
	reconciliationRuleSlot              = "REC-WT-008"
	reconciliationRuleReminder          = "REC-WT-009"

	RuleLotReplay = reconciliationRuleLotReplay
)

// IntegrityCursor 是可由调用方持久化的精简游标。
// LastID 表示 Phase 中最后一条成功检查的记录。
type IntegrityCursor struct {
	Phase   IntegrityPhase `json:"phase"`
	LastID  uint64         `json:"last_id"`
	UpperID *uint64        `json:"upper_id,omitempty"`
}

// IntegrityBatchResult 描述一次有界且已提交的扫描步骤。
// 只有全部已发现异常均完成持久化写入后，游标才会推进。
type IntegrityBatchResult struct {
	Phase          IntegrityPhase  `json:"phase"`
	Checked        int             `json:"checked"`
	Detected       int             `json:"detected"`
	NextCursor     IntegrityCursor `json:"next_cursor"`
	PhaseCompleted bool            `json:"phase_completed"`
	CycleCompleted bool            `json:"cycle_completed"`
	CycleKey       string          `json:"cycle_key,omitempty"`
	LeaseAcquired  bool            `json:"lease_acquired"`
	NextRunAt      *time.Time      `json:"next_run_at,omitempty"`
}

type reconciliationDiscrepancy struct {
	Rule             string
	Kind             string
	BizType          string
	BizID            uint64
	BizNo            *string
	IssuerMerchantID *uint64
	Severity         string
	Expected         any
	Actual           any
}

func (d reconciliationDiscrepancy) exceptionType() string {
	kind := strings.ToLower(strings.TrimSpace(d.Kind))
	kind = strings.ReplaceAll(kind, " ", "_")
	return fmt.Sprintf("%s:%s", d.Rule, kind)
}

func (d reconciliationDiscrepancy) key() string {
	return fmt.Sprintf("%s|%s|%d", d.exceptionType(), d.BizType, d.BizID)
}

func normalizeIntegrityCursor(cursor IntegrityCursor) (IntegrityCursor, error) {
	if cursor.Phase == "" {
		cursor.Phase = reconciliationPhases[0]
	}
	for _, phase := range reconciliationPhases {
		if cursor.Phase == phase {
			return cursor, nil
		}
	}
	return IntegrityCursor{}, fmt.Errorf(
		"unknown wine-ticket reconciliation phase %q",
		cursor.Phase,
	)
}

func nextIntegrityCursor(
	phase IntegrityPhase,
	lastID uint64,
	phaseCompleted bool,
	upperID *uint64,
) (IntegrityCursor, bool) {
	if !phaseCompleted {
		return IntegrityCursor{
			Phase: phase, LastID: lastID, UpperID: upperID,
		}, false
	}
	for i, current := range reconciliationPhases {
		if current != phase {
			continue
		}
		if i == len(reconciliationPhases)-1 {
			return IntegrityCursor{Phase: reconciliationPhases[0]}, true
		}
		return IntegrityCursor{Phase: reconciliationPhases[i+1]}, false
	}
	return IntegrityCursor{Phase: reconciliationPhases[0]}, false
}

func reconciliationUniqueIDs(values []uint64) []uint64 {
	result := make([]uint64, 0, len(values))
	seen := make(map[uint64]struct{}, len(values))
	for _, value := range values {
		if value == 0 {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
