package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"digiwal/pkg/metrics"
	"digiwal/services/ledger-service/internal/ledger"
)

type memKV struct {
	m       map[string]string
	failGet bool
	dels    int
}

func (k *memKV) Get(_ context.Context, key string) (string, bool, error) {
	if k.failGet {
		return "", false, errors.New("redis down")
	}
	v, ok := k.m[key]
	return v, ok, nil
}
func (k *memKV) Set(_ context.Context, key, v string, _ time.Duration) error {
	k.m[key] = v
	return nil
}
func (k *memKV) Del(_ context.Context, keys ...string) error {
	for _, key := range keys {
		delete(k.m, key)
		k.dels++
	}
	return nil
}

func twoAssets() []ledger.AssetBalance {
	return []ledger.AssetBalance{
		{AssetID: eth, Symbol: "ETH", Total: big.NewInt(100), Locked: big.NewInt(5), Available: big.NewInt(95)},
		{AssetID: "22222222-2222-2222-2222-222222222222", Symbol: "TT", Total: big.NewInt(7), Locked: big.NewInt(0), Available: big.NewInt(7)},
	}
}

func getCached(s *Server, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: "api-gateway"}}
	req.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}
	rec := httptest.NewRecorder()
	s.MTLSRoutes().ServeHTTP(rec, req)
	return rec
}

func TestBalanceCacheMissThenHitAndTheAssetFilterIsAppliedAfterTheRead(t *testing.T) {
	f, kv := &fake{sheet: twoAssets()}, &memKV{m: map[string]string{}}
	c := &BalanceCache{KV: kv, TTL: 5 * time.Second, Stats: metrics.NewCache("t", "balances")}
	s := NewServer(f).WithBalanceCache(c)

	all := getCached(s, "/v1/ledger/balances/alice")
	if all.Code != 200 || !strings.Contains(all.Body.String(), `"symbol":"TT"`) {
		t.Fatalf("first read = %d %s", all.Code, all.Body)
	}
	calls := f.calls
	again := getCached(s, "/v1/ledger/balances/alice")
	filtered := getCached(s, "/v1/ledger/balances/alice?asset_id="+eth)
	if f.calls != calls {
		t.Errorf("the database was read %d more times, want 0 (both are hits)", f.calls-calls)
	}
	if again.Body.String() != all.Body.String() {
		t.Errorf("a hit differs from the miss:\n%s\n%s", again.Body, all.Body)
	}
	if !strings.Contains(filtered.Body.String(), `"symbol":"ETH"`) || strings.Contains(filtered.Body.String(), `"symbol":"TT"`) {
		t.Errorf("the asset filter was not applied to the cached sheet: %s", filtered.Body)
	}
}

func TestBalanceCacheInvalidateThenTheNextReadSeesTheNewBalance(t *testing.T) {
	f, kv := &fake{sheet: twoAssets()}, &memKV{m: map[string]string{}}
	c := &BalanceCache{KV: kv, TTL: 5 * time.Second, Stats: metrics.NewCache("t", "balances")}
	s := NewServer(f).WithBalanceCache(c)
	getCached(s, "/v1/ledger/balances/alice")

	f.sheet[0].Total, f.sheet[0].Available = big.NewInt(60), big.NewInt(55) // a write happened
	c.Invalidate(context.Background(), []string{"alice"})
	if kv.dels != 1 || len(kv.m) != 0 {
		t.Fatalf("deletes = %d, entries left = %d", kv.dels, len(kv.m))
	}
	if body := getCached(s, "/v1/ledger/balances/alice").Body.String(); !strings.Contains(body, `"total_balance":"60"`) {
		t.Errorf("the read after the invalidation is stale: %s", body)
	}
}

func TestBalanceCacheRedisDownStillAnswersFromTheDatabase(t *testing.T) {
	f, kv := &fake{sheet: twoAssets()}, &memKV{m: map[string]string{}, failGet: true}
	s := NewServer(f).WithBalanceCache(&BalanceCache{KV: kv, TTL: time.Second, Stats: metrics.NewCache("t", "balances")})
	if rec := getCached(s, "/v1/ledger/balances/alice"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"total_balance":"100"`) {
		t.Fatalf("with Redis down: %d %s", rec.Code, rec.Body)
	}
}

func TestBalanceCacheOffByDefault(t *testing.T) {
	f := &fake{sheet: twoAssets()}
	s := NewServer(f)
	getCached(s, "/v1/ledger/balances/alice?asset_id="+eth)
	if f.gotKey != "alice|"+eth {
		t.Errorf("with the cache off the filter must reach the database query, got %q", f.gotKey)
	}
}

func TestBalanceCacheHitIsTheStoredJSONAndAnUnusableEntryIsRefilled(t *testing.T) {
	f, kv := &fake{sheet: twoAssets()}, &memKV{m: map[string]string{}}
	c := &BalanceCache{KV: kv, TTL: 5 * time.Second, Stats: metrics.NewCache("t", "balances")}
	s := NewServer(f).WithBalanceCache(c)

	miss := getCached(s, "/v1/ledger/balances/alice")
	if stored := kv.m[balKey("alice")]; stored != miss.Body.String() {
		t.Errorf("the entry is not the response body:\nstored %q\nbody   %q", stored, miss.Body.String())
	}
	if hit := getCached(s, "/v1/ledger/balances/alice"); hit.Body.String() != miss.Body.String() || hit.Header().Get("Content-Type") != "application/json" {
		t.Errorf("a hit must be byte-identical to the miss: %q vs %q", hit.Body, miss.Body)
	}

	kv.m[balKey("alice")] = "not json" // somebody else wrote the key: treated as a miss, then overwritten
	calls := f.calls
	if rec := getCached(s, "/v1/ledger/balances/alice"); rec.Code != 200 || rec.Body.String() != miss.Body.String() {
		t.Fatalf("a bad entry must be answered from the database: %d %q", rec.Code, rec.Body)
	}
	if f.calls != calls+1 || kv.m[balKey("alice")] != miss.Body.String() {
		t.Errorf("the bad entry was not refilled (database reads +%d)", f.calls-calls)
	}
}
