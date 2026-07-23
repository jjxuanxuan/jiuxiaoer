package search

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/modules/auth"
	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestMySQLSearchEventIdempotencyAggregationAndPrivacy(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run search MySQL/Redis integration test")
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

	redisClient := goredis.NewClient(&goredis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: 14})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := redisClient.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flush Redis fixture DB: %v", err)
	}
	defer func() {
		_ = redisClient.FlushDB(context.Background()).Err()
		_ = redisClient.Close()
	}()

	var customerID uint64
	if err := tx.Table("customers").Select("id").Where("status = 'active' AND deleted_at IS NULL").Order("id").Limit(1).Scan(&customerID).Error; err != nil || customerID == 0 {
		t.Fatalf("an active seeded customer is required: customer=%d err=%v", customerID, err)
	}
	if err := tx.Where("customer_id = ?", customerID).Delete(&History{}).Error; err != nil {
		t.Fatal(err)
	}
	searchCfg := config.SearchConfig{
		HistoryMax: 20, HistoryRetention: 180 * 24 * time.Hour, HotWindowDays: 7,
		StatsRetentionDays: 30, HotCacheTTL: 5 * time.Minute, EventRatePerMinute: 30,
	}
	service := NewService(searchCfg, tx, redisClient, snowflake.New(904), nil, discardSearchTestLogger())
	claims := &auth.Claims{AccountType: "customer", CustomerID: idString(customerID)}
	key := "search-mysql-idempotency-" + idString(snowflake.New(905).Next())
	request := EventRequest{Keyword: "  ＦＲＥＮＣＨ   WINE ", Source: SourceManual}

	created, err := service.RecordEvent(ctx, claims, "POST", eventPath, key, "", "", request)
	if err != nil {
		t.Fatalf("record manual event: %v", err)
	}
	if !created.CountedForHot || created.HistoryItem.Keyword != "FRENCH WINE" {
		t.Fatalf("unexpected manual event response: %#v", created)
	}
	replayed, err := service.RecordEvent(ctx, claims, "POST", eventPath, key, "", "", request)
	if err != nil || replayed.HistoryItem.ID != created.HistoryItem.ID || replayed.CountedForHot != created.CountedForHot {
		t.Fatalf("idempotent replay changed response: created=%#v replay=%#v err=%v", created, replayed, err)
	}
	if _, err := service.RecordEvent(ctx, claims, "POST", eventPath, key, "", "", EventRequest{Keyword: "另一个词", Source: SourceManual}); err == nil || problem.FromError(err).ErrorCode != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("same key with different body must conflict, got %v", err)
	}
	if _, err := service.RecordEvent(ctx, claims, "POST", eventPath, key+"-history", "", "", EventRequest{Keyword: "French Wine", Source: SourceHistory}); err != nil {
		t.Fatalf("record history-source event: %v", err)
	}
	privateResponse, err := service.RecordEvent(ctx, claims, "POST", eventPath, key+"-private", "", "", EventRequest{Keyword: "联系13800138000", Source: SourceManual})
	if err != nil {
		t.Fatalf("record private event: %v", err)
	}
	if privateResponse.CountedForHot {
		t.Fatal("PII event must not count for hot")
	}

	var history History
	if err := tx.Where("customer_id = ? AND normalized_keyword = ?", customerID, "french wine").Take(&history).Error; err != nil {
		t.Fatal(err)
	}
	if history.SearchCount != 2 {
		t.Fatalf("expected one manual and one history-source personal count, got %d", history.SearchCount)
	}
	local := time.Now().In(chinaStandardTime)
	statDate := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, chinaStandardTime)
	var stat DailyStat
	if err := tx.Where("stat_date = ? AND scope_type = ? AND scope_id = ? AND normalized_keyword = ?", statDate, ScopeGlobal, globalScopeID, "french wine").Take(&stat).Error; err != nil {
		t.Fatal(err)
	}
	if stat.SearchCount != 1 {
		t.Fatalf("history source must not reinforce hot count, got %d", stat.SearchCount)
	}
	var privateStatCount int64
	if err := tx.Model(&DailyStat{}).Where("normalized_keyword = ?", "联系13800138000").Count(&privateStatCount).Error; err != nil || privateStatCount != 0 {
		t.Fatalf("PII leaked to daily statistics: count=%d err=%v", privateStatCount, err)
	}
	discovery, err := service.Discovery(ctx, claims, "", "", 20, 20)
	if err != nil {
		t.Fatalf("read MySQL-backed discovery: %v", err)
	}
	if len(discovery.History) != 2 || len(discovery.HotKeywords) == 0 || discovery.HotKeywords[0].Keyword != "FRENCH WINE" {
		t.Fatalf("unexpected discovery result: %#v", discovery)
	}
	clearKey := "search-mysql-clear-" + idString(snowflake.New(908).Next())
	cleared, err := service.ClearHistory(ctx, claims, "DELETE", historyPath, clearKey)
	if err != nil || cleared.DeletedCount != 2 {
		t.Fatalf("clear search history: response=%#v err=%v", cleared, err)
	}
	replayedClear, err := service.ClearHistory(ctx, claims, "DELETE", historyPath, clearKey)
	if err != nil || replayedClear.DeletedCount != cleared.DeletedCount {
		t.Fatalf("clear idempotent replay changed response: first=%#v replay=%#v err=%v", cleared, replayedClear, err)
	}
	var remainingHistory, remainingStat int64
	_ = tx.Model(&History{}).Where("customer_id = ?", customerID).Count(&remainingHistory).Error
	_ = tx.Model(&DailyStat{}).Where("normalized_keyword = ?", "french wine").Count(&remainingStat).Error
	if remainingHistory != 0 || remainingStat != 1 {
		t.Fatalf("clear must remove only history: history=%d stats=%d", remainingHistory, remainingStat)
	}
}

