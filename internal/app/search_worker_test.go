package app

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
)

func TestSearchRetentionWorkerRoleDoesNotRequireRabbitMQ(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:search_worker_role?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := &Server{
		cfg: config.Config{
			App: config.AppConfig{InstanceID: "search-worker-test"},
			Search: config.SearchConfig{
				CleanupEnabled: true, CleanupInterval: time.Hour, CleanupBatchSize: 100,
				HistoryRetention: 180 * 24 * time.Hour, StatsRetentionDays: 30,
			},
		},
		log:  log,
		deps: Dependencies{DB: db, Log: log},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := server.RunMQWorker(ctx, "search-retention"); err != nil {
		t.Fatalf("search retention role should require only MySQL, got %v", err)
	}
}
