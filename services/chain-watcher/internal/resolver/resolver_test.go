package resolver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/redis/go-redis/v9"
)

var addr = common.HexToAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")

func redisClient(t *testing.T) *redis.Client {
	t.Helper()
	a := os.Getenv("TEST_REDIS_ADDR")
	if a == "" {
		t.Skip("TEST_REDIS_ADDR not set")
	}
	rdb := redis.NewClient(&redis.Options{Addr: a, DB: 9})
	if err := rdb.FlushDB(context.Background()).Err(); err != nil {
		t.Skipf("redis unavailable: %v", err)
	}
	t.Cleanup(func() { rdb.FlushDB(context.Background()); rdb.Close() })
	return rdb
}

func wallet(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !strings.HasPrefix(r.URL.Path, "/v1/addresses/") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestRedisHitNeverCallsWalletService(t *testing.T) {
	rdb := redisClient(t)
	srv, hits := wallet(t, 500, "")
	rdb.HSet(context.Background(), AddressOwnersKey, strings.ToLower(addr.Hex()), "alice")
	r := New(rdb, srv.Client(), srv.URL, 0)
	uid, ok, err := r.Resolve(context.Background(), addr)
	if err != nil || !ok || uid != "alice" {
		t.Fatalf("got %q %v %v", uid, ok, err)
	}
	if hits.Load() != 0 {
		t.Fatal("a Redis hit must not cost an HTTP call")
	}
}

func TestMissFallsBackToWalletService(t *testing.T) {
	srv, hits := wallet(t, 200, `{"user_id":"bob","chain":"evm"}`)
	r := New(redisClient(t), srv.Client(), srv.URL, 0)
	uid, ok, err := r.Resolve(context.Background(), addr)
	if err != nil || !ok || uid != "bob" || hits.Load() != 1 {
		t.Fatalf("got %q %v %v hits=%d", uid, ok, err, hits.Load())
	}
}

func Test404IsDefinitelyNotOurs(t *testing.T) {
	srv, _ := wallet(t, 404, `{"error":"not a watched address"}`)
	r := New(redisClient(t), srv.Client(), srv.URL, 0)
	_, ok, err := r.Resolve(context.Background(), addr)
	if err != nil || ok {
		t.Fatalf("a 404 is a clean 'not ours': ok=%v err=%v", ok, err)
	}
}

// The heart of invariant #1: anything that is not a clean 404 must be an error, never "not ours".
func TestOnlyA404MeansNotOurs(t *testing.T) {
	for _, code := range []int{400, 401, 403, 500, 502, 503} {
		srv, _ := wallet(t, code, "boom")
		r := New(redisClient(t), srv.Client(), srv.URL, time.Minute)
		if _, ok, err := r.Resolve(context.Background(), addr); err == nil || ok {
			t.Errorf("status %d treated as an answer (ok=%v err=%v)", code, ok, err)
		}
	}
	// Unreachable wallet-service.
	srv, _ := wallet(t, 200, "")
	client, url := srv.Client(), srv.URL
	srv.Close()
	r := New(redisClient(t), client, url, time.Minute)
	if _, ok, err := r.Resolve(context.Background(), addr); err == nil || ok {
		t.Errorf("unreachable wallet-service treated as an answer (ok=%v err=%v)", ok, err)
	}
	// A 200 without a usable owner is not an answer either.
	srv2, _ := wallet(t, 200, `{}`)
	if _, ok, err := New(redisClient(t), srv2.Client(), srv2.URL, 0).Resolve(context.Background(), addr); err == nil || ok {
		t.Errorf("200 with no user_id treated as an answer (ok=%v err=%v)", ok, err)
	}
}

func TestErrorsAreNeverCachedAs404(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte(`{"user_id":"carol"}`))
	}))
	defer srv.Close()
	r := New(redisClient(t), srv.Client(), srv.URL, time.Minute)
	if _, _, err := r.Resolve(context.Background(), addr); err == nil {
		t.Fatal("expected error while wallet-service is down")
	}
	fail.Store(false)
	uid, ok, err := r.Resolve(context.Background(), addr)
	if err != nil || !ok || uid != "carol" {
		t.Fatalf("after recovery: %q %v %v — a transient failure must not have been cached", uid, ok, err)
	}
}

func TestNegativeCacheAvoidsRepeatCalls(t *testing.T) {
	srv, hits := wallet(t, 404, "")
	r := New(redisClient(t), srv.Client(), srv.URL, time.Minute)
	for i := 0; i < 5; i++ {
		if _, ok, err := r.Resolve(context.Background(), addr); ok || err != nil {
			t.Fatal(ok, err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
	// ...and expires.
	now := time.Now()
	r.now = func() time.Time { return now.Add(2 * time.Minute) }
	_, _, _ = r.Resolve(context.Background(), addr)
	if hits.Load() != 2 {
		t.Fatalf("hits after expiry = %d, want 2", hits.Load())
	}
}

func TestRedisDownFallsBackToWalletServiceNotToNotOurs(t *testing.T) {
	srv, hits := wallet(t, 200, `{"user_id":"dave"}`)
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 100 * time.Millisecond})
	defer dead.Close()
	uid, ok, err := New(dead, srv.Client(), srv.URL, 0).Resolve(context.Background(), addr)
	if err != nil || !ok || uid != "dave" || hits.Load() != 1 {
		t.Fatalf("got %q %v %v hits=%d", uid, ok, err, hits.Load())
	}
}

func TestFetchHotAddress(t *testing.T) {
	const hot = "0x9965507D1a55bcC2695C58ba16FB37d819B0A4dc"
	serve := func(status int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/wallet/hot-address" {
				t.Errorf("path = %s", r.URL.Path)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	}
	ok := serve(200, `{"address":"`+hot+`"}`)
	defer ok.Close()
	got, err := FetchHotAddress(context.Background(), ok.Client(), ok.URL+"/")
	if err != nil || got != common.HexToAddress(hot) {
		t.Fatalf("got %s, %v", got, err)
	}
	// Anything but a clean 200 with a real address is an error: never a zero/empty Hot Wallet,
	// which would silently switch the Hot Wallet Outflow filter off.
	for name, srv := range map[string]*httptest.Server{
		"forbidden":    serve(403, `{"error":"forbidden"}`),
		"unconfigured": serve(503, `{"error":"no hot wallet"}`),
		"garbage":      serve(200, `{"address":"nope"}`),
		"zero address": serve(200, `{"address":"0x0000000000000000000000000000000000000000"}`),
	} {
		if a, err := FetchHotAddress(context.Background(), srv.Client(), srv.URL); err == nil {
			t.Errorf("%s: accepted %s", name, a)
		}
		srv.Close()
	}
}
