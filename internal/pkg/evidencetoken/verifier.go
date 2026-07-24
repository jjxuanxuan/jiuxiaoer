package evidencetoken

import (
	"encoding/hex"
	"errors"
	"path"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	// ErrInvalid 刻意不暴露具体哪个声明校验失败。
	ErrInvalid = errors.New("invalid or expired evidence token")
	// ErrScanPending 让调用方能够识别可重试的安全扫描。
	ErrScanPending = errors.New("evidence scan is pending")
)

// Claims 是售后与配送异常证据共用的上传服务契约。
// RegisteredClaims 承载 iss、aud、sub、jti 和时间声明。
type Claims struct {
	Purpose    string `json:"purpose,omitempty"`
	ObjectKey  string `json:"object_key"`
	MimeType   string `json:"mime_type"`
	SizeBytes  uint64 `json:"size_bytes"`
	SHA256     string `json:"sha256"`
	ScanStatus string `json:"scan_status"`
	jwt.RegisteredClaims
}

type MediaRule struct {
	MaxBytes uint64
}

// Policy 在共用同一套解析和签名实现的同时，将旧版售后语义与
// 更严格的配送异常语义分开。
type Policy struct {
	Secret            string
	Issuer            string
	Audience          string
	Subject           string
	Purpose           string
	AllowedMedia      map[string]MediaRule
	AllowedScanStatus map[string]bool
	ObjectKeyPrefixes []string
	MaxObjectKeyBytes int
	MaxTokenIDBytes   int
	ClockSkew         time.Duration
	Now               func() time.Time
}

type Metadata struct {
	TokenID    string
	ObjectKey  string
	MimeType   string
	SizeBytes  uint64
	SHA256     string
	ScanStatus string
}

// Verify 在不下载媒体文件的情况下校验上传令牌。调用方绝不能记录
// rawToken 或 Metadata.ObjectKey。
func Verify(rawToken string, policy Policy) (Metadata, error) {
	if strings.TrimSpace(rawToken) == "" || policy.Secret == "" || policy.Subject == "" {
		return Metadata{}, ErrInvalid
	}
	claims := new(Claims)
	options := []jwt.ParserOption{
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
	}
	if policy.Issuer != "" {
		options = append(options, jwt.WithIssuer(policy.Issuer))
	}
	if policy.Audience != "" {
		options = append(options, jwt.WithAudience(policy.Audience))
	}
	if policy.ClockSkew > 0 {
		options = append(options, jwt.WithLeeway(policy.ClockSkew))
	}
	if policy.Now != nil {
		options = append(options, jwt.WithTimeFunc(policy.Now))
	}
	parsed, err := jwt.ParseWithClaims(rawToken, claims, func(token *jwt.Token) (any, error) {
		if token.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, ErrInvalid
		}
		return []byte(policy.Secret), nil
	}, options...)
	if err != nil || parsed == nil || !parsed.Valid {
		return Metadata{}, ErrInvalid
	}

	maxObjectKey := policy.MaxObjectKeyBytes
	if maxObjectKey == 0 {
		maxObjectKey = 512
	}
	maxTokenID := policy.MaxTokenIDBytes
	if maxTokenID == 0 {
		maxTokenID = 128
	}
	sha := strings.ToLower(claims.SHA256)
	decodedSHA, shaErr := hex.DecodeString(sha)
	if claims.Subject != policy.Subject || claims.ID == "" || len(claims.ID) > maxTokenID ||
		claims.ObjectKey == "" || len(claims.ObjectKey) > maxObjectKey || shaErr != nil || len(decodedSHA) != 32 ||
		(policy.Purpose != "" && claims.Purpose != policy.Purpose) || !safeObjectKey(claims.ObjectKey) ||
		!hasAllowedPrefix(claims.ObjectKey, policy.ObjectKeyPrefixes) {
		return Metadata{}, ErrInvalid
	}
	media, ok := policy.AllowedMedia[claims.MimeType]
	if !ok || media.MaxBytes == 0 || claims.SizeBytes == 0 || claims.SizeBytes > media.MaxBytes {
		return Metadata{}, ErrInvalid
	}
	if !policy.AllowedScanStatus[claims.ScanStatus] {
		if claims.ScanStatus == "pending" {
			return Metadata{}, ErrScanPending
		}
		return Metadata{}, ErrInvalid
	}
	return Metadata{
		TokenID: claims.ID, ObjectKey: claims.ObjectKey, MimeType: claims.MimeType,
		SizeBytes: claims.SizeBytes, SHA256: sha, ScanStatus: claims.ScanStatus,
	}, nil
}

func safeObjectKey(value string) bool {
	if strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || strings.ContainsRune(value, '\x00') {
		return false
	}
	cleaned := path.Clean(value)
	return cleaned == value && cleaned != "." && cleaned != ".." && !strings.HasPrefix(cleaned, "../")
}

func hasAllowedPrefix(value string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, prefix := range prefixes {
		if prefix != "" && strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}
