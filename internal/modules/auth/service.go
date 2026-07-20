package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/requestctx"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

var phonePattern = regexp.MustCompile(`^1[3-9][0-9]{9}$`)

const (
	smsCodeTTL              = 5 * time.Minute
	smsSendInterval         = 60 * time.Second
	smsDailyLimit           = 10
	loginFailureLimit       = 5
	loginFailureWindow      = 15 * time.Minute
	loginFailureCounterBase = "rate:login:failure:"
)

// Identity 是登录后的统一身份结果，同时用于签发 JWT claims 和返回 profile。
// 这里显式保留各角色 ID，避免下游 service 再查询身份表。
type Identity struct {
	AccountType       string
	AccountID         uint64
	CustomerID        uint64
	AdminUserID       uint64
	MerchantUserID    uint64
	MerchantID        uint64
	AuthorizedShopIDs []uint64
	RiderID           uint64
	RoleCode          string
	Permissions       []string
	Profile           map[string]any
}

// AccountIDString 返回账户ID字符串。
func (i Identity) AccountIDString() string {
	return strconv.FormatUint(i.AccountID, 10)
}

// toClaims 将当前值转换为认证声明。
func (i Identity) toClaims(tokenType string) Claims {
	return Claims{
		TokenType:         tokenType,
		AccountType:       i.AccountType,
		AccountID:         strconv.FormatUint(i.AccountID, 10),
		CustomerID:        idString(i.CustomerID),
		AdminUserID:       idString(i.AdminUserID),
		MerchantUserID:    idString(i.MerchantUserID),
		MerchantID:        idString(i.MerchantID),
		AuthorizedShopIDs: uint64Strings(i.AuthorizedShopIDs),
		RiderID:           idString(i.RiderID),
		RoleCode:          i.RoleCode,
		Permissions:       i.Permissions,
	}
}

// Service 负责认证流程以及 token/session 校验。
// Redis 用于验证码、会话和登出黑名单；MySQL 仍是账号数据源。
type Service struct {
	cfg        config.Config
	repo       *Repository
	redis      *goredis.Client
	tokens     TokenManager
	idGen      *snowflake.Generator
	idStore    *idempotency.Store
	wechat     WeChatProvider
	sms        SMSProvider
	mockCode   string
	logoutHook func(context.Context, string) error
}

func (s *Service) WithCustomerLogoutHook(hook func(context.Context, string) error) *Service {
	s.logoutHook = hook
	return s
}

// NewService 创建并初始化服务。
func NewService(cfg config.Config, db *gorm.DB, redisClient *goredis.Client, idGen *snowflake.Generator, providers ...WeChatProvider) *Service {
	service := &Service{
		cfg:      cfg,
		repo:     NewRepository(db),
		redis:    redisClient,
		tokens:   NewTokenManager(cfg.JWT),
		idGen:    idGen,
		idStore:  idempotency.NewStore(db),
		mockCode: "123456",
	}
	if len(providers) > 0 {
		service.wechat = providers[0]
	}
	if cfg.SMS.Enabled && cfg.Feature.SMSMockEnabled {
		service.sms = mockSMSProvider{}
	}
	return service
}

// WithSMSProvider injects the production SMS adapter. A nil provider leaves
// the local mock (when enabled) or the disabled state unchanged.
func (s *Service) WithSMSProvider(provider SMSProvider) *Service {
	if provider != nil {
		s.sms = provider
	}
	return s
}

// SendCustomerCode 生成、保存并通过当前短信提供器发送顾客登录验证码。
// 本地 Mock 保持固定验证码，以便接口测试具备确定性。
func (s *Service) SendCustomerCode(ctx context.Context, req SendCodeReq) error {
	return s.sendSMSCode(ctx, "customer", req.Phone)
}

// SendRiderCode 发送骑手登录及申请认证验证码。
func (s *Service) SendRiderCode(ctx context.Context, req SendCodeReq) error {
	return s.sendSMSCode(ctx, "rider", req.Phone)
}

