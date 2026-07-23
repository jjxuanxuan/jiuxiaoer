package compliance

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/pagination"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/securevalue"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

const defaultPolicyVersion = "cp1-v2"

type Service struct {
	cfg      config.CP1Config
	db       *gorm.DB
	ids      *snowflake.Generator
	idem     *idempotency.Store
	provider Provider
}

// NewService 创建并初始化服务。
func NewService(cfg config.CP1Config, db *gorm.DB, ids *snowflake.Generator, p Provider) *Service {
	return &Service{cfg: cfg, db: db, ids: ids, idem: idempotency.NewStore(db), provider: p}
}

// GetMe 获取Me。
func (s *Service) GetMe(ctx context.Context, claims *auth.Claims) (VerificationDTO, error) {
	customer, err := customerID(claims)
	if err != nil {
		return VerificationDTO{}, err
	}
	var current Realname
	if err := s.db.WithContext(ctx).Where("customer_id=?", customer).First(&current).Error; err == nil {
		return realnameDTO(current), nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return VerificationDTO{}, err
	}
	var latest Request
	if err := s.db.WithContext(ctx).Where("customer_id=?", customer).Order("id DESC").First(&latest).Error; err == nil {
		return requestDTO(latest), nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return VerificationDTO{}, err
	}
	return VerificationDTO{CustomerID: idString(customer), Status: "unverified", AdultResult: AdultUnknown}, nil
}

// GetRequest 获取请求。
func (s *Service) GetRequest(ctx context.Context, claims *auth.Claims, idRaw string) (VerificationDTO, error) {
	customer, err := customerID(claims)
	if err != nil {
		return VerificationDTO{}, err
	}
	id, err := parseID(idRaw)
	if err != nil {
		return VerificationDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid verification id")
	}
	var row Request
	if err := s.db.WithContext(ctx).Where("id=? AND customer_id=?", id, customer).First(&row).Error; err != nil {
		return VerificationDTO{}, problem.NotFound("IDENTITY_VERIFICATION_NOT_FOUND", "verification not found")
	}
	return requestDTO(row), nil
}

// CreateSession 创建会话。
func (s *Service) CreateSession(ctx context.Context, claims *auth.Claims, method, path, key string, req CreateSessionReq) (VerificationDTO, error) {
	customer, err := customerID(claims)
	if err != nil {
		return VerificationDTO{}, err
	}
	if req.Purpose == "" {
		req.Purpose = "alcohol_purchase"
	}
	state, err := randomState()
	if err != nil {
		return VerificationDTO{}, err
	}
	var out VerificationDTO
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, startErr := s.idem.Start(ctx, tx, s.ids.Next(), "customer", customer, method, path, key, idempotency.RequestHash(req))
		if startErr != nil {
			return startErr
		}
		if !started {
			return cached(ctx, s.idem, tx, "customer", customer, path, key, &out)
		}
		id := s.ids.Next()
		session, providerErr := s.provider.CreateSession(ctx, ProviderSessionRequest{
			VerificationID: idString(id), SubjectReference: securevalue.HMAC(s.cfg.VerificationPepper, "identity-subject", idString(customer)),
			Purpose: req.Purpose, VerificationLevel: req.VerificationLevel, State: state,
		})
		if providerErr != nil {
			return problem.New(http.StatusServiceUnavailable, "IDENTITY_PROVIDER_UNAVAILABLE", "Service Unavailable", "identity verification is temporarily unavailable")
		}
		row := Request{
			ID: id, RequestNo: fmt.Sprintf("IV%d", id), CustomerID: customer, Provider: s.provider.Code(), ProviderRequestID: &session.RequestID,
			Purpose: req.Purpose, StateHash: securevalue.HMAC(s.cfg.VerificationPepper, "identity-state", state), Status: StatusPending,
			AdultResult: AdultUnknown, VerificationLevel: req.VerificationLevel, PolicyVersion: defaultPolicyVersion,
			ConsentVersion: req.ConsentVersion, Attempts: 1, SessionExpiresAt: &session.ExpiresAt,
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		out = requestDTO(row)
		out.SessionURL = session.URL
		if err := audit(ctx, tx, s.ids.Next(), "customer", customer, "identity_verification.session_created", id, map[string]any{
			"provider": row.Provider, "purpose": row.Purpose, "verification_level": row.VerificationLevel, "consent_version": row.ConsentVersion,
		}); err != nil {
			return err
		}
		return s.idem.Succeed(ctx, tx, "customer", customer, path, key, out)
	})
	return out, err
}

