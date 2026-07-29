package ops

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"jiuxiaoer-admin/backend-go/internal/modules/wineticket/redemption"
	"jiuxiaoer-admin/backend-go/internal/pkg/idempotency"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestSlotAdminConcurrentOverlappingCreatesCommitOneWindow(t *testing.T) {
	service, db, _ := newSlotAdminTestService(t)
	seedSlotAdminShop(t, db, 201, 101, "310100")
	claims := slotAdminClaims("wine_ticket_slot:create")
	const contenders = 24
	results := make(chan error, contenders)
	var wait sync.WaitGroup
	for index := 0; index < contenders; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			request := validSlotAdminCreateRequest()
			_, err := service.Create(
				context.Background(),
				claims,
				http.MethodPost,
				"/api/v1/admin/wine-tickets/delivery-time-slots",
				"slot-race-"+leftPadSlotAdminIndex(index),
				request,
			)
			results <- err
		}(index)
	}
	wait.Wait()
	close(results)

	successes := 0
	conflicts := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		var details *problem.Details
		if errors.As(err, &details) &&
			details.ErrorCode == "WT_CONCURRENT_MODIFICATION" {
			conflicts++
			continue
		}
		t.Fatalf("unexpected concurrent create error: %v", err)
	}
	if successes != 1 || conflicts != contenders-1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	var count int64
	if err := db.Model(&redemption.DeliveryTimeSlot{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("slot count=%d, want 1", count)
	}
}

func TestSlotAdminWriteRollsBackBusinessAuditAndIdempotencyOnOutboxFailure(
	t *testing.T,
) {
	service, db, _ := newSlotAdminTestService(t)
	seedSlotAdminShop(t, db, 201, 101, "310100")
	if err := db.Migrator().DropTable(&slotAdminTestOutbox{}); err != nil {
		t.Fatal(err)
	}
	_, err := service.Create(
		context.Background(),
		slotAdminClaims("wine_ticket_slot:create"),
		http.MethodPost,
		"/api/v1/admin/wine-tickets/delivery-time-slots",
		"slot-atomic-outbox",
		validSlotAdminCreateRequest(),
	)
	if err == nil {
		t.Fatal("missing outbox table must fail the write")
	}
	for name, model := range map[string]any{
		"slot":        &redemption.DeliveryTimeSlot{},
		"audit":       &slotAdminTestAudit{},
		"idempotency": &idempotency.Record{},
	} {
		var count int64
		if err := db.Model(model).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s rows=%d after rollback", name, count)
		}
	}
}