func (s *Service) sendSMSCode(ctx context.Context, scope, phone string) error {
	if !phonePattern.MatchString(phone) {
		return problem.InvalidArgument("AUTH_INVALID_PHONE", "invalid phone")
	}
	if !s.cfg.SMS.Enabled || s.sms == nil {
		return dependencyUnavailable("sms provider is unavailable")
	}
	if s.redis == nil {
		return dependencyUnavailable("redis is required for sms login")
	}
	if err := s.ensureSMSSendAllowed(ctx, scope, phone); err != nil {
		return err
	}
	code, err := s.newSMSCode()
	if err != nil {
		return problem.Internal("failed to generate sms code")
	}
	key := smsLoginKey(scope, phone)
	if err := s.redis.Set(ctx, key, code, smsCodeTTL).Err(); err != nil {
		return err
	}
	if err := s.sms.SendVerificationCode(ctx, phone, code, smsCodeTTL); err != nil {
		_ = deleteSMSCodeScript.Run(ctx, s.redis, []string{key}, code).Err()
		return dependencyUnavailable("sms provider is unavailable")
	}
	return nil
}

func (s *Service) newSMSCode() (string, error) {
	if s.cfg.Feature.SMSMockEnabled {
		return s.mockCode, nil
	}
	value, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", value.Int64()), nil
}

// CustomerSMSLogin 首次登录时创建 C 端账号，并返回完整 token 对。
func (s *Service) CustomerSMSLogin(ctx context.Context, req SmsLoginReq) (*TokenResp, error) {
	if !phonePattern.MatchString(req.Phone) {
		return nil, problem.InvalidArgument("AUTH_INVALID_PHONE", "invalid phone")
	}
	if err := s.ensureLoginAllowed(ctx, "customer_sms", req.Phone); err != nil {
		return nil, err
	}
	if err := s.verifySMSCode(ctx, "customer", req.Phone, req.Code); err != nil {
		s.recordLoginFailure(ctx, "customer_sms", req.Phone)
		return nil, err
	}
	if !s.repo.DBConfigured() {
		return nil, problem.Internal("mysql is not configured")
	}

	account, customer, err := s.repo.FindOrCreateCustomerByPhone(ctx, req.Phone, s.idGen.Next)
	if err != nil {
		return nil, err
	}
	if account.Status != "active" || customer.Status != "active" {
		return nil, problem.Forbidden("AUTH_ACCOUNT_DISABLED", "account disabled")
	}

	identity := Identity{
		AccountType: "customer",
		AccountID:   account.ID,
		CustomerID:  customer.ID,
		Permissions: []string{"customer:login", "product:list", "order:create"},
		Profile: map[string]any{
			"customer_id": idString(customer.ID),
			"phone":       req.Phone,
		},
	}
	resp, err := s.issueResponse(ctx, identity)
	if err != nil {
		return nil, err
	}
	s.resetLoginFailures(ctx, "customer_sms", req.Phone)
	if err := s.createAudit(ctx, identity, "auth.login", "account", identity.AccountID, "success", map[string]any{"account_type": identity.AccountType}); err != nil {
		return nil, err
	}
	return resp, nil
}

// RiderSMSLogin 只允许已审核、已激活的骑手使用手机号验证码登录。
func (s *Service) RiderSMSLogin(ctx context.Context, req SmsLoginReq) (*TokenResp, error) {
	if !phonePattern.MatchString(req.Phone) {
		return nil, problem.InvalidArgument("AUTH_INVALID_PHONE", "invalid phone")
	}
	if err := s.ensureLoginAllowed(ctx, "rider_sms", req.Phone); err != nil {
		return nil, err
	}
	if err := s.VerifyRiderSMSCode(ctx, req.Phone, req.Code); err != nil {
		s.recordLoginFailure(ctx, "rider_sms", req.Phone)
		return nil, err
	}
	if !s.repo.DBConfigured() {
		return nil, problem.Internal("mysql is not configured")
	}
	account, err := s.repo.FindAccountByPhone(ctx, "rider", req.Phone)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		s.recordLoginFailure(ctx, "rider_sms", req.Phone)
		return nil, problem.Unauthorized("AUTH_INVALID_CREDENTIAL", "invalid phone or sms code")
	}
	if err != nil {
		return nil, err
	}
	if account.Status != "active" {
		return nil, problem.Forbidden("AUTH_ACCOUNT_DISABLED", "account disabled or pending review")
	}
	identity, err := s.identityForAccount(ctx, account)
	if err != nil {
		return nil, err
	}
	resp, err := s.issueResponse(ctx, identity)
	if err != nil {
		return nil, err
	}
	s.resetLoginFailures(ctx, "rider_sms", req.Phone)
	if err := s.createAudit(ctx, identity, "auth.sms_login", "account", identity.AccountID, "success", map[string]any{"account_type": "rider", "phone_masked": maskPhone(req.Phone)}); err != nil {
		return nil, err
	}
	return resp, nil
}

