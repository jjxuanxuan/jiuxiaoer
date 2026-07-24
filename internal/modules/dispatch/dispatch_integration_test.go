package dispatch

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

// TestConcurrentGrabCreatesOneActiveAssignmentIntegration 验证并发抢单只创建一个有效分配。
func TestConcurrentGrabCreatesOneActiveAssignmentIntegration(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run dispatch integration tests")
	}
	ctx := context.Background()
	cfg := config.Load()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := mysql.Open(ctx, cfg.MySQL, log)
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get mysql connection: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	ids := snowflake.New(988)
	var shop struct {
		ID         uint64
		MerchantID uint64
		Latitude   float64
		Longitude  float64
	}
	if err := db.Table("shops").Select("id,merchant_id,latitude,longitude").
		Where("status='active' AND deleted_at IS NULL AND latitude IS NOT NULL AND longitude IS NOT NULL").First(&shop).Error; err != nil {
		t.Fatalf("find shop fixture: %v", err)
	}

	const contenders = 100
	accountIDs := make([]uint64, 0, contenders)
	riderIDs := make([]uint64, 0, contenders)
	serviceIDs := make([]uint64, 0, contenders)
	orderID, deliveryID, jobID := ids.Next(), ids.Next(), ids.Next()
	t.Cleanup(func() {
		cleanupDispatchConcurrencyFixture(db, orderID, deliveryID, jobID, accountIDs, riderIDs, serviceIDs)
	})

	now := time.Now()
	accounts := make([]map[string]any, 0, contenders)
	riders := make([]map[string]any, 0, contenders)
	services := make([]map[string]any, 0, contenders)
	runtimes := make([]map[string]any, 0, contenders)
	for i := 0; i < contenders; i++ {
		accountID, riderID, serviceID := ids.Next(), ids.Next(), ids.Next()
		accountIDs = append(accountIDs, accountID)
		riderIDs = append(riderIDs, riderID)
		serviceIDs = append(serviceIDs, serviceID)
		accounts = append(accounts, map[string]any{
			"id": accountID, "account_type": "rider", "username": fmt.Sprintf("dispatch_concurrency_%d_%d", orderID, i),
			"status": "active", "credential_version": 1,
		})
		riders = append(riders, map[string]any{
			"id": riderID, "account_id": accountID, "name": fmt.Sprintf("dispatch-rider-%d", i),
			"status": "active", "work_status": "online", "work_status_version": 1,
			"review_status": "approved", "service_scope": jsonData(map[string]any{"shop_ids": []string{idString(shop.ID)}}),
		})
		services = append(services, map[string]any{
			"id": serviceID, "rider_id": riderID, "shop_id": shop.ID, "status": "active", "source": "test",
		})
		runtimes = append(runtimes, map[string]any{
			"rider_id": riderID, "work_status": "online", "latitude": shop.Latitude, "longitude": shop.Longitude,
			"accuracy_m": 5, "captured_at": now, "heartbeat_at": now, "last_sequence": 1, "max_active_orders": 3, "version": 1,
		})
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Table("accounts").Create(&accounts).Error; err != nil {
			return err
		}
		if err := tx.Table("riders").Create(&riders).Error; err != nil {
			return err
		}
		if err := tx.Table("rider_service_shops").Create(&services).Error; err != nil {
			return err
		}
		if err := tx.Table("rider_runtime_states").Create(&runtimes).Error; err != nil {
			return err
		}
		if err := tx.Table("orders").Create(map[string]any{
			"id": orderID, "order_no": fmt.Sprintf("DSP-CONC-%d", orderID), "customer_id": ids.Next(),
			"merchant_id": shop.MerchantID, "shop_id": shop.ID, "status": "paid", "pay_status": "succeeded", "delivery_status": "pending",
		}).Error; err != nil {
			return err
		}
		expires := now.Add(time.Minute)
		if err := tx.Create(&DeliveryOrder{
			ID: deliveryID, OrderID: orderID, ShopID: shop.ID, Status: "pending_assign", AssignmentVersion: 1,
			DispatchStatus: "grab_open", CurrentDispatchJobID: &jobID, PickupReadyStatus: "waiting_store",
		}).Error; err != nil {
			return err
		}
		return tx.Create(&Job{
			ID: jobID, JobNo: fmt.Sprintf("DSP-CONC-JOB-%d", jobID), DeliveryOrderID: deliveryID, OrderID: orderID,
			ShopID: shop.ID, DispatchSeq: 1, PolicyVersion: "test/grab-v1", PolicySnapshot: jsonData(defaultPolicySnapshot()),
			Mode: "grab", Status: "grab_open", GrabOpenedAt: &now, GrabExpiresAt: &expires, NextActionAt: expires, Version: 1,
		}).Error
	}); err != nil {
		t.Fatalf("create concurrency fixture: %v", err)
	}

	service := NewService(cfg, db, nil, ids, nil, log)
	start := make(chan struct{})
	results := make(chan error, contenders)
	var wg sync.WaitGroup
	for i, riderID := range riderIDs {
		wg.Add(1)
		go func(index int, contender uint64) {
			defer wg.Done()
			<-start
			_, err := service.CommitAssignment(ctx, "POST", "/integration/dispatch/concurrent-grab", fmt.Sprintf("dispatch-grab-%03d", index), CommitInput{
				Source: "grab", DeliveryOrderID: deliveryID, RiderID: contender, ExpectedAssignmentVersion: 1,
				ActorType: "rider", ActorID: contender,
			})
			results <- err
		}(i, riderID)
	}
	close(start)
	wg.Wait()
	close(results)

	successes := 0
	failures := make(map[string]int)
	for result := range results {
		if result == nil {
			successes++
		} else {
			failures[result.Error()]++
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent grabs=%d want=1 failures=%v", successes, failures)
	}
	var activeAssignments int64
	if err := db.Table("delivery_assignments").Where("delivery_order_id=? AND status='active'", deliveryID).Count(&activeAssignments).Error; err != nil {
		t.Fatal(err)
	}
	if activeAssignments != 1 {
		t.Fatalf("active assignments=%d want=1", activeAssignments)
	}
	var delivery DeliveryOrder
	if err := db.First(&delivery, deliveryID).Error; err != nil {
		t.Fatal(err)
	}
	if delivery.RiderID == nil || delivery.Status != "accepted" || delivery.DispatchStatus != "assigned" || delivery.AssignmentVersion != 2 {
		t.Fatalf("unexpected winning delivery state: %+v", delivery)
	}
}

