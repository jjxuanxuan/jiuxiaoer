package realtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/modules/mq"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestMerchantTicketUsesCurrentDatabaseShopAuthorization 验证：
// 仅凭已签名但过期的门店列表不足以打开商户实时连接。
func TestMerchantTicketUsesCurrentDatabaseShopAuthorization(t *testing.T) {
	db := realtimeSQLite(t)
	createMerchantRealtimeTables(t, db)
	insertMerchantRealtimeAccount(t, db, 101, 201, 301, 401, "active", "active", nil)

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cfg := realtimeTestConfig()
	service := NewService(cfg, db, client, snowflake.New(21), nil)
	claims := merchantClaims("101", []string{"401"}, time.Now().Add(time.Hour))
	if err := client.HSet(context.Background(), "session:merchant:101:"+claims.SessionID, "access_jti", claims.ID).Err(); err != nil {
		t.Fatal(err)
	}

	ticket, err := service.IssueTicket(context.Background(), claims, TicketRequest{DeviceID: "merchant-device", Platform: "test", ClientVersion: "1.0.0", ProtocolVersion: 1}, "127.0.0.1")
	if err != nil {
		t.Fatalf("issue merchant ticket: %v", err)
	}
	info, err := service.ConsumeTicket(context.Background(), ticket.Ticket)
	if err != nil {
		t.Fatalf("consume merchant ticket: %v", err)
	}
	if recipientType, recipientID := info.recipient(); recipientType != recipientMerchant || recipientID != 101 || info.RiderID != 0 {
		t.Fatalf("unexpected merchant ticket identity: %+v", info)
	}

	withoutPermission := *claims
	withoutPermission.Permissions = nil
	if _, err := service.IssueTicket(context.Background(), &withoutPermission, TicketRequest{DeviceID: "merchant-device-2", Platform: "test", ClientVersion: "1.0.0", ProtocolVersion: 1}, ""); err == nil || problem.FromError(err).ErrorCode != "PERM_FORBIDDEN" {
		t.Fatalf("merchant ticket without store_order:list must fail, got %v", err)
	}
	if err := db.Exec("UPDATE merchant_user_shops SET deleted_at=? WHERE merchant_user_id=?", time.Now(), 201).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.IssueTicket(context.Background(), claims, TicketRequest{DeviceID: "merchant-device-3", Platform: "test", ClientVersion: "1.0.0", ProtocolVersion: 1}, ""); err == nil || problem.FromError(err).ErrorCode != "REALTIME_MERCHANT_FORBIDDEN" {
		t.Fatalf("stale token shop scope must not issue a ticket, got %v", err)
	}
}

