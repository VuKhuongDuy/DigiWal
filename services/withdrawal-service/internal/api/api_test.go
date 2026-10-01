package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"digiwal/services/withdrawal-service/internal/store"
)

// fakeRequests is an in-memory store keyed like the real one: (user_id, idempotency_key).
type fakeRequests struct {
	rows  map[[2]string]store.Withdrawal
	calls int
}

func newFake() *fakeRequests { return &fakeRequests{rows: map[[2]string]store.Withdrawal{}} }

func (f *fakeRequests) Create(_ context.Context, r store.NewRequest) (store.Withdrawal, bool, error) {
	f.calls++
	k := [2]string{r.UserID, r.IdempotencyKey}
	if w, ok := f.rows[k]; ok {
		return w, false, nil
	}
	w := store.Withdrawal{ID: "id-" + r.UserID + "-" + r.IdempotencyKey, UserID: r.UserID, IdempotencyKey: r.IdempotencyKey,
		AssetID: r.AssetID, Amount: r.Amount, ToAddress: r.ToAddress, Status: store.StatusRequested}
	f.rows[k] = w
	return w, true, nil
}

func (f *fakeRequests) Get(_ context.Context, userID, key string) (store.Withdrawal, error) {
	f.calls++
	if w, ok := f.rows[[2]string{userID, key}]; ok {
		return w, nil
	}
	return store.Withdrawal{}, store.ErrNotFound
}

func withPeer(r *http.Request, cn string) *http.Request {
	if cn != "" {
		cert := &x509.Certificate{Subject: pkix.Name{CommonName: cn}}
		r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}
	}
	return r
}

func do(t *testing.T, f *fakeRequests, cn, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := withPeer(httptest.NewRequest(method, path, strings.NewReader(body)), cn)
	rec := httptest.NewRecorder()
	NewServer(f, nil, Config{}).MTLSRoutes().ServeHTTP(rec, req)
	return rec
}

const body = `{"user_id":"alice","idempotency_key":"k1","asset_id":"11111111-1111-1111-1111-111111111111","amount":"1000000000000000000000000000000","to_address":"0x70997970C51812dc3A010C7d01b50e0d17dc79C8"}`

func TestOnlyTheGatewayMayCall(t *testing.T) {
	for _, cn := range []string{"sweeper", "withdrawal-service", "ledger-service", ""} {
		f := newFake()
		if rec := do(t, f, cn, "POST", "/v1/withdrawals", body); rec.Code != 403 || f.calls != 0 {
			t.Errorf("POST as CN=%q: %d (store calls %d), want 403 before any store call", cn, rec.Code, f.calls)
		}
		if rec := do(t, f, cn, "GET", "/v1/withdrawals/k1?user_id=alice", ""); rec.Code != 403 || f.calls != 0 {
			t.Errorf("GET as CN=%q: %d (store calls %d), want 403", cn, rec.Code, f.calls)
		}
	}
}

func TestCreateIsIdempotent(t *testing.T) {
	f := newFake()
	rec := do(t, f, "api-gateway", "POST", "/v1/withdrawals", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first POST: %d %s", rec.Code, rec.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["idempotency_key"] != "k1" || got["status"] != "requested" || got["tx_hash"] != nil {
		t.Fatalf("first POST body = %v", got)
	}
	if strings.Contains(rec.Body.String(), "id-alice") || strings.Contains(rec.Body.String(), "user_id") {
		t.Fatalf("response leaks internal ids: %s", rec.Body)
	}
	if rec := do(t, f, "api-gateway", "POST", "/v1/withdrawals", body); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"requested"`) {
		t.Fatalf("duplicate POST: %d %s, want 200 with the existing status", rec.Code, rec.Body)
	}
	if len(f.rows) != 1 {
		t.Fatalf("rows = %d", len(f.rows))
	}
	// Same key, different withdrawal: not a retry. Refused, and the original row is untouched.
	other := strings.Replace(body, `"amount":"1000000000000000000000000000000"`, `"amount":"5"`, 1)
	if rec := do(t, f, "api-gateway", "POST", "/v1/withdrawals", other); rec.Code != http.StatusConflict {
		t.Fatalf("reused key, different amount: %d, want 409", rec.Code)
	}
	if want, _ := new(big.Int).SetString("1000000000000000000000000000000", 10); f.rows[[2]string{"alice", "k1"}].Amount.Cmp(want) != 0 {
		t.Fatal("original row changed")
	}
	// Same key from another user is that user's own, new request (invariant #20).
	bob := strings.Replace(body, `"user_id":"alice"`, `"user_id":"bob"`, 1)
	if rec := do(t, f, "api-gateway", "POST", "/v1/withdrawals", bob); rec.Code != http.StatusAccepted {
		t.Fatalf("bob with alice's key: %d, want 202 (a new request)", rec.Code)
	}
}

func TestCreateRefusesUnstorableBodies(t *testing.T) {
	cases := map[string]string{
		"not json":        `{`,
		"unknown field":   strings.Replace(body, `"asset_id"`, `"account_id":"x","asset_id"`, 1),
		"no user":         strings.Replace(body, `"user_id":"alice"`, `"user_id":""`, 1),
		"no key":          strings.Replace(body, `"idempotency_key":"k1"`, `"idempotency_key":""`, 1),
		"key too long":    strings.Replace(body, `"idempotency_key":"k1"`, `"idempotency_key":"`+strings.Repeat("k", 129)+`"`, 1),
		"key with space":  strings.Replace(body, `"idempotency_key":"k1"`, `"idempotency_key":"a b"`, 1),
		"zero amount":     strings.Replace(body, `"amount":"1000000000000000000000000000000"`, `"amount":"0"`, 1),
		"negative amount": strings.Replace(body, `"amount":"1000000000000000000000000000000"`, `"amount":"-1"`, 1),
		"decimal amount":  strings.Replace(body, `"amount":"1000000000000000000000000000000"`, `"amount":"1.5"`, 1),
		"numeric amount":  strings.Replace(body, `"amount":"1000000000000000000000000000000"`, `"amount":5`, 1),
		"no to_address":   strings.Replace(body, `"to_address":"0x70997970C51812dc3A010C7d01b50e0d17dc79C8"`, `"to_address":""`, 1),
	}
	for name, b := range cases {
		f := newFake()
		if rec := do(t, f, "api-gateway", "POST", "/v1/withdrawals", b); rec.Code != http.StatusBadRequest || len(f.rows) != 0 {
			t.Errorf("%s: %d (rows %d), want 400 and nothing stored", name, rec.Code, len(f.rows))
		}
	}
}

func TestGetIsScopedToTheUser(t *testing.T) {
	f := newFake()
	do(t, f, "api-gateway", "POST", "/v1/withdrawals", body)
	if rec := do(t, f, "api-gateway", "GET", "/v1/withdrawals/k1?user_id=alice", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"requested"`) {
		t.Fatalf("alice's own: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, f, "api-gateway", "GET", "/v1/withdrawals/k1?user_id=bob", ""); rec.Code != 404 {
		t.Fatalf("bob reading alice's key: %d, want 404", rec.Code)
	}
	if rec := do(t, f, "api-gateway", "GET", "/v1/withdrawals/nope?user_id=alice", ""); rec.Code != 404 {
		t.Fatalf("missing key: %d, want 404", rec.Code)
	}
	if rec := do(t, f, "api-gateway", "GET", "/v1/withdrawals/k1", ""); rec.Code != 400 {
		t.Fatalf("no user_id: %d, want 400", rec.Code)
	}
}