// Callback 返回回调。
// Callback treats a provider callback only as a change notification. The
// provider adapter verifies the callback, then Query supplies the authoritative
// result that is persisted below.
func (s *Service) Callback(ctx context.Context, providerCode string, headers http.Header, body []byte) error {
	if providerCode != s.provider.Code() {
		return problem.NotFound("IDENTITY_PROVIDER_NOT_FOUND", "identity provider not found")
	}
	event, err := s.provider.ParseCallback(ctx, headers, body)
	if err != nil {
		return problem.Unauthorized("IDENTITY_CALLBACK_INVALID", "identity callback verification failed")
	}
	result, err := s.provider.Query(ctx, event.ProviderRequestID)
	if err != nil {
		return problem.New(http.StatusServiceUnavailable, "IDENTITY_PROVIDER_UNAVAILABLE", "Service Unavailable", "identity verification result is temporarily unavailable")
	}
	if result.RequestID != event.ProviderRequestID || !validProviderResult(result) {
		return problem.Unauthorized("IDENTITY_CALLBACK_INVALID", "identity provider result is invalid")
	}
	payloadHash := digest(body)
	resultHash := resultDigest(result)
	now := time.Now()
	var callbackReject error
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		callback := Callback{
			ID: s.ids.Next(), Provider: providerCode, ProviderEventID: event.EventID, ProviderRequestID: event.ProviderRequestID,
			PayloadHash: payloadHash, SignatureValid: true, ProcessStatus: "received", ReceivedAt: now, RequestID: requestctx.RequestIDPtr(ctx),
		}
		created := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "provider"}, {Name: "provider_event_id"}}, DoNothing: true}).Create(&callback)
		if created.Error != nil {
			return created.Error
		}
		if created.RowsAffected == 0 {
			return nil
		}
		finishCallback := func(status string, code *string) error {
			return tx.Model(&Callback{}).Where("id=?", callback.ID).Updates(map[string]any{"process_status": status, "error_code": code, "processed_at": &now}).Error
		}
		var row Request
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("provider=? AND provider_request_id=?", providerCode, event.ProviderRequestID).First(&row).Error; err != nil {
			code := "IDENTITY_REQUEST_NOT_FOUND"
			callbackReject = problem.NotFound(code, "identity verification request not found")
			return finishCallback("ignored", &code)
		}
		if !securevalue.EqualHMAC(row.StateHash, securevalue.HMAC(s.cfg.VerificationPepper, "identity-state", event.State)) {
			code := "IDENTITY_STATE_MISMATCH"
			callbackReject = problem.Unauthorized(code, "identity callback verification failed")
			return finishCallback("failed", &code)
		}
		if terminal(row.Status) && row.Status != result.Status && result.Status != StatusRevoked {
			code := "IDENTITY_STATE_REGRESSION"
			callbackReject = problem.Conflict(code, "identity result cannot regress")
			return finishCallback("ignored", &code)
		}
		updates := map[string]any{
			"status": result.Status, "adult_result": result.AdultResult, "verification_level": result.VerificationLevel,
			"result_hash": resultHash, "callback_event_id": event.EventID, "callback_payload_hash": payloadHash,
			"callback_received_at": &now, "expires_at": result.ValidUntil, "failure_code": nil,
		}
		if result.Status == StatusVerified {
			updates["verified_at"] = &now
		}
		if result.Status == StatusRejected {
			code := "PROVIDER_REJECTED"
			updates["failure_code"] = code
		}
		if result.Status == StatusRevoked {
			reason := "provider_revoked"
			updates["revoked_at"] = &now
			updates["revoked_reason"] = reason
		}
		if err := tx.Model(&Request{}).Where("id=?", row.ID).Updates(updates).Error; err != nil {
			return err
		}
		if err := s.upsertCurrent(tx, row, result, resultHash, now); err != nil {
			return err
		}
		if err := s.createOutcomeEvent(ctx, tx, row, result, now); err != nil {
			return err
		}
		if err := audit(ctx, tx, s.ids.Next(), "provider", row.ID, "identity_verification.callback_processed", row.ID, map[string]any{
			"provider": providerCode, "status": result.Status, "adult_result": result.AdultResult, "verification_level": result.VerificationLevel,
		}); err != nil {
			return err
		}
		return finishCallback("processed", nil)
	})
	if err != nil {
		return err
	}
	return callbackReject
}