// TestMerchantPaidOrderFanoutUsesAuthorizedAccountsAndSafeEvent 在不依赖
// WebSocket 客户端的情况下验证支付发件箱消费者边界。
func TestMerchantPaidOrderFanoutUsesAuthorizedAccountsAndSafeEvent(t *testing.T) {
	db := realtimeSQLite(t)
	createMerchantRealtimeTables(t, db)
	if err := db.Exec("CREATE TABLE orders (id INTEGER PRIMARY KEY, merchant_id INTEGER NOT NULL, shop_id INTEGER NOT NULL, pay_status TEXT NOT NULL, deleted_at DATETIME)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO orders(id,merchant_id,shop_id,pay_status) VALUES (9001,301,401,'succeeded')").Error; err != nil {
		t.Fatal(err)
	}
	insertMerchantRealtimeAccount(t, db, 101, 201, 301, 401, "active", "active", nil)
	insertMerchantRealtimeAccount(t, db, 102, 202, 301, 402, "active", "active", nil)
	insertMerchantRealtimeAccount(t, db, 103, 203, 301, 401, "disabled", "active", nil)
	deletedAt := time.Now()
	insertMerchantRealtimeAccount(t, db, 104, 204, 301, 401, "active", "active", &deletedAt)

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	subscription := client.Subscribe(context.Background(), wakeupChannel)
	if _, err := subscription.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subscription.Close() })

	service := NewService(realtimeTestConfig(), db, client, snowflake.New(22), nil)
	handler := NewMQHandler(service)
	eventID := uuid.NewString()
	occurredAt := time.Now().UTC().Truncate(time.Millisecond)
	envelope := mq.EventEnvelope{EventID: eventID, EventType: "order.paid", EventVersion: 1, AggregateType: "order", AggregateID: "9001", OccurredAt: occurredAt, Payload: json.RawMessage(`{"order_id":"9001","payment_id":"8001","customer_phone":"must-not-forward"}`)}
	result, err := handler.Handle(context.Background(), db, envelope)
	if err != nil {
		t.Fatal(err)
	}
	if result.RefType != "merchant_paid_order" || result.RefID != 9001 {
		t.Fatalf("unexpected consumer result: %+v", result)
	}
	if err := handler.AfterCommit(context.Background(), envelope, result); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	message, err := subscription.ReceiveMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var wakeup MerchantWakeup
	if err := json.Unmarshal([]byte(message.Payload), &wakeup); err != nil {
		t.Fatal(err)
	}
	if wakeup.AccountID != 101 {
		t.Fatalf("event leaked to a non-authorized account: %+v", wakeup)
	}
	assertSafeStoreOrderPaidEvent(t, wakeup.Event, eventID, "9001", "401")

	extraCtx, extraCancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer extraCancel()
	if extra, extraErr := subscription.ReceiveMessage(extraCtx); extraErr == nil {
		t.Fatalf("unexpected second merchant account fanout: %s", extra.Payload)
	}
}

// TestMerchantHubRejectsCrossShopAndDeduplicatesEventID 验证最终投递时的
// 授权检查和重复弹窗保护。
func TestMerchantHubRejectsCrossShopAndDeduplicatesEventID(t *testing.T) {
	db := realtimeSQLite(t)
	createMerchantRealtimeTables(t, db)
	insertMerchantRealtimeAccount(t, db, 101, 201, 301, 401, "active", "active", nil)
	service := NewService(realtimeTestConfig(), db, nil, snowflake.New(23), nil)
	hub := NewHub(realtimeTestConfig().Realtime, service, nil, nil, nil)
	target := &connection{id: "merchant-connection", info: TicketInfo{RecipientType: recipientMerchant, RecipientID: 101, AccountType: recipientMerchant, AccountID: "101"}, send: make(chan ServerFrame, 4)}
	if err := hub.register(target); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.unregister(target) })

	eventID := uuid.NewString()
	foreign := MerchantWakeup{AccountID: 101, Event: StoreOrderPaidEvent{EventID: eventID, OrderID: "9001", ShopID: "402", SoundKey: "new_paid_order", OccurredAt: time.Now().UTC()}}
	if err := hub.DeliverMerchant(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	if len(target.send) != 0 {
		t.Fatal("cross-shop event was delivered")
	}

	owned := foreign
	owned.Event.ShopID = "401"
	if err := hub.DeliverMerchant(context.Background(), owned); err != nil {
		t.Fatal(err)
	}
	if err := hub.DeliverMerchant(context.Background(), owned); err != nil {
		t.Fatal(err)
	}
	if len(target.send) != 1 {
		t.Fatalf("same event_id must enqueue once, got %d", len(target.send))
	}
	frame := <-target.send
	if frame.EventID != eventID || frame.EventType != "store.order.paid" || frame.DeliveryID != "" || frame.SourceEventID != "" || frame.RequiresAck {
		t.Fatalf("unexpected merchant frame: %+v", frame)
	}
	var payload map[string]any
	if err := json.Unmarshal(frame.Data, &payload); err != nil {
		t.Fatal(err)
	}
	wantKeys := map[string]bool{"event_id": true, "order_id": true, "shop_id": true, "sound_key": true, "occurred_at": true}
	if len(payload) != len(wantKeys) {
		t.Fatalf("merchant event must contain only safe keys, got %+v", payload)
	}
	for key := range payload {
		if !wantKeys[key] {
			t.Fatalf("unsafe or uncontracted merchant event field %q", key)
		}
	}
}

