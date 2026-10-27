package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"digiwal/pkg/metrics"

	"github.com/redis/go-redis/v9"
)

// KV is the slice of Redis the balance cache uses, so tests need no server.
type KV interface {
	Get(ctx context.Context, key string) (string, bool, error) // found=false is a miss
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	Del(ctx context.Context, keys ...string) error
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
func (r RedisKV) Del(ctx context.Context, keys ...string) error { return r.C.Del(ctx, keys...).Err() }

// balanceRow is one asset of a user's balance sheet as the API returns it.
type balanceRow struct {
	AssetID          string `json:"asset_id"`
	Symbol           string `json:"symbol"`
	TotalBalance     string `json:"total_balance"`
	LockedBalance    string `json:"locked_balance"`
	AvailableBalance string `json:"available_balance"`
}

// BalanceCache is the cache-aside for GET /v1/ledger/balances/{user_id} (docs/design/read-cache.md): one entry per user holding the whole
// sheet; the asset_id filter is applied after the read. Display only: Lock and Apply check balances inside their database transaction and
// never read it. A write deletes the user's entry after its commit (Store.OnBalanceChange); the TTL bounds a stale entry left by the race
// between a reader that missed just before the commit and the delete.
type BalanceCache struct {
	KV    KV
	TTL   time.Duration
	Stats *metrics.Cache
}

func balKey(userID string) string { return "dw:v1:bal:" + strings.ToLower(userID) }

func (c *BalanceCache) get(ctx context.Context, userID string) ([]balanceRow, bool) {
	v, found, err := c.KV.Get(ctx, balKey(userID))
	if err != nil {
		c.Stats.Error()
		return nil, false
	}
	if !found {
		c.Stats.Miss()
		return nil, false
	}
	var rows []balanceRow
	if err := json.Unmarshal([]byte(v), &rows); err != nil {
		c.Stats.Error() // an entry we cannot read is a miss; the refill overwrites it
		return nil, false
	}
	c.Stats.Hit()
	return rows, true
}

func (c *BalanceCache) set(ctx context.Context, userID string, rows []balanceRow) {
	b, err := json.Marshal(rows)
	if err == nil {
		err = c.KV.Set(ctx, balKey(userID), string(b), c.TTL)
	}
	if err != nil {
		c.Stats.Error()
	}
}

// Invalidate deletes the users' entries. Bounded by a short timeout of its own: the writer's request must not wait on Redis.
func (c *BalanceCache) Invalidate(ctx context.Context, userIDs []string) {
	keys := make([]string, len(userIDs))
	for i, u := range userIDs {
		keys[i] = balKey(u)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 200*time.Millisecond)
	defer cancel()
	if err := c.KV.Del(ctx, keys...); err != nil {
		c.Stats.Invalidated(false)
		log.Printf("balance cache: delete failed (entries expire in %s): %v", c.TTL, err)
		return
	}
	c.Stats.Invalidated(true)
}