// VerifyRiderSMSCode 供骑手申请模块复用同一验证码命名空间和校验规则。
func (s *Service) VerifyRiderSMSCode(ctx context.Context, phone, code string) error {
	if !phonePattern.MatchString(phone) {
		return problem.InvalidArgument("AUTH_INVALID_PHONE", "invalid phone")
	}
	return s.verifySMSCode(ctx, "rider", phone, code)
}

// WeChatLogin 返回We Chat Login。
func (s *Service) WeChatLogin(ctx context.Context, req WeChatLoginReq) (*WeChatLoginResp, error) {
	if s.wechat == nil || !s.cfg.WeChat.AuthEnabled {
		return nil, problem.New(503, "PROVIDER_UNAVAILABLE", "Service Unavailable", "wechat login is unavailable")
	}
	subject := wechatRateSubject(req.Code, req.DeviceID)
	if err := s.ensureLoginAllowed(ctx, "customer_wechat", subject); err != nil {
		return nil, err
	}
	providerIdentity, err := s.wechat.ExchangeCode(ctx, req.Code)
	if err != nil {
		s.recordLoginFailure(ctx, "customer_wechat", subject)
		return nil, mapWeChatError(err, "WECHAT_CODE_INVALID")
	}
	if !s.repo.DBConfigured() {
		return nil, problem.Internal("mysql is not configured")
	}
	account, customer, _, err := s.repo.FindOrCreateCustomerByIdentity(ctx, providerIdentity, s.idGen.Next)
	if err != nil {
		return nil, err
	}
	if account.Status != "active" || customer.Status != "active" {
		return nil, problem.Forbidden("AUTH_ACCOUNT_DISABLED", "account disabled")
	}
	phoneBound := customer.Phone != ""
	identity := Identity{
		AccountType: "customer",
		AccountID:   account.ID,
		CustomerID:  customer.ID,
		Permissions: []string{"customer:login", "product:list", "order:create"},
		Profile: map[string]any{
			"customer_id": idString(customer.ID),
			"phone":       customer.Phone,
			"phone_bound": phoneBound,
		},
	}
	tokens, err := s.issueResponse(ctx, identity)
	if err != nil {
		return nil, err
	}
	s.resetLoginFailures(ctx, "customer_wechat", subject)
	if err := s.createAudit(ctx, identity, "auth.wechat_login", "account", identity.AccountID, "success", map[string]any{"phone_bound": phoneBound}); err != nil {
		return nil, err
	}
	return &WeChatLoginResp{TokenResp: tokens, PhoneBound: phoneBound}, nil
}

// BindWeChatPhone 绑定We Chat 手机号。
func (s *Service) BindWeChatPhone(ctx context.Context, claims *Claims, method string, path string, key string, req PhoneBindReq) (PhoneBindResp, error) {
	if s.wechat == nil || !s.cfg.WeChat.AuthEnabled {
		return PhoneBindResp{}, problem.New(503, "PROVIDER_UNAVAILABLE", "Service Unavailable", "wechat phone authorization is unavailable")
	}
	if claims == nil || claims.AccountType != "customer" {
		return PhoneBindResp{}, problem.Forbidden("PERM_FORBIDDEN", "customer account required")
	}
	customerID, err := strconv.ParseUint(claims.CustomerID, 10, 64)
	if err != nil || customerID == 0 {
		return PhoneBindResp{}, problem.Forbidden("PERM_FORBIDDEN", "invalid customer identity")
	}
	phone, err := s.wechat.ResolvePhone(ctx, req.PhoneCode)
	if err != nil {
		return PhoneBindResp{}, mapWeChatError(err, "WECHAT_PHONE_CODE_INVALID")
	}
	if !phonePattern.MatchString(phone) {
		return PhoneBindResp{}, problem.InvalidArgument("AUTH_INVALID_PHONE", "invalid phone")
	}
	var result PhoneBindResp
	err = s.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		started, err := s.idStore.Start(ctx, tx, s.idGen.Next(), claims.AccountType, customerID, method, path, key, idempotency.RequestHash(req))
		if err != nil {
			return err
		}
		if !started {
			cached, err := s.idStore.CachedResponse(ctx, tx, claims.AccountType, customerID, path, key, &result)
			if err != nil {
				return err
			}
			if cached {
				return nil
			}
			return problem.Conflict("IDEMPOTENCY_CONFLICT", "idempotency response missing")
		}
		customer, err := s.repo.BindCustomerPhone(ctx, tx, customerID, phone)
		if errors.Is(err, ErrPhoneAlreadyBound) {
			return problem.Conflict("PHONE_ALREADY_BOUND", "phone belongs to another customer")
		}
		if err != nil {
			return err
		}
		result = PhoneBindResp{CustomerID: idString(customer.ID), Phone: customer.Phone}
		return s.idStore.Succeed(ctx, tx, claims.AccountType, customerID, path, key, result)
	})
	if err != nil {
		return PhoneBindResp{}, err
	}
	identity := identityFromClaims(claims)
	if err := s.createAudit(ctx, identity, "auth.phone_bind", "customer", customerID, "success", map[string]any{"phone_masked": maskPhone(phone)}); err != nil {
		return PhoneBindResp{}, err
	}
	return result, nil
}

