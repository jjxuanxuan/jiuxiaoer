package response

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestPageBodyAlwaysCarriesNextPageToken(t *testing.T) {
	payload, err := json.Marshal(PageBody{Items: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	if value, exists := body["next_page_token"]; !exists || value != "" {
		t.Fatalf("terminal page must carry an empty next_page_token, got %s", payload)
	}
}

func TestErrorUsesProblemJSONMediaType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("GET", "/api/v1/wine-tickets/packages", nil)

	Error(context, problem.InvalidArgument("VALIDATION_FAILED", "invalid request"))

	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/problem+json") {
		t.Fatalf("Content-Type=%q, want application/problem+json", got)
	}
	if recorder.Code != 400 {
		t.Fatalf("status=%d, want 400", recorder.Code)
	}
}
