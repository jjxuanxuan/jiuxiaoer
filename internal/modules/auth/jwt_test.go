package auth

import (
	"testing"
	"time"

	"jiuxiaoer-admin/backend-go/internal/config"
)

// TestTokenPairSharesSessionAndKeepsTokenTypesSeparate 验证令牌对共享会话且令牌类型相互分离。
func TestTokenPairSharesSessionAndKeepsTokenTypesSeparate(t *testing.T) {
	manager := NewTokenManager(config.JWTConfig{
		AccessSecret:  "test-access-secret",
		RefreshSecret: "test-refresh-secret",
		AccessTTL:     time.Hour,
		RefreshTTL:    24 * time.Hour,
	})
	pair, err := manager.Issue(Identity{AccountType: "customer", AccountID: 1, CredentialVersion: 7, CustomerID: 2})
	if err != nil {
		t.Fatalf("issue token pair: %v", err)
	}
	access, err := manager.ParseAccess(pair.AccessToken)
	if err != nil {
		t.Fatalf("parse access token: %v", err)
	}
	refresh, err := manager.ParseRefresh(pair.RefreshToken)
	if err != nil {
		t.Fatalf("parse refresh token: %v", err)
	}
	if pair.SessionID == "" || access.SessionID != pair.SessionID || refresh.SessionID != pair.SessionID {
		t.Fatal("access and refresh tokens must share one session id")
	}
	if access.CredentialVersion != 7 || refresh.CredentialVersion != 7 {
		t.Fatal("access and refresh tokens must carry the current credential version")
	}
	if _, err := manager.ParseAccess(pair.RefreshToken); err == nil {
		t.Fatal("refresh token must not be accepted as access token")
	}
}

// TestSessionKeyMatchesPRDShape 验证会话密钥 Matches PRD Shape的预期行为。
func TestSessionKeyMatchesPRDShape(t *testing.T) {
	got := sessionKey("admin", "3001", "sid-1")
	if got != "session:admin:3001:sid-1" {
		t.Fatalf("unexpected session key: %s", got)
	}
}

// TestApplicationTokenIsRestrictedAndHasNoRefreshToken 验证申请令牌权限受限且没有刷新令牌。
func TestApplicationTokenIsRestrictedAndHasNoRefreshToken(t *testing.T) {
	manager := NewTokenManager(config.JWTConfig{
		AccessSecret:  "test-access-secret",
		RefreshSecret: "test-refresh-secret",
		AccessTTL:     time.Hour,
		RefreshTTL:    24 * time.Hour,
	})
	token, err := manager.IssueApplication(101, 202, 3, []string{"rider_application:self_view"}, 30*time.Minute)
	if err != nil {
		t.Fatalf("issue application token: %v", err)
	}
	claims, err := manager.ParseApplication(token.Token)
	if err != nil {
		t.Fatalf("parse application token: %v", err)
	}
	if claims.TokenType != "application_access" || claims.AccountID != "101" || claims.ApplicationID != "202" || claims.CredentialVersion != 3 {
		t.Fatalf("unexpected application claims: %+v", claims)
	}
	if claims.RiderID != "" || token.ExpiresIn != 1800 {
		t.Fatalf("application token must not contain a formal rider identity: %+v", claims)
	}
	if _, err := manager.ParseAccess(token.Token); err == nil {
		t.Fatal("application token must be rejected by the normal access parser")
	}
	if _, err := manager.ParseRefresh(token.Token); err == nil {
		t.Fatal("application token must be rejected by the refresh parser")
	}
}
