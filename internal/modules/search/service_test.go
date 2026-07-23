package search

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

type searchCustomerRow struct {
	ID        uint64 `gorm:"primaryKey"`
	Status    string
	DeletedAt *time.Time
}

func (searchCustomerRow) TableName() string { return "customers" }

type searchConfigRow struct {
	ID          uint64 `gorm:"primaryKey"`
	ConfigKey   string `gorm:"uniqueIndex"`
	ConfigValue string
	Status      string
	DeletedAt   *time.Time
}

func (searchConfigRow) TableName() string { return "system_configs" }

type fakeIdempotency struct {
	mu        sync.Mutex
	hashes    map[string]string
	responses map[string][]byte
}

func newFakeIdempotency() *fakeIdempotency {
	return &fakeIdempotency{hashes: make(map[string]string), responses: make(map[string][]byte)}
}

func (f *fakeIdempotency) ReplayCompleted(_ context.Context, _ *gorm.DB, actorType string, actorID uint64, path, key, requestHash string, out any) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lookup := fmt.Sprintf("%s:%d:%s:%s", actorType, actorID, path, key)
	payload, ok := f.responses[lookup]
	if !ok {
		return false, nil
	}
	if f.hashes[lookup] != requestHash {
		return false, problem.Conflict("IDEMPOTENCY_KEY_REUSED", "same idempotency key used with different request")
	}
	return true, json.Unmarshal(payload, out)
}

func (f *fakeIdempotency) Start(_ context.Context, _ *gorm.DB, _ uint64, actorType string, actorID uint64, _, path, key, requestHash string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lookup := fmt.Sprintf("%s:%d:%s:%s", actorType, actorID, path, key)
	if existing, ok := f.hashes[lookup]; ok {
		if existing != requestHash {
			return false, problem.Conflict("IDEMPOTENCY_KEY_REUSED", "same idempotency key used with different request")
		}
		return false, nil
	}
	f.hashes[lookup] = requestHash
	return true, nil
}

func (f *fakeIdempotency) Succeed(_ context.Context, _ *gorm.DB, actorType string, actorID uint64, path, key string, response any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	lookup := fmt.Sprintf("%s:%d:%s:%s", actorType, actorID, path, key)
	f.responses[lookup], _ = json.Marshal(response)
	return nil
}

func (f *fakeIdempotency) CachedResponse(_ context.Context, _ *gorm.DB, actorType string, actorID uint64, path, key string, out any) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	payload, ok := f.responses[fmt.Sprintf("%s:%d:%s:%s", actorType, actorID, path, key)]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(payload, out)
}

type completionWindowIdempotency struct {
	startReached chan struct{}
	releaseStart chan struct{}
	startOnce    sync.Once
	releaseOnce  sync.Once

	mu           sync.Mutex
	response     []byte
	cachedCalls  int
	succeedCalls int
}

func newCompletionWindowIdempotency() *completionWindowIdempotency {
	return &completionWindowIdempotency{startReached: make(chan struct{}), releaseStart: make(chan struct{})}
}

func (f *completionWindowIdempotency) ReplayCompleted(context.Context, *gorm.DB, string, uint64, string, string, string, any) (bool, error) {
	return false, nil
}

