package docs

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/goccy/go-yaml"
)

// TestOpenAPIAndSwaggerRoutes 验证打开 API And Swagger Routes的预期行为。
func TestOpenAPIAndSwaggerRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRoutes(router.Group("/api/v1"))

	openAPI := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/docs/openapi.yaml", nil)
	router.ServeHTTP(openAPI, req)
	if openAPI.Code != http.StatusOK {
		t.Fatalf("expected openapi status 200, got %d", openAPI.Code)
	}
	if !strings.Contains(openAPI.Body.String(), "openapi: 3.0.3") {
		t.Fatal("expected openapi document")
	}
	for _, required := range []string{
		"/store/orders/{id}/start-preparing:",
		"#/components/parameters/IdempotencyKey",
		"#/components/responses/Problem",
		"ProblemDetails:",
		"AdminStockAdjustReq:",
		"/payments/{provider}/callbacks:",
		"PaymentCreateReq:",
		"/store/print-settings:",
		"/messages/read-all:",
		"/delivery/orders/{id}/pickup:",
		"/delivery/orders/{id}/route:",
		"/admin/merchants/provision:",
		"/auth/rider/send-code:",
		"/auth/rider/sms-login:",
		"/auth/rider-application/sms-login:",
		"/admin/riders:",
		"/admin/riders/{id}/review:",
		"RiderCreateReq:",
		"RiderReviewReq:",
		"/admin/deliveries/{id}/reassign:",
		"/identity-verifications:",
		"pickup_code: { type: string, pattern: \"^[0-9]{6}$\" }",
		"IdentitySessionReq:",
	} {
		if !strings.Contains(openAPI.Body.String(), required) {
			t.Fatalf("expected openapi document to contain %s", required)
		}
	}
	operationCount := len(regexp.MustCompile(`(?m)^    (get|post|put|patch|delete):$`).FindAllString(openAPI.Body.String(), -1))
	if count := strings.Count(openAPI.Body.String(), "default: { $ref: \"#/components/responses/Problem\" }"); count != operationCount {
		t.Fatalf("expected all %d operations to define the standard problem response, got %d", operationCount, count)
	}
	assertOpenAPIRefsResolve(t, openAPI.Body.Bytes())

	swagger := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/swagger/index.html", nil)
	router.ServeHTTP(swagger, req)
	if swagger.Code != http.StatusOK {
		t.Fatalf("expected swagger status 200, got %d", swagger.Code)
	}
	if !strings.Contains(swagger.Body.String(), "SwaggerUIBundle") {
		t.Fatal("expected swagger ui html")
	}
}

// assertOpenAPIRefsResolve 防止文档语法合法、但组件引用路径无效。
func assertOpenAPIRefsResolve(t *testing.T, content []byte) {
	t.Helper()
	var document map[string]any
	if err := yaml.Unmarshal(content, &document); err != nil {
		t.Fatalf("parse openapi document: %v", err)
	}

	var walk func(any)
	walk = func(value any) {
		switch current := value.(type) {
		case map[string]any:
			for key, child := range current {
				if key == "$ref" {
					ref, ok := child.(string)
					if !ok || !strings.HasPrefix(ref, "#/components/") || !componentRefExists(document, ref) {
						t.Errorf("unresolved component reference: %v", child)
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range current {
				walk(child)
			}
		}
	}
	walk(document)
}

// componentRefExists 判断component Ref Exists。
func componentRefExists(document map[string]any, ref string) bool {
	current := any(document)
	for _, segment := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		object, ok := current.(map[string]any)
		if !ok {
			return false
		}
		current, ok = object[segment]
		if !ok {
			return false
		}
	}
	return true
}
