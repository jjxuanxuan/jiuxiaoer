package pagination

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

// TestFromGinAcceptsWhitelistedOrderAndSimpleFilter 验证From Gin Accepts Whitelisted 订单 And 简单操作筛选条件的预期行为。
func TestFromGinAcceptsWhitelistedOrderAndSimpleFilter(t *testing.T) {
	c := testContext("/items?page_size=10&order_by=created_at%20desc,id%20asc&filter=status:paid,created_at>=2026-01-01")

	query, err := FromGin(c)
	if err != nil {
		t.Fatalf("expected valid query, got error: %v", err)
	}
	if query.PageSize != 10 {
		t.Fatalf("expected page_size 10, got %d", query.PageSize)
	}
	if query.OrderBy == "" || query.Filter == "" {
		t.Fatalf("expected order_by and filter to be retained")
	}
}

// TestFromGinRejectsUnknownOrderField 验证From Gin Rejects Unknown 订单 Field的预期行为。
func TestFromGinRejectsUnknownOrderField(t *testing.T) {
	c := testContext("/items?order_by=deleted_at%20desc")

	_, err := FromGin(c)
	if err == nil {
		t.Fatal("expected invalid order_by error")
	}
	assertProblemCode(t, err, "VALIDATION_INVALID_QUERY")
}

// TestFromGinRejectsInvalidFilterSyntax 验证From Gin Rejects 无效筛选条件 Syntax的预期行为。
func TestFromGinRejectsInvalidFilterSyntax(t *testing.T) {
	c := testContext("/items?filter=status==(paid")

	_, err := FromGin(c)
	if err == nil {
		t.Fatal("expected invalid filter error")
	}
	assertProblemCode(t, err, "VALIDATION_INVALID_QUERY")
}

// TestFromGinAcceptsAdditionalWhitelistedOrderFields 验证From Gin Accepts Additional Whitelisted 订单 Fields的预期行为。
func TestFromGinAcceptsAdditionalWhitelistedOrderFields(t *testing.T) {
	c := testContext("/items?order_by=available_qty%20desc,status%20asc")

	query, err := FromGin(c)
	if err != nil {
		t.Fatalf("expected valid order fields, got error: %v", err)
	}
	if query.OrderBy == "" {
		t.Fatal("expected order_by to be retained")
	}
}

// TestPageTokenKeepsExactNextOffset 验证分页令牌 Keeps Exact Next Offset的预期行为。
func TestPageTokenKeepsExactNextOffset(t *testing.T) {
	first := testContext("/items?page_size=2&order_by=created_at%20desc&filter=status:paid")
	query, err := FromGin(first)
	if err != nil {
		t.Fatalf("first page query failed: %v", err)
	}
	token := NextPageToken(query)
	second := testContext("/items?page_size=2&order_by=created_at%20desc&filter=status:paid&page_token=" + token)
	nextQuery, err := FromGin(second)
	if err != nil {
		t.Fatalf("next page query failed: %v", err)
	}
	if nextQuery.Offset != 2 {
		t.Fatalf("expected offset 2, got %d", nextQuery.Offset)
	}
}

// TestPageTokenRejectsChangedQuery 验证分页令牌 Rejects Changed 查询的预期行为。
func TestPageTokenRejectsChangedQuery(t *testing.T) {
	first := testContext("/items?page_size=2&filter=status:paid")
	query, err := FromGin(first)
	if err != nil {
		t.Fatalf("first page query failed: %v", err)
	}
	token := NextPageToken(query)
	changed := testContext("/items?page_size=3&filter=status:paid&page_token=" + token)
	_, err = FromGin(changed)
	if err == nil {
		t.Fatal("expected changed query to reject page token")
	}
	assertProblemCode(t, err, "VALIDATION_INVALID_QUERY")
}

// testContext 返回test 上下文。
func testContext(target string) *gin.Context {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", target, nil)
	return c
}

// assertProblemCode 断言问题详情代码符合预期。
func assertProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	details := problem.FromError(err)
	if details.ErrorCode != code {
		t.Fatalf("expected error code %s, got %s", code, details.ErrorCode)
	}
}
