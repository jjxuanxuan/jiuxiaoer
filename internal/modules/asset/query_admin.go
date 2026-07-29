package asset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
)

// Summaries 返回摘要列表。
func (s *Service) Summaries(ctx context.Context, claims *auth.Claims) ([]AssetSummaryDTO, error) {
	if !s.cfg.Asset.ReadEnabled {
		return nil, problem.New(503, "ASSET_READ_DISABLED", "Service Unavailable", "asset reads are disabled")
	}
	customerID, err := customerIDFromClaims(claims)
	if err != nil {
		return nil, err
	}
	return s.summariesForCustomer(ctx, customerID)
}

// summariesForCustomer 返回客户的资产摘要列表。
func (s *Service) summariesForCustomer(ctx context.Context, customerID uint64) ([]AssetSummaryDTO, error) {
	types := []string{TypeGrowth, TypeWineCoin, TypeBalance}
	out := make([]AssetSummaryDTO, 0, len(types))
	for _, assetType := range types {
		unit, _ := UnitFor(assetType)
		dto := AssetSummaryDTO{AssetType: assetType, Unit: unit}
		var row struct {
			Available, Frozen int64
			UpdatedAt         *time.Time
		}
		err := s.db.WithContext(ctx).Table("asset_accounts a").Select("COALESCE(MAX(CASE WHEN b.bucket='available' THEN b.amount END),0) available, COALESCE(MAX(CASE WHEN b.bucket='frozen' THEN b.amount END),0) frozen, MAX(b.updated_at) updated_at").Joins("LEFT JOIN asset_balances b ON b.account_id=a.id").Where("a.owner_type='customer' AND a.owner_id=? AND a.asset_type=? AND a.unit=?", customerID, assetType, unit).Group("a.id").Scan(&row).Error
		if err != nil {
			return nil, err
		}
		dto.AvailableAmount = row.Available
		dto.FrozenAmount = row.Frozen
		if row.UpdatedAt != nil {
			dto.UpdatedAt = row.UpdatedAt.UTC().Format(time.RFC3339Nano)
		}
		var lot struct {
			Amount    int64
			ExpiresAt *time.Time
		}
		_ = s.db.WithContext(ctx).Table("asset_lots l").Select("l.available_amount amount,l.expires_at").Joins("JOIN asset_accounts a ON a.id=l.account_id").Where("a.owner_type='customer' AND a.owner_id=? AND a.asset_type=? AND l.available_amount>0 AND l.expires_at IS NOT NULL", customerID, assetType).Order("l.expires_at,l.id").Take(&lot).Error
		if lot.ExpiresAt != nil {
			dto.NextExpiringAmount = lot.Amount
			dto.NextExpiresAt = lot.ExpiresAt.UTC().Format(time.RFC3339Nano)
		}
		out = append(out, dto)
	}
	return out, nil
}

type transactionView struct {
	Transaction
	CustomerID          uint64
	Bucket              string
	Delta, BalanceAfter int64
}

// ListCustomerTransactions 查询用户交易列表。
func (s *Service) ListCustomerTransactions(ctx context.Context, claims *auth.Claims, assetType string, q pagination.Query) ([]TransactionDTO, string, error) {
	if !s.cfg.Asset.ReadEnabled {
		return nil, "", problem.New(503, "ASSET_READ_DISABLED", "Service Unavailable", "asset reads are disabled")
	}
	customerID, err := customerIDFromClaims(claims)
	if err != nil {
		return nil, "", err
	}
	if _, err = UnitFor(assetType); err != nil {
		return nil, "", err
	}
	rows, hasMore, err := s.listTransactions(ctx, customerID, assetType, "", "", q)
	return pageTransactions(rows, q, hasMore, err, false)
}

