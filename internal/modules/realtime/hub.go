package realtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
)

type connection struct {
	id         string
	info       TicketInfo
	ws         *websocket.Conn
	send       chan ServerFrame
	cancel     context.CancelFunc
	resumeOnce sync.Once
	closeOnce  sync.Once
}

// stop 处理stop相关逻辑。
func (c *connection) stop(code websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		go func() {
			_ = c.ws.Close(code, reason)
			c.cancel()
		}()
	})
}

type Hub struct {
	cfg         config.RealtimeConfig
	service     *Service
	redis       *redis.Client
	metrics     *metricState
	log         *slog.Logger
	mu          sync.RWMutex
	connections map[uint64]map[string]*connection
	draining    bool
	activeWG    sync.WaitGroup
}

// NewHub 创建并初始化消息中心。
func NewHub(cfg config.RealtimeConfig, service *Service, redisClient *redis.Client, metrics *metricState, log *slog.Logger) *Hub {
	if metrics == nil {
		metrics = newMetricState(nil, "")
	}
	return &Hub{cfg: cfg, service: service, redis: redisClient, metrics: metrics, log: log, connections: make(map[uint64]map[string]*connection)}
}

// RunSubscriber 运行Subscriber处理流程。
func (h *Hub) RunSubscriber(ctx context.Context) {
	if !h.cfg.Enabled || h.redis == nil {
		return
	}
	pubsub := h.redis.Subscribe(ctx, wakeupChannel)
	defer pubsub.Close()
	if _, err := pubsub.Receive(ctx); err != nil {
		h.logError("realtime Redis subscription failed", err)
		return
	}
	channel := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case message, ok := <-channel:
			if !ok {
				return
			}
			var wakeup Wakeup
			if json.Unmarshal([]byte(message.Payload), &wakeup) != nil || wakeup.DeliveryID == 0 || wakeup.RiderID == 0 {
				continue
			}
			if !wakeup.ExpiresAt.IsZero() && !wakeup.ExpiresAt.After(time.Now()) {
				continue
			}
			if err := h.Deliver(ctx, wakeup); err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				h.logError("realtime wakeup delivery failed", err)
			}
		}
	}
}

// Deliver 返回Deliver。
func (h *Hub) Deliver(ctx context.Context, wakeup Wakeup) error {
	h.mu.RLock()
	targets := make([]*connection, 0, len(h.connections[wakeup.RiderID]))
	for _, target := range h.connections[wakeup.RiderID] {
		targets = append(targets, target)
	}
	h.mu.RUnlock()
	if len(targets) == 0 {
		return nil
	}
	row, err := h.service.LoadDelivery(ctx, wakeup.RiderID, wakeup.DeliveryID)
	if err != nil {
		return err
	}
	frame := deliveryFrame(row)
	for _, target := range targets {
		if !h.enqueue(target, frame) {
			h.metrics.slowClose()
			target.stop(websocket.StatusTryAgainLater, "slow consumer")
		}
	}
	return nil
}

// Serve 返回Serve。
func (h *Hub) Serve(ctx context.Context, ws *websocket.Conn, info TicketInfo) error {
	connectionCtx, cancel := context.WithCancel(ctx)
	target := &connection{id: "rtc_" + uuid.NewString(), info: info, ws: ws, send: make(chan ServerFrame, h.cfg.SendQueueSize), cancel: cancel}
	if err := h.register(target); err != nil {
		cancel()
		_ = ws.Close(websocket.StatusTryAgainLater, "connection limit reached")
		return err
	}
	defer func() {
		cancel()
		ws.CloseNow()
		h.unregister(target)
	}()

	hello := frameNow(FrameHello)
	hello.ConnectionID = target.id
	hello.HeartbeatIntervalSeconds = int(h.cfg.HeartbeatInterval.Seconds())
	hello.MaxResumeItems = h.cfg.ResumeLimit
	h.enqueue(target, hello)

	writerErr := make(chan error, 1)
	go func() { writerErr <- h.writeLoop(connectionCtx, target) }()
	readerErr := make(chan error, 1)
	go func() { readerErr <- h.readLoop(connectionCtx, target) }()
	go h.defaultResume(connectionCtx, target)

	select {
	case err := <-readerErr:
		cancel()
		<-writerErr
		return normalizeCloseError(err)
	case err := <-writerErr:
		cancel()
		<-readerErr
		return normalizeCloseError(err)
	}
}

