package printjob

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestPrintTaskListQueryContractIsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, target := range []string{
		"/api/v1/store/print-tasks?order_by=id%20desc",
		"/api/v1/store/print-tasks?filter=status:pending",
		"/api/v1/store/print-tasks?unknown=value",
		"/api/v1/store/print-tasks?status=provider_secret",
	} {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodGet, target, nil)
		if _, err := printTaskListStatusFromGin(ctx); problem.FromError(err).ErrorCode != "VALIDATION_INVALID_QUERY" {
			t.Fatalf("invalid print-task query accepted for %s: %v", target, err)
		}
	}

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/store/print-tasks?page_size=20&status=retry_wait", nil)
	if status, err := printTaskListStatusFromGin(ctx); err != nil || status != "retry_wait" {
		t.Fatalf("documented print-task query rejected: status=%q err=%v", status, err)
	}
}
