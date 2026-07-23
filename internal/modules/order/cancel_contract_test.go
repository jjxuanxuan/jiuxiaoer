package order

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestOrderCancelExpectedVersionIsRequiredAndAllowsZero(t *testing.T) {
	gin.SetMode(gin.TestMode)

	bind := func(body string) error {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest("POST", "/api/v1/orders/1/cancel", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		var request OrderCancelReq
		return ctx.ShouldBindJSON(&request)
	}

	if err := bind(`{"reason":"changed mind"}`); err == nil {
		t.Fatal("missing expected_version must be rejected")
	}
	if err := bind(`{"expected_version":0,"reason":"changed mind"}`); err != nil {
		t.Fatalf("version zero is a present optimistic-lock value and must bind: %v", err)
	}
}
