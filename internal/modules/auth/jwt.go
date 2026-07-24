package auth

import (
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"jiuxiaoer-admin/backend-go/internal/config"
)

// Claims 携带账号类型以及各角色对应的对象 ID。
// service 仍必须校验资源归属，不能只相信路径参数。
type Claims struct {
	TokenType         string   `json:"token_type"`
	SessionID         string   `json:"session_id"`
	AccountType       string   `json:"account_type"`
	AccountID         string   `json:"account_id"`
	ApplicationID     string   `json:"application_id,omitempty"`
	CredentialVersion uint     `json:"credential_version,omitempty"`
	CustomerID        string   `json:"customer_id,omitempty"`
	AdminUserID       string   `json:"admin_user_id,omitempty"`
	MerchantUserID    string   `json:"merchant_user_id,omitempty"`
	MerchantID        string   `json:"merchant_id,omitempty"`
	AuthorizedShopIDs []string `json:"authorized_shop_ids,omitempty"`
	RiderID           string   `json:"rider_id,omitempty"`
	RoleCode          string   `json:"role_code,omitempty"`
	Permissions       []string `json:"permissions,omitempty"`
	jwt.RegisteredClaims
}

type TokenPair struct {
	AccessToken  string
	RefreshToken string
	AccessJTI    string
	RefreshJTI   string
	SessionID    string
	ExpiresIn    int64
}

// ApplicationToken 刻意不使用 TokenPair：申请人绝不会获得刷新令牌
// 或正式骑手会话。
type ApplicationToken struct {
	Token     string
	ExpiresIn int64
}

// TokenManager 封装 access/refresh token 的签发和解析。
// 分离密钥便于后续轮换 refresh token，而不必立刻废弃所有 access token。
type TokenManager struct {
	cfg config.JWTConfig
}

// NewTokenManager 创建并初始化令牌 Manager。
func NewTokenManager(cfg config.JWTConfig) TokenManager {
	return TokenManager{cfg: cfg}
}

// Issue 为一次身份快照签发短期 access token 和长期 refresh token。
func (m TokenManager) Issue(identity Identity) (TokenPair, error) {
	now := time.Now()
	sessionID := uuid.NewString()
	accessJTI := uuid.NewString()
	refreshJTI := uuid.NewString()

	accessClaims := identity.toClaims("access")
	accessClaims.SessionID = sessionID
	accessClaims.RegisteredClaims = jwt.RegisteredClaims{
		ID:        accessJTI,
		Subject:   identity.AccountIDString(),
		Issuer:    "jiuxiaoer-api",
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(m.cfg.AccessTTL)),
	}

	refreshClaims := identity.toClaims("refresh")
	refreshClaims.SessionID = sessionID
	refreshClaims.RegisteredClaims = jwt.RegisteredClaims{
		ID:        refreshJTI,
		Subject:   identity.AccountIDString(),
		Issuer:    "jiuxiaoer-api",
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(m.cfg.RefreshTTL)),
	}

	accessToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims).SignedString([]byte(m.cfg.AccessSecret))
	if err != nil {
		return TokenPair{}, err
	}
	refreshToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, refreshClaims).SignedString([]byte(m.cfg.RefreshSecret))
	if err != nil {
		return TokenPair{}, err
	}

	return TokenPair{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		AccessJTI:    accessJTI,
		RefreshJTI:   refreshJTI,
		SessionID:    sessionID,
		ExpiresIn:    int64(m.cfg.AccessTTL.Seconds()),
	}, nil
}

// IssueApplication 返回Issue 申请。
// IssueApplication 签发仅在骑手申请模块有效的短期令牌。它共用访问签名密钥，
// 但使用独立的 token_type，因此普通访问令牌解析器会拒绝它。
func (m TokenManager) IssueApplication(accountID, applicationID uint64, credentialVersion uint, permissions []string, ttl time.Duration) (ApplicationToken, error) {
	now := time.Now()
	claims := Claims{
		TokenType:         "application_access",
		SessionID:         uuid.NewString(),
		AccountType:       "rider",
		AccountID:         strconv.FormatUint(accountID, 10),
		ApplicationID:     strconv.FormatUint(applicationID, 10),
		CredentialVersion: credentialVersion,
		Permissions:       append([]string(nil), permissions...),
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(),
			Subject:   strconv.FormatUint(accountID, 10),
			Issuer:    "jiuxiaoer-api",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(m.cfg.AccessSecret))
	if err != nil {
		return ApplicationToken{}, err
	}
	return ApplicationToken{Token: raw, ExpiresIn: int64(ttl.Seconds())}, nil
}

// ParseAccess 解析访问令牌。
func (m TokenManager) ParseAccess(raw string) (*Claims, error) {
	return m.parse(raw, m.cfg.AccessSecret, "access")
}

// ParseRefresh 解析刷新令牌。
func (m TokenManager) ParseRefresh(raw string) (*Claims, error) {
	return m.parse(raw, m.cfg.RefreshSecret, "refresh")
}

// ParseApplication 解析申请。
func (m TokenManager) ParseApplication(raw string) (*Claims, error) {
	claims, err := m.parse(raw, m.cfg.AccessSecret, "application_access")
	if err != nil {
		return nil, err
	}
	if claims.ApplicationID == "" || claims.AccountType != "rider" || claims.CredentialVersion == 0 {
		return nil, jwt.ErrTokenInvalidClaims
	}
	return claims, nil
}

// parse 解析认证声明。
func (m TokenManager) parse(raw string, secret string, tokenType string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		return nil, err
	}
	if claims.TokenType != tokenType {
		return nil, jwt.ErrTokenInvalidClaims
	}
	if claims.SessionID == "" || claims.ID == "" || claims.AccountID == "" {
		return nil, jwt.ErrTokenInvalidClaims
	}
	return claims, nil
}

// uint64Strings 将 64 位无符号整数列表转换为字符串列表。
func uint64Strings(values []uint64) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, strconv.FormatUint(value, 10))
	}
	return result
}