// ListAdminTransactions 查询管理端交易列表。
func (s *Service) ListAdminTransactions(ctx context.Context, claims *auth.Claims, query ListQuery) ([]TransactionDTO, string, error) {
	if _, err := adminIDWithPermission(claims, "asset_transaction:list"); err != nil {
		return nil, "", err
	}
	rows, hasMore, err := s.listTransactions(ctx, 0, query.AssetType, query.SourceType, query.Action, query.Query)
	return pageTransactions(rows, query.Query, hasMore, err, true)
}

// listTransactions 查询交易列表。
func (s *Service) listTransactions(ctx context.Context, customerID uint64, assetType, sourceType, action string, q pagination.Query) ([]transactionView, bool, error) {
	base := s.db.WithContext(ctx).Table("asset_transactions t").Joins("JOIN asset_entries e ON e.transaction_id=t.id").Joins("JOIN asset_accounts a ON a.id=e.account_id AND a.owner_type='customer'")
	if customerID > 0 {
		base = base.Where("a.owner_id=?", customerID)
	}
	if assetType != "" {
		base = base.Where("t.asset_type=?", assetType)
	}
	if sourceType != "" {
		base = base.Where("t.source_type=?", sourceType)
	}
	if action != "" {
		base = base.Where("t.action=?", action)
	}
	var ids []uint64
	if err := base.Select("t.id").Group("t.id,t.created_at").Order("t.created_at DESC,t.id DESC").Offset(q.Offset).Limit(q.PageSize+1).Pluck("t.id", &ids).Error; err != nil {
		return nil, false, err
	}
	hasMore := len(ids) > q.PageSize
	if hasMore {
		ids = ids[:q.PageSize]
	}
	if len(ids) == 0 {
		return []transactionView{}, false, nil
	}
	db := s.db.WithContext(ctx).Table("asset_transactions t").Select("t.*,a.owner_id customer_id,e.bucket,e.delta,e.balance_after").Joins("JOIN asset_entries e ON e.transaction_id=t.id").Joins("JOIN asset_accounts a ON a.id=e.account_id AND a.owner_type='customer'").Where("t.id IN ?", ids)
	var rows []transactionView
	err := db.Order("t.created_at DESC,t.id DESC,e.entry_seq").Scan(&rows).Error
	return rows, hasMore, err
}

// pageTransactions 返回分页交易。
func pageTransactions(rows []transactionView, q pagination.Query, hasMore bool, err error, admin bool) ([]TransactionDTO, string, error) {
	if err != nil {
		return nil, "", err
	}
	next := ""
	if hasMore {
		next = pagination.NextPageToken(q)
	}
	out := make([]TransactionDTO, 0, len(rows))
	seen := map[uint64]int{}
	for _, row := range rows {
		idx, ok := seen[row.ID]
		if !ok {
			meta := map[string]any{}
			_ = json.Unmarshal(row.Metadata, &meta)
			dto := dtoFromTransaction(row.Transaction, row.CustomerID, 0, 0, row.BalanceAfter, meta)
			if !admin {
				dto.SourceID = ""
			}
			out = append(out, dto)
			idx = len(out) - 1
			seen[row.ID] = idx
		}
		if row.Bucket == "available" {
			out[idx].AvailableDelta += row.Delta
			out[idx].BalanceAfter = row.BalanceAfter
		} else {
			out[idx].FrozenDelta += row.Delta
		}
	}
	return out, next, nil
}

// AdminTransaction 返回管理端交易。
func (s *Service) AdminTransaction(ctx context.Context, claims *auth.Claims, rawID string) (TransactionDTO, error) {
	if _, err := adminIDWithPermission(claims, "asset_transaction:view"); err != nil {
		return TransactionDTO{}, err
	}
	id, err := parseID(rawID)
	if err != nil {
		return TransactionDTO{}, problem.NotFound("ASSET_TRANSACTION_NOT_FOUND", "asset transaction not found")
	}
	var rows []transactionView
	err = s.db.WithContext(ctx).Table("asset_transactions t").Select("t.*,a.owner_id customer_id,e.bucket,e.delta,e.balance_after").Joins("JOIN asset_entries e ON e.transaction_id=t.id").Joins("JOIN asset_accounts a ON a.id=e.account_id AND a.owner_type='customer'").Where("t.id=?", id).Order("e.entry_seq").Scan(&rows).Error
	if errors.Is(err, gorm.ErrRecordNotFound) || len(rows) == 0 {
		return TransactionDTO{}, problem.NotFound("ASSET_TRANSACTION_NOT_FOUND", "asset transaction not found")
	}
	if err != nil {
		return TransactionDTO{}, err
	}
	items, _, err := pageTransactions(rows, pagination.Query{}, false, nil, true)
	if err != nil {
		return TransactionDTO{}, err
	}
	return items[0], nil
}

