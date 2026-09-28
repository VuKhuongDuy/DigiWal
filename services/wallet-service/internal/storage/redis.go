package storage

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// depositAddressSet is the Redis set chain-watcher (a later phase) will
// scan against on every incoming chain event — checking set membership
// is O(1) instead of a Postgres round trip per transaction the chain
// produces, most of which belong to someone else entirely.
const depositAddressSet = "wallet:deposit_addresses"

type Cache struct {
	rdb *redis.Client
}

func NewCache(rdb *redis.Client) *Cache {
	return &Cache{rdb: rdb}
}

// AddDepositAddress registers a newly created address in the cache.
// Best-effort: Postgres is the source of truth, so a cache miss just
// costs chain-watcher a fallback Postgres lookup later, it never causes
// a missed deposit.
func (c *Cache) AddDepositAddress(ctx context.Context, address string) error {
	return c.rdb.SAdd(ctx, depositAddressSet, address).Err()
}

func (c *Cache) IsDepositAddress(ctx context.Context, address string) (bool, error) {
	return c.rdb.SIsMember(ctx, depositAddressSet, address).Result()
}
