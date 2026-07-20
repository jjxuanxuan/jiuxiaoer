package routeplanning

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestCurrentHandlerRejectsUnknownQueryParameters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f := newServiceFixture(t)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("auth_claims", f.claims)
		c.Next()
	})
	RegisterRoutes(router.Group("/api/v1/delivery"), NewHandler(f.service))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/delivery/orders/30/route?mode=driving", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "VALIDATION_FAILED") || f.provider.count() != 0 {
		t.Fatalf("status=%d body=%s provider_calls=%d", recorder.Code, recorder.Body.String(), f.provider.count())
	}
}