// CreateAdjustment 创建调整单并由当前管理员直接执行。
func (s *Service) CreateAdjustment(ctx context.Context, claims *auth.Claims, method, path, key string, req AdjustmentCreateReq) (AdjustmentDTO, error) {
	adminID, err := adminIDWithPermission(claims, "asset_adjustment:create")
	if err != nil {
		return AdjustmentDTO{}, err
	}
	customerID, err := parseID(req.CustomerID)
	if err != nil {
		return AdjustmentDTO{}, problem.NotFound("MEMBER_NOT_FOUND", "member not found")
	}
	unit, err := UnitFor(req.AssetType)
	if err != nil {
		return AdjustmentDTO{}, err
	}
	if req.Direction != "credit" && req.Direction != "debit" {
		return AdjustmentDTO{}, problem.New(422, "ASSET_AMOUNT_INVALID", "Unprocessable Entity", "manual adjustment direction is invalid")
	}
	limit := int64(100000)
	if req.AssetType == TypeBalance {
		limit = 50000
	}
	if req.Amount <= 0 || req.Amount > limit {
		return AdjustmentDTO{}, problem.New(422, "ASSET_AMOUNT_INVALID", "Unprocessable Entity", "manual adjustment exceeds configured limit")
	}
	var out AdjustmentDTO
	var executionErr error
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		allowed, authErr := activeAdminHasPermission(
			ctx,
			tx,
			adminID,
			"asset_adjustment:create",
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
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), "admin", adminID, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		var row Adjustment
		if !started {
			ok, err := s.idem.CachedResponse(ctx, tx, "admin", adminID, path, key, &out)
			if err != nil {
				return err
			}
			if !ok {
				return problem.Conflict("IDEMPOTENCY_CONFLICT", "idempotent response unavailable")
			}
			id, parseErr := parseID(out.ID)
			if parseErr != nil {
				return problem.Conflict("IDEMPOTENCY_CONFLICT", "cached adjustment response is invalid")
			}
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=? AND created_by=?", id, adminID).Take(&row).Error; err != nil {
				return problem.Conflict("IDEMPOTENCY_CONFLICT", "cached adjustment no longer exists")
			}
			out = adjustmentDTO(row)
			switch row.Status {
			case "succeeded", "rejected":
				return s.idem.Succeed(ctx, tx, "admin", adminID, path, key, out)
			case "failed":
				executionErr = adjustmentExecutionError(out.FailureCode)
				return s.idem.Succeed(ctx, tx, "admin", adminID, path, key, out)
			case "pending", "executing":
				// 兼容升级前已创建但尚未完成的历史调整单；下面会幂等续办。
			default:
				return problem.Conflict("ASSET_ADJUSTMENT_STATE_CONFLICT", "adjustment state is invalid")
			}
		} else {
			if err := s.requireCustomer(ctx, tx, customerID); err != nil {
				return err
			}
			evidence := jsonData(req.EvidenceRefs)
			row = Adjustment{ID: s.ids.Next(), AdjustmentNo: "AD" + fmt.Sprint(s.ids.Next()), CustomerID: customerID, AssetType: req.AssetType, Unit: unit, Direction: req.Direction, Amount: req.Amount, ReasonCode: req.ReasonCode, Reason: req.Reason, EvidenceRefs: evidence, Status: "pending", CreatedBy: adminID, IdempotencyKeyHash: keyHash(key), Version: 1}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			if err := tx.Create(s.adjustmentAudit(
				ctx,
				adminID,
				"asset_adjustment.create",
				row,
				nil,
				map[string]any{
					"status":               row.Status,
					"version":              row.Version,
					"customer_id":          req.CustomerID,
					"asset_type":           req.AssetType,
					"amount":               req.Amount,
					"direction":            req.Direction,
					"reason_code":          req.ReasonCode,
					"reason":               req.Reason,
					"idempotency_key_hash": row.IdempotencyKeyHash,
				},
				"success",
				nil,
			)).Error; err != nil {
				return err
			}
		}

		executionBeforeStatus := row.Status
		executionBeforeVersion := row.Version
		if row.Status == "pending" {
			now := s.now().UTC()
			result := tx.Model(&Adjustment{}).Where("id=? AND status='pending' AND version=?", row.ID, row.Version).Updates(map[string]any{"status": "executing", "reviewed_by": adminID, "reviewed_at": now, "version": gorm.Expr("version+1")})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return problem.Conflict("ASSET_ADJUSTMENT_STATE_CONFLICT", "adjustment state or version changed")
			}
			row.Status = "executing"
			row.ReviewedBy = &adminID
			row.ReviewedAt = &now
			row.Version++
		}

		// 历史 executing 记录可能仍保留旧 reviewer；本次直执流水和审计必须
		// 归因于当前已认证、且刚刚通过事务内权限复核的管理员。
		actorID := adminID
		action := "credit"
		direction := int64(1)
		if row.Direction == "debit" {
			action = "debit"
			direction = -1
		}
		cmd := Command{CustomerID: row.CustomerID, AssetType: row.AssetType, Unit: row.Unit, Amount: row.Amount, SourceType: "manual_adjustment", SourceID: idString(row.ID), Action: action, IdempotencyKey: key, ActorType: "admin", ActorID: actorID, Metadata: map[string]any{"adjustment_no": row.AdjustmentNo, "reason_code": row.ReasonCode}}
		var txDTO TransactionDTO
		txDTO, executionErr = s.postTransferWithDB(ctx, tx, cmd, direction, nil)
		if executionErr != nil {
			code := problem.FromError(executionErr).ErrorCode
			if !isTerminalAdjustmentFailure(code) {
				return executionErr
			}
			result := tx.Model(&Adjustment{}).Where("id=? AND status='executing'", row.ID).Updates(map[string]any{"status": "failed", "failure_code": code, "version": gorm.Expr("version+1")})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return problem.Conflict("ASSET_ADJUSTMENT_STATE_CONFLICT", "adjustment state changed while execution failed")
			}
		} else {
			txID, parseErr := parseID(txDTO.ID)
			if parseErr != nil {
				return problem.Internal("asset transaction id is invalid")
			}
			result := tx.Model(&Adjustment{}).Where("id=? AND status='executing'", row.ID).Updates(map[string]any{"status": "succeeded", "asset_transaction_id": txID, "failure_code": nil, "version": gorm.Expr("version+1")})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return problem.Conflict("ASSET_ADJUSTMENT_STATE_CONFLICT", "adjustment state changed while execution succeeded")
			}
		}
		if err := tx.Where("id=?", row.ID).Take(&row).Error; err != nil {
			return err
		}
		out = adjustmentDTO(row)
		result := "success"
		var errorCode *string
		if executionErr != nil {
			result = "failed"
			code := out.FailureCode
			errorCode = &code
		}
		if err := tx.Create(s.adjustmentAudit(
			ctx,
			actorID,
			"asset_adjustment.execute",
			row,
			map[string]any{
				"status":  executionBeforeStatus,
				"version": executionBeforeVersion,
			},
			map[string]any{
				"status":               out.Status,
				"version":              out.Version,
				"asset_transaction_id": out.AssetTransactionID,
				"transaction_action":   action,
				"failure_code":         out.FailureCode,
				"reason_code":          row.ReasonCode,
				"reason":               row.Reason,
				"idempotency_key_hash": row.IdempotencyKeyHash,
			},
			result,
			errorCode,
		)).Error; err != nil {
			return err
		}
		return s.idem.Succeed(ctx, tx, "admin", adminID, path, key, out)
	})
	if err != nil {
		return AdjustmentDTO{}, err
	}
	return out, executionErr
}

