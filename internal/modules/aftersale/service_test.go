package aftersale

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"gorm.io/datatypes"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestReturnPolicy 验证Return 策略的预期行为。
func TestReturnPolicy(t *testing.T) {
	tests := []struct {
		name     string
		snapshot string
		known    bool
		eligible bool
	}{
		{name: "eligible", snapshot: `{"return_policy":{"eligible":true}}`, known: true, eligible: true},
		{name: "ineligible", snapshot: `{"return_policy":{"eligible":false}}`, known: true, eligible: false},
		{name: "historical", snapshot: `{"name":"old order"}`, known: false},
		{name: "invalid", snapshot: `{`, known: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			known, eligible := returnPolicy(datatypes.JSON(test.snapshot))
			if known != test.known || eligible != test.eligible {
				t.Fatalf("returnPolicy() = (%v,%v), want (%v,%v)", known, eligible, test.known, test.eligible)
			}
		})
	}
}

// TestResolutionAllowed 验证Resolution 允许状态的预期行为。
func TestResolutionAllowed(t *testing.T) {
	allowed := [][2]string{{"unopened_return", "return_and_refund"}, {"damaged", "refund_only"}, {"damaged", "replacement"}, {"missing_item", "replacement"}, {"out_of_stock", "refund_only"}, {"late_delivery", "compensation"}, {"other", "return_and_refund"}}
	for _, value := range allowed {
		if !resolutionAllowed(value[0], value[1]) {
			t.Fatalf("expected %s/%s to be allowed", value[0], value[1])
		}
	}
	denied := [][2]string{{"unopened_return", "refund_only"}, {"late_delivery", "refund_only"}, {"out_of_stock", "replacement"}, {"damaged", "compensation"}, {"unknown", "refund_only"}}
	for _, value := range denied {
		if resolutionAllowed(value[0], value[1]) {
			t.Fatalf("expected %s/%s to be denied", value[0], value[1])
		}
	}
}

// TestEvidenceTokenValidation 验证Evidence 令牌校验的预期行为。
func TestEvidenceTokenValidation(t *testing.T) {
	cfg := config.Load()
	now := time.Now().UTC()
	service := &Service{cfg: cfg, ids: snowflake.New(993), now: func() time.Time { return now }}
	makeToken := func(subject, mime, scan string, expires time.Time) string {
		claims := evidenceClaims{ObjectKey: "evidence/random-object", MimeType: mime, SizeBytes: 1024, SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", ScanStatus: scan, RegisteredClaims: jwt.RegisteredClaims{Issuer: "jxe-upload", Subject: subject, ID: "token-" + subject + mime + scan, ExpiresAt: jwt.NewNumericDate(expires)}}
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(cfg.AfterSale.EvidenceTokenSecret))
		if err != nil {
			t.Fatal(err)
		}
		return token
	}

	rows, err := service.evidence(1, 42, []string{makeToken("42", "image/jpeg", "clean", now.Add(time.Minute))})
	if err != nil || len(rows) != 1 || rows[0].Status != "verified" {
		t.Fatalf("valid token rejected: rows=%+v err=%v", rows, err)
	}
	rows, err = service.evidence(1, 42, []string{makeToken("42", "video/mp4", "pending", now.Add(time.Minute))})
	if err != nil || rows[0].Status != "quarantined" {
		t.Fatalf("pending scan token rejected: rows=%+v err=%v", rows, err)
	}
	for _, token := range []string{
		makeToken("43", "image/jpeg", "clean", now.Add(time.Minute)),
		makeToken("42", "application/pdf", "clean", now.Add(time.Minute)),
		makeToken("42", "image/jpeg", "infected", now.Add(time.Minute)),
		makeToken("42", "image/jpeg", "clean", now.Add(-time.Minute)),
	} {
		if _, err := service.evidence(1, 42, []string{token}); err == nil {
			t.Fatal("invalid evidence token was accepted")
		}
	}
}

// TestEligibleWindowsAndOrderState 验证Eligible Windows And 订单状态的预期行为。
func TestEligibleWindowsAndOrderState(t *testing.T) {
	cfg := config.Load()
	cfg.AfterSale.StandardWindow = 48 * time.Hour
	cfg.AfterSale.UnopenedReturnWindow = 7 * 24 * time.Hour
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	service := &Service{cfg: cfg, now: func() time.Time { return now }}

	tests := []struct {
		name string
		row  OrderRow
		kind string
		code string
	}{
		{name: "active paid order", row: OrderRow{Status: "delivering", PayStatus: "succeeded"}, kind: "damaged"},
		{name: "unpaid order", row: OrderRow{Status: "pending_payment", PayStatus: "pending"}, kind: "damaged", code: "AFTER_SALE_NOT_ELIGIBLE"},
		{name: "unopened before completion", row: OrderRow{Status: "delivering", PayStatus: "succeeded"}, kind: "unopened_return", code: "AFTER_SALE_NOT_ELIGIBLE"},
		{name: "standard expired", row: completed(now.Add(-49 * time.Hour)), kind: "damaged", code: "AFTER_SALE_NOT_ELIGIBLE"},
		{name: "unopened within seven days", row: completed(now.Add(-6 * 24 * time.Hour)), kind: "unopened_return"},
		{name: "unopened expired", row: completed(now.Add(-8 * 24 * time.Hour)), kind: "unopened_return", code: "AFTER_SALE_NOT_ELIGIBLE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := service.eligible(test.row, test.kind)
			if test.code == "" && err != nil {
				t.Fatalf("eligible() error = %v", err)
			}
			if test.code != "" {
				details, ok := err.(*problem.Details)
				if !ok || details.ErrorCode != test.code {
					t.Fatalf("eligible() error = %#v, want %s", err, test.code)
				}
			}
		})
	}
}

// completed 返回completed。
func completed(at time.Time) OrderRow {
	return OrderRow{Status: "completed", PayStatus: "succeeded", CompletedAt: &at}
}
