package gift

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestGiftPreviewReadsOnlySensitiveHeaderAndNeverCachesFailures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service, _, _ := newGiftTestService(t)
	router := gin.New()
	api := router.Group("/api/v1")
	RegisterGiftPublicRoutes(api, NewGiftHandler(service))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/wine-tickets/gift-claims/preview", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "WT_GIFT_TOKEN_INVALID") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("cache control=%q", recorder.Header().Get("Cache-Control"))
	}

	// 类似令牌的查询参数会被拒绝，不会被当作领取凭证。
	// 服务只读取 X-Wine-Gift-Token。
	request = httptest.NewRequest(
		http.MethodGet,
		"/api/v1/wine-tickets/gift-claims/preview?token=must-not-be-read",
		nil,
	)
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "VALIDATION_INVALID_QUERY") {
		t.Fatalf("query credential status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("query failure cache control=%q", recorder.Header().Get("Cache-Control"))
	}
}
