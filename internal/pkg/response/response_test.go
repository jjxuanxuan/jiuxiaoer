package response

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

// TestRateLimitedResponseIncludesRetryAfter 验证速率 Limited 响应 Includes 重试售后的预期行为。
func TestRateLimitedResponseIncludesRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/dispatch", nil)
	err := problem.TooManyRequests("RATE_LIMITED", "try later")
	err.Data = map[string]any{"retry_after_seconds": 30}

	Error(ctx, err)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want=%d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After=%q want=30", got)
	}
}

func TestTooEarlyResponseIncludesRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/evidence", nil)
	err := problem.New(http.StatusTooEarly, "SCAN_PENDING", "Too Early", "scan is pending")
	err.Data = map[string]any{"retry_after_seconds": 10}

	Error(ctx, err)

	if recorder.Code != http.StatusTooEarly {
		t.Fatalf("status=%d want=%d", recorder.Code, http.StatusTooEarly)
	}
	if got := recorder.Header().Get("Retry-After"); got != "10" {
		t.Fatalf("Retry-After=%q want=10", got)
	}
}