// TestMerchantReconnectExplicitlyRequiresOrderListResync 验证临时
// WebSocket 通道绝不会假装能够重放订单事实。
func TestMerchantReconnectExplicitlyRequiresOrderListResync(t *testing.T) {
	hub := NewHub(realtimeTestConfig().Realtime, nil, nil, nil, nil)
	target := &connection{info: TicketInfo{RecipientType: recipientMerchant, RecipientID: 101}, send: make(chan ServerFrame, 3)}
	if err := hub.handleResume(context.Background(), target, ClientFrame{RequestID: "resume-merchant"}); err != nil {
		t.Fatal(err)
	}
	resync := <-target.send
	complete := <-target.send
	if resync.EventType != "realtime.resync_required" || string(resync.Data) != `{"reason_code":"store_order_list_required"}` {
		t.Fatalf("merchant reconnect did not require list compensation: %+v", resync)
	}
	if complete.Type != FrameSyncComplete || complete.RequestID != "resume-merchant" || complete.HasMore == nil || *complete.HasMore {
		t.Fatalf("unexpected merchant sync completion: %+v", complete)
	}
}

func merchantClaims(accountID string, shopIDs []string, expiresAt time.Time) *auth.Claims {
	return &auth.Claims{
		TokenType: "access", SessionID: "merchant-session", AccountType: recipientMerchant, AccountID: accountID,
		MerchantUserID: "201", MerchantID: "301", AuthorizedShopIDs: shopIDs, Permissions: []string{"store_order:list"},
		RegisteredClaims: jwt.RegisteredClaims{ID: "merchant-access-jti", ExpiresAt: jwt.NewNumericDate(expiresAt)},
	}
}

func createMerchantRealtimeTables(t *testing.T, db *gorm.DB) {
	t.Helper()
	statements := []string{
		"CREATE TABLE accounts (id INTEGER PRIMARY KEY, account_type TEXT NOT NULL, status TEXT NOT NULL, deleted_at DATETIME)",
		"CREATE TABLE merchant_users (id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, merchant_id INTEGER NOT NULL, status TEXT NOT NULL, deleted_at DATETIME)",
		"CREATE TABLE merchant_user_shops (id INTEGER PRIMARY KEY, merchant_user_id INTEGER NOT NULL, merchant_id INTEGER NOT NULL, shop_id INTEGER NOT NULL, deleted_at DATETIME)",
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func insertMerchantRealtimeAccount(t *testing.T, db *gorm.DB, accountID, userID, merchantID, shopID uint64, accountStatus, userStatus string, mappingDeletedAt *time.Time) {
	t.Helper()
	if err := db.Exec("INSERT INTO accounts(id,account_type,status) VALUES (?,? ,?)", accountID, recipientMerchant, accountStatus).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO merchant_users(id,account_id,merchant_id,status) VALUES (?,?,?,?)", userID, accountID, merchantID, userStatus).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO merchant_user_shops(id,merchant_user_id,merchant_id,shop_id,deleted_at) VALUES (?,?,?,?,?)", userID+1000, userID, merchantID, shopID, mappingDeletedAt).Error; err != nil {
		t.Fatal(err)
	}
}

func assertSafeStoreOrderPaidEvent(t *testing.T, event StoreOrderPaidEvent, eventID, orderID, shopID string) {
	t.Helper()
	if event.EventID != eventID || event.OrderID != orderID || event.ShopID != shopID || event.SoundKey != "new_paid_order" || event.OccurredAt.IsZero() {
		t.Fatalf("unexpected merchant paid event: %+v", event)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"event_id": true, "order_id": true, "shop_id": true, "sound_key": true, "occurred_at": true}
	if len(payload) != len(allowed) {
		t.Fatalf("merchant event contains uncontracted fields: %+v", payload)
	}
	for key := range payload {
		if !allowed[key] {
			t.Fatalf("merchant event contains unsafe field %q", key)
		}
	}
}
