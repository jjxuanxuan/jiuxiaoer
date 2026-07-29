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
	seenMu     sync.Mutex
	seenEvents map[string]struct{}
	seenOrder  []string
}

// stop 停止连接中心。
func (c *connection) stop(code websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		go func() {
			_ = c.ws.Close(code, reason)
			c.cancel()
		}()
	})
}

// markEvent 在活动连接上抑制重复的商户 event_id。持久 MQ 消费者回执
// 防止正常重复投递；此有界缓存还保护客户端免受重复 Redis 唤醒影响。
func (c *connection) markEvent(eventID string) bool {
	if eventID == "" {
		return false
	}
	c.seenMu.Lock()
	defer c.seenMu.Unlock()
	if c.seenEvents == nil {
		c.seenEvents = make(map[string]struct{})
	}
	if _, exists := c.seenEvents[eventID]; exists {
		return false
	}
	c.seenEvents[eventID] = struct{}{}
	c.seenOrder = append(c.seenOrder, eventID)
	const maximumSeenEvents = 256
	if len(c.seenOrder) > maximumSeenEvents {
		oldest := c.seenOrder[0]
		c.seenOrder = c.seenOrder[1:]
		delete(c.seenEvents, oldest)
	}
	return true
}

type Hub struct {
	cfg         config.RealtimeConfig
	service     *Service
	redis       *redis.Client
	metrics     *metricState
	log         *slog.Logger
	mu          sync.RWMutex
	connections map[string]map[string]*connection
	draining    bool
	activeWG    sync.WaitGroup
}

// NewHub 创建并初始化消息中心。
func NewHub(cfg config.RealtimeConfig, service *Service, redisClient *redis.Client, metrics *metricState, log *slog.Logger) *Hub {
	if metrics == nil {
		metrics = newMetricState(nil, "")
	}
	return &Hub{cfg: cfg, service: service, redis: redisClient, metrics: metrics, log: log, connections: make(map[string]map[string]*connection)}
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
			var merchant MerchantWakeup
			if json.Unmarshal([]byte(message.Payload), &merchant) == nil && validMerchantWakeup(merchant) {
				if err := h.DeliverMerchant(ctx, merchant); err != nil {
					h.logError("merchant realtime wakeup delivery failed", err)
				}
				continue
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
	key := connectionKey(recipientRider, wakeup.RiderID)
	h.mu.RLock()
	targets := make([]*connection, 0, len(h.connections[key]))
	for _, target := range h.connections[key] {
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

// DeliverMerchant 在发送前立即重新检查当前账户与门店授权，
// 绝不信任 WebSocket 票据中的过期门店列表。
func (h *Hub) DeliverMerchant(ctx context.Context, wakeup MerchantWakeup) error {
	if !validMerchantWakeup(wakeup) {
		return fmt.Errorf("merchant wakeup is invalid")
	}
	shopID, err := strconv.ParseUint(wakeup.Event.ShopID, 10, 64)
	if err != nil || shopID == 0 {
		return fmt.Errorf("merchant wakeup shop_id is invalid")
	}
	key := connectionKey(recipientMerchant, wakeup.AccountID)
	h.mu.RLock()
	targets := make([]*connection, 0, len(h.connections[key]))
	for _, target := range h.connections[key] {
		targets = append(targets, target)
	}
	h.mu.RUnlock()
	if len(targets) == 0 {
		return nil
	}
	if h.service == nil {
		return fmt.Errorf("realtime service is unavailable")
	}
	allowed, err := h.service.MerchantAccountAuthorized(ctx, wakeup.AccountID, shopID)
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}
	frame := storeOrderPaidFrame(wakeup.Event)
	for _, target := range targets {
		if !target.markEvent(wakeup.Event.EventID) {
			continue
		}
		if !h.enqueue(target, frame) {
			h.metrics.slowClose()
			target.stop(websocket.StatusTryAgainLater, "slow consumer")
		}
	}
	return nil
}

func validMerchantWakeup(wakeup MerchantWakeup) bool {
	if wakeup.AccountID == 0 || wakeup.Event.EventID == "" || wakeup.Event.OrderID == "" || wakeup.Event.ShopID == "" || wakeup.Event.OccurredAt.IsZero() {
		return false
	}
	switch wakeup.Event.EventType {
	case "":
		if wakeup.Event.SoundKey != "new_paid_order" {
			return false
		}
	case "store.order.paid":
		if wakeup.Event.SoundKey != "new_paid_order" {
			return false
		}
	case "store.wine_ticket.redemption.created":
		if wakeup.Event.SoundKey != "new_wine_ticket_redemption" {
			return false
		}
	default:
		return false
	}
	orderID, orderErr := strconv.ParseUint(wakeup.Event.OrderID, 10, 64)
	shopID, shopErr := strconv.ParseUint(wakeup.Event.ShopID, 10, 64)
	return orderErr == nil && shopErr == nil && orderID > 0 && shopID > 0
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

// writeLoop 运行连接写循环。
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
	recipientType, _ := target.info.recipient()
	if recipientType == recipientMerchant {
		// 商户 WebSocket 事件只是唤醒信号，绝不是订单事实。
		// 每次重连都会明确通知客户端刷新限定范围的列表。
		resync := frameNow(FrameEvent)
		resync.EventType = "realtime.resync_required"
		resync.Data = json.RawMessage(`{"reason_code":"store_order_list_required"}`)
		if !h.enqueue(target, resync) {
			return fmt.Errorf("realtime send queue full")
		}
		complete := frameNow(FrameSyncComplete)
		complete.RequestID = frame.RequestID
		complete.LastDeliveryID = "0"
		hasMore := false
		complete.HasMore = &hasMore
		if !h.enqueue(target, complete) {
			return fmt.Errorf("realtime send queue full")
		}
		return nil
	}
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

// initialResume 返回初始续传帧。
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

// enqueue 尝试将消息加入发送队列。
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
	recipientType, recipientID := target.info.recipient()
	if recipientID == 0 || (recipientType != recipientRider && recipientType != recipientMerchant) {
		return fmt.Errorf("realtime recipient is invalid")
	}
	key := connectionKey(recipientType, recipientID)
	connections := h.connections[key]
	if connections == nil {
		connections = make(map[string]*connection)
		h.connections[key] = connections
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
func (h *Hub) CanRegister(info TicketInfo) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	recipientType, recipientID := info.recipient()
	if recipientID == 0 || (recipientType != recipientRider && recipientType != recipientMerchant) {
		return false
	}
	return !h.draining && len(h.connections[connectionKey(recipientType, recipientID)]) < h.cfg.MaxConnectionsPerRider
}

// Accepting 判断Accepting。
func (h *Hub) Accepting() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return !h.draining
}

// unregister 注销连接。
func (h *Hub) unregister(target *connection) {
	h.mu.Lock()
	defer h.mu.Unlock()
	recipientType, recipientID := target.info.recipient()
	key := connectionKey(recipientType, recipientID)
	connections := h.connections[key]
	if _, ok := connections[target.id]; !ok {
		return
	}
	delete(connections, target.id)
	if len(connections) == 0 {
		delete(h.connections, key)
	}
	h.activeWG.Done()
	h.metrics.connection(-1)
}

func connectionKey(recipientType string, recipientID uint64) string {
	return recipientType + ":" + strconv.FormatUint(recipientID, 10)
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

// normalizeCloseError 规范化关闭错误。
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