// PasswordLogin 只覆盖 admin 和 merchant；骑手只能使用短信登录。
// 角色 profile 会在 bcrypt 校验通过后再加载。
func (s *Service) PasswordLogin(ctx context.Context, accountType string, req PasswordLoginReq) (*TokenResp, error) {
	if accountType != "admin" && accountType != "merchant" {
		return nil, problem.Forbidden("AUTH_LOGIN_METHOD_NOT_ALLOWED", "password login is not allowed for this account type")
	}
	if !s.repo.DBConfigured() {
		return nil, problem.Internal("mysql is not configured")
	}
	if err := s.ensureLoginAllowed(ctx, accountType, req.Username); err != nil {
		return nil, err
	}

	account, err := s.repo.FindAccountByUsername(ctx, accountType, req.Username)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		s.recordLoginFailure(ctx, accountType, req.Username)
		return nil, problem.Unauthorized("AUTH_INVALID_CREDENTIAL", "invalid username or password")
	}
	if err != nil {
		return nil, err
	}
	if account.Status != "active" || account.PasswordHash == nil {
		s.recordLoginFailure(ctx, accountType, req.Username)
		return nil, problem.Forbidden("AUTH_ACCOUNT_DISABLED", "account disabled")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(*account.PasswordHash), []byte(req.Password)); err != nil {
		s.recordLoginFailure(ctx, accountType, req.Username)
		return nil, problem.Unauthorized("AUTH_INVALID_CREDENTIAL", "invalid username or password")
	}

	identity, err := s.identityForAccount(ctx, account)
	if err != nil {
		return nil, err
	}
	resp, err := s.issueResponse(ctx, identity)
	if err != nil {
		return nil, err
	}
	s.resetLoginFailures(ctx, accountType, req.Username)
	if err := s.createAudit(ctx, identity, "auth.login", "account", identity.AccountID, "success", map[string]any{"account_type": identity.AccountType, "username": req.Username}); err != nil {
		return nil, err
	}
	return resp, nil
}

// Refresh 刷新令牌 Resp。
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*TokenResp, error) {
	claims, err := s.tokens.ParseRefresh(refreshToken)
	if err != nil {
		return nil, problem.Unauthorized("AUTH_REFRESH_EXPIRED", "refresh token expired")
	}
	if err := s.ensureSession(ctx, claims, "refresh"); err != nil {
		return nil, err
	}

	accountID, err := strconv.ParseUint(claims.AccountID, 10, 64)
	if err != nil {
		return nil, problem.Unauthorized("AUTH_REFRESH_EXPIRED", "invalid refresh token")
	}
	account, err := s.repo.FindAccountByID(ctx, accountID)
	if err != nil {
		return nil, problem.Unauthorized("AUTH_SESSION_REVOKED", "session revoked")
	}
	if account.Status != "active" {
		return nil, problem.Forbidden("AUTH_ACCOUNT_DISABLED", "account disabled")
	}
	if account.TokenInvalidBefore != nil && claims.IssuedAt != nil && !claims.IssuedAt.Time.After(*account.TokenInvalidBefore) {
		return nil, problem.Unauthorized("AUTH_SESSION_REVOKED", "session revoked")
	}
	identity, err := s.identityForAccount(ctx, account)
	if err != nil {
		return nil, err
	}
	return s.issueRotatedResponse(ctx, identity, claims)
}

