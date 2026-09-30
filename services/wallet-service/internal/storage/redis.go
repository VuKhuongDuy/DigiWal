package storage

import (
	"context"
	"strings"

	"github.com/redis/go-redis/v9"
)

// AddressOwnersKey is the Redis hash chain-watcher resolves deposits with (deposit-flow.md,
// "Address Resolution"): field = the deposit address, LOWER-CASED, value = its user_id.
// It is a hash, not a set: chain-watcher needs the owner, not just a yes/no. (It replaces
// the old "wallet:deposit_addresses" set, which cannot hold the owner.)
const AddressOwnersKey = "wallet:address_owners"

type Cache struct {
	rdb *redis.Client
}

func NewCache(rdb *redis.Client) *Cache {
	return &Cache{rdb: rdb}
}

// AddDepositAddress records address -> user_id the moment an address is issued.
// Best-effort: Postgres is the source of truth, so a cache miss just costs chain-watcher a
// fallback to GET /v1/addresses/{address}, it never causes a missed deposit.
func (c *Cache) AddDepositAddress(ctx context.Context, address, userID string) error {
	return c.rdb.HSet(ctx, AddressOwnersKey, strings.ToLower(address), userID).Err()
}

// OwnerOf returns the user owning a cached address, or ("", false) on a miss.
func (c *Cache) OwnerOf(ctx context.Context, address string) (string, bool, error) {
	v, err := c.rdb.HGet(ctx, AddressOwnersKey, strings.ToLower(address)).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}