// List 查询核验 DTO列表列表。
func (s *Service) List(ctx context.Context, claims *auth.Claims, q pagination.Query, status string) ([]VerificationDTO, string, error) {
	if _, err := adminID(claims, "identity_verification:list"); err != nil {
		return nil, "", err
	}
	db := s.db.WithContext(ctx).Model(&Request{})
	if status != "" {
		db = db.Where("status=?", status)
	}
	var rows []Request
	if err := db.Order("id DESC").Offset(q.Offset).Limit(q.PageSize + 1).Find(&rows).Error; err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > q.PageSize {
		rows = rows[:q.PageSize]
		next = pagination.NextPageToken(q)
	}
	out := make([]VerificationDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, requestDTO(row))
	}
	return out, next, nil
}

// Review 审核核验DTO。
func (s *Service) Review(ctx context.Context, claims *auth.Claims, method, path, key, idRaw string, req ReviewReq) (VerificationDTO, error) {
	actor, err := adminID(claims, "identity_verification:review")
	if err != nil {
		return VerificationDTO{}, err
	}
	id, err := parseID(idRaw)
	if err != nil {
		return VerificationDTO{}, problem.InvalidArgument("VALIDATION_FAILED", "invalid verification id")
	}
	var out VerificationDTO
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, startErr := s.idem.Start(ctx, tx, s.ids.Next(), "admin", actor, method, path, key, idempotency.ResourceRequestHash("identity_verification.review", id, req))
		if startErr != nil {
			return startErr
		}
		if !started {
			return cached(ctx, s.idem, tx, "admin", actor, path, key, &out)
		}
		var row Request
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, id).Error; err != nil {
			return problem.NotFound("IDENTITY_VERIFICATION_NOT_FOUND", "verification not found")
		}
		if req.Decision == "approved" && row.AdultResult != AdultAdult {
			return problem.New(http.StatusUnprocessableEntity, "UNDERAGE_RESTRICTED", "Unprocessable Entity", "an underage or unknown result cannot be approved")
		}
		now := time.Now()
		status := StatusVerified
		var validUntil *time.Time
		if req.ExpiresAt != "" {
			parsed, parseErr := time.Parse(time.RFC3339, req.ExpiresAt)
			if parseErr != nil {
				return problem.InvalidArgument("VALIDATION_FAILED", "invalid valid_until")
			}
			if req.Decision == "approved" && !parsed.After(now) {
				return problem.InvalidArgument("VALIDATION_FAILED", "valid_until must be in the future")
			}
			validUntil = &parsed
		}
		if req.Decision == "rejected" {
			status = StatusRejected
		}
		result := ProviderResult{
			RequestID: value(row.ProviderRequestID), Status: status, AdultResult: row.AdultResult,
			VerificationLevel: row.VerificationLevel, ValidUntil: validUntil, ResultReference: "manual-review:" + idString(id),
		}
		resultHash := resultDigest(result)
		updates := map[string]any{
			"status": status, "result_hash": resultHash, "expires_at": validUntil, "failure_code": nil,
			"revoked_at": nil, "revoked_reason": nil,
		}
		if status == StatusVerified {
			updates["verified_at"] = &now
		} else {
			reason := "manual_review_rejected"
			updates["revoked_at"] = &now
			updates["revoked_reason"] = reason
			updates["failure_code"] = "MANUAL_REJECTED"
		}
		if err := tx.Model(&Request{}).Where("id=?", id).Updates(updates).Error; err != nil {
			return err
		}
		row.Status = status
		row.ResultHash = &resultHash
		row.ExpiresAt = validUntil
		if status == StatusVerified {
			row.VerifiedAt = &now
			row.RevokedAt = nil
			row.RevokedReason = nil
		} else {
			reason := "manual_review_rejected"
			row.RevokedAt = &now
			row.RevokedReason = &reason
		}
		if err := s.upsertCurrent(tx, row, result, resultHash, now); err != nil {
			return err
		}
		if err := s.createOutcomeEvent(ctx, tx, row, result, now); err != nil {
			return err
		}
		out = requestDTO(row)
		if err := audit(ctx, tx, s.ids.Next(), "admin", actor, "identity_verification.review", id, map[string]any{"decision": req.Decision, "reason": req.Reason}); err != nil {
			return err
		}
		return s.idem.Succeed(ctx, tx, "admin", actor, path, key, out)
	})
	return out, err
}