// activeAdminHasPermission 在高风险资产写事务内复核当前授权链，
// 避免只依赖令牌中的旧权限快照。
func activeAdminHasPermission(
	ctx context.Context,
	tx *gorm.DB,
	adminID uint64,
	permissionCode string,
) (bool, error) {
	var row struct {
		ID uint64
	}
	err := tx.WithContext(ctx).
		Table("admin_users au").
		Select("au.id").
		Joins("JOIN accounts a ON a.id = au.account_id").
		Joins("JOIN roles r ON r.id = au.role_id").
		Joins("JOIN role_permissions rp ON rp.role_id = r.id").
		Joins("JOIN permissions p ON p.id = rp.permission_id").
		Where(`au.id = ?
			AND au.status = 'active' AND au.deleted_at IS NULL
			AND a.account_type = 'admin' AND a.status = 'active' AND a.deleted_at IS NULL
			AND r.status = 'active' AND r.deleted_at IS NULL
			AND rp.deleted_at IS NULL
			AND p.code = ? AND p.status = 'active' AND p.deleted_at IS NULL`,
			adminID,
			permissionCode,
		).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s *Service) adjustmentAudit(
	ctx context.Context,
	actorID uint64,
	action string,
	row Adjustment,
	before map[string]any,
	after map[string]any,
	result string,
	errorCode *string,
) *AuditLog {
	if after == nil {
		after = map[string]any{}
	}
	requestID := requestctx.RequestID(ctx)
	after["permission"] = "asset_adjustment:create"
	after["request_id"] = requestID
	// 当前 HTTP 链路以 request ID 作为缺省 correlation ID；这样即使调用方
	// 未单独提供链路 ID，审计仍能跨日志、Outbox 和幂等事实稳定关联。
	after["correlation_id"] = requestID
	after["service_instance"] = defaultString(
		strings.TrimSpace(s.cfg.App.InstanceID),
		"unknown",
	)
	after["idempotency_key_hash"] = row.IdempotencyKeyHash
	after["reason_code"] = row.ReasonCode
	after["status"] = row.Status
	after["version"] = row.Version
	evidenceRefs := make([]string, 0)
	if len(row.EvidenceRefs) > 0 {
		_ = json.Unmarshal(row.EvidenceRefs, &evidenceRefs)
	}
	after["evidence_count"] = len(evidenceRefs)
	if len(evidenceRefs) > 0 {
		// 证据引用由请求方提供，可能包含路径或 URL；审计仅保留数量和稳定
		// 摘要，避免复制潜在敏感内容。
		after["evidence_refs_hash"] = idempotency.RequestHash(evidenceRefs)
	}

	version := uint64(row.Version)
	var beforeStatus *string
	if value, ok := before["status"].(string); ok && value != "" {
		beforeStatus = &value
	}
	afterStatus := row.Status
	reasonCode := row.ReasonCode
	return &AuditLog{
		ID:           s.ids.Next(),
		ActorType:    "admin",
		ActorID:      actorID,
		Action:       action,
		ResourceType: "asset_adjustment",
		ResourceID:   row.ID,
		BeforeData:   jsonData(before),
		AfterData:    jsonData(after),
		Result:       result,
		Version:      &version,
		ErrorCode:    errorCode,
		ReasonCode:   &reasonCode,
		BeforeStatus: beforeStatus,
		AfterStatus:  &afterStatus,
		RequestID:    requestctx.RequestIDPtr(ctx),
		IPHash:       requestctx.IPHashPtr(ctx),
		UserAgent:    requestctx.UserAgentPtr(ctx),
	}
}

