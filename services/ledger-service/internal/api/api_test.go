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

	"digiwal/services/ledger-service/internal/ledger"
)

type fake struct {
	status  ledger.Status
	lock    ledger.Lock
	settle  ledger.SettleResult
	err     error
	gotTx   ledger.Transaction
	gotLock ledger.LockRequest
	gotKey  string
	calls   int
	sheet   []ledger.AssetBalance
}

func (f *fake) Apply(_ context.Context, t ledger.Transaction) (ledger.Status, error) {
	f.calls++
	f.gotTx = t
	return f.status, f.err
}
func (f *fake) Lock(_ context.Context, r ledger.LockRequest) (ledger.Lock, error) {
	f.calls++
	f.gotLock = r
	return f.lock, f.err
}
func (f *fake) Settle(_ context.Context, key string) (ledger.SettleResult, error) {
	f.calls++
	f.gotKey = key
	return f.settle, f.err
}
func (f *fake) Release(_ context.Context, key string) error {
	f.calls++
	f.gotKey = key
	return f.err
}

func (f *fake) BalanceSheet(_ context.Context, userID, assetID string) ([]ledger.AssetBalance, error) {
	f.calls++
	f.gotKey = userID + "|" + assetID
	return f.sheet, f.err
}

func call(t *testing.T, f *fake, cn, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	if cn != "" {
		cert := &x509.Certificate{Subject: pkix.Name{CommonName: cn}}
		req.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}
	}
	rec := httptest.NewRecorder()
	NewServer(f).MTLSRoutes().ServeHTTP(rec, req)
	return rec
}

const (
	eth      = "11111111-1111-1111-1111-111111111111"
	entryA   = "33333333-3333-3333-3333-333333333333"
	entryB   = "44444444-4444-4444-4444-444444444444"
	lockBody = `{"idempotency_key":"w1","user_id":"alice","asset_id":"` + eth + `","amount":"500"}`
	corrBody = `{"idempotency_key":"w1:refund","source_type":"correction","entries":[
		{"account_ref":"user","user_id":"mallory","asset_id":"` + eth + `","amount":"999","corrects_entry_id":"` + entryA + `"},
		{"corrects_entry_id":"` + entryB + `"}]}`
)

var endpoints = map[string]string{
	"/v1/ledger/transactions":     corrBody,
	"/v1/ledger/locks":            lockBody,
	"/v1/ledger/locks/w1/settle":  ``,
	"/v1/ledger/locks/w1/release": ``,
}

// service-auth-mtls.md invariant #7: only withdrawal-service, on all four endpoints.
func TestEveryMutatingEndpointIsWithdrawalServiceOnly(t *testing.T) {
	for path, body := range endpoints {
		for cn, want := range map[string]int{"withdrawal-service": 200, "sweeper": 403, "api-gateway": 403, "chain-watcher": 403, "": 403} {
			f := &fake{status: ledger.Applied, lock: ledger.Lock{Amount: big.NewInt(1)}}
			if got := call(t, f, cn, path, body).Code; got != want {
				t.Errorf("%s CN=%q: %d, want %d", path, cn, got, want)
			}
			if want == 403 && f.calls != 0 {
				t.Errorf("%s CN=%q reached the ledger", path, cn)
			}
		}
	}
}

func TestLock_WireContract(t *testing.T) {
	f := &fake{lock: ledger.Lock{IdempotencyKey: "w1", Status: ledger.LockActive, Amount: big.NewInt(500)}}
	rec := call(t, f, "withdrawal-service", "/v1/ledger/locks", lockBody)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"active"`) || !strings.Contains(rec.Body.String(), `"amount":"500"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	g := f.gotLock
	if g.IdempotencyKey != "w1" || g.UserID != "alice" || g.AssetID != eth || g.Amount.String() != "500" {
		t.Fatalf("parsed wrong: %+v", g)
	}
	// callers never name an account id
	if rec := call(t, &fake{}, "withdrawal-service", "/v1/ledger/locks", strings.Replace(lockBody, `"amount"`, `"account_id":"x","amount"`, 1)); rec.Code != 400 {
		t.Fatalf("account_id accepted: %d", rec.Code)
	}
}

