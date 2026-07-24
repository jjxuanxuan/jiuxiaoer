package reconciliation

import (
	"context"
	"crypto/sha1" // #nosec G505 -- 微信固定使用 SHA1 验证账单摘要。
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/paygateway"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type Service struct {
	cfg      config.Config
	repo     *repository
	ids      *snowflake.Generator
	provider Provider
	idem     *idempotency.Store
	log      *slog.Logger
	now      func() time.Time
}

type RunResult struct {
	RunID            uint64 `json:"run_id"`
	BillDate         string `json:"bill_date"`
	BillType         string `json:"bill_type"`
	Status           string `json:"status"`
	Rows             uint64 `json:"rows"`
	Discrepancies    uint64 `json:"discrepancies"`
	AlreadyCompleted bool   `json:"already_completed"`
}

func NewService(cfg config.Config, db *gorm.DB, ids *snowflake.Generator, provider Provider, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{cfg: cfg, repo: newRepository(db), ids: ids, provider: provider, idem: idempotency.NewStore(db), log: log, now: time.Now}
}

// RunBill 对账一个不可变的日期和类型。成功或无账单的运行具备幂等性，
// 不会重复下载。
func (s *Service) RunBill(ctx context.Context, billDate time.Time, billType string) (RunResult, error) {
	billDate = normalizeBillDate(billDate)
	if billType != BillTypeTradeAll && billType != BillTypeFundflowBase {
		return RunResult{}, fmt.Errorf("unsupported bill type %q", billType)
	}
	if s.provider == nil {
		return RunResult{}, errors.New("WeChat bill provider is unavailable")
	}
	now := s.now()
	run, acquired, err := s.repo.acquireRun(ctx, s.ids.Next(), billDate, billType, now, s.cfg.Reconciliation.RunningTimeout)
	if err != nil {
		return RunResult{}, err
	}
	if !acquired {
		return RunResult{RunID: run.ID, BillDate: billDate.Format("2006-01-02"), BillType: billType, Status: run.Status, Rows: run.RowCount, Discrepancies: run.DiscrepancyCount, AlreadyCompleted: run.Status == "succeeded" || run.Status == "no_statement"}, nil
	}

	file, err := s.provider.OpenBill(ctx, billDate, billType)
	if err != nil {
		if paygateway.IsCode(err, "NO_STATEMENT_EXIST") {
			return s.finishNoStatement(ctx, run, billDate, billType, err)
		}
		providerRequestID, downloadRequestID := failureRequestIDs(err)
		s.markFailed(run, paygateway.Code(err, "BILL_APPLY_FAILED"), err, providerRequestID, downloadRequestID)
		return RunResult{}, err
	}
	if file.Body == nil {
		err = errors.New("WeChat bill provider returned an empty body")
		s.markFailed(run, "BILL_DOWNLOAD_EMPTY", err, file.ProviderRequestID, file.DownloadRequestID)
		return RunResult{}, err
	}
	defer file.Body.Close()

	result, processErr := s.processFile(ctx, run, billDate, billType, file)
	if processErr != nil {
		code := "BILL_RECONCILIATION_FAILED"
		if errors.Is(processErr, errBillFormat) {
			code = "BILL_FORMAT_INVALID"
		} else if errors.Is(processErr, errDigestMismatch) {
			code = "BILL_DIGEST_MISMATCH"
		}
		s.markFailed(run, code, processErr, file.ProviderRequestID, file.DownloadRequestID)
		return RunResult{}, processErr
	}
	return result, nil
}

var errDigestMismatch = errors.New("WeChat bill digest mismatch")

