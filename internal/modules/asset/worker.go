package asset

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"log/slog"
	"time"
)

type Worker struct {
	cfg      config.Config
	service  *Service
	instance string
	log      *slog.Logger
}

// NewWorker 创建并初始化工作器。
func NewWorker(cfg config.Config, service *Service, log *slog.Logger) *Worker {
	return &Worker{cfg: cfg, service: service, instance: cfg.App.InstanceID, log: log}
}

// Run 运行当前实例的核心处理流程。
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.Asset.WorkerInterval)
	defer ticker.Stop()
	for {
		w.runBatch(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runBatch 运行批次处理流程。
func (w *Worker) runBatch(ctx context.Context) {
	if w.cfg.Asset.CompensationIssueEnabled {
		for i := 0; i < w.cfg.Asset.WorkerBatchSize; i++ {
			row, err := w.claimCompensation(ctx)
			if errors.Is(err, gorm.ErrRecordNotFound) {
				break
			}
			if err != nil {
				w.log.Error("claim compensation", slog.String("error", err.Error()))
				break
			}
			if err := w.issueCompensation(ctx, row); err != nil {
				w.log.Error("issue compensation", slog.Uint64("compensation_id", row.ID), slog.String("error", err.Error()))
			}
		}
	}
	if w.cfg.Asset.ExpiryEnabled {
		if _, err := w.service.ExpireDueHolds(ctx, w.cfg.Asset.WorkerBatchSize); err != nil {
			w.log.Error("expire asset holds", slog.String("error", err.Error()))
			return
		}
		if _, err := w.service.ExpireDueLots(ctx, w.cfg.Asset.WorkerBatchSize); err != nil {
			w.log.Error("expire asset lots", slog.String("error", err.Error()))
		}
	}
}

// claimCompensation 认领Compensation。
func (w *Worker) claimCompensation(ctx context.Context) (Compensation, error) {
	var row Compensation
	err := w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("status IN ('approved','issuing') AND (next_retry_at IS NULL OR next_retry_at<=?) AND (locked_until IS NULL OR locked_until<=?)", now, now).Order("next_retry_at,id").Take(&row).Error; err != nil {
			return err
		}
		until := now.Add(30 * time.Second)
		return tx.Model(&Compensation{}).Where("id=?", row.ID).Updates(map[string]any{"status": "issuing", "locked_by": w.instance, "locked_until": until, "attempts": gorm.Expr("attempts+1")}).Error
	})
	return row, err
}

// issueCompensation 返回issue Compensation。
func (w *Worker) issueCompensation(ctx context.Context, row Compensation) error {
	if row.AssetType != "balance" || row.Amount <= 0 {
		code := "ASSET_TYPE_INVALID"
		_ = w.service.db.Model(&Compensation{}).Where("id=?", row.ID).Updates(map[string]any{"status": "failed", "failure_code": code, "locked_by": nil, "locked_until": nil}).Error
		return problem.New(422, code, "Unprocessable Entity", "compensation asset mapping is invalid")
	}
	dto, err := w.service.Credit(ctx, Command{CustomerID: row.CustomerID, AssetType: TypeBalance, Unit: UnitCNY, Amount: row.Amount, SourceType: "compensation", SourceID: idString(row.ID), IdempotencyKey: "compensation-" + row.CompensationNo, ActorType: "system", ActorID: 0, Metadata: map[string]any{"compensation_no": row.CompensationNo, "after_sale_id": idString(row.AfterSaleID)}})
	if err != nil {
		code := problem.FromError(err).ErrorCode
		next := time.Now().UTC().Add(w.cfg.Asset.WorkerInterval)
		_ = w.service.db.Model(&Compensation{}).Where("id=?", row.ID).Updates(map[string]any{"status": "approved", "failure_code": code, "next_retry_at": next, "locked_by": nil, "locked_until": nil}).Error
		return err
	}
	txID, err := parseID(dto.ID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	res := w.service.db.Model(&Compensation{}).Where("id=? AND status='issuing'", row.ID).Updates(map[string]any{"status": "issued", "asset_transaction_id": txID, "issued_at": now, "failure_code": nil, "next_retry_at": nil, "locked_by": nil, "locked_until": nil})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		var current Compensation
		if err := w.service.db.Where("id=?", row.ID).Take(&current).Error; err != nil {
			return err
		}
		if current.Status == "issued" && current.AssetTransactionID != nil && *current.AssetTransactionID == txID {
			return nil
		}
		return problem.Conflict("COMPENSATION_STATE_CONFLICT", "compensation state changed")
	}
	return nil
}