func TestSettleAndReleaseWireContract(t *testing.T) {
	f := &fake{settle: ledger.SettleResult{IdempotencyKey: "w1", LedgerTransactionID: "tx-1"}}
	rec := call(t, f, "withdrawal-service", "/v1/ledger/locks/w1/settle", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"settled"`) || !strings.Contains(rec.Body.String(), `"ledger_transaction_id":"tx-1"`) || f.gotKey != "w1" {
		t.Fatalf("settle: %d %s (key %q)", rec.Code, rec.Body, f.gotKey)
	}
	f = &fake{}
	rec = call(t, f, "withdrawal-service", "/v1/ledger/locks/w2/release", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"released"`) || f.gotKey != "w2" {
		t.Fatalf("release: %d %s", rec.Code, rec.Body)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := map[error]int{
		ledger.ErrInsufficientFunds:    422,
		ledger.ErrLockNotFound:         404,
		ledger.ErrIdempotencyKeyReused: 409,
		ledger.ErrLockAlreadyReleased:  409, // never a 200: the caller must not believe money moved
		ledger.ErrLockAlreadySettled:   409,
		ledger.ErrInvalid:              400,
		ledger.ErrUnbalanced:           400,
		ledger.ErrUnknownAsset:         400,
		errors.New("db down"):          500,
	}
	for err, want := range cases {
		for _, path := range []string{"/v1/ledger/locks", "/v1/ledger/locks/w1/settle", "/v1/ledger/locks/w1/release"} {
			body := ""
			if path == "/v1/ledger/locks" {
				body = lockBody
			}
			if got := call(t, &fake{err: err}, "withdrawal-service", path, body).Code; got != want {
				t.Errorf("%s: %v → %d, want %d", path, err, got, want)
			}
		}
	}
	if rec := call(t, &fake{err: ledger.ErrInsufficientFunds}, "withdrawal-service", "/v1/ledger/locks", lockBody); !strings.Contains(rec.Body.String(), `"status":"insufficient_funds"`) {
		t.Fatalf("422 body: %s", rec.Body)
	}
	if rec := call(t, &fake{err: ledger.ErrInvalidCorrection}, "withdrawal-service", "/v1/ledger/transactions", corrBody); rec.Code != 400 || !strings.Contains(rec.Body.String(), `"status":"invalid_correction"`) {
		t.Fatalf("invalid correction: %d %s", rec.Code, rec.Body)
	}
}

// A correction names entries to reverse; the account and amount the caller adds are dropped.
func TestCorrection_OnlyCorrectsEntryIDsReachTheLedger(t *testing.T) {
	f := &fake{status: ledger.Applied}
	rec := call(t, f, "withdrawal-service", "/v1/ledger/transactions", corrBody)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"applied"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if f.gotTx.Source != ledger.SourceCorrection || len(f.gotTx.Entries) != 2 {
		t.Fatalf("%+v", f.gotTx)
	}
	for _, e := range f.gotTx.Entries {
		if e.UserID != "" || e.Amount != nil || e.AssetID != "" || e.Ref != "" {
			t.Fatalf("a caller-supplied field reached the ledger: %+v", e)
		}
	}
	if f.gotTx.Entries[0].CorrectsEntryID != entryA || f.gotTx.Entries[1].CorrectsEntryID != entryB {
		t.Fatalf("%+v", f.gotTx.Entries)
	}
}

func TestBadRequestsNeverReachTheLedger(t *testing.T) {
	bad := map[string][2]string{
		"deposit over REST (must go through screening)": {"/v1/ledger/transactions", strings.Replace(corrBody, `"correction"`, `"deposit"`, 1)},
		"withdrawal source is gone":                     {"/v1/ledger/transactions", strings.Replace(corrBody, `"correction"`, `"withdrawal"`, 1)},
		"not json":                                      {"/v1/ledger/locks", `nope`},
		"unknown field":                                 {"/v1/ledger/locks", strings.Replace(lockBody, `"user_id"`, `"bonus":1,"user_id"`, 1)},
		"amount as number":                              {"/v1/ledger/locks", strings.Replace(lockBody, `"500"`, `500`, 1)},
		"amount not a number":                           {"/v1/ledger/locks", strings.Replace(lockBody, `"500"`, `"abc"`, 1)},
	}
	for name, c := range bad {
		f := &fake{}
		if rec := call(t, f, "withdrawal-service", c[0], c[1]); rec.Code != 400 || f.calls != 0 {
			t.Errorf("%s: %d (ledger calls %d), want 400 and 0", name, rec.Code, f.calls)
		}
	}
}

func TestHealthzIsOnThePlainListener(t *testing.T) {
	rec := httptest.NewRecorder()
	NewServer(&fake{}).Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	for path := range endpoints {
		rec = httptest.NewRecorder()
		NewServer(&fake{}).Routes().ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader("{}")))
		if rec.Code == 200 {
			t.Fatalf("%s must not exist on the plain listener", path)
		}
	}
}

func get(f *fake, cn, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	if cn != "" {
		cert := &x509.Certificate{Subject: pkix.Name{CommonName: cn}}
		req.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}
	}
	rec := httptest.NewRecorder()
	NewServer(f).MTLSRoutes().ServeHTTP(rec, req)
	return rec
}

func TestBalancesAreReadableByAPIGatewayOnly(t *testing.T) {
	for cn, want := range map[string]int{"api-gateway": 200, "withdrawal-service": 403, "sweeper": 403, "chain-watcher": 403, "": 403} {
		f := &fake{}
		if got := get(f, cn, "/v1/ledger/balances/alice").Code; got != want {
			t.Errorf("CN=%q: %d, want %d", cn, got, want)
		}
		if want == 403 && f.calls != 0 {
			t.Errorf("CN=%q reached the ledger", cn)
		}
	}
}

func TestBalancesWireContract(t *testing.T) {
	f := &fake{sheet: []ledger.AssetBalance{{AssetID: eth, Symbol: "ETH", Total: big.NewInt(0).Mul(big.NewInt(1e18), big.NewInt(100)), Locked: big.NewInt(5), Available: big.NewInt(0).Sub(big.NewInt(0).Mul(big.NewInt(1e18), big.NewInt(100)), big.NewInt(5))}}}
	rec := get(f, "api-gateway", "/v1/ledger/balances/alice?asset_id="+eth)
	if rec.Code != 200 || f.gotKey != "alice|"+eth {
		t.Fatalf("%d %s (%s)", rec.Code, rec.Body, f.gotKey)
	}
	want := `[{"asset_id":"` + eth + `","symbol":"ETH","total_balance":"100000000000000000000","locked_balance":"5","available_balance":"99999999999999999995"}]`
	if strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("body = %s", rec.Body)
	}
	if rec := get(&fake{}, "api-gateway", "/v1/ledger/balances/alice?asset_id=nope"); rec.Code != 400 {
		t.Fatalf("bad asset_id = %d", rec.Code)
	}
	// An empty sheet is an empty array, never null.
	if rec := get(&fake{}, "api-gateway", "/v1/ledger/balances/alice"); strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("empty sheet = %s", rec.Body)
	}
}
