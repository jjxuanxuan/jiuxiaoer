package fixedwindow

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestRedisFixedWindow(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	limiter := New(client)

	for attempt := 1; attempt <= 3; attempt++ {
		result := limiter.Allow(context.Background(), "rate:test", time.Minute, 2)
		if result.Degraded {
			t.Fatalf("attempt %d unexpectedly degraded", attempt)
		}
		if got, want := result.Allowed, attempt <= 2; got != want {
			t.Fatalf("attempt %d allowed=%v want=%v", attempt, got, want)
		}
		if result.RetryAfter < time.Second || result.RetryAfter > time.Minute {
			t.Fatalf("attempt %d retry=%s", attempt, result.RetryAfter)
		}
	}

	server.FastForward(time.Minute)
	if result := limiter.Allow(context.Background(), "rate:test", time.Minute, 2); !result.Allowed {
		t.Fatal("new Redis window should allow")
	}
}

func TestLocalFallbackIsBoundedAndResets(t *testing.T) {
	limiter := New(nil)
	now := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	limiter.now = func() time.Time { return now }

	first := limiter.Allow(context.Background(), "rate:local", time.Minute, 1)
	second := limiter.Allow(context.Background(), "rate:local", time.Minute, 1)
	if !first.Allowed || !first.Degraded || second.Allowed || !second.Degraded {
		t.Fatalf("unexpected fallback decisions: first=%+v second=%+v", first, second)
	}

	now = now.Add(time.Minute)
	if result := limiter.Allow(context.Background(), "rate:local", time.Minute, 1); !result.Allowed || !result.Degraded {
		t.Fatalf("reset result=%+v", result)
	}
}

func TestRedisFailureFallsBackLocally(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: time.Millisecond, ReadTimeout: time.Millisecond, WriteTimeout: time.Millisecond})
	t.Cleanup(func() { _ = client.Close() })
	limiter := New(client)

	startedAt := time.Now()
	if result := limiter.Allow(context.Background(), "rate:failed", time.Minute, 1); !result.Allowed || !result.Degraded {
		t.Fatalf("result=%+v", result)
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("fail-soft Redis fallback took %s", elapsed)
	}
}
