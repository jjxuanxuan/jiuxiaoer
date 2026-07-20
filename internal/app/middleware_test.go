package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/gin-gonic/gin"
)

// TestRequestLimitsRejectKnownOversizedBody 验证请求 Limits Reject Known Oversized 请求体的预期行为。
func TestRequestLimitsRejectKnownOversizedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(requestIDMiddleware(), requestLimitsMiddleware(time.Second, 8))
	router.POST("/write", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/write", strings.NewReader("123456789"))
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", recorder.Code)
	}
}

// TestRequestDeadlineIsSkippedAfterWebSocketUpgrade 验证请求 Deadline Is Skipped 售后 Web Socket Upgrade的预期行为。
func TestRequestDeadlineIsSkippedAfterWebSocketUpgrade(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(requestIDMiddleware(), requestLimitsMiddleware(10*time.Millisecond, 1024))
	router.GET("/ws", func(c *gin.Context) {
		connection, err := websocket.Accept(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		select {
		case <-c.Request.Context().Done():
			return
		case <-time.After(50 * time.Millisecond):
			_ = wsjson.Write(c.Request.Context(), connection, map[string]bool{"alive": true})
		}
	})
	server := httptest.NewServer(router)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	var frame map[string]bool
	if err := wsjson.Read(ctx, connection, &frame); err != nil {
		t.Fatalf("WebSocket inherited ordinary request deadline: %v", err)
	}
	if !frame["alive"] {
		t.Fatalf("unexpected frame: %+v", frame)
	}
}

// TestRequestDeadlineReturnsGatewayTimeoutBeforeWrite 验证请求 Deadline Returns 网关超时 Before 写入的预期行为。
func TestRequestDeadlineReturnsGatewayTimeoutBeforeWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(requestIDMiddleware(), requestLimitsMiddleware(10*time.Millisecond, 1024))
	router.GET("/slow", func(c *gin.Context) {
		<-c.Request.Context().Done()
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/slow", nil))
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d", recorder.Code)
	}
}