func (s *Service) processFile(ctx context.Context, run Run, billDate time.Time, billType string, file BillFile) (RunResult, error) {
	if !strings.EqualFold(strings.TrimSpace(file.HashType), "SHA1") || strings.TrimSpace(file.ExpectedHash) == "" {
		return RunResult{}, fmt.Errorf("%w: unsupported or empty digest metadata", errDigestMismatch)
	}
	hasher := sha1.New() // #nosec G401 -- 微信账单 API 响应强制要求使用 SHA1。
	stream := io.TeeReader(file.Body, hasher)
	var rowCount, discrepancyCount uint64
	stats := map[string]uint64{}
	err := s.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		buffer := make([]Observation, 0, s.cfg.Reconciliation.InsertBatchSize)
		flush := func() error {
			if len(buffer) == 0 {
				return nil
			}
			if err := tx.CreateInBatches(&buffer, s.cfg.Reconciliation.InsertBatchSize).Error; err != nil {
				return err
			}
			buffer = buffer[:0]
			return nil
		}
		parsed, err := parseBill(stream, billType, func(entry parsedEntry) error {
			if entry.OccurredAt == nil || !normalizeBillDate(*entry.OccurredAt).Equal(billDate) {
				return fmt.Errorf("%w: line %d belongs to a different bill date", errBillFormat, entry.LineNo)
			}
			buffer = append(buffer, observationFromEntry(run.ID, entry))
			if len(buffer) >= s.cfg.Reconciliation.InsertBatchSize {
				return flush()
			}
			return nil
		})
		if err != nil {
			return err
		}
		if err := flush(); err != nil {
			return err
		}
		// parseBill 会在明细区结束后停止；还需读取汇总区，
		// 使哈希准确覆盖完整下载响应。
		if _, err := io.Copy(io.Discard, stream); err != nil {
			return fmt.Errorf("finish bill download: %w", err)
		}
		rowCount = parsed
		computed := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(computed, strings.TrimSpace(file.ExpectedHash)) {
			return fmt.Errorf("%w: expected=%s computed=%s", errDigestMismatch, file.ExpectedHash, computed)
		}
		count, reconcileStats, err := s.compare(ctx, tx, run, billDate, billType)
		if err != nil {
			return err
		}
		discrepancyCount, stats = count, reconcileStats
		// 观测记录是事务范围内的暂存行。差异记录保留相关的本地与微信值及行哈希；
		// 删除暂存数据可避免无限期保留每日账单的每一行。
		if err := tx.Where("run_id=?", run.ID).Delete(&Observation{}).Error; err != nil {
			return err
		}
		statsJSON, err := json.Marshal(stats)
		if err != nil {
			return err
		}
		completedAt := s.now()
		result := tx.Model(&Run{}).Where("id=? AND status='running' AND version=?", run.ID, run.Version).Updates(map[string]any{
			"status": "succeeded", "completed_at": completedAt,
			"hash_type": strings.ToUpper(file.HashType), "expected_hash": strings.ToLower(file.ExpectedHash), "computed_hash": computed,
			"provider_request_id": optionalString(file.ProviderRequestID), "download_request_id": optionalString(file.DownloadRequestID),
			"row_count": rowCount, "discrepancy_count": discrepancyCount, "stats_json": datatypes.JSON(statsJSON),
			"error_code": nil, "error_detail": nil, "version": gorm.Expr("version+1"),
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errRunLeaseLost
		}
		return nil
	})
	if err != nil {
		return RunResult{}, err
	}
	s.log.Info("WeChat bill reconciliation completed", "run_id", run.ID, "bill_date", billDate.Format("2006-01-02"), "bill_type", billType, "rows", rowCount, "discrepancies", discrepancyCount, "provider_request_id", file.ProviderRequestID, "download_request_id", file.DownloadRequestID)
	return RunResult{RunID: run.ID, BillDate: billDate.Format("2006-01-02"), BillType: billType, Status: "succeeded", Rows: rowCount, Discrepancies: discrepancyCount}, nil
}

func (s *Service) finishNoStatement(ctx context.Context, run Run, billDate time.Time, billType string, providerErr error) (RunResult, error) {
	var discrepancyCount uint64
	stats := map[string]uint64{}
	err := s.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		if billType == BillTypeTradeAll {
			discrepancyCount, stats, err = s.compare(ctx, tx, run, billDate, billType)
			if err != nil {
				return err
			}
		}
		statsJSON, err := json.Marshal(stats)
		if err != nil {
			return err
		}
		completedAt := s.now()
		result := tx.Model(&Run{}).Where("id=? AND status='running' AND version=?", run.ID, run.Version).Updates(map[string]any{
			"status": "no_statement", "completed_at": completedAt, "row_count": 0,
			"discrepancy_count": discrepancyCount, "stats_json": datatypes.JSON(statsJSON),
			"provider_request_id": optionalString(paygateway.RequestID(providerErr)), "error_code": "NO_STATEMENT_EXIST",
			"error_detail": nil, "version": gorm.Expr("version+1"),
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errRunLeaseLost
		}
		return nil
	})
	if err != nil {
		s.markFailed(run, "BILL_NO_STATEMENT_COMPARE_FAILED", err, paygateway.RequestID(providerErr), "")
		return RunResult{}, err
	}
	return RunResult{RunID: run.ID, BillDate: billDate.Format("2006-01-02"), BillType: billType, Status: "no_statement", Discrepancies: discrepancyCount}, nil
}

func (s *Service) markFailed(run Run, code string, cause error, providerRequestID, downloadRequestID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	if err := s.repo.markFailed(ctx, run, code, detail, providerRequestID, downloadRequestID, s.now()); err != nil {
		s.log.Error("failed to persist WeChat bill reconciliation failure", "run_id", run.ID, "bill_type", run.BillType, "error", err)
	}
}

func failureRequestIDs(err error) (string, string) {
	providerErr, ok := paygateway.As(err)
	if !ok {
		return "", ""
	}
	if providerErr.Operation == "bill.download" {
		return "", providerErr.RequestID
	}
	return providerErr.RequestID, ""
}

func observationFromEntry(runID uint64, entry parsedEntry) Observation {
	return Observation{RunID: runID, LineNo: entry.LineNo, EntryKind: entry.Kind,
		BusinessNo: stringPtr(entry.BusinessNo), ProviderTradeNo: stringPtr(entry.ProviderTradeNo), ProviderRefundNo: stringPtr(entry.ProviderRefundNo),
		ProviderStatus: stringPtr(entry.ProviderStatus), Currency: stringPtr(entry.Currency), Amount: entry.Amount, OccurredAt: entry.OccurredAt, RawHash: entry.RawHash}
}

func normalizeBillDate(value time.Time) time.Time {
	year, month, day := value.In(chinaLocation()).Date()
	return time.Date(year, month, day, 0, 0, 0, 0, chinaLocation())
}

func stringPtr(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" || value == "0" {
		return nil
	}
	return &value
}

func digestKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:])
}
