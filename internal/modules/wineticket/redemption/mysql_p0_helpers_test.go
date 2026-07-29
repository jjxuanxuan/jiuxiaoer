package redemption

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"jiuxiaoer-admin/backend-go/internal/config"
	mysqlinfra "jiuxiaoer-admin/backend-go/internal/infra/mysql"
)

const mysqlP0Concurrency = 100

func openWineTicketMySQLAcceptance(
	t *testing.T,
	timeout time.Duration,
) (context.Context, *gorm.DB) {
	t.Helper()
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run wine-ticket MySQL P0 acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	cfg := config.Load()
	cfg.MySQL.Required = true
	cfg.MySQL.RequiredTimeZone = "+08:00"
	cfg.MySQL.RequireWineTicketSchema = true
	cfg.MySQL.RequireWineTicketMoneyContract = false
	if cfg.MySQL.MaxOpenConns < 64 {
		cfg.MySQL.MaxOpenConns = 64
	}
	if cfg.MySQL.MaxIdleConns < 16 {
		cfg.MySQL.MaxIdleConns = 16
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := mysqlinfra.Open(ctx, cfg.MySQL, log)
	if err != nil || db == nil {
		t.Fatalf("open schema- and timezone-verified mysql: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get mysql pool: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return ctx, db
}

func runMySQLConcurrentErrors(
	concurrency int,
	action func(index int) error,
) []error {
	start := make(chan struct{})
	results := make([]error, concurrency)
	var wait sync.WaitGroup
	wait.Add(concurrency)
	for index := 0; index < concurrency; index++ {
		go func(index int) {
			defer wait.Done()
			<-start
			results[index] = action(index)
		}(index)
	}
	close(start)
	wait.Wait()
	return results
}