// VerifyAccess 先校验 JWT 本身，再在 Redis 可用时校验会话和黑名单。
func (s *Service) VerifyAccess(ctx context.Context, rawToken string) (*Claims, error) {
	claims, err := s.tokens.ParseAccess(rawToken)
	if err != nil {
		return nil, problem.Unauthorized("AUTH_UNAUTHORIZED", "invalid access token")
	}
	if s.redis == nil {
		return nil, dependencyUnavailable("redis is required for session verification")
	}

	blacklisted, err := s.redis.Exists(ctx, "jwt:blacklist:"+claims.ID).Result()
	if err != nil {
		return nil, err
	}
	if blacklisted > 0 {
		return nil, problem.Unauthorized("AUTH_SESSION_REVOKED", "session revoked")
	}
	if err := s.ensureSession(ctx, claims, "access"); err != nil {
		return nil, err
	}
	accountID, err := strconv.ParseUint(claims.AccountID, 10, 64)
	if err != nil {
		return nil, problem.Unauthorized("AUTH_UNAUTHORIZED", "invalid access token")
	}
	account, err := s.repo.FindAccountByID(ctx, accountID)
	if err != nil || account.Status != "active" {
		return nil, problem.Unauthorized("AUTH_SESSION_REVOKED", "session revoked")
	}
	if account.TokenInvalidBefore != nil && claims.IssuedAt != nil && !claims.IssuedAt.Time.After(*account.TokenInvalidBefore) {
		return nil, problem.Unauthorized("AUTH_SESSION_REVOKED", "session revoked")
	}
	return claims, nil
}

// VerifyAccessForLogout 核验Access For Logout是否有效。
func (s *Service) VerifyAccessForLogout(ctx context.Context, rawToken string) (*Claims, error) {
	claims, err := s.tokens.ParseAccess(rawToken)
	if err != nil {
		return nil, problem.Unauthorized("AUTH_UNAUTHORIZED", "invalid access token")
	}
	return claims, nil
}

// Logout 返回Logout。
func (s *Service) Logout(ctx context.Context, claims *Claims) error {
	if s.redis == nil {
		return dependencyUnavailable("redis is required for logout")
	}
	ttl := time.Until(claims.ExpiresAt.Time)
	if ttl <= 0 {
		ttl = time.Minute
	}
	if err := s.redis.Set(ctx, "jwt:blacklist:"+claims.ID, "1", ttl).Err(); err != nil {
		return err
	}
	if err := s.redis.Del(ctx, sessionKey(claims.AccountType, claims.AccountID, claims.SessionID)).Err(); err != nil {
		return err
	}
	if claims.AccountType == "customer" && s.logoutHook != nil {
		if err := s.logoutHook(ctx, claims.CustomerID); err != nil {
			return err
		}
	}
	return s.createAudit(ctx, identityFromClaims(claims), "auth.logout", "account", parseUintOrZero(claims.AccountID), "success", map[string]any{"account_type": claims.AccountType})
}

// verifySMSCode 核验SMS 代码是否有效。
func (s *Service) verifySMSCode(ctx context.Context, scope, phone string, code string) error {
	if s.redis == nil {
		return dependencyUnavailable("redis is required for sms login")
	}
	result, err := consumeSMSCodeScript.Run(ctx, s.redis, []string{smsLoginKey(scope, phone)}, code).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return problem.Unauthorized("AUTH_INVALID_CODE", "invalid sms code")
	}
	return nil
}

var consumeSMSCodeScript = goredis.NewScript(`
local stored = redis.call('GET', KEYS[1])
if not stored then
  return 0
end
if stored ~= ARGV[1] then
  return -1
end
redis.call('DEL', KEYS[1])
return 1
`)

var deleteSMSCodeScript = goredis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