func isTerminalAdjustmentFailure(code string) bool {
	switch code {
	case "ASSET_INSUFFICIENT_AVAILABLE",
		"ASSET_AMOUNT_INVALID",
		"ASSET_TYPE_INVALID",
		"ASSET_SOURCE_NOT_ALLOWED",
		"MEMBER_NOT_FOUND":
		return true
	default:
		return false
	}
}

// adjustmentDTO 返回调整单DTO。
func adjustmentDTO(row Adjustment) AdjustmentDTO {
	dto := AdjustmentDTO{ID: idString(row.ID), AdjustmentNo: row.AdjustmentNo, CustomerID: idString(row.CustomerID), AssetType: row.AssetType, Unit: row.Unit, Direction: row.Direction, Amount: row.Amount, ReasonCode: row.ReasonCode, Reason: row.Reason, Status: row.Status, CreatedBy: idString(row.CreatedBy), Version: row.Version, CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: row.UpdatedAt.UTC().Format(time.RFC3339Nano)}
	if row.ReviewedBy != nil {
		dto.ReviewedBy = idString(*row.ReviewedBy)
	}
	if row.AssetTransactionID != nil {
		dto.AssetTransactionID = idString(*row.AssetTransactionID)
	}
	if row.FailureCode != nil {
		dto.FailureCode = *row.FailureCode
	}
	return dto
}

