package search

import (
	"strings"
	"testing"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestNormalizeKeywordUsesNFKCWhitespaceAndASCIICase(t *testing.T) {
	display, normalized, err := normalizeKeyword("  ＡＢＣ　 啤酒  ")
	if err != nil {
		t.Fatalf("normalize keyword: %v", err)
	}
	if display != "ABC 啤酒" || normalized != "abc 啤酒" {
		t.Fatalf("unexpected normalized keyword: display=%q normalized=%q", display, normalized)
	}
}

func TestNormalizeKeywordRejectsUnsafeOrOversizedInput(t *testing.T) {
	for _, raw := range []string{"", "啤\n酒", "啤\u200d酒", strings.Repeat("酒", 65)} {
		if _, _, err := normalizeKeyword(raw); err == nil {
			t.Fatalf("expected %q to be rejected", raw)
		}
	}
}

func TestEligibleForHotRejectsPIIAndBlocklist(t *testing.T) {
	blocklist := map[string]struct{}{"内部词": {}}
	for _, keyword := range []string{"联系13800138000", "身份证11010519491231002X", "a@example.com", "订单12345678", "内部词"} {
		if eligibleForHot(keyword, blocklist) {
			t.Fatalf("expected %q to be excluded from hot ranking", keyword)
		}
	}
	if !eligibleForHot("精酿啤酒", blocklist) {
		t.Fatal("expected ordinary product keyword to be eligible")
	}
}

func TestMergeHotUsesCityThenGlobalThenDefaultsAndDeduplicates(t *testing.T) {
	city := []hotAggregate{
		{NormalizedKeyword: "啤酒", DisplayKeyword: "啤酒"},
		{NormalizedKeyword: "红酒", DisplayKeyword: "红酒"},
	}
	global := []hotAggregate{
		{NormalizedKeyword: "啤酒", DisplayKeyword: "啤酒"},
		{NormalizedKeyword: "白酒", DisplayKeyword: "白酒"},
		{NormalizedKeyword: "联系13800138000", DisplayKeyword: "联系13800138000"},
	}
	items := mergeHot(city, global, []string{"白酒", "威士忌"}, "440100", 4, map[string]struct{}{"红酒": {}})
	if len(items) != 3 {
		t.Fatalf("expected 3 safe unique items, got %#v", items)
	}
	if items[0].Keyword != "啤酒" || items[0].SourceScope != ScopeCity || items[0].CityCode != "440100" {
		t.Fatalf("expected city item first, got %#v", items[0])
	}
	if items[1].Keyword != "白酒" || items[1].SourceScope != ScopeGlobal {
		t.Fatalf("expected global fallback second, got %#v", items[1])
	}
	if items[2].Keyword != "威士忌" || items[2].SourceScope != "default" || items[2].Rank != 3 {
		t.Fatalf("expected default fallback third, got %#v", items[2])
	}
}

func TestQueryLimitAndIdempotencyValidation(t *testing.T) {
	if value, err := queryLimit("", 10, 20); err != nil || value != 10 {
		t.Fatalf("expected default limit, value=%d err=%v", value, err)
	}
	for _, raw := range []string{"0", "21", "bad"} {
		if _, err := queryLimit(raw, 10, 20); err == nil {
			t.Fatalf("expected invalid limit %q to fail", raw)
		}
	}
	for _, key := range []string{"", "short", "valid-key\nsmuggled"} {
		if err := validateIdempotencyKey(key); err == nil {
			t.Fatalf("expected idempotency key %q to fail", key)
		}
	}
	if err := validateIdempotencyKey("search-event-0001"); err != nil {
		t.Fatalf("expected valid idempotency key: %v", err)
	}
}

func TestSearchCustomerUsesOnlyCustomerClaims(t *testing.T) {
	id, raw, err := searchCustomer(&auth.Claims{AccountType: "customer", CustomerID: "42"}, true)
	if err != nil || id != 42 || raw != "42" {
		t.Fatalf("unexpected customer identity: id=%d raw=%q err=%v", id, raw, err)
	}
	for _, claims := range []*auth.Claims{nil, {AccountType: "admin", AdminUserID: "1"}, {AccountType: "customer", CustomerID: "not-an-id"}} {
		_, _, err := searchCustomer(claims, true)
		if err == nil {
			t.Fatalf("expected claims %#v to be rejected", claims)
		}
		details := problem.FromError(err)
		if claims == nil && details.Status != 401 {
			t.Fatalf("expected guest to receive 401, got %#v", details)
		}
		if claims != nil && details.Status != 403 {
			t.Fatalf("expected non-customer/invalid claims to receive 403, got %#v", details)
		}
	}
	if id, _, err := searchCustomer(nil, false); err != nil || id != 0 {
		t.Fatalf("expected guest discovery identity, id=%d err=%v", id, err)
	}
}