// identityForAccount 将通用 accounts 账号扩展为具体角色的身份边界。
func (s *Service) identityForAccount(ctx context.Context, account Account) (Identity, error) {
	switch account.AccountType {
	case "admin":
		admin, roleCode, permissions, err := s.repo.AdminProfile(ctx, account.ID)
		if err != nil {
			return Identity{}, err
		}
		if admin.Status != "active" {
			return Identity{}, problem.Forbidden("AUTH_ACCOUNT_DISABLED", "account disabled")
		}
		return Identity{
			AccountType: account.AccountType,
			AccountID:   account.ID,
			AdminUserID: admin.ID,
			RoleCode:    roleCode,
			Permissions: permissions,
			Profile: map[string]any{
				"admin_user_id":  idString(admin.ID),
				"admin_sub_role": admin.AdminSubRole,
				"role_code":      roleCode,
				"name":           admin.Name,
				"permissions":    permissions,
			},
		}, nil
	case "merchant":
		merchantUser, shopIDs, err := s.repo.MerchantProfile(ctx, account.ID)
		if err != nil {
			return Identity{}, err
		}
		if merchantUser.Status != "active" {
			return Identity{}, problem.Forbidden("MERCHANT_DISABLED", "merchant disabled")
		}
		permissions := []string{"store_order:list", "store_order:accept", "store_order:prepare", "shop_product:list", "shop_product:create", "shop_product:update", "shop:business_status", "inventory:view", "inventory:adjust", "after_sale:list_shop", "after_sale:view_shop", "after_sale:review_shop", "after_sale:receive_return", "after_sale:create_replacement", "print_setting:view_shop", "print_setting:update_shop", "print_task:list_shop", "print_task:reprint_shop", "delivery_verification:view_shop", "delivery_incident:view_shop", "delivery_return:list_shop", "delivery_return:view_shop", "delivery_return:receive_shop"}
		return Identity{
			AccountType:       account.AccountType,
			AccountID:         account.ID,
			MerchantUserID:    merchantUser.ID,
			MerchantID:        merchantUser.MerchantID,
			AuthorizedShopIDs: shopIDs,
			Permissions:       permissions,
			Profile: map[string]any{
				"merchant_user_id":    idString(merchantUser.ID),
				"merchant_id":         idString(merchantUser.MerchantID),
				"authorized_shop_ids": uint64Strings(shopIDs),
				"name":                merchantUser.Name,
				"permissions":         permissions,
			},
		}, nil
	case "rider":
		rider, err := s.repo.RiderProfile(ctx, account.ID)
		if err != nil {
			return Identity{}, err
		}
		if rider.Status != "active" {
			return Identity{}, problem.Forbidden("RIDER_DISABLED", "rider disabled")
		}
		permissions := []string{"delivery:list", "delivery:accept", "delivery:update_status", "delivery:route", "rider_work_status:update", "rider_location:update", "delivery_offer:list", "delivery_offer:accept", "delivery_offer:reject", "delivery_incident:create", "delivery_incident:view_own", "delivery_incident:evidence_add", "delivery_return:create", "delivery_return:view_own", "delivery_return:arrive"}
		return Identity{
			AccountType: account.AccountType,
			AccountID:   account.ID,
			RiderID:     rider.ID,
			Permissions: permissions,
			Profile: map[string]any{
				"rider_id":    idString(rider.ID),
				"name":        rider.Name,
				"work_status": rider.WorkStatus,
				"permissions": permissions,
			},
		}, nil
	case "customer":
		customer, err := s.repo.CustomerProfile(ctx, account.ID)
		if err != nil {
			return Identity{}, err
		}
		permissions := []string{"customer:login", "product:list", "order:create"}
		return Identity{
			AccountType: account.AccountType,
			AccountID:   account.ID,
			CustomerID:  customer.ID,
			Permissions: permissions,
			Profile: map[string]any{
				"customer_id": idString(customer.ID),
				"phone":       customer.Phone,
				"permissions": permissions,
			},
		}, nil
	default:
		return Identity{}, problem.Forbidden("AUTH_ACCOUNT_DISABLED", "unsupported account type")
	}
}

// issueResponse 返回issue 响应。
func (s *Service) issueResponse(ctx context.Context, identity Identity) (*TokenResp, error) {
	pair, err := s.tokens.Issue(identity)
	if err != nil {
		return nil, err
	}
	if err := s.storeSessions(ctx, identity, pair); err != nil {
		return nil, err
	}
	_ = s.repo.TouchLastLogin(ctx, identity.AccountID)
	return &TokenResp{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		ExpiresIn:    pair.ExpiresIn,
		AccountType:  identity.AccountType,
		AccountID:    identity.AccountIDString(),
		Profile:      identity.Profile,
	}, nil
}

// issueRotatedResponse 原子替换 refresh 会话，旧 refresh token 在刷新成功后立即失效。
func (s *Service) issueRotatedResponse(ctx context.Context, identity Identity, oldClaims *Claims) (*TokenResp, error) {
	pair, err := s.tokens.Issue(identity)
	if err != nil {
		return nil, err
	}
	if err := s.rotateSession(ctx, identity, pair, oldClaims); err != nil {
		return nil, err
	}
	_ = s.repo.TouchLastLogin(ctx, identity.AccountID)
	return tokenResponse(identity, pair), nil
}

// storeSessions 返回门店 Sessions。
func (s *Service) storeSessions(ctx context.Context, identity Identity, pair TokenPair) error {
	if s.redis == nil {
		return dependencyUnavailable("redis is required for session storage")
	}
	key := sessionKey(identity.AccountType, identity.AccountIDString(), pair.SessionID)
	_, err := s.redis.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
		pipe.HSet(ctx, key, "access_jti", pair.AccessJTI, "refresh_jti", pair.RefreshJTI)
		pipe.Expire(ctx, key, s.cfg.JWT.RefreshTTL)
		return nil
	})
	return err
}

