package reconciliation

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

type RunDTO struct {
	ID                string         `json:"id"`
	BillDate          string         `json:"bill_date"`
	BillType          string         `json:"bill_type"`
	Status            string         `json:"status"`
	StartedAt         *time.Time     `json:"started_at,omitempty"`
	CompletedAt       *time.Time     `json:"completed_at,omitempty"`
	HashType          *string        `json:"hash_type,omitempty"`
	ExpectedHash      *string        `json:"expected_hash,omitempty"`
	ComputedHash      *string        `json:"computed_hash,omitempty"`
	ProviderRequestID *string        `json:"provider_request_id,omitempty"`
	DownloadRequestID *string        `json:"download_request_id,omitempty"`
	RowCount          uint64         `json:"row_count"`
	DiscrepancyCount  uint64         `json:"discrepancy_count"`
	Stats             datatypes.JSON `json:"stats,omitempty"`
	ErrorCode         *string        `json:"error_code,omitempty"`
	ErrorDetail       *string        `json:"error_detail,omitempty"`
}

type DiscrepancyDTO struct {
	ID               string         `json:"id"`
	RunID            string         `json:"run_id"`
	BillDate         string         `json:"bill_date"`
	BillType         string         `json:"bill_type"`
	DiscrepancyType  string         `json:"discrepancy_type"`
	Status           string         `json:"status"`
	BusinessNo       *string        `json:"business_no,omitempty"`
	ProviderTradeNo  *string        `json:"provider_trade_no,omitempty"`
	ProviderRefundNo *string        `json:"provider_refund_no,omitempty"`
	LocalValue       datatypes.JSON `json:"local_value,omitempty"`
	WeChatValue      datatypes.JSON `json:"wechat_value,omitempty"`
	HandlingNote     *string        `json:"handling_note,omitempty"`
	HandledBy        string         `json:"handled_by,omitempty"`
	HandledAt        *time.Time     `json:"handled_at,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
}

func (s *Service) RunBillManual(ctx context.Context, claims *auth.Claims, method, path, key, rawDate, billType string) (RunResult, error) {
	actorID, err := reconciliationAdmin(claims, "refund:exception")
	if err != nil {
		return RunResult{}, err
	}
	billDate, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(rawDate), chinaLocation())
	if err != nil {
		return RunResult{}, problem.InvalidArgument("VALIDATION_FAILED", "bill_date must use YYYY-MM-DD")
	}
	billDate = normalizeBillDate(billDate)
	if billType != BillTypeTradeAll && billType != BillTypeFundflowBase {
		return RunResult{}, problem.InvalidArgument("VALIDATION_FAILED", "bill_type must be trade_all or fundflow_basic")
	}
	localNow := s.now().In(chinaLocation())
	latest := normalizeBillDate(localNow.AddDate(0, 0, -1))
	if localNow.Hour() < s.cfg.Reconciliation.RunHour {
		latest = latest.AddDate(0, 0, -1)
	}
	oldest := latest.AddDate(0, 0, -89)
	if billDate.Before(oldest) || billDate.After(latest) {
		return RunResult{}, problem.InvalidArgument("VALIDATION_FAILED", "bill_date is outside the downloadable three-month window")
	}
	requestHash := idempotency.RequestHash(map[string]any{"bill_date": billDate.Format("2006-01-02"), "bill_type": billType})
	var cached RunResult
	execute := false
	err = s.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, startErr := s.idem.Start(ctx, tx, s.ids.Next(), claims.AccountType, actorID, method, path, key, requestHash)
		if startErr != nil {
			return startErr
		}
		if !started {
			found, cacheErr := s.idem.CachedResponse(ctx, tx, claims.AccountType, actorID, path, key, &cached)
			if cacheErr != nil {
				return cacheErr
			}
			if !found {
				return problem.Conflict("IDEMPOTENCY_CONFLICT", "manual reconciliation request is already processing")
			}
			return nil
		}
		execute = true
		return nil
	})
	if err != nil || !execute {
		return cached, err
	}
	result, runErr := s.RunBill(ctx, billDate, billType)
	if runErr != nil {
		_ = s.repo.db.WithContext(context.Background()).Transaction(func(tx *gorm.DB) error {
			return s.idem.Fail(context.Background(), tx, claims.AccountType, actorID, path, key)
		})
		return RunResult{}, runErr
	}
	err = s.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.idem.Succeed(ctx, tx, claims.AccountType, actorID, path, key, result)
	})
	return result, err
}

func (s *Service) ListRuns(ctx context.Context, claims *auth.Claims, limit int) ([]RunDTO, error) {
	if _, err := reconciliationAdmin(claims, "refund:view"); err != nil {
		return nil, err
	}
	rows, err := s.repo.listRuns(ctx, limit)
	if err != nil {
		return nil, err
	}
	result := make([]RunDTO, 0, len(rows))
	for _, row := range rows {
		result = append(result, RunDTO{ID: strconv.FormatUint(row.ID, 10), BillDate: row.BillDate.Format("2006-01-02"), BillType: row.BillType, Status: row.Status, StartedAt: row.StartedAt, CompletedAt: row.CompletedAt, HashType: row.HashType, ExpectedHash: row.ExpectedHash, ComputedHash: row.ComputedHash, ProviderRequestID: row.ProviderRequestID, DownloadRequestID: row.DownloadRequestID, RowCount: row.RowCount, DiscrepancyCount: row.DiscrepancyCount, Stats: row.StatsJSON, ErrorCode: row.ErrorCode, ErrorDetail: row.ErrorDetail})
	}
	return result, nil
}

func (s *Service) ListDiscrepancies(ctx context.Context, claims *auth.Claims, status string, limit int) ([]DiscrepancyDTO, error) {
	if _, err := reconciliationAdmin(claims, "refund:view"); err != nil {
		return nil, err
	}
	status = strings.TrimSpace(status)
	if status != "" && status != "open" && status != "resolved" {
		return nil, problem.InvalidArgument("VALIDATION_FAILED", "status must be open or resolved")
	}
	rows, err := s.repo.listDiscrepancies(ctx, status, limit)
	if err != nil {
		return nil, err
	}
	result := make([]DiscrepancyDTO, 0, len(rows))
	for _, row := range rows {
		handledBy := ""
		if row.HandledBy != nil {
			handledBy = strconv.FormatUint(*row.HandledBy, 10)
		}
		result = append(result, DiscrepancyDTO{ID: strconv.FormatUint(row.ID, 10), RunID: strconv.FormatUint(row.RunID, 10), BillDate: row.BillDate.Format("2006-01-02"), BillType: row.BillType, DiscrepancyType: row.DiscrepancyType, Status: row.Status, BusinessNo: row.BusinessNo, ProviderTradeNo: row.ProviderTradeNo, ProviderRefundNo: row.ProviderRefundNo, LocalValue: row.LocalValue, WeChatValue: row.WeChatValue, HandlingNote: row.HandlingNote, HandledBy: handledBy, HandledAt: row.HandledAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt})
	}
	return result, nil
}

// ResolveDiscrepancy 记录明确的人工处置结果。
// 它刻意不更新支付、退款、订单或任何余额。
func (s *Service) ResolveDiscrepancy(ctx context.Context, claims *auth.Claims, rawID, note string) error {
	actorID, err := reconciliationAdmin(claims, "refund:exception")
	if err != nil {
		return err
	}
	id, err := strconv.ParseUint(rawID, 10, 64)
	if err != nil || id == 0 {
		return problem.InvalidArgument("VALIDATION_FAILED", "invalid discrepancy id")
	}
	note = strings.TrimSpace(note)
	if len(note) < 3 || len(note) > 1000 {
		return problem.InvalidArgument("VALIDATION_FAILED", "handling_note must contain 3 to 1000 characters")
	}
	if err := s.repo.resolveDiscrepancy(ctx, id, actorID, note, s.now()); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.NotFound("RECONCILIATION_DISCREPANCY_NOT_FOUND", "discrepancy not found")
		}
		if errors.Is(err, errDiscrepancyNotOpen) {
			return problem.Conflict("RECONCILIATION_DISCREPANCY_NOT_OPEN", "discrepancy is not open")
		}
		return err
	}
	return nil
}

func reconciliationAdmin(claims *auth.Claims, permission string) (uint64, error) {
	if claims == nil || claims.AccountType != "admin" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "admin account required")
	}
	actorID, err := strconv.ParseUint(claims.AdminUserID, 10, 64)
	if err != nil || actorID == 0 {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "invalid admin identity")
	}
	for _, value := range claims.Permissions {
		if value == permission {
			return actorID, nil
		}
	}
	return 0, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
}
