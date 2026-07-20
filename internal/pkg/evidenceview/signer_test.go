package evidenceview

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSignerProducesOpaqueBoundedViewToken(t *testing.T) {
	now := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	signer, err := New("https://media.example.test/private/evidence", "01234567890123456789012345678901", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	signer.now = func() time.Time { return now }
	result, err := signer.Sign(Input{EvidenceID: 11, IncidentID: 22, ObjectKey: "riders/7/private.jpg", MimeType: "image/jpeg",
		SHA256: strings.Repeat("a", 64), ActorType: "merchant", ActorID: 33})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.URL, "riders/7/private.jpg") || result.ExpiresAt.Sub(now) != 5*time.Minute {
		t.Fatalf("view result leaks an object key or has wrong TTL: %+v", result)
	}
	parsed, err := url.Parse(result.URL)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := Open("01234567890123456789012345678901", parsed.Query().Get("token"), now.Add(time.Minute))
	if err != nil || claims.ObjectKey != "riders/7/private.jpg" || claims.EvidenceID != "11" || claims.IncidentID != "22" || claims.ActorType != "merchant" || claims.ActorID != "33" {
		t.Fatalf("opaque token contract mismatch: claims=%+v err=%v", claims, err)
	}
	if _, err := Open("01234567890123456789012345678901", parsed.Query().Get("token"), now.Add(5*time.Minute)); err == nil {
		t.Fatal("expired view token was accepted")
	}
}

func TestSignerRejectsIncompleteOrUnsafeConfiguration(t *testing.T) {
	if _, err := New("https://media.example.test/view", "", time.Minute); err == nil {
		t.Fatal("incomplete evidence view configuration was accepted")
	}
	signer, err := New("https://media.example.test/view", strings.Repeat("s", 32), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signer.Sign(Input{EvidenceID: 1, IncidentID: 2, ObjectKey: "../private.jpg", MimeType: "image/jpeg", SHA256: strings.Repeat("a", 64), ActorType: "admin", ActorID: 3}); err == nil {
		t.Fatal("unsafe object key was accepted")
	}
}
