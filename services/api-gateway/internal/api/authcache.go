package api

import (
	"context"
	"encoding/hex"
	"errors"
	"log"
	"time"

	"digiwal/pkg/metrics"
	"digiwal/services/api-gateway/internal/store"

	"github.com/redis/go-redis/v9"
)

// KV is the slice of Redis the caches use, so tests need no server.
type KV interface {
	Get(ctx context.Context, key string) (string, bool, error) // found=false is a miss
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	Del(ctx context.Context, key string) error
}

// RedisKV adapts a go-redis client to KV.
type RedisKV struct{ C *redis.Client }

func (r RedisKV) Get(ctx context.Context, key string) (string, bool, error) {
	v, err := r.C.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	return v, err == nil, err
}
func (r RedisKV) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return r.C.Set(ctx, key, value, ttl).Err()
}
func (r RedisKV) Del(ctx context.Context, key string) error { return r.C.Del(ctx, key).Err() }

// Revoker revokes an API key in the database (store.Store.RevokeKey).
type Revoker interface {
	RevokeKey(ctx context.Context, apiKey string) (bool, error)
}

// AuthCache is a cache-aside in front of an Authenticator (docs/design/read-cache.md): a hit returns the cached user id, a miss asks the
// database and sets the entry, a revocation writes the database and then deletes the entry. Redis trouble is never an error for the caller:
// it counts as a miss and the database answers. Invalid keys are never cached (a just-minted key must work at once, and junk must not fill Redis).
type AuthCache struct {
	Next    Authenticator
	Revoker Revoker
	KV      KV
	TTL     time.Duration
	Stats   *metrics.Cache
}

func authKey(apiKey string) string {
	h := store.HashKey(apiKey)
	return "dw:v1:apikey:" + hex.EncodeToString(h)
}

func (c *AuthCache) Authenticate(ctx context.Context, apiKey string) (string, error) {
	k := authKey(apiKey)
	switch uid, found, err := c.KV.Get(ctx, k); {
	case err != nil:
		c.Stats.Error()
	case found:
		c.Stats.Hit()
		return uid, nil
	default:
		c.Stats.Miss()
	}
	uid, err := c.Next.Authenticate(ctx, apiKey)
	if err != nil {
		return "", err
	}
	if err := c.KV.Set(ctx, k, uid, c.TTL); err != nil {
		c.Stats.Error()
	}
	return uid, nil
}

// RevokeKey revokes the key in the database, then deletes its cache entry (after the UPDATE, so a reader that misses now sees it revoked).
func (c *AuthCache) RevokeKey(ctx context.Context, apiKey string) (bool, error) {
	ok, err := c.Revoker.RevokeKey(ctx, apiKey)
	if err != nil {
		return ok, err
	}
	if derr := c.KV.Del(ctx, authKey(apiKey)); derr != nil {
		c.Stats.Invalidated(false)
		log.Printf("auth cache: deleting a revoked key's entry failed (it expires in %s): %v", c.TTL, derr)
	} else {
		c.Stats.Invalidated(true)
	}
	return ok, nil
}
