package storage

import (
	"context"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"
)

// The address -> user_id hash chain-watcher reads. Needs a real Redis: TEST_REDIS_ADDR.
func TestAddressOwnersHash(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set TEST_REDIS_ADDR to run against a real Redis")
	}
	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer rdb.Close()
	rdb.Del(ctx, AddressOwnersKey)
	t.Cleanup(func() { rdb.Del(ctx, AddressOwnersKey) })
	c := NewCache(rdb)

	if err := c.AddDepositAddress(ctx, "0x70997970C51812dc3A010C7d01b50e0d17dc79C8", "alice"); err != nil {
		t.Fatal(err)
	}
	// stored lower-cased, so chain-watcher can HGET with a normalized address
	if v, err := rdb.HGet(ctx, AddressOwnersKey, "0x70997970c51812dc3a010c7d01b50e0d17dc79c8").Result(); err != nil || v != "alice" {
		t.Fatalf("HGET lower-cased field: %q %v", v, err)
	}
	for _, spelling := range []string{"0x70997970C51812dc3A010C7d01b50e0d17dc79C8", "0x70997970c51812dc3a010c7d01b50e0d17dc79c8"} {
		if u, ok, err := c.OwnerOf(ctx, spelling); err != nil || !ok || u != "alice" {
			t.Fatalf("OwnerOf(%s) = %q %v %v", spelling, u, ok, err)
		}
	}
	if _, ok, err := c.OwnerOf(ctx, "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC"); err != nil || ok {
		t.Fatalf("a miss must report ok=false, not an error: %v %v", ok, err)
	}
}