// ensureSession 确保会话存在且处于可用状态。
func (s *Service) ensureSession(ctx context.Context, claims *Claims, tokenType string) error {
	if s.redis == nil {
		return dependencyUnavailable("redis is required for session verification")
	}
	field := tokenType + "_jti"
	storedJTI, err := s.redis.HGet(ctx, sessionKey(claims.AccountType, claims.AccountID, claims.SessionID), field).Result()
	if errors.Is(err, goredis.Nil) {
		return problem.Unauthorized("AUTH_SESSION_REVOKED", "session revoked")
	}
	if err != nil {
		return err
	}
	if storedJTI != claims.ID {
		return problem.Unauthorized("AUTH_SESSION_REVOKED", "session revoked")
	}
	return nil
}

var rotateSessionScript = goredis.NewScript(`
if redis.call('HGET', KEYS[1], 'refresh_jti') ~= ARGV[1] then
  return 0
end
redis.call('HSET', KEYS[2], 'access_jti', ARGV[2], 'refresh_jti', ARGV[3])
redis.call('PEXPIRE', KEYS[2], ARGV[4])
redis.call('DEL', KEYS[1])
return 1
`)

// rotateSession 返回rotate 会话。
func (s *Service) rotateSession(ctx context.Context, identity Identity, pair TokenPair, oldClaims *Claims) error {
	if s.redis == nil {
		return dependencyUnavailable("redis is required for session rotation")
	}
	oldKey := sessionKey(oldClaims.AccountType, oldClaims.AccountID, oldClaims.SessionID)
	newKey := sessionKey(identity.AccountType, identity.AccountIDString(), pair.SessionID)
	result, err := rotateSessionScript.Run(ctx, s.redis, []string{oldKey, newKey}, oldClaims.ID, pair.AccessJTI, pair.RefreshJTI, s.cfg.JWT.RefreshTTL.Milliseconds()).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return problem.Unauthorized("AUTH_SESSION_REVOKED", "session revoked")
	}
	return nil
}

// sessionKey 返回会话密钥。
func sessionKey(accountType string, accountID string, sessionID string) string {
	return "session:" + accountType + ":" + accountID + ":" + sessionID
}

// tokenResponse 返回令牌响应。
func tokenResponse(identity Identity, pair TokenPair) *TokenResp {
	return &TokenResp{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		ExpiresIn:    pair.ExpiresIn,
		AccountType:  identity.AccountType,
		AccountID:    identity.AccountIDString(),
		Profile:      identity.Profile,
	}
}

// dependencyUnavailable 返回dependency Unavailable。
func dependencyUnavailable(detail string) error {
	return problem.New(503, "SYSTEM_DEPENDENCY_UNAVAILABLE", "Service Unavailable", detail)
}

// ensureSMSSendAllowed 确保SMS Send 允许状态存在且处于可用状态。
func (s *Service) ensureSMSSendAllowed(ctx context.Context, scope, phone string) error {
	cooldownKey := "rate:sms:login:" + scope + ":cooldown:" + phone
	allowed, err := s.redis.SetNX(ctx, cooldownKey, "1", smsSendInterval).Result()
	if err != nil {
		return err
	}
	if !allowed {
		return problem.TooManyRequests("AUTH_SMS_TOO_FREQUENT", "sms code is sent too frequently")
	}

	dailyKey := "rate:sms:login:" + scope + ":daily:" + phone
	count, err := s.redis.Incr(ctx, dailyKey).Result()
	if err != nil {
		return err
	}
	if count == 1 {
		_ = s.redis.Expire(ctx, dailyKey, 24*time.Hour).Err()
	}
	if count > smsDailyLimit {
		return problem.TooManyRequests("AUTH_SMS_DAILY_LIMIT", "sms daily limit exceeded")
	}
	return nil
}

func smsLoginKey(scope, phone string) string {
	return "sms:login:" + scope + ":" + phone
}

// ensureLoginAllowed 确保Login 允许状态存在且处于可用状态。
func (s *Service) ensureLoginAllowed(ctx context.Context, accountType string, subject string) error {
	if s.redis == nil {
		return nil
	}
	count, err := s.redis.Get(ctx, loginFailureKey(accountType, subject)).Int()
	if errors.Is(err, goredis.Nil) {
		return nil
	}
	if err != nil {
		return err
	}
	if count >= loginFailureLimit {
		return problem.TooManyRequests("AUTH_LOGIN_RATE_LIMITED", "too many failed login attempts")
	}
	return nil
}

