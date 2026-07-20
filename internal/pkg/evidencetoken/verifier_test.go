package evidencetoken

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestVerifyStrictPolicy(t *testing.T) {
	now := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	policy := Policy{
		Secret: "secret", Issuer: "jxe-upload", Audience: "delivery-incident",
		Subject: "rider:42", Purpose: "delivery_incident_evidence",
		AllowedMedia:      map[string]MediaRule{"image/jpeg": {MaxBytes: 20 << 20}},
		AllowedScanStatus: map[string]bool{"clean": true}, ObjectKeyPrefixes: []string{"riders/42/"}, Now: func() time.Time { return now },
	}
	sign := func(modify func(*Claims)) string {
		claims := Claims{Purpose: policy.Purpose, ObjectKey: "riders/42/evidence.jpg", MimeType: "image/jpeg", SizeBytes: 1024,
			SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", ScanStatus: "clean",
			RegisteredClaims: jwt.RegisteredClaims{Issuer: "jxe-upload", Audience: jwt.ClaimStrings{"delivery-incident"}, Subject: "rider:42", ID: "token-1", IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))}}
		if modify != nil {
			modify(&claims)
		}
		raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(policy.Secret))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	meta, err := Verify(sign(nil), policy)
	if err != nil || meta.TokenID != "token-1" || meta.ObjectKey == "" {
		t.Fatalf("valid token rejected: meta=%+v err=%v", meta, err)
	}
	for name, modify := range map[string]func(*Claims){
		"audience":  func(c *Claims) { c.Audience = jwt.ClaimStrings{"after-sale"} },
		"subject":   func(c *Claims) { c.Subject = "rider:43" },
		"purpose":   func(c *Claims) { c.Purpose = "other" },
		"traversal": func(c *Claims) { c.ObjectKey = "riders/42/../43/evidence.jpg" },
		"media":     func(c *Claims) { c.MimeType = "image/svg+xml" },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Verify(sign(modify), policy); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Verify() error = %v, want ErrInvalid", err)
			}
		})
	}
	if _, err := Verify(sign(func(c *Claims) { c.ScanStatus = "pending" }), policy); !errors.Is(err, ErrScanPending) {
		t.Fatalf("pending scan error = %v", err)
	}
}