// writeLoop 写入Loop。
func (h *Hub) writeLoop(ctx context.Context, target *connection) error {
	heartbeat := time.NewTicker(h.cfg.HeartbeatInterval)
	sessionCheck := time.NewTicker(h.cfg.SessionCheckInterval)
	accessExpiry := time.NewTimer(max(time.Until(target.info.AccessExpiresAt), time.Millisecond))
	defer heartbeat.Stop()
	defer sessionCheck.Stop()
	defer accessExpiry.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame := <-target.send:
			writeCtx, cancel := context.WithTimeout(ctx, h.cfg.PongTimeout)
			err := wsjson.Write(writeCtx, target.ws, frame)
			cancel()
			if err != nil {
				h.metrics.incPair(h.metrics.sends, metricEventType(frame), "error")
				return err
			}
			h.metrics.incPair(h.metrics.sends, metricEventType(frame), "success")
			if frame.Type == FrameEvent && frame.OccurredAt != nil {
				h.metrics.observeLag(frame.EventType, time.Since(*frame.OccurredAt))
			}
		case <-heartbeat.C:
			pingCtx, cancel := context.WithTimeout(ctx, h.cfg.PongTimeout)
			err := target.ws.Ping(pingCtx)
			cancel()
			if err != nil {
				h.metrics.sessionClose("pong_timeout")
				return err
			}
		case <-sessionCheck.C:
			valid, err := h.service.SessionValid(ctx, target.info)
			if err != nil {
				h.metrics.sessionClose("dependency_unavailable")
				_ = target.ws.Close(websocket.StatusTryAgainLater, "session dependency unavailable")
				return fmt.Errorf("realtime session dependency unavailable")
			}
			if !valid {
				h.metrics.sessionClose("revoked")
				_ = target.ws.Close(websocket.StatusPolicyViolation, "session revoked")
				return fmt.Errorf("realtime session invalid")
			}
		case <-accessExpiry.C:
			h.metrics.sessionClose("access_expired")
			_ = target.ws.Close(websocket.StatusPolicyViolation, "access token expired")
			return fmt.Errorf("realtime access token expired")
		}
	}
}

// readLoop 读取Loop。
func (h *Hub) readLoop(ctx context.Context, target *connection) error {
	target.ws.SetReadLimit(h.cfg.MaxFrameBytes)
	windowStarted := time.Now()
	ackFrames, resumeFrames := 0, 0
	for {
		var frame ClientFrame
		messageType, payload, err := target.ws.Read(ctx)
		if err != nil {
			if ctx.Err() == nil && websocket.CloseStatus(err) == -1 {
				_ = target.ws.Close(websocket.StatusProtocolError, "invalid JSON frame")
			}
			return err
		}
		if messageType != websocket.MessageText {
			_ = target.ws.Close(websocket.StatusProtocolError, "text frames required")
			return fmt.Errorf("realtime binary frame rejected")
		}
		decoder := json.NewDecoder(bytes.NewReader(payload))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&frame); err != nil {
			_ = target.ws.Close(websocket.StatusProtocolError, "invalid JSON frame")
			return err
		}
		if time.Since(windowStarted) >= time.Minute {
			windowStarted, ackFrames, resumeFrames = time.Now(), 0, 0
		}
		if frame.ProtocolVersion != ProtocolVersion || frame.RequestID == "" || len(frame.RequestID) > 128 {
			h.sendError(target, frame.RequestID, "REALTIME_PROTOCOL_UNSUPPORTED", "unsupported protocol version or invalid request_id")
			_ = target.ws.Close(websocket.StatusProtocolError, "invalid protocol frame")
			return fmt.Errorf("realtime protocol frame invalid")
		}
		switch frame.Type {
		case FrameResume:
			resumeFrames++
			if resumeFrames > h.cfg.ResumeRatePerMinute {
				h.sendError(target, frame.RequestID, "REALTIME_RATE_LIMITED", "rate limited")
				continue
			}
			if err := h.initialResume(ctx, target, frame); err != nil {
				h.sendError(target, frame.RequestID, "REALTIME_RESUME_FAILED", "resume request failed")
			}
		case FrameAck:
			ackFrames++
			if ackFrames > h.cfg.ACKRatePerMinute {
				_ = target.ws.Close(websocket.StatusPolicyViolation, "rate limited")
				return fmt.Errorf("realtime ack rate exceeded")
			}
			if err := h.service.Acknowledge(ctx, target.info, frame); err != nil {
				h.sendError(target, frame.RequestID, "REALTIME_ACK_FAILED", "acknowledgement was rejected")
				continue
			}
			accepted := frameNow(FrameAckResult)
			accepted.RequestID = frame.RequestID
			accepted.DeliveryID = frame.DeliveryID
			accepted.Accepted = true
			h.enqueue(target, accepted)
		default:
			h.sendError(target, frame.RequestID, "REALTIME_FRAME_TYPE_UNSUPPORTED", "frame type is unsupported")
		}
	}
}