// adjustmentExecutionError 根据持久化失败码重建可重放的业务错误。
func adjustmentExecutionError(code string) error {
	switch code {
	case "ASSET_INSUFFICIENT_AVAILABLE":
		return problem.Conflict(code, "available asset balance is insufficient")
	case "ASSET_WRITE_DISABLED":
		return problem.New(503, code, "Service Unavailable", "asset writes are disabled")
	case "ASSET_AMOUNT_INVALID":
		return problem.New(422, code, "Unprocessable Entity", "asset amount is invalid")
	case "ASSET_TYPE_INVALID":
		return problem.New(422, code, "Unprocessable Entity", "asset type is invalid")
	case "ASSET_SOURCE_NOT_ALLOWED":
		return problem.Forbidden(code, "asset source is not allowed")
	case "MEMBER_NOT_FOUND":
		return problem.NotFound(code, "member not found")
	case "":
		return problem.Internal("asset adjustment execution failed")
	default:
		return problem.New(500, code, "Internal Error", "asset adjustment execution failed")
	}
}

// RunReconciliation 运行对账处理流程。
func (s *Service) RunReconciliation(ctx context.Context, claims *auth.Claims, key string, req ReconcileReq) (ReconciliationDTO, error) {
	adminID, err := adminIDWithPermission(claims, "asset_reconcile:run")
	if err != nil {
		return ReconciliationDTO{}, err
	}
	if req.Scope != "all" && req.ScopeID == "" {
		return ReconciliationDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "scope_id is required")
	}
	if len(key) < 8 || len(key) > 128 {
		return ReconciliationDTO{}, problem.InvalidArgument("IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must be between 8 and 128 characters")
	}
	var existing ReconciliationJob
	if err := s.db.WithContext(ctx).Where("requested_by=? AND idempotency_key_hash=?", adminID, keyHash(key)).Take(&existing).Error; err == nil {
		return reconciliationDTO(existing), nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return ReconciliationDTO{}, err
	}
	now := s.now().UTC()
	scopeID := req.ScopeID
	job := ReconciliationJob{ID: s.ids.Next(), JobNo: "AR" + fmt.Sprint(s.ids.Next()), Scope: req.Scope, ScopeID: &scopeID, Mode: "dry_run", Status: "running", RequestedBy: adminID, IdempotencyKeyHash: keyHash(key), StartedAt: &now, RequestID: valueOrEmpty(requestctx.RequestIDPtr(ctx))}
	if err := s.db.Create(&job).Error; err != nil {
		if isDuplicateError(err) {
			if lookupErr := s.db.Where("requested_by=? AND idempotency_key_hash=?", adminID, keyHash(key)).Take(&existing).Error; lookupErr == nil {
				return reconciliationDTO(existing), nil
			}
		}
		return ReconciliationDTO{}, err
	}
	scanned, diffs, critical, err := s.reconcile(ctx, job)
	finished := s.now().UTC()
	status := "succeeded"
	if err != nil {
		status = "failed"
	}
	_ = s.db.Model(&ReconciliationJob{}).Where("id=?", job.ID).Updates(map[string]any{"status": status, "scanned_count": scanned, "difference_count": diffs, "critical_count": critical, "finished_at": finished}).Error
	if err != nil {
		return ReconciliationDTO{}, err
	}
	job.Status = status
	job.ScannedCount = scanned
	job.DifferenceCount = diffs
	job.CriticalCount = critical
	job.FinishedAt = &finished
	return reconciliationDTO(job), nil
}