// upsertCurrent 新增或更新Current。
func (s *Service) upsertCurrent(tx *gorm.DB, row Request, result ProviderResult, resultHash string, now time.Time) error {
	var revokedAt *time.Time
	var revokedReason *string
	if result.Status == StatusRejected || result.Status == StatusRevoked {
		revokedAt = &now
		reason := "provider_" + result.Status
		if len(result.ResultReference) >= len("manual-review:") && result.ResultReference[:len("manual-review:")] == "manual-review:" {
			reason = "manual_review_rejected"
		}
		revokedReason = &reason
	}
	subject := result.Subject
	current := Realname{
		CustomerID: row.CustomerID, RequestID: row.ID, Status: result.Status, Provider: row.Provider, ProviderSubject: optional(subject),
		AdultResult: result.AdultResult, VerificationLevel: result.VerificationLevel, PolicyVersion: row.PolicyVersion,
		ResultHash: &resultHash, VerifiedAt: nil, ExpiresAt: result.ValidUntil, RevokedAt: revokedAt, RevokedReason: revokedReason, Version: 1,
	}
	if result.Status == StatusVerified {
		current.VerifiedAt = &now
	}
	updateColumns := []string{
		"request_id", "status", "provider", "adult_result", "verification_level", "policy_version", "result_hash",
		"verified_at", "expires_at", "revoked_at", "revoked_reason", "updated_at",
	}
	if result.Subject != "" {
		updateColumns = append(updateColumns, "provider_subject")
	}
	updates := clause.AssignmentColumns(updateColumns)
	updates = append(updates, clause.Assignment{Column: clause.Column{Name: "version"}, Value: gorm.Expr("version + 1")})
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "customer_id"}}, DoUpdates: updates}).Create(&current).Error
}

// createOutcomeEvent 创建Outcome 事件。
func (s *Service) createOutcomeEvent(ctx context.Context, tx *gorm.DB, row Request, result ProviderResult, now time.Time) error {
	eventType := "identity.verification.updated"
	errorCode := ""
	if result.Status == StatusRejected {
		eventType = "identity.verification.failed"
		errorCode = "PROVIDER_REJECTED"
		if len(result.ResultReference) >= len("manual-review:") && result.ResultReference[:len("manual-review:")] == "manual-review:" {
			errorCode = "MANUAL_REJECTED"
		}
	}
	if result.Status == StatusRevoked {
		eventType = "identity.verification.revoked"
		errorCode = "PROVIDER_REVOKED"
	}
	payload := map[string]any{
		"verification_request_id": idString(row.ID), "customer_id": idString(row.CustomerID), "status": result.Status,
		"adult_result": result.AdultResult, "verification_level": result.VerificationLevel, "policy_version": row.PolicyVersion,
	}
	if errorCode != "" {
		payload["error_code"] = errorCode
	}
	raw, _ := json.Marshal(payload)
	return tx.WithContext(ctx).Table("outbox_events").Create(map[string]any{
		"id": s.ids.Next(), "event_id": uuid.NewString(), "event_type": eventType, "event_version": 1,
		"aggregate_type": "identity_verification", "aggregate_id": row.ID, "payload": datatypes.JSON(raw), "status": "pending",
		"request_id": requestctx.RequestIDPtr(ctx), "created_at": now,
	}).Error
}

