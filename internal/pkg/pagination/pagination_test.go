package pagination

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

// TestFromGinAcceptsWhitelistedOrderAndSimpleFilter 验证 Gin 参数解析接受白名单排序和简单筛选条件。
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

// TestFromGinRejectsUnknownOrderField 验证 Gin 参数解析拒绝未知排序字段。
func TestFromGinRejectsUnknownOrderField(t *testing.T) {
	c := testContext("/items?order_by=deleted_at%20desc")

	_, err := FromGin(c)
	if err == nil {
		t.Fatal("expected invalid order_by error")
	}
	assertProblemCode(t, err, "VALIDATION_INVALID_QUERY")
}

func TestPageTokenRejectsTamperingAndDifferentActorScope(t *testing.T) {
	first := testContext("/items?page_size=2&filter=status:paid")
	query, err := FromGin(first, "customer", "100")
	if err != nil {
		t.Fatal(err)
	}
	token := NextPageTokenWithCursor(query, "2026-07-22T12:00:00Z", "99")

	replacement := byte('A')
	if token[len(token)-1] == replacement {
		replacement = 'B'
	}
	tampered := token[:len(token)-1] + string(replacement)
	if _, err := FromGin(testContext("/items?page_size=2&filter=status:paid&page_token="+tampered), "customer", "100"); err == nil {
		t.Fatal("tampered token must be rejected")
	} else {
		assertProblemCode(t, err, "PAGE_TOKEN_INVALID")
	}
	if _, err := FromGin(testContext("/items?page_size=2&filter=status:paid&page_token="+token), "customer", "101"); err == nil {
		t.Fatal("token reused by another actor must be rejected")
	} else {
		assertProblemCode(t, err, "PAGE_TOKEN_INVALID")
	}
}

// TestFromGinRejectsInvalidFilterSyntax 验证 Gin 参数解析拒绝无效筛选语法。
func TestFromGinRejectsInvalidFilterSyntax(t *testing.T) {
	c := testContext("/items?filter=status==(paid")

	_, err := FromGin(c)
	if err == nil {
		t.Fatal("expected invalid filter error")
	}
	assertProblemCode(t, err, "VALIDATION_INVALID_QUERY")
}

// TestFromGinAcceptsAdditionalWhitelistedOrderFields 验证 Gin 参数解析接受额外的白名单排序字段。
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

// TestPageTokenKeepsExactNextOffset 验证分页令牌保留准确的下一页偏移量。
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

// TestPageTokenRejectsChangedQuery 验证分页令牌拒绝发生变化的查询条件。
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
	assertProblemCode(t, err, "PAGE_TOKEN_INVALID")
}

// testContext 返回测试上下文。
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
