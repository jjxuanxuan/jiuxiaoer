package nodelease

import (
	"context"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"jiuxiaoer-admin/backend-go/internal/config"
)

// TestSnowflakeNodeLeaseRejectsConcurrentOwner 验证雪花节点租约拒绝并发持有者。
func TestSnowflakeNodeLeaseRejectsConcurrentOwner(t *testing.T) {
	if os.Getenv("JXE_RUN_INTEGRATION") != "1" {
		t.Skip("set JXE_RUN_INTEGRATION=1 to run Redis lease integration test")
	}
	cfg := config.Load()
	client := goredis.NewClient(&goredis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: 15})
	defer client.Close()
	ctx := context.Background()
	client.Del(ctx, "jxe:infra:snowflake:node:1000")

	first, err := Acquire(ctx, client, 1000, "instance-a", 3*time.Second)
	if err != nil {
		t.Fatalf("acquire first lease: %v", err)
	}
	defer first.Release(ctx)
	if _, err := Acquire(ctx, client, 1000, "instance-b", 3*time.Second); err == nil {
		t.Fatal("expected duplicate node lease to fail")
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("release first lease: %v", err)
	}
	second, err := Acquire(ctx, client, 1000, "instance-b", 3*time.Second)
	if err != nil {
		t.Fatalf("acquire released lease: %v", err)
	}
	defer second.Release(ctx)
}
