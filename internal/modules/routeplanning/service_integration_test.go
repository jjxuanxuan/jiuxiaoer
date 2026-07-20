package routeplanning

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestMySQLRuntimeUserCanPlanAndCacheDeliveryRoute(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run route planning integration test")
	}
	cfg := config.Load()
	dsn := os.Getenv("JXE_MYSQL_RUNTIME_DSN")
	if dsn == "" {
		dsn = cfg.MySQL.DSN
	}
	if dsn == "" || cfg.Redis.Addr == "" {
		t.Fatal("local MySQL runtime DSN and Redis are required")
	}
	db, err := gorm.Open(gormmysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()

	redisClient := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: 12})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := redisClient.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = redisClient.FlushDB(context.Background()).Err()
		_ = redisClient.Close()
	}()

	ids := snowflake.New(812)
	accountID, riderID, orderID, deliveryID := ids.Next(), ids.Next(), ids.Next(), ids.Next()
	username := fmt.Sprintf("route_it_%d", accountID)
	if err := tx.Table("accounts").Create(map[string]any{"id": accountID, "account_type": "rider", "username": username, "status": "active", "credential_version": 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Table("riders").Create(map[string]any{"id": riderID, "account_id": accountID, "name": "route integration", "status": "active", "review_status": "approved", "work_status": "online", "work_status_version": 1, "service_scope": json.RawMessage(`{"shop_ids":["4201"]}`)}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := tx.Table("rider_runtime_states").Create(map[string]any{"rider_id": riderID, "work_status": "online", "latitude": 22.541, "longitude": 113.931, "coordinate_system": "gcj02", "accuracy_m": 10, "captured_at": now, "heartbeat_at": now, "last_sequence": 1, "version": 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Table("orders").Create(map[string]any{"id": orderID, "order_no": fmt.Sprintf("ROUTE-%d", orderID), "customer_id": ids.Next(), "merchant_id": 4001, "shop_id": 4201, "status": "paid", "pay_status": "succeeded", "delivery_status": "accepted"}).Error; err != nil {
		t.Fatal(err)
	}
	pickup := json.RawMessage(`{"latitude":22.542,"longitude":113.932,"coordinate_system":"gcj02"}`)
	recipient := json.RawMessage(`{"latitude":22.552,"longitude":113.942,"coordinate_system":"gcj02"}`)
	if err := tx.Table("delivery_orders").Create(map[string]any{"id": deliveryID, "order_id": orderID, "shop_id": 4201, "rider_id": riderID, "status": "accepted", "pickup_snapshot": pickup, "recipient_snapshot": recipient, "assignment_version": 1, "dispatch_status": "assigned", "pickup_ready_status": "waiting_store"}).Error; err != nil {
		t.Fatal(err)
	}
	type orderState struct {
		Status, DeliveryStatus string
		UpdatedAt              time.Time
	}
	type deliveryState struct {
		Status, DispatchStatus, PickupReadyStatus string
		AssignmentVersion                         uint64
		UpdatedAt                                 time.Time
	}
	var orderBefore orderState
	var deliveryBefore deliveryState
	var outboxBefore int64
	if err := tx.Table("orders").Where("id=?", orderID).Take(&orderBefore).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Table("delivery_orders").Where("id=?", deliveryID).Take(&deliveryBefore).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Table("outbox_events").Where("aggregate_id IN ?", []uint64{orderID, deliveryID}).Count(&outboxBefore).Error; err != nil {
		t.Fatal(err)
	}

	routeCfg := cfg.MapRoute
	routeCfg.Enabled = true
	service := NewService(routeCfg, tx, redisClient, NewFakeProvider(), nil, nil)
	claims := &auth.Claims{AccountType: "rider", AccountID: fmt.Sprint(accountID), RiderID: fmt.Sprint(riderID), Permissions: []string{"delivery:route"}}
	first, err := service.Current(ctx, claims, fmt.Sprint(deliveryID), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Current(ctx, claims, fmt.Sprint(deliveryID), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Source != "fake" || second.Source != "cache" || first.Stage != "pickup" {
		t.Fatalf("unexpected route sources: first=%s second=%s stage=%s", first.Source, second.Source, first.Stage)
	}
	var orderAfter orderState
	var deliveryAfter deliveryState
	var outboxAfter int64
	if err := tx.Table("orders").Where("id=?", orderID).Take(&orderAfter).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Table("delivery_orders").Where("id=?", deliveryID).Take(&deliveryAfter).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Table("outbox_events").Where("aggregate_id IN ?", []uint64{orderID, deliveryID}).Count(&outboxAfter).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(orderBefore, orderAfter) || !reflect.DeepEqual(deliveryBefore, deliveryAfter) || outboxBefore != outboxAfter {
		t.Fatalf("route query changed fulfillment data: order=%#v/%#v delivery=%#v/%#v outbox=%d/%d", orderBefore, orderAfter, deliveryBefore, deliveryAfter, outboxBefore, outboxAfter)
	}
}
