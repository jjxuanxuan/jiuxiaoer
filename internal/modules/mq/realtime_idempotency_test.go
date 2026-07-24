package mq

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type flakyReliableAfterCommitHandler struct {
	handleCalls    int
	afterCalls     int
	deliveredCount int
}

func (h *flakyReliableAfterCommitHandler) Handle(_ context.Context, _ *gorm.DB, _ EventEnvelope) (ConsumerResult, error) {
	h.handleCalls++
	return ConsumerResult{RefType: "merchant_paid_order", RefID: 9001}, nil
}

func (h *flakyReliableAfterCommitHandler) AfterCommit(_ context.Context, _ EventEnvelope, _ ConsumerResult) error {
	h.afterCalls++
	if h.afterCalls == 1 {
		return errors.New("redis temporarily unavailable")
	}
	h.deliveredCount++
	return nil
}

func (h *flakyReliableAfterCommitHandler) RequiresSuccessfulAfterCommit(envelope EventEnvelope, _ ConsumerResult) bool {
	return envelope.EventType == "order.paid"
}

// TestRealtimeConsumerReceiptSuppressesDuplicatePaidEvent 验证商户 WebSocket
// 扇出前使用的精确 event_id 防重屏障。
func TestRealtimeConsumerReceiptSuppressesDuplicatePaidEvent(t *testing.T) {
	db := realtimeReceiptDB(t)
	calls := 0
	handler := ConsumerHandlerFunc(func(_ context.Context, _ *gorm.DB, _ EventEnvelope) (ConsumerResult, error) {
		calls++
		return ConsumerResult{RefType: "merchant_paid_order", RefID: 9001}, nil
	})
	spec, err := DefaultConsumerSpec("realtime")
	if err != nil {
		t.Fatal(err)
	}
	runtime := NewConsumerRuntime(spec, db, nil, MustDefaultEventRegistry(), handler, snowflake.New(31), nil, "test", nil)
	envelope := EventEnvelope{EventID: uuid.NewString(), EventType: "order.paid", EventVersion: 1, AggregateType: "order", AggregateID: "9001", OccurredAt: time.Now()}

	duplicate, first, err := runtime.processEnvelope(context.Background(), envelope)
	if err != nil || duplicate || first.RefID != 9001 {
		t.Fatalf("first paid event process failed duplicate=%v result=%+v err=%v", duplicate, first, err)
	}
	duplicate, second, err := runtime.processEnvelope(context.Background(), envelope)
	if err != nil || !duplicate || second.RefID != 0 {
		t.Fatalf("duplicate paid event was not suppressed duplicate=%v result=%+v err=%v", duplicate, second, err)
	}
	if calls != 1 {
		t.Fatalf("same event_id invoked realtime handler %d times", calls)
	}
}

// TestReliableAfterCommitRetriesBeforeClosingReceipt 覆盖 Redis 故障边界：
// 首次扇出失败，持久回执重新变为可处理状态，恢复后仅投递一次，
// 随后重复消息被抑制。
func TestReliableAfterCommitRetriesBeforeClosingReceipt(t *testing.T) {
	db := realtimeReceiptDB(t)
	handler := &flakyReliableAfterCommitHandler{}
	spec, err := DefaultConsumerSpec("realtime")
	if err != nil {
		t.Fatal(err)
	}
	runtime := NewConsumerRuntime(spec, db, nil, MustDefaultEventRegistry(), handler, snowflake.New(32), nil, "test", nil)
	envelope := EventEnvelope{EventID: uuid.NewString(), EventType: "order.paid", EventVersion: 1, AggregateType: "order", AggregateID: "9001", OccurredAt: time.Now()}

	duplicate, result, err := runtime.processEnvelope(context.Background(), envelope)
	if err != nil || duplicate {
		t.Fatalf("first process failed duplicate=%v err=%v", duplicate, err)
	}
	var receipt ConsumerReceipt
	if err := db.Where("consumer_name=? AND event_id=?", "realtime", envelope.EventID).Take(&receipt).Error; err != nil || receipt.Status != "post_commit" {
		t.Fatalf("receipt must wait for fanout: %+v err=%v", receipt, err)
	}
	postErr := runtime.finalizeAfterCommit(context.Background(), envelope, result)
	if postErr == nil {
		t.Fatal("expected first transient fanout failure")
	}
	failure := classifyConsumerError(postErr)
	if err := runtime.recordFailure(context.Background(), envelope, 1, failure); err != nil {
		t.Fatal(err)
	}
	if err := db.Where("consumer_name=? AND event_id=?", "realtime", envelope.EventID).Take(&receipt).Error; err != nil || receipt.Status != "processing" {
		t.Fatalf("failed fanout receipt must be retryable: %+v err=%v", receipt, err)
	}

	duplicate, result, err = runtime.processEnvelope(context.Background(), envelope)
	if err != nil || duplicate {
		t.Fatalf("retry process failed duplicate=%v err=%v", duplicate, err)
	}
	if err := runtime.finalizeAfterCommit(context.Background(), envelope, result); err != nil {
		t.Fatalf("recovered fanout failed: %v", err)
	}
	duplicate, _, err = runtime.processEnvelope(context.Background(), envelope)
	if err != nil || !duplicate {
		t.Fatalf("completed event was not deduplicated duplicate=%v err=%v", duplicate, err)
	}
	if handler.afterCalls != 2 || handler.deliveredCount != 1 {
		t.Fatalf("expected one eventual delivery after one failure: %+v", handler)
	}
}

func realtimeReceiptDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&ConsumerReceipt{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE UNIQUE INDEX uk_test_mq_receipt ON mq_consumer_receipts(consumer_name,event_id)").Error; err != nil {
		t.Fatal(err)
	}
	return db
}
