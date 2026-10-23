package api

import (
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Needs a real Redis (the Lua script): TEST_REDIS_ADDR.
func TestRedisLimiter(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set TEST_REDIS_ADDR to run against a real Redis")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, DB: 10})
	defer rdb.Close()
	rdb.FlushDB(t.Context())

	l := &RedisLimiter{Client: rdb, Prefix: "t:", Limit: 3, Window: 2 * time.Second, Fallback: NewLimiter(1000, time.Minute)}
	// Two gateway instances sharing one budget.
	l2 := &RedisLimiter{Client: rdb, Prefix: "t:", Limit: 3, Window: 2 * time.Second, Fallback: NewLimiter(1000, time.Minute)}

	for i, lim := range []*RedisLimiter{l, l2, l} {
		if ok, _ := lim.Allow("a"); !ok {
			t.Fatalf("request %d rejected within limit", i)
		}
	}
	ok, retry := l2.Allow("a")
	if ok || retry <= 0 || retry > 2*time.Second {
		t.Fatalf("4th request across instances: ok=%v retry=%v, want rejected with retry in (0, 2s]", ok, retry)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("another key must have its own budget")
	}
	if ttl := rdb.PTTL(t.Context(), "t:a").Val(); ttl <= 0 || ttl > 4*time.Second {
		t.Fatalf("key ttl %v, want it to expire within two windows", ttl)
	}

	time.Sleep(4100 * time.Millisecond) // two windows idle: clean slate
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("request rejected after the key aged out")
	}
}

// Redis down must not take the gateway down, nor leave it without a limit.
func TestRedisLimiterFailsOpenToFallback(t *testing.T) {
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 100 * time.Millisecond})
	defer dead.Close()
	l := &RedisLimiter{Client: dead, Prefix: "t:", Limit: 2, Window: time.Minute, Fallback: NewLimiter(2, time.Minute)}

	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("request %d rejected though the fallback has budget", i)
		}
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("fallback limit not enforced while Redis is down")
	}
	start := time.Now()
	for i := 0; i < 50; i++ {
		l.Allow("b")
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("50 checks took %v during an outage; Redis should be skipped during the cooldown", d)
	}
}
