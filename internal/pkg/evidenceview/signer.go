package evidenceview

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"jiuxiaoer-admin/backend-go/internal/pkg/securevalue"
)

const (
	Purpose     = "evidence_view"
	Audience    = "jxe-evidence-view"
	MaxTokenTTL = 5 * time.Minute
)

var ErrInvalid = errors.New("invalid or expired evidence view token")

// Claims is the encrypted contract consumed by the private media gateway. The
// object key is never exposed in clear text to API clients.
type Claims struct {
	Version    int    `json:"v"`
	Purpose    string `json:"purpose"`
	Audience   string `json:"aud"`
	EvidenceID string `json:"evidence_id"`
	IncidentID string `json:"incident_id"`
	ObjectKey  string `json:"object_key"`
	MimeType   string `json:"mime_type"`
	SHA256     string `json:"sha256"`
	ActorType  string `json:"actor_type"`
	ActorID    string `json:"actor_id"`
	IssuedAt   int64  `json:"iat"`
	ExpiresAt  int64  `json:"exp"`
}

type Input struct {
	EvidenceID uint64
	IncidentID uint64
	ObjectKey  string
	MimeType   string
	SHA256     string
	ActorType  string
	ActorID    uint64
}

type Result struct {
	URL       string
	ExpiresAt time.Time
}

type Signer struct {
	baseURL string
	secret  string
	ttl     time.Duration
	now     func() time.Time
}

func New(baseURL, secret string, ttl time.Duration) (*Signer, error) {
	baseURL, secret = strings.TrimSpace(baseURL), strings.TrimSpace(secret)
	if baseURL == "" && secret == "" {
		return &Signer{}, nil
	}
	if baseURL == "" || secret == "" || ttl <= 0 || ttl > MaxTokenTTL {
		return nil, fmt.Errorf("evidence view base URL, secret and TTL must be configured together with TTL <= %s", MaxTokenTTL)
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, fmt.Errorf("invalid evidence view base URL")
	}
	return &Signer{baseURL: parsed.String(), secret: secret, ttl: ttl, now: time.Now}, nil
}

func (s *Signer) Available() bool {
	return s != nil && s.baseURL != "" && s.secret != "" && s.ttl > 0
}

func (s *Signer) Sign(input Input) (Result, error) {
	if !s.Available() || input.EvidenceID == 0 || input.IncidentID == 0 || input.ActorID == 0 ||
		strings.TrimSpace(input.ActorType) == "" || !safeObjectKey(input.ObjectKey) || strings.TrimSpace(input.MimeType) == "" || strings.TrimSpace(input.SHA256) == "" {
		return Result{}, ErrInvalid
	}
	now := s.now().UTC()
	expiresAt := now.Add(s.ttl)
	claims := Claims{
		Version: 1, Purpose: Purpose, Audience: Audience,
		EvidenceID: strconv.FormatUint(input.EvidenceID, 10), IncidentID: strconv.FormatUint(input.IncidentID, 10),
		ObjectKey: input.ObjectKey, MimeType: input.MimeType, SHA256: strings.ToLower(input.SHA256),
		ActorType: input.ActorType, ActorID: strconv.FormatUint(input.ActorID, 10),
		IssuedAt: now.Unix(), ExpiresAt: expiresAt.Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return Result{}, err
	}
	sealed, err := securevalue.Seal(s.secret, string(payload))
	if err != nil {
		return Result{}, err
	}
	target, _ := url.Parse(s.baseURL)
	query := target.Query()
	query.Set("token", string(sealed))
	target.RawQuery = query.Encode()
	return Result{URL: target.String(), ExpiresAt: expiresAt}, nil
}

// Open is the media-gateway side of the contract. It decrypts and validates an
// opaque token without accepting an object key from the caller.
func Open(secret, token string, now time.Time) (Claims, error) {
	plaintext, err := securevalue.Open(strings.TrimSpace(secret), []byte(strings.TrimSpace(token)))
	if err != nil {
		return Claims{}, ErrInvalid
	}
	var claims Claims
	if err := json.Unmarshal([]byte(plaintext), &claims); err != nil {
		return Claims{}, ErrInvalid
	}
	now = now.UTC()
	if claims.Version != 1 || claims.Purpose != Purpose || claims.Audience != Audience ||
		claims.EvidenceID == "" || claims.IncidentID == "" || claims.ActorID == "" || strings.TrimSpace(claims.ActorType) == "" ||
		!safeObjectKey(claims.ObjectKey) || claims.MimeType == "" || claims.SHA256 == "" ||
		claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt || claims.ExpiresAt-claims.IssuedAt > int64(MaxTokenTTL/time.Second) ||
		now.Unix() < claims.IssuedAt-30 || now.Unix() >= claims.ExpiresAt {
		return Claims{}, ErrInvalid
	}
	if _, err := strconv.ParseUint(claims.EvidenceID, 10, 64); err != nil {
		return Claims{}, ErrInvalid
	}
	if _, err := strconv.ParseUint(claims.IncidentID, 10, 64); err != nil {
		return Claims{}, ErrInvalid
	}
	if _, err := strconv.ParseUint(claims.ActorID, 10, 64); err != nil {
		return Claims{}, ErrInvalid
	}
	return claims, nil
}

func safeObjectKey(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || strings.ContainsRune(value, '\x00') {
		return false
	}
	cleaned := path.Clean(value)
	return cleaned == value && cleaned != "." && cleaned != ".." && !strings.HasPrefix(cleaned, "../")
}