// CheckOrder 检查订单是否满足要求。
// CheckOrder is deliberately called before inventory locks or writes.
func CheckOrder(ctx context.Context, tx *gorm.DB, cfg config.CP1Config, customerID uint64, shopProductIDs []uint64) (datatypes.JSON, error) {
	if cfg.ComplianceMode == "off" {
		return nil, nil
	}
	var restricted []uint64
	if err := tx.WithContext(ctx).Table("shop_products sp").Select("sp.id").Joins("JOIN products p ON p.id=sp.product_id").Joins("JOIN categories c ON c.id=p.category_id").Where("sp.id IN ? AND (p.age_restricted=1 OR c.age_restricted=1)", shopProductIDs).Scan(&restricted).Error; err != nil {
		return nil, err
	}
	if len(restricted) == 0 {
		return nil, nil
	}
	var current Realname
	err := tx.WithContext(ctx).Where("customer_id=?", customerID).First(&current).Error
	now := time.Now()
	valid := err == nil && current.Status == StatusVerified && current.AdultResult == AdultAdult && current.RevokedAt == nil && (current.ExpiresAt == nil || current.ExpiresAt.After(now))
	if cfg.ComplianceMode == "enforce" && !valid {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			var pending int64
			tx.WithContext(ctx).Model(&Request{}).
				Where("customer_id=? AND status IN ? AND (session_expires_at IS NULL OR session_expires_at>?)", customerID, []string{StatusCreating, StatusPending}, now).
				Count(&pending)
			if pending > 0 {
				return nil, problem.Conflict("IDENTITY_VERIFICATION_PENDING", "identity verification is still processing")
			}
			return nil, problem.New(http.StatusUnprocessableEntity, "REALNAME_REQUIRED", "Unprocessable Entity", "valid real-name verification required")
		}
		if current.Status == StatusVerified && current.AdultResult == AdultMinor {
			return nil, problem.New(http.StatusUnprocessableEntity, "UNDERAGE_RESTRICTED", "Unprocessable Entity", "restricted products require an adult customer")
		}
		return nil, problem.New(http.StatusUnprocessableEntity, "REALNAME_REQUIRED", "Unprocessable Entity", "valid real-name verification required")
	}
	verificationID := ""
	adultResult := AdultUnknown
	status := "unverified"
	level := ""
	policy := defaultPolicyVersion
	if err == nil {
		verificationID = idString(current.RequestID)
		adultResult = current.AdultResult
		status = current.Status
		level = current.VerificationLevel
		policy = current.PolicyVersion
	}
	raw, _ := json.Marshal(map[string]any{
		"policy_version": policy, "verification_id": verificationID, "status": status, "adult_result": adultResult,
		"verification_level": level, "checked_at": now.Format(time.RFC3339), "age_restricted_shop_product_ids": restricted,
		"mode": cfg.ComplianceMode, "would_allow": valid,
	})
	return raw, nil
}

// validProviderResult 判断有效提供器结果。
func validProviderResult(result ProviderResult) bool {
	if result.Status != StatusVerified && result.Status != StatusRejected && result.Status != StatusRevoked {
		return false
	}
	if result.AdultResult != AdultAdult && result.AdultResult != AdultMinor && result.AdultResult != AdultUnknown {
		return false
	}
	return result.VerificationLevel != ""
}

// terminal 判断terminal。
func terminal(status string) bool {
	return status == StatusVerified || status == StatusRejected || status == StatusRevoked || status == StatusExpired
}