// handleResume 处理Resume请求。
func (h *Hub) handleResume(ctx context.Context, target *connection, frame ClientFrame) error {
	var afterID uint64
	var err error
	if frame.AfterDeliveryID != "" && frame.AfterDeliveryID != "0" {
		afterID, err = strconv.ParseUint(frame.AfterDeliveryID, 10, 64)
		if err != nil {
			return err
		}
	}
	rows, hasMore, err := h.service.Resume(ctx, target.info, afterID)
	if err != nil {
		return err
	}
	lastID := afterID
	if hasMore {
		resync := frameNow(FrameEvent)
		resync.EventType = "realtime.resync_required"
		resync.Data = json.RawMessage(`{"reason_code":"resume_overflow"}`)
		if !h.enqueue(target, resync) {
			return fmt.Errorf("realtime send queue full")
		}
	} else {
		for _, row := range rows {
			if !h.enqueue(target, deliveryFrame(row)) {
				return fmt.Errorf("realtime send queue full")
			}
			lastID = row.ID
		}
	}
	complete := frameNow(FrameSyncComplete)
	complete.RequestID = frame.RequestID
	complete.LastDeliveryID = strconv.FormatUint(lastID, 10)
	complete.HasMore = &hasMore
	if !h.enqueue(target, complete) {
		return fmt.Errorf("realtime send queue full")
	}
	return nil
}

// initialResume 返回initial Resume。
func (h *Hub) initialResume(ctx context.Context, target *connection, frame ClientFrame) error {
	executed := false
	var result error
	target.resumeOnce.Do(func() {
		executed = true
		result = h.handleResume(ctx, target, frame)
	})
	if !executed {
		return fmt.Errorf("initial resume already completed")
	}
	return result
}

// defaultResume 处理默认项 Resume相关逻辑。
func (h *Hub) defaultResume(ctx context.Context, target *connection) {
	timer := time.NewTimer(h.cfg.HandshakeTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
		frame := ClientFrame{ProtocolVersion: ProtocolVersion, Type: FrameResume, RequestID: "server_default_resume"}
		if err := h.initialResume(ctx, target, frame); err != nil && !errors.Is(err, context.Canceled) {
			h.sendError(target, frame.RequestID, "REALTIME_RESUME_FAILED", "resume request failed")
		}
	}
}

// sendError 发送错误。
func (h *Hub) sendError(target *connection, requestID, code, detail string) {
	frame := frameNow(FrameError)
	frame.RequestID, frame.ErrorCode, frame.Detail = requestID, code, detail
	if !h.enqueue(target, frame) {
		target.stop(websocket.StatusTryAgainLater, "slow consumer")
	}
}

// enqueue 判断enqueue。
func (h *Hub) enqueue(target *connection, frame ServerFrame) bool {
	select {
	case target.send <- frame:
		return true
	default:
		return false
	}
}

// register 注册实时消息。
func (h *Hub) register(target *connection) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.draining {
		return fmt.Errorf("realtime server is draining")
	}
	connections := h.connections[target.info.RiderID]
	if connections == nil {
		connections = make(map[string]*connection)
		h.connections[target.info.RiderID] = connections
	}
	if len(connections) >= h.cfg.MaxConnectionsPerRider {
		return fmt.Errorf("realtime rider connection limit reached")
	}
	connections[target.id] = target
	h.activeWG.Add(1)
	h.metrics.connection(1)
	return nil
}

// CanRegister 判断当前条件是否允许Register。
func (h *Hub) CanRegister(riderID uint64) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return !h.draining && len(h.connections[riderID]) < h.cfg.MaxConnectionsPerRider
}

// Accepting 判断Accepting。
func (h *Hub) Accepting() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return !h.draining
}

// unregister 处理unregister相关逻辑。
func (h *Hub) unregister(target *connection) {
	h.mu.Lock()
	defer h.mu.Unlock()
	connections := h.connections[target.info.RiderID]
	if _, ok := connections[target.id]; !ok {
		return
	}
	delete(connections, target.id)
	if len(connections) == 0 {
		delete(h.connections, target.info.RiderID)
	}
	h.activeWG.Done()
	h.metrics.connection(-1)
}

// Shutdown 平滑停止当前实例并释放相关资源。
func (h *Hub) Shutdown(ctx context.Context) {
	h.mu.Lock()
	h.draining = true
	connections := make([]*connection, 0)
	for _, riders := range h.connections {
		for _, target := range riders {
			connections = append(connections, target)
		}
	}
	h.mu.Unlock()
	frame := frameNow(FrameServerShutdown)
	for _, target := range connections {
		if !h.enqueue(target, frame) {
			target.stop(websocket.StatusTryAgainLater, "slow consumer")
		}
	}
	if len(connections) == 0 {
		return
	}
	allDone := make(chan struct{})
	go func() {
		h.activeWG.Wait()
		close(allDone)
	}()
	timer := time.NewTimer(h.cfg.ShutdownDrainTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-allDone:
		return
	case <-timer.C:
	}
	for _, target := range connections {
		target.cancel()
		_ = target.ws.CloseNow()
	}
}

// logError 处理日志错误相关逻辑。
func (h *Hub) logError(message string, err error) {
	if h.log != nil {
		h.log.Error(message, slog.Any("error", err))
	}
}

// normalizeCloseError 规范化Close 错误。
func normalizeCloseError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return nil
	}
	status := websocket.CloseStatus(err)
	if status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway {
		return nil
	}
	return err
}

// metricEventType 返回指标事件 Type。
func metricEventType(frame ServerFrame) string {
	if frame.EventType != "" {
		return frame.EventType
	}
	return frame.Type
}
