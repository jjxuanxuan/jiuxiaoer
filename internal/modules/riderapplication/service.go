package riderapplication

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/metrics"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

const (
	applicantActorType = "rider_application_applicant"
	publicActorType    = "rider_application_public"
	loginFailureLimit  = 5
	loginFailureWindow = 15 * time.Minute
)

var rateLimitScript = goredis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return count
`)

type RiderSMSVerifier interface {
	VerifyRiderSMSCode(ctx context.Context, phone, code string) error
}

type Service struct {
	cfg    config.RiderApplicationConfig
	db     *gorm.DB
	redis  *goredis.Client
	ids    *snowflake.Generator
	idem   *idempotency.Store
	tokens auth.TokenManager
	metric *metricState
	sms    RiderSMSVerifier
}

// WithSMSVerifier 注入与正式骑手登录共享的短信验证码校验器。
func (s *Service) WithSMSVerifier(verifier RiderSMSVerifier) *Service {
	s.sms = verifier
	return s
}

// NewService 创建并初始化服务。
func NewService(cfg config.Config, db *gorm.DB, redisClient *goredis.Client, ids *snowflake.Generator, registries ...*metrics.Registry) *Service {
	service := &Service{
		cfg:    cfg.RiderApplication,
		db:     db,
		redis:  redisClient,
		ids:    ids,
		idem:   idempotency.NewStore(db),
		tokens: auth.NewTokenManager(cfg.JWT),
	}
	var registry *metrics.Registry
	if len(registries) > 0 {
		registry = registries[0]
	}
	service.metric = newMetricState(db, registry)
	return service
}

// Submit 返回Submit。
func (s *Service) Submit(ctx context.Context, ip, method, path, key string, input SubmitRequest) (ApplicationDTO, error) {
	if err := s.requireEnabled(); err != nil {
		return ApplicationDTO{}, err
	}
	req, shopIDs, err := input.normalized(s.cfg.MaxShops)
	if err != nil {
		return ApplicationDTO{}, err
	}
	if err := s.checkRate(ctx, "submit_ip", ip, s.cfg.SubmitIPRatePerHour, time.Hour); err != nil {
		return ApplicationDTO{}, err
	}
	if err := s.checkRate(ctx, "submit_phone", req.Phone, s.cfg.SubmitPhoneRatePerDay, 24*time.Hour); err != nil {
		return ApplicationDTO{}, err
	}
	if len(key) < 8 || len(key) > 128 {
		return ApplicationDTO{}, problem.InvalidArgument("IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must be between 8 and 128 characters")
	}
	requestHash := s.hmacJSON(req)
	actorID := s.publicActorID(req.Phone)
	var out ApplicationDTO
	replayed, err := s.idem.ReplayCompleted(ctx, s.db, publicActorType, actorID, path, key, requestHash, &out)
	if err != nil {
		mapped := s.mapSubmitError(ctx, err, req.Phone)
		s.metric.incSubmission(metricResult(mapped))
		return ApplicationDTO{}, mapped
	}
	if replayed {
		s.metric.incSubmission("success")
		return out, nil
	}
	if s.sms == nil {
		return ApplicationDTO{}, problem.New(503, "SMS_VERIFIER_UNAVAILABLE", "Service Unavailable", "rider sms verification is unavailable")
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idem.Start(ctx, tx, s.ids.Next(), publicActorType, actorID, method, path, key, requestHash)
		if err != nil {
			return err
		}
		if !started {
			return cachedResponse(ctx, s.idem, tx, publicActorType, actorID, path, key, &out)
		}
		// 在消耗短信一次性验证码前认领幂等键。这样精确重试无需第二个验证码
		// 即可返回原始响应，而每个新幂等键仍会消耗新的验证码。
		if err := s.sms.VerifyRiderSMSCode(ctx, req.Phone, req.Code); err != nil {
			return err
		}
		if err := s.ensureIdentityAvailable(ctx, tx, req.Phone); err != nil {
			return err
		}
		if err := validateActiveShops(ctx, tx, shopIDs); err != nil {
			return err
		}

		now := time.Now()
		accountID := s.ids.Next()
		applicationID := s.ids.Next()
		if err := sensitiveSession(tx).WithContext(ctx).Table("accounts").Create(map[string]any{
			"id": accountID, "account_type": "rider", "phone": req.Phone,
			"status": "disabled", "credential_version": 1,
			"created_by": accountID, "updated_by": accountID,
		}).Error; err != nil {
			return err
		}
		scopeJSON, _ := json.Marshal(req.ServiceScope)
		creator := accountID
		application := Application{
			ID: applicationID, ApplicationNo: fmt.Sprintf("RA%d", applicationID), AccountID: accountID,
			Name: req.Name, ServiceScope: datatypes.JSON(scopeJSON), Status: StatusSubmitted,
			SubmissionCount: 1, Version: 1,
			CreateIdempotencyKeyHash: s.hmacString("create-idempotency:" + req.Phone + ":" + key),
			CreateRequestHash:        requestHash, LastSubmittedAt: now, CreatedBy: &creator, UpdatedBy: &creator,
		}
		if err := tx.WithContext(ctx).Create(&application).Error; err != nil {
			return err
		}
		if err := s.writeAudit(ctx, tx, applicantActorType, accountID, "rider_application.submit", applicationID, map[string]any{
			"application_id": idString(applicationID), "status": StatusSubmitted, "submission_count": 1,
		}); err != nil {
			return err
		}
		if err := s.writeOutbox(ctx, tx, "rider.application.submitted", applicationID, map[string]any{
			"application_id": idString(applicationID), "application_no": application.ApplicationNo, "submission_no": 1,
		}); err != nil {
			return err
		}
		out = dtoFrom(applicationRecord{Application: application, Phone: req.Phone}, false)
		return s.idem.Succeed(ctx, tx, publicActorType, actorID, path, key, out)
	})
	if err != nil {
		mapped := s.mapSubmitError(ctx, err, req.Phone)
		s.metric.incSubmission(metricResult(mapped))
		return ApplicationDTO{}, mapped
	}
	s.metric.incSubmission("success")
	return out, nil
}

// Login 返回Login。
func (s *Service) Login(ctx context.Context, ip string, input LoginRequest) (LoginResponse, error) {
	if err := s.requireEnabled(); err != nil {
		return LoginResponse{}, err
	}
	req, err := input.normalized()
	if err != nil {
		return LoginResponse{}, err
	}
	if err := s.checkRate(ctx, "login_ip", ip, s.cfg.LoginIPRatePerMinute, time.Minute); err != nil {
		return LoginResponse{}, err
	}
	if err := s.checkRate(ctx, "login_phone", req.Phone, s.cfg.LoginPhoneRatePer15Minutes, 15*time.Minute); err != nil {
		return LoginResponse{}, err
	}
	if err := s.ensureLoginFailureAllowed(ctx, req.Phone); err != nil {
		return LoginResponse{}, err
	}
	if s.sms == nil {
		return LoginResponse{}, problem.New(503, "SMS_VERIFIER_UNAVAILABLE", "Service Unavailable", "rider sms verification is unavailable")
	}
	if err := s.sms.VerifyRiderSMSCode(ctx, req.Phone, req.Code); err != nil {
		if recordErr := s.recordLoginFailure(ctx, req.Phone); recordErr != nil {
			return LoginResponse{}, recordErr
		}
		return LoginResponse{}, err
	}

	var account auth.Account
	err = s.db.WithContext(ctx).
		Where("account_type = 'rider' AND phone = ? AND deleted_at IS NULL", req.Phone).
		First(&account).Error
	if err != nil {
		if recordErr := s.recordLoginFailure(ctx, req.Phone); recordErr != nil {
			return LoginResponse{}, recordErr
		}
		return LoginResponse{}, problem.Unauthorized("AUTH_INVALID_CREDENTIAL", "invalid phone or sms code")
	}
	var application Application
	if err := s.db.WithContext(ctx).Where("account_id = ?", account.ID).First(&application).Error; err != nil {
		if clearErr := s.clearLoginFailures(ctx, req.Phone); clearErr != nil {
			return LoginResponse{}, clearErr
		}
		return LoginResponse{}, problem.Unauthorized("AUTH_INVALID_CREDENTIAL", "invalid phone or sms code")
	}
	if err := s.clearLoginFailures(ctx, req.Phone); err != nil {
		return LoginResponse{}, err
	}
	if application.Status == StatusApproved {
		return LoginResponse{}, problem.Conflict("RIDER_APPLICATION_ALREADY_APPROVED", "application is approved; use formal rider login")
	}
	if account.Status != "disabled" || (application.Status != StatusSubmitted && application.Status != StatusRejected) {
		return LoginResponse{}, problem.Unauthorized("AUTH_INVALID_CREDENTIAL", "invalid phone or sms code")
	}
	token, err := s.tokens.IssueApplication(account.ID, application.ID, account.CredentialVersion, []string{
		"rider_application:self_view", "rider_application:self_update", "rider_application:self_resubmit",
	}, s.cfg.TokenTTL)
	if err != nil {
		return LoginResponse{}, err
	}
	_ = s.db.WithContext(ctx).Table("accounts").Where("id = ?", account.ID).Update("last_login_at", time.Now()).Error
	return LoginResponse{
		ApplicationAccessToken: token.Token,
		ExpiresIn:              token.ExpiresIn,
		ApplicationID:          idString(application.ID),
		ApplicationStatus:      application.Status,
	}, nil
}

// VerifyApplicationToken 核验申请令牌是否有效。
func (s *Service) VerifyApplicationToken(ctx context.Context, raw string) (*auth.Claims, error) {
	if err := s.requireEnabled(); err != nil {
		return nil, err
	}
	claims, err := s.tokens.ParseApplication(raw)
	if err != nil {
		return nil, problem.Unauthorized("AUTH_UNAUTHORIZED", "invalid application token")
	}
	accountID, err := parseID(claims.AccountID)
	if err != nil {
		return nil, problem.Unauthorized("AUTH_UNAUTHORIZED", "invalid application token")
	}
	applicationID, err := parseID(claims.ApplicationID)
	if err != nil {
		return nil, problem.Unauthorized("AUTH_UNAUTHORIZED", "invalid application token")
	}
	record, err := s.loadRecord(ctx, s.db, applicationID)
	if err != nil || record.AccountID != accountID || record.AccountStatus != "disabled" ||
		record.CredentialVersion != claims.CredentialVersion ||
		(record.Status != StatusSubmitted && record.Status != StatusRejected) {
		return nil, problem.Unauthorized("AUTH_UNAUTHORIZED", "application token is no longer valid")
	}
	if record.TokenInvalidBefore != nil && claims.IssuedAt != nil && claims.IssuedAt.Time.Before(record.TokenInvalidBefore.Truncate(time.Second)) {
		return nil, problem.Unauthorized("AUTH_UNAUTHORIZED", "application token is no longer valid")
	}
	return claims, nil
}

// requireEnabled 校验并确保启用状态满足要求。
func (s *Service) requireEnabled() error {
	if !s.cfg.Enabled {
		return problem.New(503, "RIDER_APPLICATION_DISABLED", "Service Unavailable", "rider application is disabled")
	}
	if s.db == nil {
		return problem.New(503, "DEPENDENCY_UNAVAILABLE", "Service Unavailable", "mysql is unavailable")
	}
	return nil
}

// checkRate 检查速率是否满足要求。
func (s *Service) checkRate(ctx context.Context, scope, subject string, limit int, window time.Duration) error {
	if s.redis == nil {
		return problem.New(503, "DEPENDENCY_UNAVAILABLE", "Service Unavailable", "redis is unavailable")
	}
	key := "rate:rider_application:" + scope + ":" + s.hmacString(subject)
	count, err := rateLimitScript.Run(ctx, s.redis, []string{key}, window.Milliseconds()).Int64()
	if err != nil {
		return problem.New(503, "DEPENDENCY_UNAVAILABLE", "Service Unavailable", "redis rate limiter is unavailable")
	}
	if count > int64(limit) {
		s.metric.incRateLimited(scope)
		retry := int64(window.Seconds())
		if ttl, err := s.redis.PTTL(ctx, key).Result(); err == nil && ttl > 0 {
			retry = int64(ttl.Seconds()) + 1
		}
		details := problem.TooManyRequests("RATE_LIMITED", "rate limit exceeded")
		details.Data = map[string]any{"retry_after_seconds": retry}
		return details
	}
	return nil
}

// ensureLoginFailureAllowed 确保当前登录失败次数仍在允许范围内。
func (s *Service) ensureLoginFailureAllowed(ctx context.Context, phone string) error {
	if s.redis == nil {
		return problem.New(503, "DEPENDENCY_UNAVAILABLE", "Service Unavailable", "redis is unavailable")
	}
	key := s.loginFailureKey(phone)
	count, err := s.redis.Get(ctx, key).Int64()
	if errors.Is(err, goredis.Nil) {
		return nil
	}
	if err != nil {
		return problem.New(503, "DEPENDENCY_UNAVAILABLE", "Service Unavailable", "redis login protection is unavailable")
	}
	if count >= loginFailureLimit {
		s.metric.incRateLimited("login_failure")
		details := problem.TooManyRequests("RATE_LIMITED", "too many failed login attempts")
		details.Data = map[string]any{"retry_after_seconds": int64(loginFailureWindow.Seconds())}
		return details
	}
	return nil
}

// recordLoginFailure 记录登录失败。
func (s *Service) recordLoginFailure(ctx context.Context, phone string) error {
	if s.redis == nil {
		return problem.New(503, "DEPENDENCY_UNAVAILABLE", "Service Unavailable", "redis is unavailable")
	}
	if _, err := rateLimitScript.Run(ctx, s.redis, []string{s.loginFailureKey(phone)}, loginFailureWindow.Milliseconds()).Int64(); err != nil {
		return problem.New(503, "DEPENDENCY_UNAVAILABLE", "Service Unavailable", "redis login protection is unavailable")
	}
	return nil
}

// clearLoginFailures 清空登录失败记录。
func (s *Service) clearLoginFailures(ctx context.Context, phone string) error {
	if s.redis == nil {
		return problem.New(503, "DEPENDENCY_UNAVAILABLE", "Service Unavailable", "redis is unavailable")
	}
	if err := s.redis.Del(ctx, s.loginFailureKey(phone)).Err(); err != nil {
		return problem.New(503, "DEPENDENCY_UNAVAILABLE", "Service Unavailable", "redis login protection is unavailable")
	}
	return nil
}

// loginFailureKey 返回登录失败计数键。
func (s *Service) loginFailureKey(phone string) string {
	return "rate:rider_application:login_failure:" + s.hmacString(strings.TrimSpace(phone))
}

// ensureIdentityAvailable 确保身份可用存在且处于可用状态。
func (s *Service) ensureIdentityAvailable(ctx context.Context, tx *gorm.DB, phone string) error {
	type identityRow struct {
		AccountID     uint64
		ApplicationID *uint64
	}
	var byPhone identityRow
	err := tx.WithContext(ctx).Table("accounts a").
		Select("a.id AS account_id, ra.id AS application_id").
		Joins("LEFT JOIN rider_applications ra ON ra.account_id = a.id").
		Where("a.account_type = 'rider' AND a.phone = ? AND a.deleted_at IS NULL", phone).
		Take(&byPhone).Error
	if err == nil {
		if byPhone.ApplicationID != nil {
			return problem.Conflict("RIDER_APPLICATION_EXISTS", "this phone already has a rider application")
		}
		return problem.Conflict("RIDER_IDENTITY_CONFLICT", "phone is already in use")
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return nil
}

// validateActiveShops 校验启用状态 Shops是否合法。
func validateActiveShops(ctx context.Context, tx *gorm.DB, shopIDs []uint64) error {
	var activeIDs []uint64
	if err := tx.WithContext(ctx).Table("shops").Clauses(clause.Locking{Strength: "SHARE"}).
		Select("id").Where("id IN ? AND status = 'active' AND deleted_at IS NULL", shopIDs).Find(&activeIDs).Error; err != nil {
		return err
	}
	if len(activeIDs) != len(shopIDs) {
		return problem.InvalidArgument("VALIDATION_FAILED", "service_scope contains a missing or inactive shop")
	}
	return nil
}

// loadRecord 加载记录。
func (s *Service) loadRecord(ctx context.Context, db *gorm.DB, applicationID uint64, locking ...bool) (applicationRecord, error) {
	query := db.WithContext(ctx).Table("rider_applications ra").
		Select("ra.*, a.phone, a.status AS account_status, a.credential_version, a.token_invalid_before").
		Joins("JOIN accounts a ON a.id = ra.account_id AND a.deleted_at IS NULL").
		Where("ra.id = ?", applicationID)
	if len(locking) > 0 && locking[0] {
		query = query.Clauses(clause.Locking{Strength: "UPDATE", Table: clause.Table{Name: "ra"}})
	}
	var record applicationRecord
	if err := query.Take(&record).Error; err != nil {
		return applicationRecord{}, err
	}
	return record, nil
}

// loadLockedRecord 加载并锁定申请记录。
func (s *Service) loadLockedRecord(ctx context.Context, tx *gorm.DB, applicationID uint64) (applicationRecord, error) {
	var application Application
	if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).First(&application, applicationID).Error; err != nil {
		return applicationRecord{}, err
	}
	var account auth.Account
	if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).First(&account, application.AccountID).Error; err != nil {
		return applicationRecord{}, err
	}
	return applicationRecord{
		Application:        application,
		Phone:              pointerString(account.Phone),
		AccountStatus:      account.Status,
		CredentialVersion:  account.CredentialVersion,
		TokenInvalidBefore: account.TokenInvalidBefore,
	}, nil
}

// mapSubmitError 映射并返回Submit 错误。
func (s *Service) mapSubmitError(ctx context.Context, err error, phone string) error {
	var details *problem.Details
	if errors.As(err, &details) {
		return err
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		var count int64
		_ = s.db.WithContext(ctx).Table("rider_applications ra").
			Joins("JOIN accounts a ON a.id = ra.account_id").
			Where("a.account_type = 'rider' AND a.phone = ? AND a.deleted_at IS NULL", phone).Count(&count).Error
		if count > 0 {
			return problem.Conflict("RIDER_APPLICATION_EXISTS", "this phone already has a rider application")
		}
		return problem.Conflict("RIDER_IDENTITY_CONFLICT", "phone is already in use")
	}
	return err
}

// publicActorID 返回公开请求的主体 ID。
func (s *Service) publicActorID(phone string) uint64 {
	sum := hmac.New(sha256.New, []byte(s.cfg.HMACSecret))
	_, _ = sum.Write([]byte("public-actor:" + phone))
	id := binary.BigEndian.Uint64(sum.Sum(nil)[:8])
	if id == 0 {
		return 1
	}
	return id
}

// hmacString 返回HMAC字符串。
func (s *Service) hmacString(value string) string {
	mac := hmac.New(sha256.New, []byte(s.cfg.HMACSecret))
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

// hmacJSON 返回HMACJSON。
func (s *Service) hmacJSON(value any) string {
	raw, _ := json.Marshal(value)
	return s.hmacString(string(raw))
}

// writeAudit 写入审计。
func (s *Service) writeAudit(ctx context.Context, tx *gorm.DB, actorType string, actorID uint64, action string, applicationID uint64, after any) error {
	raw, _ := json.Marshal(after)
	row := map[string]any{
		"id": s.ids.Next(), "actor_type": actorType, "actor_id": actorID, "action": action,
		"resource_type": "rider_application", "resource_id": applicationID,
		"after_data": datatypes.JSON(raw), "result": "success", "request_id": requestctx.RequestIDPtr(ctx),
	}
	if actorType == applicantActorType {
		row["account_id"] = actorID
	}
	return tx.WithContext(ctx).Table("audit_logs").Create(row).Error
}

// writeOutbox 写入发件箱事件。
func (s *Service) writeOutbox(ctx context.Context, tx *gorm.DB, eventType string, applicationID uint64, payload any) error {
	return s.writeOutboxAggregate(ctx, tx, eventType, "rider_application", applicationID, payload)
}

// writeOutboxAggregate 写入发件箱事件 Aggregate。
func (s *Service) writeOutboxAggregate(ctx context.Context, tx *gorm.DB, eventType, aggregateType string, aggregateID uint64, payload any) error {
	raw, _ := json.Marshal(payload)
	return tx.WithContext(ctx).Table("outbox_events").Create(map[string]any{
		"id": s.ids.Next(), "event_id": uuid.NewString(), "event_type": eventType,
		"aggregate_type": aggregateType, "aggregate_id": aggregateID,
		"payload": datatypes.JSON(raw), "status": "pending", "request_id": requestctx.RequestIDPtr(ctx),
	}).Error
}

// cachedResponse 返回缓存响应。
func cachedResponse(ctx context.Context, store *idempotency.Store, tx *gorm.DB, actorType string, actorID uint64, path, key string, out any) error {
	ok, err := store.CachedResponse(ctx, tx, actorType, actorID, path, key, out)
	if err != nil {
		return err
	}
	if !ok {
		return problem.Conflict("IDEMPOTENCY_CONFLICT", "idempotency response is not available")
	}
	return nil
}

// dtoFrom 根据数据记录构造 DTO。
func dtoFrom(record applicationRecord, fullPhone bool) ApplicationDTO {
	var scope ServiceScope
	_ = json.Unmarshal(record.ServiceScope, &scope)
	phone := maskPhone(record.Phone)
	if fullPhone {
		phone = record.Phone
	}
	dto := ApplicationDTO{
		ID: idString(record.ID), ApplicationNo: record.ApplicationNo, AccountID: idString(record.AccountID),
		Name: record.Name, Phone: phone, ServiceScope: scope,
		Status: record.Status, SubmissionCount: record.SubmissionCount, Version: record.Version,
		LastSubmittedAt: record.LastSubmittedAt.Format(time.RFC3339), CreatedAt: record.CreatedAt.Format(time.RFC3339),
	}
	if record.RiderID != nil {
		dto.RiderID = idString(*record.RiderID)
	}
	if record.LastReviewedAt != nil {
		dto.LastReviewedAt = record.LastReviewedAt.Format(time.RFC3339)
	}
	if record.ApprovedAt != nil {
		dto.ApprovedAt = record.ApprovedAt.Format(time.RFC3339)
	}
	return dto
}

// maskPhone 对手机号进行脱敏。
func maskPhone(phone string) string {
	if len(phone) != 11 {
		return ""
	}
	return phone[:3] + "****" + phone[7:]
}

// pointerString 返回字符串指针。
func pointerString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// parseID 解析并校验字符串形式的 ID。
func parseID(raw string) (uint64, error) {
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, fmt.Errorf("invalid id")
	}
	return id, nil
}

// idString 将数字 ID 转换为字符串。
func idString(id uint64) string { return strconv.FormatUint(id, 10) }

// hasPermission 判断是否存在权限。
func hasPermission(claims *auth.Claims, permission string) bool {
	if claims == nil {
		return false
	}
	for _, candidate := range claims.Permissions {
		if candidate == permission {
			return true
		}
	}
	return false
}

// applicantIDs 返回申请人 ID 列表。
func applicantIDs(claims *auth.Claims, permission string) (uint64, uint64, error) {
	if claims == nil || claims.TokenType != "application_access" || !hasPermission(claims, permission) {
		return 0, 0, problem.Forbidden("PERM_FORBIDDEN", "application permission denied")
	}
	accountID, err := parseID(claims.AccountID)
	if err != nil {
		return 0, 0, problem.Forbidden("PERM_FORBIDDEN", "invalid applicant identity")
	}
	applicationID, err := parseID(claims.ApplicationID)
	if err != nil {
		return 0, 0, problem.Forbidden("PERM_FORBIDDEN", "invalid applicant identity")
	}
	return accountID, applicationID, nil
}

// adminID 从认证声明中解析并返回管理员 ID。
func adminID(claims *auth.Claims, permission string) (uint64, error) {
	if claims == nil || claims.AccountType != "admin" || !hasPermission(claims, permission) {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "permission denied")
	}
	id, err := parseID(claims.AdminUserID)
	if err != nil {
		return 0, problem.Forbidden("PERM_FORBIDDEN", "invalid admin identity")
	}
	return id, nil
}

// stateConflict 返回状态冲突。
func stateConflict() error {
	return problem.Conflict("RIDER_APPLICATION_STATE_CONFLICT", "application state does not allow this action")
}

// versionConflict 返回版本冲突。
func versionConflict() error {
	return problem.Conflict("RIDER_APPLICATION_VERSION_CONFLICT", "application version is stale")
}

// isDuplicate 判断重复项是否成立。
func isDuplicate(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 || strings.Contains(strings.ToLower(fmt.Sprint(err)), "duplicate")
}

// sensitiveSession 返回敏感信息会话。
// sensitiveSession 防止注入或自定义 GORM 日志器把申请人凭据或手机号
// 插值到失败的 SQL 语句中。共享 MySQL 日志器同样使用参数化记录；
// 这是针对测试和替代构造器的纵深防御。
func sensitiveSession(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{Logger: db.Logger.LogMode(gormlogger.Silent)})
}
