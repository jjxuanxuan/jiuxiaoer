package search

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"jiuxiaoer-admin/backend-go/internal/config"
)

func TestRepositoryUpsertsTrimsAndFiltersHistory(t *testing.T) {
	db := searchTestDB(t)
	repo := NewRepository(db)
	ctx := context.Background()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

	for index, keyword := range []string{"白酒", "啤酒", "红酒"} {
		at := now.Add(time.Duration(index) * time.Minute)
		_, err := repo.UpsertHistory(ctx, db, &History{ID: uint64(index + 1), CustomerID: 42, Keyword: keyword, NormalizedKeyword: keyword, SearchCount: 1, LastSearchedAt: at, CreatedAt: at, UpdatedAt: at})
		if err != nil {
			t.Fatalf("insert history: %v", err)
		}
	}
	stored, err := repo.UpsertHistory(ctx, db, &History{ID: 99, CustomerID: 42, Keyword: "啤酒", NormalizedKeyword: "啤酒", SearchCount: 1, LastSearchedAt: now.Add(5 * time.Minute), CreatedAt: now, UpdatedAt: now.Add(5 * time.Minute)})
	if err != nil {
		t.Fatalf("upsert history: %v", err)
	}
	if stored.ID != 2 || stored.SearchCount != 2 {
		t.Fatalf("expected existing row count increment, got %#v", stored)
	}
	if err := repo.TrimHistory(ctx, db, 42, 2); err != nil {
		t.Fatalf("trim history: %v", err)
	}
	rows, err := repo.ListHistory(ctx, 42, now.Add(-time.Hour), 20)
	if err != nil {
		t.Fatalf("list history: %v", err)
	}
	if len(rows) != 2 || rows[0].Keyword != "啤酒" || rows[1].Keyword != "红酒" {
		t.Fatalf("unexpected history order/content: %#v", rows)
	}
	rows, err = repo.ListHistory(ctx, 42, now.Add(4*time.Minute), 20)
	if err != nil || len(rows) != 1 || rows[0].Keyword != "啤酒" {
		t.Fatalf("expected retention boundary to hide older rows: rows=%#v err=%v", rows, err)
	}
}

func TestRepositoryAggregatesHotRankingAndWorkerCleansExpiredRows(t *testing.T) {
	db := searchTestDB(t)
	repo := NewRepository(db)
	ctx := context.Background()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	today := time.Date(2026, 7, 18, 0, 0, 0, 0, chinaStandardTime)

	stats := []DailyStat{
		{ID: 10, StatDate: today, ScopeType: ScopeGlobal, ScopeID: globalScopeID, NormalizedKeyword: "啤酒", DisplayKeyword: "啤酒", SearchCount: 1, LastSearchedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: 11, StatDate: today.AddDate(0, 0, -1), ScopeType: ScopeGlobal, ScopeID: globalScopeID, NormalizedKeyword: "啤酒", DisplayKeyword: "啤酒", SearchCount: 1, LastSearchedAt: now.Add(-time.Hour), CreatedAt: now, UpdatedAt: now},
		{ID: 12, StatDate: today, ScopeType: ScopeGlobal, ScopeID: globalScopeID, NormalizedKeyword: "白酒", DisplayKeyword: "白酒", SearchCount: 1, LastSearchedAt: now.Add(-2 * time.Hour), CreatedAt: now, UpdatedAt: now},
		{ID: 13, StatDate: today.AddDate(0, 0, -31), ScopeType: ScopeGlobal, ScopeID: globalScopeID, NormalizedKeyword: "过期", DisplayKeyword: "过期", SearchCount: 1, LastSearchedAt: now.AddDate(0, 0, -31), CreatedAt: now, UpdatedAt: now},
		{ID: 14, StatDate: today.AddDate(0, 0, -7), ScopeType: ScopeGlobal, ScopeID: globalScopeID, NormalizedKeyword: "第八天", DisplayKeyword: "第八天", SearchCount: 99, LastSearchedAt: now.AddDate(0, 0, -7), CreatedAt: now, UpdatedAt: now},
		{ID: 15, StatDate: today.AddDate(0, 0, -30), ScopeType: ScopeGlobal, ScopeID: globalScopeID, NormalizedKeyword: "统计边界", DisplayKeyword: "统计边界", SearchCount: 1, LastSearchedAt: now.AddDate(0, 0, -30), CreatedAt: now, UpdatedAt: now},
	}
	for index := range stats {
		if err := repo.UpsertDailyStat(ctx, db, &stats[index]); err != nil {
			t.Fatalf("insert daily stat: %v", err)
		}
	}
	rows, err := repo.HotKeywords(ctx, ScopeGlobal, globalScopeID, today.AddDate(0, 0, -6), today, 20)
	if err != nil {
		t.Fatalf("aggregate hot keywords: %v", err)
	}
	if len(rows) != 2 || rows[0].NormalizedKeyword != "啤酒" || rows[0].SearchCount != 2 || rows[1].NormalizedKeyword != "白酒" {
		t.Fatalf("unexpected hot ranking: %#v", rows)
	}

	oldHistory := History{ID: 20, CustomerID: 9, Keyword: "旧历史", NormalizedKeyword: "旧历史", SearchCount: 1, LastSearchedAt: now.AddDate(0, 0, -181), CreatedAt: now, UpdatedAt: now}
	newHistory := History{ID: 21, CustomerID: 9, Keyword: "新历史", NormalizedKeyword: "新历史", SearchCount: 1, LastSearchedAt: now, CreatedAt: now, UpdatedAt: now}
	boundaryHistory := History{ID: 22, CustomerID: 9, Keyword: "历史边界", NormalizedKeyword: "历史边界", SearchCount: 1, LastSearchedAt: now.Add(-180 * 24 * time.Hour), CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&[]History{oldHistory, newHistory, boundaryHistory}).Error; err != nil {
		t.Fatalf("insert cleanup histories: %v", err)
	}
	worker := NewWorker(config.SearchConfig{HistoryRetention: 180 * 24 * time.Hour, StatsRetentionDays: 30, CleanupBatchSize: 100, CleanupInterval: time.Hour}, db, nil, "test", nil)
	worker.now = func() time.Time { return now }
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("run cleanup: %v", err)
	}
	var historyCount, statsCount int64
	if err := db.Model(&History{}).Count(&historyCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&DailyStat{}).Count(&statsCount).Error; err != nil {
		t.Fatal(err)
	}
	if historyCount != 2 || statsCount != 5 {
		t.Fatalf("expected only expired rows deleted, history=%d stats=%d", historyCount, statsCount)
	}
}

func searchTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&History{}, &DailyStat{}); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}
	return db
}
