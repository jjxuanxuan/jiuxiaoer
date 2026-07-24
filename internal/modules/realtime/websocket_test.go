package realtime

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestWebSocketHelloResumeAndSyncComplete 验证 WebSocket 握手、续传和同步完成流程。
func TestWebSocketHelloResumeAndSyncComplete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	db := realtimeSQLite(t)
	cfg := realtimeTestConfig()
	ids := snowflake.New(4)
	metrics := newMetricState(nil, "test")
	service := NewService(cfg, db, client, ids, metrics)
	hub := NewHub(cfg.Realtime, service, client, metrics, nil)
	handler := NewHandler(cfg.Realtime, service, hub)
	claims := riderClaims("101", time.Now().Add(time.Hour))
	if err := client.HSet(context.Background(), "session:rider:"+claims.AccountID+":"+claims.SessionID, "access_jti", claims.ID).Err(); err != nil {
		t.Fatal(err)
	}
	ticket, err := service.IssueTicket(context.Background(), claims, TicketRequest{DeviceID: "device", Platform: "test", ClientVersion: "1.0.0", ProtocolVersion: 1}, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.GET("/api/v1/realtime/ws", handler.websocket)
	server := httptest.NewServer(router)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + ticket.WSPath + "?ticket=" + ticket.Ticket
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	var hello ServerFrame
	if err := wsjson.Read(ctx, connection, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.Type != FrameHello || hello.ProtocolVersion != 1 || hello.HeartbeatIntervalSeconds != 25 {
		t.Fatalf("unexpected hello: %+v", hello)
	}
	if err := wsjson.Write(ctx, connection, ClientFrame{ProtocolVersion: 1, Type: FrameResume, RequestID: "resume-1"}); err != nil {
		t.Fatal(err)
	}
	var complete ServerFrame
	if err := wsjson.Read(ctx, connection, &complete); err != nil {
		t.Fatal(err)
	}
	if complete.Type != FrameSyncComplete || complete.RequestID != "resume-1" || complete.HasMore == nil || *complete.HasMore {
		t.Fatalf("unexpected sync completion: %+v", complete)
	}
}