// cleanupDispatchConcurrencyFixture 清理调度 Concurrency 测试夹具。
func cleanupDispatchConcurrencyFixture(db *gorm.DB, orderID, deliveryID, jobID uint64, accountIDs, riderIDs, serviceIDs []uint64) {
	_ = db.Transaction(func(tx *gorm.DB) error {
		tx.Exec("DELETE FROM notification_deliveries WHERE event_id IN (SELECT event_id FROM outbox_events WHERE aggregate_id IN ?)", []uint64{deliveryID, jobID})
		tx.Exec("DELETE FROM audit_logs WHERE resource_id IN ?", []uint64{deliveryID, jobID})
		tx.Exec("DELETE FROM order_logs WHERE order_id=?", orderID)
		tx.Exec("DELETE FROM outbox_events WHERE aggregate_id IN ?", []uint64{deliveryID, jobID})
		tx.Exec("DELETE FROM idempotency_keys WHERE path='/integration/dispatch/concurrent-grab' AND actor_id IN ?", riderIDs)
		tx.Exec("DELETE FROM delivery_assignments WHERE delivery_order_id=?", deliveryID)
		tx.Exec("DELETE FROM dispatch_offers WHERE job_id=?", jobID)
		tx.Exec("DELETE FROM dispatch_candidates WHERE job_id=?", jobID)
		tx.Exec("DELETE FROM dispatch_jobs WHERE id=?", jobID)
		tx.Exec("DELETE FROM delivery_orders WHERE id=?", deliveryID)
		tx.Exec("DELETE FROM orders WHERE id=?", orderID)
		tx.Exec("DELETE FROM rider_runtime_states WHERE rider_id IN ?", riderIDs)
		tx.Exec("DELETE FROM rider_service_shops WHERE id IN ?", serviceIDs)
		tx.Exec("DELETE FROM riders WHERE id IN ?", riderIDs)
		tx.Exec("DELETE FROM accounts WHERE id IN ?", accountIDs)
		return nil
	})
}
