package servicearea

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"

	goredis "github.com/redis/go-redis/v9"

	"jiuxiaoer-admin/backend-go/internal/config"
	"jiuxiaoer-admin/backend-go/internal/infra/mysql"
)

// TestConcurrentResolveReturnsStableShop 验证Concurrent Resolve Returns Stable 门店的预期行为。
func TestConcurrentResolveReturnsStableShop(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run MySQL/Redis concurrency test")
	}
	ctx := context.Background()
	cfg := config.Load()
	db, err := mysql.Open(ctx, cfg.MySQL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || db == nil {
		t.Fatalf("open mysql: %v", err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	redisClient := goredis.NewClient(&goredis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: 12})
	if err := redisClient.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flush redis: %v", err)
	}
	defer redisClient.Close()
	defer redisClient.FlushDB(ctx)
	service := NewService(cfg.Service, db, redisClient, nil)
	input := ResolveInput{CityCode: "440300", Latitude: 22.54, Longitude: 113.93}
	warm, err := service.Resolve(ctx, input)
	if err != nil {
		t.Fatalf("warm resolver cache: %v", err)
	}

	errorsCh := make(chan error, 1000)
	var wg sync.WaitGroup
	for index := 0; index < 1000; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := service.Resolve(ctx, input)
			if err != nil {
				errorsCh <- err
				return
			}
			if result.ServiceShop.ID != warm.ServiceShop.ID {
				errorsCh <- &unstableShopError{want: warm.ServiceShop.ID, got: result.ServiceShop.ID}
			}
		}()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Fatalf("concurrent resolver failed: %v", err)
	}
}

type unstableShopError struct{ want, got string }

// Error 返回当前错误的文本描述。
func (e *unstableShopError) Error() string {
	return "unstable service shop: want=" + e.want + " got=" + e.got
}