func TestMySQLSearchConcurrentHistoryInvariant(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run search concurrency integration test")
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
	redisClient := goredis.NewClient(&goredis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: 15})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := redisClient.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	ids := snowflake.New(909)
	accountID, customerID := ids.Next(), ids.Next()
	phone := fmt.Sprintf("199%08d", accountID%100000000)
	if err := db.Table("accounts").Create(map[string]any{"id": accountID, "account_type": "customer", "username": "search_it_" + idString(accountID), "phone": phone, "status": "active"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("customers").Create(map[string]any{"id": customerID, "account_id": accountID, "nickname": "search concurrency", "phone": phone, "status": "active"}).Error; err != nil {
		_ = db.Table("accounts").Where("id = ?", accountID).Delete(nil).Error
		t.Fatal(err)
	}
	uniqueWord := "并发同词" + alphaID(customerID)
	idempotentWord := "并发同键" + alphaID(customerID)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_ = db.WithContext(cleanupCtx).Where("customer_id = ?", customerID).Delete(&History{}).Error
		_ = db.WithContext(cleanupCtx).Where("normalized_keyword IN ?", []string{uniqueWord, idempotentWord}).Delete(&DailyStat{}).Error
		_ = db.WithContext(cleanupCtx).Table("idempotency_keys").Where("actor_type = 'customer' AND actor_id = ? AND path IN ?", customerID, []string{eventPath, historyPath}).Delete(nil).Error
		_ = db.WithContext(cleanupCtx).Table("customers").Where("id = ?", customerID).Delete(nil).Error
		_ = db.WithContext(cleanupCtx).Table("accounts").Where("id = ?", accountID).Delete(nil).Error
		_ = redisClient.FlushDB(cleanupCtx).Err()
		_ = redisClient.Close()
	})

	service := NewService(config.SearchConfig{
		HistoryMax: 20, HistoryRetention: 180 * 24 * time.Hour, HotWindowDays: 7,
		StatsRetentionDays: 30, HotCacheTTL: 5 * time.Minute, EventRatePerMinute: 200,
	}, db, redisClient, ids, nil, discardSearchTestLogger())
	claims := &auth.Claims{AccountType: "customer", CustomerID: idString(customerID)}

	const sameKeyCalls = 20
	sameKey := "search-concurrent-idempotent-" + idString(ids.Next())
	type idempotentResult struct {
		response EventResponse
		err      error
	}
	idempotentResults := make(chan idempotentResult, sameKeyCalls)
	var idempotentWaitGroup sync.WaitGroup
	for index := 0; index < sameKeyCalls; index++ {
		idempotentWaitGroup.Add(1)
		go func() {
			defer idempotentWaitGroup.Done()
			response, callErr := service.RecordEvent(ctx, claims, "POST", eventPath, sameKey, "", "", EventRequest{Keyword: idempotentWord, Source: SourceManual})
			idempotentResults <- idempotentResult{response: response, err: callErr}
		}()
	}
	idempotentWaitGroup.Wait()
	close(idempotentResults)
	var firstResponse EventResponse
	for result := range idempotentResults {
		if result.err != nil {
			t.Fatalf("same-key concurrent replay returned an error: %v", result.err)
		}
		if firstResponse.HistoryItem.ID == "" {
			firstResponse = result.response
			continue
		}
		if result.response.HistoryItem.ID != firstResponse.HistoryItem.ID || result.response.HistoryItem.Keyword != firstResponse.HistoryItem.Keyword || result.response.CountedForHot != firstResponse.CountedForHot {
			t.Fatalf("same-key concurrent response changed: first=%#v replay=%#v", firstResponse, result.response)
		}
	}
	var idempotentHistory History
	if err := db.Where("customer_id = ? AND normalized_keyword = ?", customerID, idempotentWord).Take(&idempotentHistory).Error; err != nil {
		t.Fatal(err)
	}
	if idempotentHistory.SearchCount != 1 {
		t.Fatalf("same-key concurrent replay repeated the write: count=%d", idempotentHistory.SearchCount)
	}

	runConcurrent := func(count int, request func(int) EventRequest, keyPrefix string) {
		t.Helper()
		errorsChannel := make(chan error, count)
		var waitGroup sync.WaitGroup
		for index := 0; index < count; index++ {
			index := index
			waitGroup.Add(1)
			go func() {
				defer waitGroup.Done()
				_, callErr := service.RecordEvent(ctx, claims, "POST", eventPath, fmt.Sprintf("%s-%03d", keyPrefix, index), "", "", request(index))
				if callErr != nil {
					errorsChannel <- callErr
				}
			}()
		}
		waitGroup.Wait()
		close(errorsChannel)
		for callErr := range errorsChannel {
			t.Errorf("concurrent search event failed: %v", callErr)
		}
	}
	prefix := "并发历史" + alphaID(customerID)
	runConcurrent(50, func(index int) EventRequest {
		return EventRequest{Keyword: fmt.Sprintf("%s%c%c", prefix, 'a'+rune(index/26), 'a'+rune(index%26)), Source: SourceHistory}
	}, "search-concurrent-history")
	var historyCount int64
	if err := db.Model(&History{}).Where("customer_id = ?", customerID).Count(&historyCount).Error; err != nil || historyCount != 20 {
		t.Fatalf("concurrent distinct writes violated max 20: count=%d err=%v", historyCount, err)
	}
	runConcurrent(50, func(int) EventRequest {
		return EventRequest{Keyword: uniqueWord, Source: SourceManual}
	}, "search-concurrent-same")
	var history History
	if err := db.Where("customer_id = ? AND normalized_keyword = ?", customerID, uniqueWord).Take(&history).Error; err != nil {
		t.Fatal(err)
	}
	if history.SearchCount != 50 {
		t.Fatalf("expected one history row with count 50, got %#v", history)
	}
	local := time.Now().In(chinaStandardTime)
	statDate := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, chinaStandardTime)
	var stat DailyStat
	if err := db.Where("stat_date = ? AND scope_type = ? AND scope_id = ? AND normalized_keyword = ?", statDate, ScopeGlobal, globalScopeID, uniqueWord).Take(&stat).Error; err != nil {
		t.Fatal(err)
	}
	if stat.SearchCount != 50 {
		t.Fatalf("expected concurrent hot count 50, got %d", stat.SearchCount)
	}
}

func idString(value uint64) string {
	return fmt.Sprintf("%d", value)
}

func alphaID(value uint64) string {
	if value == 0 {
		return "a"
	}
	buffer := make([]byte, 0, 16)
	for value > 0 {
		buffer = append(buffer, byte('a'+value%26))
		value /= 26
	}
	return string(buffer)
}

func discardSearchTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
