package search

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/pkg/snowflake"
)

func TestHotScopeCacheAsideHandlesHitCorruptionAndRedisFailure(t *testing.T) {
	db := searchTestDB(t)
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	today := time.Date(2026, 7, 18, 0, 0, 0, 0, chinaStandardTime)
	row := DailyStat{ID: 1, StatDate: today, ScopeType: ScopeGlobal, ScopeID: globalScopeID, NormalizedKeyword: "啤酒", DisplayKeyword: "啤酒", SearchCount: 2, LastSearchedAt: now, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("insert stat: %v", err)
	}
	service := NewService(config.SearchConfig{HotWindowDays: 7, HotCacheTTL: 5 * time.Minute}, db, client, snowflake.New(900), nil, nil)
	service.now = func() time.Time { return now }

	items, err := service.hotScope(context.Background(), ScopeGlobal, globalScopeID, now)
	if err != nil || len(items) != 1 || items[0].SearchCount != 2 {
		t.Fatalf("expected MySQL miss result, items=%#v err=%v", items, err)
	}
	cacheKey := "search:hot:v1:global:*"
	if !server.Exists(cacheKey) {
		t.Fatalf("expected %s to be cached", cacheKey)
	}
	if err := db.Model(&DailyStat{}).Where("id = ?", 1).Update("search_count", 9).Error; err != nil {
		t.Fatal(err)
	}
	items, err = service.hotScope(context.Background(), ScopeGlobal, globalScopeID, now)
	if err != nil || items[0].SearchCount != 2 {
		t.Fatalf("expected cache hit to preserve prior value, items=%#v err=%v", items, err)
	}
	server.Set(cacheKey, "{broken-json")
	items, err = service.hotScope(context.Background(), ScopeGlobal, globalScopeID, now)
	if err != nil || items[0].SearchCount != 9 {
		t.Fatalf("expected corrupt JSON to fall back to MySQL, items=%#v err=%v", items, err)
	}
	server.Close()
	items, err = service.hotScope(context.Background(), ScopeGlobal, globalScopeID, now)
	if err != nil || items[0].SearchCount != 9 {
		t.Fatalf("expected Redis failure to fall back to MySQL, items=%#v err=%v", items, err)
	}
}