// randomState 返回random 状态。
func randomState() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// resultDigest 返回结果摘要。
func resultDigest(result ProviderResult) string {
	raw, _ := json.Marshal(map[string]any{
		"request_id": result.RequestID, "subject": result.Subject, "status": result.Status, "adult_result": result.AdultResult,
		"verification_level": result.VerificationLevel, "valid_until": ts(result.ValidUntil), "result_reference": result.ResultReference,
	})
	return digest(raw)
}

// digest 计算字符串的摘要。
func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// customerID 返回用户ID。
func customerID(claims *auth.Claims) (uint64, error) {
	if claims == nil || claims.AccountType != "customer" {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "customer required")
	}
	return parseID(claims.CustomerID)
}

// adminID 从认证声明中解析并返回管理员 ID。
func adminID(claims *auth.Claims, permission string) (uint64, error) {
	if claims == nil || claims.AccountType != "admin" || !has(claims.Permissions, permission) {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
	}
	return parseID(claims.AdminUserID)
}

// has 判断是否存在合规。
func has(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

// parseID 解析并校验字符串形式的 ID。
func parseID(value string) (uint64, error) {
	id, err := strconv.ParseUint(value, 10, 64)
	if err != nil || id == 0 {
		return 0, fmt.Errorf("invalid id")
	}
	return id, nil
}

// idString 将数字 ID 转换为字符串。
func idString(value uint64) string { return strconv.FormatUint(value, 10) }

// ts 返回ts。
func ts(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.Format(time.RFC3339)
}

// value 返回值。
func value(input *string) string {
	if input == nil {
		return ""
	}
	return *input
}

// optional 返回optional。
func optional(input string) *string {
	if input == "" {
		return nil
	}
	return &input
}

// requestDTO 返回请求DTO。
func requestDTO(row Request) VerificationDTO {
	status := row.Status
	if status == StatusPending && row.SessionExpiresAt != nil && !row.SessionExpiresAt.After(time.Now()) {
		status = StatusExpired
	}
	return VerificationDTO{
		ID: idString(row.ID), CustomerID: idString(row.CustomerID), Status: status, Provider: row.Provider,
		AdultResult: row.AdultResult, VerificationLevel: row.VerificationLevel, SessionExpiresAt: ts(row.SessionExpiresAt),
		VerifiedAt: ts(row.VerifiedAt), ValidUntil: ts(row.ExpiresAt), RevokedAt: ts(row.RevokedAt),
	}
}

// realnameDTO 返回realname DTO。
func realnameDTO(row Realname) VerificationDTO {
	status := row.Status
	if status == StatusVerified && row.ExpiresAt != nil && !row.ExpiresAt.After(time.Now()) {
		status = StatusExpired
	}
	return VerificationDTO{
		ID: idString(row.RequestID), CustomerID: idString(row.CustomerID), Status: status, Provider: row.Provider,
		AdultResult: row.AdultResult, VerificationLevel: row.VerificationLevel, VerifiedAt: ts(row.VerifiedAt),
		ValidUntil: ts(row.ExpiresAt), RevokedAt: ts(row.RevokedAt),
	}
}

// cached 返回缓存。
func cached(ctx context.Context, store *idempotency.Store, tx *gorm.DB, actorType string, actorID uint64, path, key string, target any) error {
	ok, err := store.CachedResponse(ctx, tx, actorType, actorID, path, key, target)
	if err != nil {
		return err
	}
	if !ok {
		return problem.Conflict("IDEMPOTENCY_CONFLICT", "idempotency response missing")
	}
	return nil
}

// audit 返回审计。
func audit(ctx context.Context, tx *gorm.DB, id uint64, actorType string, actorID uint64, action string, resourceID uint64, after any) error {
	raw, _ := json.Marshal(after)
	return tx.Table("audit_logs").Create(map[string]any{
		"id": id, "actor_type": actorType, "actor_id": actorID, "action": action, "resource_type": "identity_verification",
		"resource_id": resourceID, "after_data": datatypes.JSON(raw), "result": "success", "request_id": requestctx.RequestIDPtr(ctx),
	}).Error
}
