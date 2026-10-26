package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"digiwal/pkg/metrics"
	"digiwal/services/api-gateway/internal/store"
)

type fakeKV struct {
	m       map[string]string
	ttl     map[string]time.Duration
	failGet bool
	failDel bool
}

func newFakeKV() *fakeKV { return &fakeKV{m: map[string]string{}, ttl: map[string]time.Duration{}} }
func (f *fakeKV) Get(_ context.Context, k string) (string, bool, error) {
	if f.failGet {
		return "", false, errors.New("redis down")
	}
	v, ok := f.m[k]
	return v, ok, nil
}
func (f *fakeKV) Set(_ context.Context, k, v string, ttl time.Duration) error {
	f.m[k], f.ttl[k] = v, ttl
	return nil
}
func (f *fakeKV) Del(_ context.Context, k string) error {
	if f.failDel {
		return errors.New("redis down")
	}
	delete(f.m, k)
	return nil
}

// db stands in for the database: one valid key, and a count of lookups.
type db struct {
	user    string
	revoked bool
	calls   int
}

func (d *db) Authenticate(_ context.Context, key string) (string, error) {
	d.calls++
	if key != "dw_good" || d.revoked {
		return "", store.ErrInvalidKey
	}
	return d.user, nil
}
func (d *db) RevokeKey(_ context.Context, key string) (bool, error) {
	d.revoked = true
	return true, nil
}

func newAuthCache(d *db, kv *fakeKV) *AuthCache {
	return &AuthCache{Next: d, Revoker: d, KV: kv, TTL: 5 * time.Minute, Stats: metrics.NewCache("t", "apikey")}
}

func TestAuthCacheMissThenHit(t *testing.T) {
	d, kv := &db{user: "u1"}, newFakeKV()
	c := newAuthCache(d, kv)
	for i := 0; i < 3; i++ {
		if uid, err := c.Authenticate(context.Background(), "dw_good"); err != nil || uid != "u1" {
			t.Fatalf("Authenticate = %q, %v", uid, err)
		}
	}
	if d.calls != 1 {
		t.Errorf("database lookups = %d, want 1 (the second and third are hits)", d.calls)
	}
	for k, ttl := range kv.ttl {
		if ttl != 5*time.Minute {
			t.Errorf("%s has ttl %s, want 5m", k, ttl)
		}
		if strings.Contains(k, "dw_good") {
			t.Errorf("the raw API key is in the cache key %q", k)
		}
	}
}

func TestAuthCacheNeverCachesAnInvalidKey(t *testing.T) {
	d, kv := &db{user: "u1"}, newFakeKV()
	c := newAuthCache(d, kv)
	for i := 0; i < 2; i++ {
		if _, err := c.Authenticate(context.Background(), "dw_bad"); !errors.Is(err, store.ErrInvalidKey) {
			t.Fatalf("err = %v, want ErrInvalidKey", err)
		}
	}
	if d.calls != 2 || len(kv.m) != 0 {
		t.Errorf("lookups = %d, cached entries = %d; want 2 and 0", d.calls, len(kv.m))
	}
}

func TestAuthCacheRedisDownFallsThroughToTheDatabase(t *testing.T) {
	d, kv := &db{user: "u1"}, newFakeKV()
	kv.failGet = true
	c := newAuthCache(d, kv)
	if uid, err := c.Authenticate(context.Background(), "dw_good"); err != nil || uid != "u1" {
		t.Fatalf("Authenticate with Redis down = %q, %v", uid, err)
	}
}

func TestAuthCacheRevokeDeletesTheEntryAndTheNextReadSeesIt(t *testing.T) {
	d, kv := &db{user: "u1"}, newFakeKV()
	c := newAuthCache(d, kv)
	if _, err := c.Authenticate(context.Background(), "dw_good"); err != nil {
		t.Fatal(err)
	}
	if len(kv.m) != 1 {
		t.Fatalf("entries after the first read = %d, want 1", len(kv.m))
	}
	if ok, err := c.RevokeKey(context.Background(), "dw_good"); err != nil || !ok {
		t.Fatalf("RevokeKey = %v, %v", ok, err)
	}
	if len(kv.m) != 0 {
		t.Errorf("the entry survived the revocation")
	}
	if _, err := c.Authenticate(context.Background(), "dw_good"); !errors.Is(err, store.ErrInvalidKey) {
		t.Errorf("a revoked key authenticated: err = %v", err)
	}
}