// isDuplicateError 判断重复项错误是否成立。
func isDuplicateError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate")
}

// reconcile 执行uint 64对账。
func (s *Service) reconcile(ctx context.Context, job ReconciliationJob) (uint64, uint64, uint64, error) {
	query := s.db.WithContext(ctx).Table("asset_balances b").Select("b.id,b.account_id,b.bucket,b.amount,COALESCE(SUM(e.delta),0) expected").Joins("LEFT JOIN asset_entries e ON e.account_id=b.account_id AND e.bucket=b.bucket").Group("b.id,b.account_id,b.bucket,b.amount")
	if job.Scope == "account" {
		query = query.Where("b.account_id=?", *job.ScopeID)
	} else if job.Scope == "customer" {
		query = query.Joins("JOIN asset_accounts a ON a.id=b.account_id").Where("a.owner_type='customer' AND a.owner_id=?", *job.ScopeID)
	}
	var rows []struct {
		ID, AccountID    uint64
		Bucket           string
		Amount, Expected int64
	}
	if err := query.Scan(&rows).Error; err != nil {
		return 0, 0, 0, err
	}
	var diffs uint64
	for _, row := range rows {
		if row.Amount == row.Expected {
			continue
		}
		diffs++
		expected, actual := row.Expected, row.Amount
		item := ReconciliationItem{ID: s.ids.Next(), JobID: job.ID, ObjectType: "balance", ObjectID: idString(row.ID), DiffType: "projection_mismatch", ExpectedAmount: &expected, ActualAmount: &actual, Severity: "critical", Status: "open", Detail: jsonData(map[string]any{"account_id": idString(row.AccountID), "bucket": row.Bucket})}
		if err := s.db.Create(&item).Error; err != nil {
			return uint64(len(rows)), diffs, diffs, err
		}
	}
	var unbalanced []struct {
		ID  uint64
		Sum int64
	}
	if err := s.db.WithContext(ctx).Table("asset_transactions t").Select("t.id,SUM(e.delta) sum").Joins("JOIN asset_entries e ON e.transaction_id=t.id").Group("t.id").Having("SUM(e.delta)<>0").Scan(&unbalanced).Error; err != nil {
		return uint64(len(rows)), diffs, diffs, err
	}
	for _, row := range unbalanced {
		diffs++
		expected := int64(0)
		actual := row.Sum
		item := ReconciliationItem{ID: s.ids.Next(), JobID: job.ID, ObjectType: "transaction", ObjectID: idString(row.ID), DiffType: "unbalanced_entries", ExpectedAmount: &expected, ActualAmount: &actual, Severity: "critical", Status: "open"}
		if err := s.db.Create(&item).Error; err != nil {
			return uint64(len(rows)), diffs, diffs, err
		}
	}
	return uint64(len(rows)), diffs, diffs, nil
}