// recordLoginFailure 处理记录 Login Failure相关逻辑。
func (s *Service) recordLoginFailure(ctx context.Context, accountType string, subject string) {
	if s.redis == nil {
		return
	}
	key := loginFailureKey(accountType, subject)
	count, err := s.redis.Incr(ctx, key).Result()
	if err != nil {
		return
	}
	if count == 1 {
		_ = s.redis.Expire(ctx, key, loginFailureWindow).Err()
	}
}

// resetLoginFailures 重置Login Failures。
func (s *Service) resetLoginFailures(ctx context.Context, accountType string, subject string) {
	if s.redis == nil {
		return
	}
	_ = s.redis.Del(ctx, loginFailureKey(accountType, subject)).Err()
}

// loginFailureKey 返回login Failure 密钥。
func loginFailureKey(accountType string, subject string) string {
	return loginFailureCounterBase + rateKeyPart(accountType) + ":" + rateKeyPart(subject)
}

// rateKeyPart 返回速率密钥 Part。
func rateKeyPart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	replacer := strings.NewReplacer(" ", "_", ":", "_", "\n", "_", "\r", "_", "\t", "_")
	if value == "" {
		return "empty"
	}
	return replacer.Replace(value)
}

// wechatRateSubject 返回微信速率 Subject。
func wechatRateSubject(code string, deviceID string) string {
	sum := sha256.Sum256([]byte(code))
	return fmt.Sprintf("%x:%s", sum[:8], rateKeyPart(deviceID))
}

// mapWeChatError 映射并返回We Chat 错误。
func mapWeChatError(err error, invalidCode string) error {
	if errors.Is(err, ErrWeChatCodeInvalid) {
		return problem.Unauthorized(invalidCode, "wechat authorization code is invalid")
	}
	return problem.New(503, "PROVIDER_UNAVAILABLE", "Service Unavailable", "wechat provider is unavailable")
}

// maskPhone 对手机号进行脱敏。
func maskPhone(phone string) string {
	if len(phone) != 11 {
		return "***"
	}
	return phone[:3] + "****" + phone[7:]
}

// createAudit 创建审计。
func (s *Service) createAudit(ctx context.Context, identity Identity, action string, resourceType string, resourceID uint64, result string, after any) error {
	if !s.repo.DBConfigured() {
		return nil
	}
	return s.repo.CreateAuditLog(ctx, AuditLog{
		ID:           s.idGen.Next(),
		ActorType:    identity.AccountType,
		ActorID:      actorIDForIdentity(identity),
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		AfterData:    jsonData(after),
		Result:       result,
		RequestID:    requestctx.RequestIDPtr(ctx),
		IP:           requestctx.IPPtr(ctx),
		UserAgent:    requestctx.UserAgentPtr(ctx),
	})
}

// identityFromClaims 返回身份 From 认证声明。
func identityFromClaims(claims *Claims) Identity {
	if claims == nil {
		return Identity{AccountType: "unknown"}
	}
	return Identity{
		AccountType:    claims.AccountType,
		AccountID:      parseUintOrZero(claims.AccountID),
		CustomerID:     parseUintOrZero(claims.CustomerID),
		AdminUserID:    parseUintOrZero(claims.AdminUserID),
		MerchantUserID: parseUintOrZero(claims.MerchantUserID),
		MerchantID:     parseUintOrZero(claims.MerchantID),
		RiderID:        parseUintOrZero(claims.RiderID),
	}
}

// actorIDForIdentity 返回actor ID For 身份。
func actorIDForIdentity(identity Identity) uint64 {
	switch identity.AccountType {
	case "customer":
		if identity.CustomerID != 0 {
			return identity.CustomerID
		}
	case "admin":
		if identity.AdminUserID != 0 {
			return identity.AdminUserID
		}
	case "merchant":
		if identity.MerchantUserID != 0 {
			return identity.MerchantUserID
		}
	case "rider":
		if identity.RiderID != 0 {
			return identity.RiderID
		}
	}
	return identity.AccountID
}

// jsonData 将输入值序列化为 JSON 数据。
func jsonData(value any) datatypes.JSON {
	if value == nil {
		return nil
	}
	payload, _ := json.Marshal(value)
	return datatypes.JSON(payload)
}

// parseUintOrZero 解析Uint Or Zero。
func parseUintOrZero(raw string) uint64 {
	id, _ := strconv.ParseUint(raw, 10, 64)
	return id
}

// idString 将数字 ID 转换为字符串。
func idString(id uint64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatUint(id, 10)
}
