package customerlocation

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"jiuxiaoer-admin/backend-go/internal/pkg/problem"
)

type ContextStore struct {
	redis *goredis.Client
	ttl   time.Duration
}

func NewContextStore(client *goredis.Client, ttl time.Duration) *ContextStore {
	return &ContextStore{redis: client, ttl: ttl}
}

func (s *ContextStore) Create(ctx context.Context, value LocationContext) (LocationContext, error) {
	if s.redis == nil {
		return LocationContext{}, problem.New(503, "LOCATION_CONTEXT_UNAVAILABLE", "Service Unavailable", "location context store unavailable")
	}
	id, err := opaqueID()
	if err != nil {
		return LocationContext{}, problem.Internal("location context id generation failed")
	}
	now := time.Now().UTC()
	value.ID = id
	value.Version = 1
	value.CreatedAt = now
	value.ExpiresAt = now.Add(s.ttl)
	payload, err := json.Marshal(value)
	if err != nil {
		return LocationContext{}, err
	}
	if err := s.redis.Set(ctx, contextKey(id), payload, s.ttl).Err(); err != nil {
		return LocationContext{}, problem.New(503, "LOCATION_CONTEXT_UNAVAILABLE", "Service Unavailable", "location context store unavailable")
	}
	return value, nil
}

func (s *ContextStore) Index(ctx context.Context, actorKey, id string) error {
	if s.redis == nil {
		return contextUnavailable()
	}
	pipe := s.redis.TxPipeline()
	pipe.SAdd(ctx, actorKey, id)
	pipe.Expire(ctx, actorKey, s.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		_ = s.redis.Del(ctx, contextKey(id)).Err()
		return contextUnavailable()
	}
	return nil
}

func (s *ContextStore) RevokeActor(ctx context.Context, actorKey string) error {
	if s.redis == nil {
		return contextUnavailable()
	}
	ids, err := s.redis.SMembers(ctx, actorKey).Result()
	if err != nil {
		return contextUnavailable()
	}
	keys := make([]string, 0, len(ids)+1)
	keys = append(keys, actorKey)
	for _, id := range ids {
		keys = append(keys, contextKey(id))
	}
	if err := s.redis.Del(ctx, keys...).Err(); err != nil {
		return contextUnavailable()
	}
	return nil
}

func (s *ContextStore) Get(ctx context.Context, id string) (LocationContext, error) {
	if s.redis == nil {
		return LocationContext{}, problem.New(503, "LOCATION_CONTEXT_UNAVAILABLE", "Service Unavailable", "location context store unavailable")
	}
	raw, err := s.redis.Get(ctx, contextKey(id)).Bytes()
	if errors.Is(err, goredis.Nil) {
		return LocationContext{}, problem.New(410, "LOCATION_CONTEXT_EXPIRED", "Gone", "location context expired")
	}
	if err != nil {
		return LocationContext{}, problem.New(503, "LOCATION_CONTEXT_UNAVAILABLE", "Service Unavailable", "location context store unavailable")
	}
	var value LocationContext
	if err := json.Unmarshal(raw, &value); err != nil || value.ID != id || value.Version == 0 {
		return LocationContext{}, problem.New(503, "LOCATION_CONTEXT_UNAVAILABLE", "Service Unavailable", "location context is invalid")
	}
	return value, nil
}

func (s *ContextStore) Update(ctx context.Context, id string, expectedVersion uint32, mutate func(*LocationContext) error) (LocationContext, error) {
	if s.redis == nil {
		return LocationContext{}, problem.New(503, "LOCATION_CONTEXT_UNAVAILABLE", "Service Unavailable", "location context store unavailable")
	}
	key := contextKey(id)
	var updated LocationContext
	err := s.redis.Watch(ctx, func(tx *goredis.Tx) error {
		raw, err := tx.Get(ctx, key).Bytes()
		if errors.Is(err, goredis.Nil) {
			return problem.New(410, "LOCATION_CONTEXT_EXPIRED", "Gone", "location context expired")
		}
		if err != nil {
			return err
		}
		var value LocationContext
		if err := json.Unmarshal(raw, &value); err != nil {
			return problem.New(503, "LOCATION_CONTEXT_UNAVAILABLE", "Service Unavailable", "location context is invalid")
		}
		if value.Version != expectedVersion {
			return problem.Conflict("LOCATION_CONTEXT_VERSION_CONFLICT", "location context version changed")
		}
		if err := mutate(&value); err != nil {
			return err
		}
		value.Version++
		payload, err := json.Marshal(value)
		if err != nil {
			return err
		}
		ttl, err := tx.TTL(ctx, key).Result()
		if err != nil || ttl <= 0 {
			return problem.New(410, "LOCATION_CONTEXT_EXPIRED", "Gone", "location context expired")
		}
		_, err = tx.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
			pipe.Set(ctx, key, payload, ttl)
			return nil
		})
		updated = value
		return err
	}, key)
	if errors.Is(err, goredis.TxFailedErr) {
		return LocationContext{}, problem.Conflict("LOCATION_CONTEXT_VERSION_CONFLICT", "location context version changed")
	}
	if err != nil {
		var details *problem.Details
		if errors.As(err, &details) {
			return LocationContext{}, details
		}
		return LocationContext{}, problem.New(503, "LOCATION_CONTEXT_UNAVAILABLE", "Service Unavailable", "location context store unavailable")
	}
	return updated, nil
}

func verifyActor(value LocationContext, actor Actor) error {
	if value.ActorType != actor.Type {
		return problem.Forbidden("LOCATION_CONTEXT_FORBIDDEN", "location context is not available")
	}
	if value.ActorType == "customer" {
		if subtle.ConstantTimeCompare([]byte(value.ActorID), []byte(actor.ID)) != 1 {
			return problem.Forbidden("LOCATION_CONTEXT_FORBIDDEN", "location context is not available")
		}
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(value.SessionHash), []byte(actor.SessionHash)) != 1 {
		return problem.Forbidden("LOCATION_CONTEXT_FORBIDDEN", "location context is not available")
	}
	return nil
}

func opaqueID() (string, error) {
	buffer := make([]byte, 24)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return "loc_" + base64.RawURLEncoding.EncodeToString(buffer), nil
}

func contextKey(id string) string { return "lbs:ctx:v1:" + id }
