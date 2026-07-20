package customerlocation

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

func TestContextStoreLifecycleAndVersionCAS(t *testing.T) {
	mini := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewContextStore(client, 10*time.Minute)
	ctx := context.Background()

	created, err := store.Create(ctx, LocationContext{
		ActorType: "customer",
		ActorID:   "42",
		Source:    "device_location",
		Location:  AdministrativeLocationDTO{CityCode: "440300"},
	})
	if err != nil {
		t.Fatalf("create context: %v", err)
	}
	if !locationContextIDPattern.MatchString(created.ID) || created.Version != 1 {
		t.Fatalf("unexpected context identity: id=%q version=%d", created.ID, created.Version)
	}
	loaded, err := store.Get(ctx, created.ID)
	if err != nil || loaded.ActorID != "42" {
		t.Fatalf("get context: value=%+v err=%v", loaded, err)
	}
	updated, err := store.Update(ctx, created.ID, 1, func(value *LocationContext) error {
		value.SelectionSource = "manual"
		return nil
	})
	if err != nil || updated.Version != 2 || updated.SelectionSource != "manual" {
		t.Fatalf("update context: value=%+v err=%v", updated, err)
	}
	if _, err := store.Update(ctx, created.ID, 1, func(*LocationContext) error { return nil }); problem.FromError(err).ErrorCode != "LOCATION_CONTEXT_VERSION_CONFLICT" {
		t.Fatalf("expected version conflict, got %v", err)
	}
	if err := store.Index(ctx, "lbs:actor:v1:opaque", created.ID); err != nil {
		t.Fatalf("index context: %v", err)
	}
	if err := store.RevokeActor(ctx, "lbs:actor:v1:opaque"); err != nil {
		t.Fatalf("revoke actor contexts: %v", err)
	}
	if _, err := store.Get(ctx, created.ID); problem.FromError(err).ErrorCode != "LOCATION_CONTEXT_EXPIRED" {
		t.Fatalf("expected revoked context to be gone, got %v", err)
	}

	created, err = store.Create(ctx, LocationContext{ActorType: "customer", ActorID: "42", Source: "device_location"})
	if err != nil {
		t.Fatalf("create expiring context: %v", err)
	}

	mini.FastForward(11 * time.Minute)
	if _, err := store.Get(ctx, created.ID); problem.FromError(err).ErrorCode != "LOCATION_CONTEXT_EXPIRED" {
		t.Fatalf("expected expired context, got %v", err)
	}
}

func TestVerifyActorEnforcesObjectBinding(t *testing.T) {
	if err := verifyActor(LocationContext{ActorType: "customer", ActorID: "42"}, Actor{Type: "customer", ID: "42"}); err != nil {
		t.Fatalf("expected matching customer: %v", err)
	}
	if err := verifyActor(LocationContext{ActorType: "customer", ActorID: "42"}, Actor{Type: "customer", ID: "43"}); problem.FromError(err).ErrorCode != "LOCATION_CONTEXT_FORBIDDEN" {
		t.Fatalf("expected customer binding error, got %v", err)
	}
	if err := verifyActor(LocationContext{ActorType: "anonymous", SessionHash: "session-a"}, Actor{Type: "anonymous", SessionHash: "session-b"}); problem.FromError(err).ErrorCode != "LOCATION_CONTEXT_FORBIDDEN" {
		t.Fatalf("expected session binding error, got %v", err)
	}
}
