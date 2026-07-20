package realtime

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/response"
)

type Handler struct {
	cfg     config.RealtimeConfig
	service *Service
	hub     *Hub
}

// NewHandler 创建并初始化Handler。
func NewHandler(cfg config.RealtimeConfig, service *Service, hub *Hub) *Handler {
	return &Handler{cfg: cfg, service: service, hub: hub}
}

// RegisterRoutes 注册Routes。
func RegisterRoutes(api, protected *gin.RouterGroup, handler *Handler) {
	protected.POST("/realtime/tickets", handler.issueTicket)
	api.GET("/realtime/ws", handler.websocket)
}

// issueTicket 处理issue Ticket相关逻辑。
func (h *Handler) issueTicket(c *gin.Context) {
	if !h.cfg.Enabled {
		response.Error(c, realtimeDisabled())
		return
	}
	if !h.hub.Accepting() {
		response.Error(c, realtimeUnavailable("realtime server is draining"))
		return
	}
	var req TicketRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, problem.InvalidArgument("REALTIME_TICKET_REQUEST_INVALID", "request body is invalid"))
		return
	}
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		response.Error(c, problem.Unauthorized("AUTH_TOKEN_REQUIRED", "access token is required"))
		return
	}
	requestCtx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	result, err := h.service.IssueTicket(requestCtx, claims, req, c.ClientIP())
	if err != nil {
		response.Error(c, err)
		return
	}
	response.WithStatus(c, http.StatusCreated, result)
}

// websocket 处理WebSocket相关逻辑。
func (h *Handler) websocket(c *gin.Context) {
	if !h.cfg.Enabled {
		response.Error(c, realtimeDisabled())
		return
	}
	if !h.hub.Accepting() {
		response.Error(c, realtimeUnavailable("realtime server is draining"))
		return
	}
	ticket := strings.TrimSpace(c.Query("ticket"))
	if ticket == "" {
		response.Error(c, problem.Unauthorized("REALTIME_TICKET_EXPIRED", "ticket expired or invalid"))
		return
	}
	handshakeCtx, cancel := context.WithTimeout(c.Request.Context(), h.cfg.HandshakeTimeout)
	defer cancel()
	if err := h.service.CheckUpgradeRate(handshakeCtx, c.ClientIP()); err != nil {
		response.Error(c, err)
		return
	}
	info, err := h.service.ConsumeTicket(handshakeCtx, ticket)
	if err != nil {
		response.Error(c, err)
		return
	}
	if !h.hub.CanRegister(info.RiderID) {
		response.Error(c, problem.TooManyRequests("REALTIME_CONNECTION_LIMIT", "connection limit reached"))
		return
	}
	request := c.Request.Clone(handshakeCtx)
	ws, err := websocket.Accept(c.Writer, request, &websocket.AcceptOptions{
		OriginPatterns: h.cfg.AllowedOrigins, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	cancel()
	_ = h.hub.Serve(c.Request.Context(), ws, info)
}
