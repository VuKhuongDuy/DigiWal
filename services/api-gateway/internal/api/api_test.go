package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"digiwal/services/api-gateway/internal/api"
	"digiwal/services/api-gateway/internal/store"
	"digiwal/services/api-gateway/internal/testdb"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testdb.Terminate()
	os.Exit(code)
}

// fakeWallet stands in for wallet-service and records exactly which user it was asked about.
type fakeWallet struct {
	mu        sync.Mutex
	requests  []string // "METHOD path"
	addresses map[string]string
	fail      bool
}

func (f *fakeWallet) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		if f.fail {
			w.WriteHeader(500)
			return
		}
		const prefix, suffix = "/v1/users/", "/deposit-address"
		user := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), suffix)
		addr, has := f.addresses[user]
		if r.Method != http.MethodPost { // the gateway must only ever use the idempotent get-or-create
			w.WriteHeader(405)
			return
		}
		if has {
			_, _ = w.Write([]byte(`{"user_id":"` + user + `","chain":"evm","address":"` + addr + `"}`))
			return
		}
		if f.addresses == nil {
			f.addresses = map[string]string{}
		}
		f.addresses[user] = "0xADDR_FOR_" + user
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"user_id":"` + user + `","chain":"evm","address":"` + f.addresses[user] + `"}`))
	})
}

// fakeLedger stands in for ledger-service and records who it was asked about.
type fakeLedger struct {
	mu       sync.Mutex
	requests []string // request URI
	status   int
	body     string
}

func (f *fakeLedger) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(f.body))
			return
		}
		_, _ = w.Write([]byte(`[{"asset_id":"a","symbol":"ETH","total_balance":"100","locked_balance":"30","available_balance":"70"}]`))
	})
}

type env struct {
	t         *testing.T
	st        *store.Store
	srv       *httptest.Server
	wallet    *fakeWallet
	walletSrv *httptest.Server
	ledger    *fakeLedger
	wd        *fakeWithdrawals
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Connect(context.Background(), testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	fw := &fakeWallet{}
	ws := httptest.NewServer(fw.handler())
	t.Cleanup(ws.Close)
	fl := &fakeLedger{}
	ls := httptest.NewServer(fl.handler())
	t.Cleanup(ls.Close)
	fwd := &fakeWithdrawals{}
	wds := httptest.NewServer(fwd.handler())
	t.Cleanup(wds.Close)
	s := &api.Server{Auth: st, Users: st, Wallet: &api.WalletClient{BaseURL: ws.URL, HTTP: ws.Client()},
		Ledger:      &api.LedgerClient{BaseURL: ls.URL, HTTP: ls.Client()},
		Withdrawals: &api.WithdrawalClient{BaseURL: wds.URL, HTTP: wds.Client()}}
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return &env{t: t, st: st, srv: srv, wallet: fw, walletSrv: ws, ledger: fl, wd: fwd}
}

func (e *env) do(method, path, auth string, body string, hdr ...string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *env) newUser() (id, key string) {
	e.t.Helper()
	code, body := e.do("POST", "/users", "", `{}`)
	if code != 201 {
		e.t.Fatalf("POST /users = %d %s", code, body)
	}
	var out struct{ UserID, APIKey string }
	var raw map[string]string
	_ = json.Unmarshal([]byte(body), &raw)
	out.UserID, out.APIKey = raw["user_id"], raw["api_key"]
	return out.UserID, out.APIKey
}

func TestCreateUserMintsIdentityAndStoresOnlyAHash(t *testing.T) {
	e := newEnv(t)
	id, key := e.newUser()
	if len(id) != 36 || !strings.HasPrefix(key, store.KeyPrefix) || len(key) != len(store.KeyPrefix)+64 {
		t.Fatalf("user_id %q, api_key %q", id, key)
	}
	// invariant #2: the plaintext key is nowhere in the database.
	var n int
	// Computed by Postgres itself, independent of the Go code that wrote it: SHA-256 of the key.
	if err := e.st.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM api_keys WHERE key_hash = sha256(convert_to($1, 'UTF8')) AND user_id::text = $2`, key, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("hash row count %d, %v", n, err)
	}
	var leaked int
	if err := e.st.Pool().QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM api_keys a WHERE a::text LIKE '%' || $1 || '%') + (SELECT count(*) FROM users u WHERE u::text LIKE '%' || $1 || '%')`,
		key[len(store.KeyPrefix):]).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("the plaintext api key appears in the database (%d rows, %v)", leaked, err)
	}
}

func TestUserIDIsSystemGeneratedNotClientChosen(t *testing.T) {
	e := newEnv(t)
	code, body := e.do("POST", "/users", "", `{"user_id":"alice","id":"alice"}`)
	if code != 201 || strings.Contains(body, `"alice"`) {
		t.Fatalf("client-supplied id was honoured: %d %s", code, body)
	}
}

func TestKeysAndUsersAreUnique(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	ids, keys := map[string]bool{}, map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, key := e.newUser()
			mu.Lock()
			ids[id], keys[key] = true, true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(ids) != 20 || len(keys) != 20 {
		t.Fatalf("collisions: %d ids, %d keys", len(ids), len(keys))
	}
}

// invariant #3: everything but POST /users needs a valid key — including paths that do not exist.
func TestEverythingElseRequiresAValidKey(t *testing.T) {
	e := newEnv(t)
	_, key := e.newUser()
	bad := map[string]string{
		"none":         "",
		"wrong scheme": "Basic " + key,
		"no scheme":    key,
		"empty bearer": "Bearer ",
		"unknown key":  "Bearer dw_" + strings.Repeat("0", 64),
		"garbage":      "Bearer !!!",
		"two tokens":   "Bearer " + key + " extra",
	}
	for _, route := range [][2]string{
		{"GET", "/v1/deposit-address"}, {"GET", "/v1/balances"}, {"GET", "/nope"}, {"GET", "/"},
		{"POST", "/v1/withdrawals"}, {"GET", "/v1/withdrawals/k1"},
		{"GET", "/v1/whitelist"}, {"POST", "/v1/whitelist"}, {"DELETE", "/v1/whitelist/x"},
	} {
		for name, auth := range bad {
			if code, _ := e.do(route[0], route[1], auth, `{}`); code != 401 {
				t.Errorf("%s %s with %s = %d, want 401", route[0], route[1], name, code)
			}
		}
	}
	// A valid key on a route that does not exist is a 404, not a 401.
	if code, _ := e.do("GET", "/nope", "Bearer "+key, ""); code != 404 {
		t.Errorf("valid key, unknown route = %d, want 404", code)
	}
	if len(e.wallet.requests) != 0 || len(e.wd.requests) != 0 {
		t.Fatalf("an unauthenticated request reached a service: %v %v", e.wallet.requests, e.wd.requests)
	}
}

// invariant #6: revocation applies to the very next request.
func TestRevokedKeyIsRejectedImmediately(t *testing.T) {
	e := newEnv(t)
	_, key := e.newUser()
	if code, _ := e.do("GET", "/v1/deposit-address", "Bearer "+key, ""); code != 201 && code != 200 {
		t.Fatalf("valid key rejected: %d", code)
	}
	if ok, err := e.st.RevokeKey(context.Background(), key); err != nil || !ok {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	before := len(e.wallet.requests)
	if code, _ := e.do("GET", "/v1/deposit-address", "Bearer "+key, ""); code != 401 {
		t.Fatalf("revoked key = %d, want 401", code)
	}
	if len(e.wallet.requests) != before {
		t.Fatal("a revoked key still reached wallet-service")
	}
}

// invariant #4: the user is the key's owner, whatever the request claims.
func TestClientCannotNameAnotherUser(t *testing.T) {
	e := newEnv(t)
	aliceID, aliceKey := e.newUser()
	bobID, _ := e.newUser()

	for _, tc := range []struct {
		path, body string
		hdr        []string
	}{
		{"/v1/deposit-address?user_id=" + bobID, "", nil},
		{"/v1/deposit-address?userID=" + bobID, "", nil},
		{"/v1/deposit-address", `{"user_id":"` + bobID + `"}`, nil},
		{"/v1/deposit-address", "", []string{"X-User-Id", bobID, "X-Forwarded-User", bobID}},
	} {
		e.wallet.requests = nil
		code, body := e.do("GET", tc.path, "Bearer "+aliceKey, tc.body, tc.hdr...)
		if code != 200 && code != 201 {
			t.Fatalf("%s: %d %s", tc.path, code, body)
		}
		for _, r := range e.wallet.requests {
			if strings.Contains(r, bobID) || !strings.Contains(r, aliceID) {
				t.Fatalf("%s: wallet-service was asked %q — must only ever be alice's id", tc.path, r)
			}
		}
	}
}

func TestDepositAddressAlwaysUsesTheIdempotentPost(t *testing.T) {
	e := newEnv(t)
	id, key := e.newUser()
	code, body := e.do("GET", "/v1/deposit-address", "Bearer "+key, "")
	if code != 201 || !strings.Contains(body, "0xADDR_FOR_"+id) {
		t.Fatalf("first call: %d %s", code, body)
	}
	e.wallet.requests = nil
	code, body = e.do("GET", "/v1/deposit-address", "Bearer "+key, "")
	if code != 200 || !strings.Contains(body, "0xADDR_FOR_"+id) {
		t.Fatalf("second call: %d %s", code, body)
	}
	if len(e.wallet.requests) != 1 || !strings.HasPrefix(e.wallet.requests[0], "POST ") {
		t.Fatalf("every call must be one POST (no GET, no create-on-404 round trip): %v", e.wallet.requests)
	}
}

func TestBalancesAreTheAuthenticatedUsersOwn(t *testing.T) {
	e := newEnv(t)
	aliceID, aliceKey := e.newUser()
	bobID, _ := e.newUser()
	code, body := e.do("GET", "/v1/balances?asset_id=abc&user_id="+bobID, "Bearer "+aliceKey, `{"user_id":"`+bobID+`"}`, "X-User-Id", bobID)
	if code != 200 || !strings.Contains(body, `"available_balance":"70"`) {
		t.Fatalf("%d %s", code, body)
	}
	if len(e.ledger.requests) != 1 || e.ledger.requests[0] != "GET /v1/ledger/balances/"+aliceID+"?asset_id=abc" {
		t.Fatalf("ledger was asked %v — must be alice's id only, with just the asset filter", e.ledger.requests)
	}
	// No filter: no query string at all.
	e.ledger.requests = nil
	e.do("GET", "/v1/balances", "Bearer "+aliceKey, "")
	if e.ledger.requests[0] != "GET /v1/ledger/balances/"+aliceID {
		t.Fatalf("ledger was asked %v", e.ledger.requests)
	}
}

func TestBalancesNeedAKeyAndPassUpstreamErrorsThrough(t *testing.T) {
	e := newEnv(t)
	_, key := e.newUser()
	if code, _ := e.do("GET", "/v1/balances", "", ""); code != 401 {
		t.Fatalf("no key = %d", code)
	}
	if len(e.ledger.requests) != 0 {
		t.Fatal("an unauthenticated request reached ledger-service")
	}
	e.ledger.status, e.ledger.body = 400, `{"error":"asset_id must be a uuid"}`
	if code, body := e.do("GET", "/v1/balances?asset_id=x", "Bearer "+key, ""); code != 400 || !strings.Contains(body, "uuid") {
		t.Fatalf("upstream 400 = %d %s", code, body)
	}
	e.ledger.status = 0
	// ledger-service unreachable
	e2 := newEnv(t)
	_, k2 := e2.newUser()
	e2.srv.Config.Handler = (&api.Server{Auth: e2.st, Users: e2.st, Wallet: &api.WalletClient{BaseURL: e2.walletSrv.URL, HTTP: e2.walletSrv.Client()},
		Ledger: &api.LedgerClient{BaseURL: "http://127.0.0.1:1", HTTP: http.DefaultClient}}).Routes()
	if code, _ := e2.do("GET", "/v1/balances", "Bearer "+k2, ""); code != 502 {
		t.Fatalf("unreachable ledger = %d, want 502", code)
	}
}

func TestUpstreamFailureIsABadGatewayNotAnAuthError(t *testing.T) {
	e := newEnv(t)
	_, key := e.newUser()
	e.wallet.fail = true
	if code, _ := e.do("GET", "/v1/deposit-address", "Bearer "+key, ""); code != 500 {
		t.Fatalf("upstream 500 should pass through verbatim, got %d", code)
	}
	e.walletSrv.Close()
	if code, _ := e.do("GET", "/v1/deposit-address", "Bearer "+key, ""); code != 502 {
		t.Fatalf("unreachable wallet-service = %d, want 502", code)
	}
}

func TestAuthLookupFailureIsUnavailableNotUnauthorized(t *testing.T) {
	e := newEnv(t)
	_, key := e.newUser()
	e.st.Close() // the database goes away
	if code, _ := e.do("GET", "/v1/deposit-address", "Bearer "+key, ""); code != 503 {
		t.Fatalf("auth lookup failure = %d, want 503 (a valid caller must not be told 401)", code)
	}
}

func TestHealthzNeedsNoAuth(t *testing.T) {
	e := newEnv(t)
	if code, _ := e.do("GET", "/healthz", "", ""); code != 200 {
		t.Fatalf("healthz = %d", code)
	}
}

// Design invariant 6: /metrics is never served on the public listener (it would be reachable by clients and
// would disclose the user count). Metrics live on their own internal port.
func TestMetricsAreNotOnThePublicListener(t *testing.T) {
	e := newEnv(t)
	_, key := e.newUser()
	for _, auth := range []string{"", "Bearer " + key} {
		for _, path := range []string{"/metrics", "/metrics/"} {
			code, body := e.do("GET", path, auth, "")
			if code == 200 || strings.Contains(body, "go_goroutines") || strings.Contains(body, "digiwal_users_total") {
				t.Errorf("GET %s (auth %q) = %d, metrics must not be served here:\n%.200s", path, auth, code, body)
			}
		}
	}
}