func (f *completionWindowIdempotency) Start(ctx context.Context, _ *gorm.DB, _ uint64, _ string, _ uint64, _, _, _, _ string) (bool, error) {
	f.startOnce.Do(func() { close(f.startReached) })
	select {
	case <-f.releaseStart:
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (f *completionWindowIdempotency) CachedResponse(_ context.Context, _ *gorm.DB, _ string, _ uint64, _, _ string, out any) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cachedCalls++
	if len(f.response) == 0 {
		return false, nil
	}
	return true, json.Unmarshal(f.response, out)
}

func (f *completionWindowIdempotency) Succeed(context.Context, *gorm.DB, string, uint64, string, string, any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.succeedCalls++
	return fmt.Errorf("completion-window replay must not execute business success")
}

func (f *completionWindowIdempotency) complete(response any) {
	payload, _ := json.Marshal(response)
	f.mu.Lock()
	f.response = payload
	f.mu.Unlock()
	f.releaseOnce.Do(func() { close(f.releaseStart) })
}

func (f *completionWindowIdempotency) calls() (cached, succeeded int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cachedCalls, f.succeedCalls
}

func TestCompletionBetweenReplayAndStartReturnsCachedResponse(t *testing.T) {
	newService := func(t *testing.T) (*Service, *gorm.DB, *auth.Claims) {
		t.Helper()
		db := searchTestDB(t)
		if err := db.AutoMigrate(&searchCustomerRow{}, &searchConfigRow{}); err != nil {
			t.Fatalf("migrate completion-window fixtures: %v", err)
		}
		if err := db.Create(&searchCustomerRow{ID: 77, Status: "active"}).Error; err != nil {
			t.Fatalf("insert completion-window customer: %v", err)
		}
		if err := db.Create(&[]searchConfigRow{
			{ID: 1, ConfigKey: blocklistConfig, ConfigValue: `[]`, Status: "active"},
			{ID: 2, ConfigKey: defaultConfig, ConfigValue: `[]`, Status: "active"},
		}).Error; err != nil {
			t.Fatalf("insert completion-window configs: %v", err)
		}
		redisServer := miniredis.RunT(t)
		redisClient := goredis.NewClient(&goredis.Options{Addr: redisServer.Addr()})
		t.Cleanup(func() { _ = redisClient.Close() })
		service := NewService(config.SearchConfig{
			HistoryMax: 20, HistoryRetention: 180 * 24 * time.Hour, HotWindowDays: 7,
			StatsRetentionDays: 30, HotCacheTTL: 5 * time.Minute, EventRatePerMinute: 30,
		}, db, redisClient, snowflake.New(906), nil, nil)
		service.now = func() time.Time { return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC) }
		return service, db, &auth.Claims{AccountType: "customer", CustomerID: "77"}
	}

	waitForStart := func(t *testing.T, store *completionWindowIdempotency) {
		t.Helper()
		select {
		case <-store.startReached:
		case <-time.After(3 * time.Second):
			t.Fatal("request did not reach Start after the initial replay miss")
		}
	}

	t.Run("record event", func(t *testing.T) {
		service, db, claims := newService(t)
		store := newCompletionWindowIdempotency()
		service.idem = store
		expected := EventResponse{
			HistoryItem:   HistoryDTO{ID: "cached-history", Keyword: "白酒", LastSearchedAt: time.Date(2026, 7, 22, 11, 59, 0, 0, time.UTC)},
			CountedForHot: true,
		}
		type result struct {
			response EventResponse
			err      error
		}
		resultCh := make(chan result, 1)
		go func() {
			response, err := service.RecordEvent(context.Background(), claims, "POST", eventPath, "completion-window-event", "", "", EventRequest{Keyword: "白酒", Source: SourceManual})
			resultCh <- result{response: response, err: err}
		}()

		waitForStart(t, store)
		store.complete(expected)
		got := <-resultCh
		if got.err != nil || !reflect.DeepEqual(got.response, expected) {
			t.Fatalf("completion-window replay response=%#v err=%v, want %#v", got.response, got.err, expected)
		}
		var historyCount, statCount int64
		_ = db.Model(&History{}).Count(&historyCount).Error
		_ = db.Model(&DailyStat{}).Count(&statCount).Error
		if historyCount != 0 || statCount != 0 {
			t.Fatalf("cached replay must not repeat search writes: history=%d stats=%d", historyCount, statCount)
		}
		if cached, succeeded := store.calls(); cached != 1 || succeeded != 0 {
			t.Fatalf("idempotency calls cached=%d succeeded=%d, want 1/0", cached, succeeded)
		}
	})

	t.Run("clear history", func(t *testing.T) {
		service, db, claims := newService(t)
		now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
		if err := db.Create(&History{ID: 1, CustomerID: 77, Keyword: "保留", NormalizedKeyword: "保留", SearchCount: 1, LastSearchedAt: now, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
			t.Fatalf("seed history: %v", err)
		}
		store := newCompletionWindowIdempotency()
		service.idem = store
		expected := ClearResponse{DeletedCount: 1}
		type result struct {
			response ClearResponse
			err      error
		}
		resultCh := make(chan result, 1)
		go func() {
			response, err := service.ClearHistory(context.Background(), claims, "DELETE", historyPath, "completion-window-clear")
			resultCh <- result{response: response, err: err}
		}()

		waitForStart(t, store)
		store.complete(expected)
		got := <-resultCh
		if got.err != nil || got.response != expected {
			t.Fatalf("completion-window clear response=%#v err=%v, want %#v", got.response, got.err, expected)
		}
		var historyCount int64
		_ = db.Model(&History{}).Where("customer_id = ?", 77).Count(&historyCount).Error
		if historyCount != 1 {
			t.Fatalf("cached clear replay must not delete history again: count=%d", historyCount)
		}
		if cached, succeeded := store.calls(); cached != 1 || succeeded != 0 {
			t.Fatalf("idempotency calls cached=%d succeeded=%d, want 1/0", cached, succeeded)
		}
	})
}

func TestMissingCachedResponseUsesStableInProgressCode(t *testing.T) {
	err := cachedResponse(context.Background(), newCompletionWindowIdempotency(), nil, 77, eventPath, "missing-cached-response", &EventResponse{})
	details := problem.FromError(err)
	if details == nil || details.Status != 409 || details.ErrorCode != "IDEMPOTENCY_IN_PROGRESS" {
		t.Fatalf("problem=%+v, want HTTP 409 IDEMPOTENCY_IN_PROGRESS", details)
	}
}

func TestServiceEventRulesHistoryLimitPrivacyIdempotencyAndClearIsolation(t *testing.T) {
	db := searchTestDB(t)
	if err := db.AutoMigrate(&searchCustomerRow{}, &searchConfigRow{}); err != nil {
		t.Fatalf("migrate service fixtures: %v", err)
	}
	if err := db.Create(&[]searchCustomerRow{{ID: 42, Status: "active"}, {ID: 43, Status: "active"}}).Error; err != nil {
		t.Fatalf("insert customers: %v", err)
	}
	if err := db.Create(&[]searchConfigRow{
		{ID: 1, ConfigKey: blocklistConfig, ConfigValue: `[]`, Status: "active"},
		{ID: 2, ConfigKey: defaultConfig, ConfigValue: `[]`, Status: "active"},
	}).Error; err != nil {
		t.Fatalf("insert search configs: %v", err)
	}
	redisServer := miniredis.RunT(t)
	redisClient := goredis.NewClient(&goredis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	service := NewService(config.SearchConfig{
		HistoryMax: 20, HistoryRetention: 180 * 24 * time.Hour, HotWindowDays: 7,
		StatsRetentionDays: 30, HotCacheTTL: 5 * time.Minute, EventRatePerMinute: 30,
	}, db, redisClient, snowflake.New(901), nil, nil)
	service.idem = newFakeIdempotency()
	service.now = func() time.Time { return now }
	claims := &auth.Claims{AccountType: "customer", CustomerID: "42"}
	ctx := context.Background()

	for index := 0; index < 21; index++ {
		responseValue, err := service.RecordEvent(ctx, claims, "POST", eventPath, fmt.Sprintf("event-key-%03d", index), "", "", EventRequest{Keyword: fmt.Sprintf("酒%02d", index), Source: SourceManual})
		if err != nil {
			t.Fatalf("record event %d: %v", index, err)
		}
		if !responseValue.CountedForHot {
			t.Fatalf("expected manual event %d to count for hot", index)
		}
		now = now.Add(time.Second)
	}
	if _, err := service.RecordEvent(ctx, claims, "POST", eventPath, "event-key-history", "", "", EventRequest{Keyword: "酒20", Source: SourceHistory}); err != nil {
		t.Fatalf("record history-source event: %v", err)
	}
	now = now.Add(time.Second)
	phoneResponse, err := service.RecordEvent(ctx, claims, "POST", eventPath, "event-key-private", "", "", EventRequest{Keyword: "联系13800138000", Source: SourceManual})
	if err != nil {
		t.Fatalf("record private event: %v", err)
	}
	if phoneResponse.CountedForHot {
		t.Fatal("expected PII keyword to stay out of hot statistics")
	}
	replay, err := service.RecordEvent(ctx, claims, "POST", eventPath, "event-key-private", "", "", EventRequest{Keyword: "联系13800138000", Source: SourceManual})
	if err != nil || !reflect.DeepEqual(replay, phoneResponse) {
		t.Fatalf("expected stable idempotent replay, replay=%#v original=%#v err=%v", replay, phoneResponse, err)
	}
	if _, err := service.RecordEvent(ctx, claims, "POST", eventPath, "event-key-private", "", "", EventRequest{Keyword: "另一个词", Source: SourceManual}); err == nil || problem.FromError(err).ErrorCode != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("expected same key with another request to conflict, got %v", err)
	}
	for index := 0; index < 7; index++ {
		if _, err := service.RecordEvent(ctx, claims, "POST", eventPath, fmt.Sprintf("event-key-hot-%02d", index), "", "", EventRequest{Keyword: fmt.Sprintf("热门入口%02d", index), Source: SourceHot}); err != nil {
			t.Fatalf("record non-reinforcing hot-source event %d: %v", index, err)
		}
	}
	if _, err := service.RecordEvent(ctx, claims, "POST", eventPath, "event-key-over-limit", "", "", EventRequest{Keyword: "超限", Source: SourceHot}); err == nil {
		t.Fatal("expected the 31st event in a minute to be rate limited")
	} else {
		details := problem.FromError(err)
		if details.Status != 429 || details.ErrorCode != "SEARCH_EVENT_RATE_LIMITED" || details.Data == nil {
			t.Fatalf("unexpected rate limit error: %#v", details)
		}
	}

	var historyCount, statCount, phoneStatCount int64
	if err := db.Model(&History{}).Where("customer_id = ?", 42).Count(&historyCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&DailyStat{}).Count(&statCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&DailyStat{}).Where("normalized_keyword = ?", "联系13800138000").Count(&phoneStatCount).Error; err != nil {
		t.Fatal(err)
	}
	if historyCount != 20 || statCount != 21 || phoneStatCount != 0 {
		t.Fatalf("unexpected persisted counts: history=%d stats=%d phone_stats=%d", historyCount, statCount, phoneStatCount)
	}
	var repeated DailyStat
	if err := db.Where("scope_type = ? AND scope_id = ? AND normalized_keyword = ?", ScopeGlobal, globalScopeID, "酒20").Take(&repeated).Error; err != nil {
		t.Fatal(err)
	}
	if repeated.SearchCount != 1 {
		t.Fatalf("history source must not reinforce hot ranking, got count=%d", repeated.SearchCount)
	}
	unavailableLimiter := NewService(config.SearchConfig{
		HistoryMax: 20, HistoryRetention: 180 * 24 * time.Hour, HotWindowDays: 7,
		StatsRetentionDays: 30, HotCacheTTL: 5 * time.Minute, EventRatePerMinute: 30,
	}, db, nil, snowflake.New(903), nil, nil)
	unavailableLimiter.idem = newFakeIdempotency()
	unavailableLimiter.now = func() time.Time { return now }
	if _, err := unavailableLimiter.RecordEvent(ctx, &auth.Claims{AccountType: "customer", CustomerID: "43"}, "POST", eventPath, "event-key-no-redis", "", "", EventRequest{Keyword: "白酒", Source: SourceManual}); err == nil {
		t.Fatal("expected unavailable cluster rate limiter to protect the write path")
	} else if details := problem.FromError(err); details.Status != 503 || details.ErrorCode != "SEARCH_RATE_LIMIT_UNAVAILABLE" {
		t.Fatalf("unexpected unavailable limiter error: %#v", details)
	}
	otherHistory := History{ID: snowflake.New(902).Next(), CustomerID: 43, Keyword: "其他用户", NormalizedKeyword: "其他用户", SearchCount: 1, LastSearchedAt: now, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&otherHistory).Error; err != nil {
		t.Fatal(err)
	}
	cleared, err := service.ClearHistory(ctx, claims, "DELETE", historyPath, "clear-key-0001")
	if err != nil || cleared.DeletedCount != 20 {
		t.Fatalf("clear history: response=%#v err=%v", cleared, err)
	}
	var ownAfter, otherAfter int64
	_ = db.Model(&History{}).Where("customer_id = ?", 42).Count(&ownAfter).Error
	_ = db.Model(&History{}).Where("customer_id = ?", 43).Count(&otherAfter).Error
	if ownAfter != 0 || otherAfter != 1 {
		t.Fatalf("clear must be customer-isolated: own=%d other=%d", ownAfter, otherAfter)
	}
}