// ListReconciliations 查询Reconciliations列表。
func (s *Service) ListReconciliations(ctx context.Context, claims *auth.Claims, q pagination.Query) ([]ReconciliationDTO, string, error) {
	if _, err := adminIDWithPermission(claims, "asset_reconcile:view"); err != nil {
		return nil, "", err
	}
	var rows []ReconciliationJob
	if err := s.db.WithContext(ctx).Order("created_at DESC,id DESC").Offset(q.Offset).Limit(q.PageSize + 1).Find(&rows).Error; err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > q.PageSize {
		rows = rows[:q.PageSize]
		next = pagination.NextPageToken(q)
	}
	out := make([]ReconciliationDTO, len(rows))
	for i, row := range rows {
		out[i] = reconciliationDTO(row)
	}
	return out, next, nil
}

// RepairReconciliation 修复对账。
func (s *Service) RepairReconciliation(ctx context.Context, claims *auth.Claims, rawID string) (ReconciliationDTO, error) {
	if _, err := adminIDWithPermission(claims, "asset_reconcile:repair"); err != nil {
		return ReconciliationDTO{}, err
	}
	if !s.cfg.Asset.RepairEnabled {
		return ReconciliationDTO{}, problem.Forbidden("RECONCILIATION_REPAIR_FORBIDDEN", "asset projection repair is disabled")
	}
	id, err := parseID(rawID)
	if err != nil {
		return ReconciliationDTO{}, problem.NotFound("ASSET_RECONCILIATION_NOT_FOUND", "reconciliation not found")
	}
	var critical int64
	if err = s.db.Table("asset_reconciliation_items").Where("job_id=? AND diff_type='unbalanced_entries'", id).Count(&critical).Error; err != nil {
		return ReconciliationDTO{}, err
	}
	if critical > 0 {
		return ReconciliationDTO{}, problem.Forbidden("RECONCILIATION_REPAIR_FORBIDDEN", "unbalanced entries cannot be repaired automatically")
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var items []ReconciliationItem
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("job_id=? AND diff_type='projection_mismatch' AND status='open'", id).Find(&items).Error; err != nil {
			return err
		}
		for _, item := range items {
			balanceID, err := parseID(item.ObjectID)
			if err != nil {
				return err
			}
			var balance Balance
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id=?", balanceID).Take(&balance).Error; err != nil {
				return err
			}
			var expected int64
			if err := tx.Table("asset_entries").Select("COALESCE(SUM(delta),0)").Where("account_id=? AND bucket=?", balance.AccountID, balance.Bucket).Scan(&expected).Error; err != nil {
				return err
			}
			if err := tx.Model(&Balance{}).Where("id=?", balance.ID).Updates(map[string]any{"amount": expected, "version": gorm.Expr("version+1")}).Error; err != nil {
				return err
			}
			if err := tx.Model(&ReconciliationItem{}).Where("id=?", item.ID).Update("status", "repaired").Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ReconciliationDTO{}, err
	}
	var job ReconciliationJob
	if err = s.db.Where("id=?", id).Take(&job).Error; err != nil {
		return ReconciliationDTO{}, err
	}
	return reconciliationDTO(job), nil
}

// reconciliationDTO 返回对账DTO。
func reconciliationDTO(row ReconciliationJob) ReconciliationDTO {
	dto := ReconciliationDTO{ID: idString(row.ID), JobNo: row.JobNo, Scope: row.Scope, Mode: row.Mode, Status: row.Status, ScannedCount: row.ScannedCount, DifferenceCount: row.DifferenceCount, CriticalCount: row.CriticalCount, CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339Nano)}
	if row.ScopeID != nil {
		dto.ScopeID = *row.ScopeID
	}
	if row.FinishedAt != nil {
		dto.FinishedAt = row.FinishedAt.UTC().Format(time.RFC3339Nano)
	}
	return dto
}

// valueOrEmpty 返回值或空字符串。
func valueOrEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

var _ = strconv.Itoa
