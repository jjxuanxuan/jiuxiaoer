package refund

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/paygateway"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestRefundCallbackBodyBoundaries(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run refund callback boundary test")
	}
	db := openRefundAcceptanceDB(t)
	ids := snowflake.New(991)
	fx := insertRefundAcceptanceFixture(t, db, ids, "pending", 400, 1000)
	defer cleanupRefundAcceptance(t, db, fx)
	provider := &acceptanceProvider{callbackState: successState(fx)}
	service := NewService(config.Load(), db, ids, provider)
	router := gin.New()
	router.POST("/refunds/:provider/callbacks", NewHandler(service).Callback)

	tooLarge := httptest.NewRequest(http.MethodPost, "/refunds/wechat/callbacks", bytes.NewReader(bytes.Repeat([]byte{'x'}, int(paygateway.MaxCallbackBodyBytes+1))))
	tooLarge.Header.Set("X-Event-ID", "refund-boundary-too-large-"+fx.refundNo)
	tooLargeResp := httptest.NewRecorder()
	router.ServeHTTP(tooLargeResp, tooLarge)
	if tooLargeResp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too-large status=%d", tooLargeResp.Code)
	}

	readFailure := httptest.NewRequest(http.MethodPost, "/refunds/wechat/callbacks", nil)
	readFailure.Body = io.NopCloser(refundErrorReader{})
	readFailure.Header.Set("X-Event-ID", "refund-boundary-read-failure-"+fx.refundNo)
	readFailureResp := httptest.NewRecorder()
	router.ServeHTTP(readFailureResp, readFailure)
	if readFailureResp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("read-failure status=%d", readFailureResp.Code)
	}

	maximum := httptest.NewRequest(http.MethodPost, "/refunds/wechat/callbacks", bytes.NewReader(bytes.Repeat([]byte{'y'}, int(paygateway.MaxCallbackBodyBytes))))
	maximum.Header.Set("X-Event-ID", "refund-boundary-maximum-"+fx.refundNo)
	maximumResp := httptest.NewRecorder()
	router.ServeHTTP(maximumResp, maximum)
	if maximumResp.Code != http.StatusOK {
		t.Fatalf("maximum legal body status=%d body=%s", maximumResp.Code, maximumResp.Body.String())
	}
	assertRefundLedger(t, db, fx, "succeeded", 400)
}

type refundErrorReader struct{}

func (refundErrorReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
