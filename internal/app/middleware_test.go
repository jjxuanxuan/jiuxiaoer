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

// TestRequestLimitsRejectKnownOversizedBody 验证请求限制拒绝已知的超大请求体。
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

func TestUntrustedForwardedForCannotSpoofClientIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	if err := router.SetTrustedProxies(nil); err != nil {
		t.Fatal(err)
	}
	router.GET("/ip", func(c *gin.Context) { c.String(http.StatusOK, c.ClientIP()) })

	request := httptest.NewRequest(http.MethodGet, "/ip", nil)
	request.RemoteAddr = "192.0.2.10:54321"
	request.Header.Set("X-Forwarded-For", "198.51.100.77")
	request.Header.Set("X-Real-IP", "198.51.100.78")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Body.String() != "192.0.2.10" {
		t.Fatalf("spoofed forwarding header became client IP: %q", recorder.Body.String())
	}
}

// TestRequestDeadlineIsSkippedAfterWebSocketUpgrade 验证 WebSocket 升级后跳过请求截止时间。
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

// TestRequestDeadlineReturnsGatewayTimeoutBeforeWrite 验证写入响应前请求超时会返回网关超时。
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
