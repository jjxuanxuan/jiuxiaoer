package deliveryverification

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSensitiveVerificationResponsesAreNeverCacheableOnRejection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, call := range []struct {
		name string
		run  func(*Handler, *gin.Context)
	}{
		{name: "store", run: func(handler *Handler, ctx *gin.Context) { handler.GetStore(ctx) }},
		{name: "customer", run: func(handler *Handler, ctx *gin.Context) { handler.GetCustomer(ctx) }},
	} {
		t.Run(call.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest("GET", "/verification", nil)
			call.run(&Handler{}, ctx)
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
			if recorder.Code != 401 {
				t.Fatalf("status = %d, want 401", recorder.Code)
			}
		})
	}
}

func TestUnlockRequestRequiresPositiveExpectedVersion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bind := func(body string) error {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest("POST", "/admin/deliveries/1/verification/unlock", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		var req UnlockReq
		return ctx.ShouldBindJSON(&req)
	}
	if err := bind(`{"stage":"pickup","reason_code":"locked","reason":"operator verified the incident"}`); err == nil {
		t.Fatal("missing expected_version must be rejected")
	}
	if err := bind(`{"stage":"pickup","reason_code":"locked","reason":"operator verified the incident","expected_version":1}`); err != nil {
		t.Fatalf("valid unlock request was rejected: %v", err)
	}
}
