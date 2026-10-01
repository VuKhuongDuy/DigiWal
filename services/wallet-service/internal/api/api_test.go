package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"digiwal/services/wallet-service/internal/signing"
	"digiwal/services/wallet-service/internal/storage"
)

type fakeHot struct {
	resp signing.Response
	err  error
	req  signing.Request
	n    int
}

func (f *fakeHot) Sign(_ context.Context, req signing.Request) (signing.Response, error) {
	f.n++
	f.req = req
	return f.resp, f.err
}

func (f *fakeHot) HotWalletAddress() common.Address {
	return common.HexToAddress("0x3d0290A916B57AE48a2840E7d3547fA317F976c3")
}

// withPeer fakes what the TLS stack leaves on a request after a successful
// mTLS handshake for a client certificate with the given CN.
func withPeer(r *http.Request, cn string) *http.Request {
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: cn}}
	r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}
	return r
}

func call(t *testing.T, h *fakeHot, cn, body string) *httptest.ResponseRecorder {
	t.Helper()
	srv := &Server{hotSigner: h}
	if h == nil {
		srv = &Server{}
	}
	req := httptest.NewRequest("POST", "/v1/sign/hot-wallet", strings.NewReader(body))
	if cn != "" {
		req = withPeer(req, cn)
	}
	rec := httptest.NewRecorder()
	srv.MTLSRoutes().ServeHTTP(rec, req)
	return rec
}

func TestSignHotWallet_Authorization(t *testing.T) {
	for cn, want := range map[string]int{
		"withdrawal-service": 200, "sweeper": 200, "chain-watcher": 403, "api-gateway": 403, "": 403,
	} {
		h := &fakeHot{resp: signing.Response{SignedRawTx: "0x01", Nonce: 3}}
		rec := call(t, h, cn, `{"idempotency_key":"k"}`)
		if rec.Code != want {
			t.Errorf("CN=%q: %d, want %d", cn, rec.Code, want)
		}
		if want == 403 && h.n != 0 {
			t.Errorf("CN=%q: handler logic ran for an unauthorized caller", cn)
		}
	}
}

func TestSignHotWallet_ResponseAndErrorMapping(t *testing.T) {
	ok := call(t, &fakeHot{resp: signing.Response{SignedRawTx: "0xabc", Nonce: 9}}, "sweeper", `{"idempotency_key":"k"}`)
	if ok.Code != 200 || !strings.Contains(ok.Body.String(), `"signed_raw_tx":"0xabc"`) || !strings.Contains(ok.Body.String(), `"nonce":9`) {
		t.Fatalf("%d %s", ok.Code, ok.Body)
	}
	cases := map[error]int{
		signing.ErrInvalidRequest:       400,
		signing.ErrUnknownAsset:         400,
		signing.ErrUnsupportedChain:     400,
		storage.ErrIdempotencyKeyReused: 409,
		signing.ErrGasCeilingExceeded:   422,
		storage.ErrNonceGapOpen:         503,
		errors.New("db exploded"):       500,
	}
	for err, want := range cases {
		if got := call(t, &fakeHot{err: err}, "sweeper", `{}`).Code; got != want {
			t.Errorf("%v → %d, want %d", err, got, want)
		}
	}
	if got := call(t, &fakeHot{}, "sweeper", `not json`).Code; got != 400 {
		t.Errorf("malformed body → %d", got)
	}
	// a client trying to smuggle in a key selector is rejected outright
	if got := call(t, &fakeHot{}, "sweeper", `{"idempotency_key":"k","derivation_index":5}`).Code; got != 400 {
		t.Errorf("unknown field → %d, want 400 (no caller-chosen key)", got)
	}
	if got := call(t, nil, "sweeper", `{}`).Code; got != 503 {
		t.Errorf("unconfigured → %d, want 503", got)
	}
}

type fakeUser struct {
	err error
	n   int
}

func (f *fakeUser) Sign(context.Context, signing.UserRequest) (signing.UserResponse, error) {
	f.n++
	return signing.UserResponse{SignedRawTx: "0x02"}, f.err
}

func callUser(t *testing.T, u *fakeUser, cn string) *httptest.ResponseRecorder {
	t.Helper()
	srv := &Server{userSign: u}
	req := httptest.NewRequest("POST", "/v1/sign/user-address", strings.NewReader(`{"user_id":"alice"}`))
	if cn != "" {
		req = withPeer(req, cn)
	}
	rec := httptest.NewRecorder()
	srv.MTLSRoutes().ServeHTTP(rec, req)
	return rec
}

// service-auth-mtls.md invariant #4: withdrawal-service can never pick a user key.
func TestSignUserAddress_OnlySweeperMayCall(t *testing.T) {
	for cn, want := range map[string]int{"sweeper": 200, "withdrawal-service": 403, "chain-watcher": 403, "api-gateway": 403, "": 403} {
		u := &fakeUser{}
		if got := callUser(t, u, cn).Code; got != want {
			t.Errorf("CN=%q: %d, want %d", cn, got, want)
		}
		if want == 403 && u.n != 0 {
			t.Errorf("CN=%q reached the signer", cn)
		}
	}
}

func TestSignUserAddress_ErrorMapping(t *testing.T) {
	cases := map[error]int{
		signing.ErrDestinationNotHotWallet: 403,
		signing.ErrUnknownUser:             404,
		signing.ErrInvalidRequest:          400,
		errors.New("boom"):                 500,
	}
	for err, want := range cases {
		if got := callUser(t, &fakeUser{err: err}, "sweeper").Code; got != want {
			t.Errorf("%v → %d, want %d", err, got, want)
		}
	}
}

type fakeAddresses struct {
	user, chain string
	err         error
	got         string
	calls       int
}

func (f *fakeAddresses) UserForAddress(_ context.Context, a string) (string, string, error) {
	f.calls++
	f.got = a
	return f.user, f.chain, f.err
}

func lookup(t *testing.T, f *fakeAddresses, cn, addr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/addresses/"+addr, nil)
	if cn != "" {
		req = withPeer(req, cn)
	}
	rec := httptest.NewRecorder()
	(&Server{addresses: f}).MTLSRoutes().ServeHTTP(rec, req)
	return rec
}

const someAddr = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"

// service-auth-mtls.md matrix: chain-watcher only, from day one.
func TestAddressLookup_OnlyChainWatcherMayCall(t *testing.T) {
	for cn, want := range map[string]int{"chain-watcher": 200, "withdrawal-service": 200, "sweeper": 403, "api-gateway": 403, "ledger-service": 403, "": 403} {
		f := &fakeAddresses{user: "alice", chain: "evm"}
		if got := lookup(t, f, cn, someAddr).Code; got != want {
			t.Errorf("CN=%q: %d, want %d", cn, got, want)
		}
		if want == 403 && f.calls != 0 {
			t.Errorf("CN=%q reached the lookup", cn)
		}
	}
}

func TestAddressLookup_WireContract(t *testing.T) {
	f := &fakeAddresses{user: "alice", chain: "evm"}
	rec := lookup(t, f, "chain-watcher", strings.ToLower(someAddr)) // any casing in, checksum form to the store
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"user_id":"alice"`) || !strings.Contains(rec.Body.String(), `"chain":"evm"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if f.got != someAddr {
		t.Fatalf("store queried with %q, want the EIP-55 form %q", f.got, someAddr)
	}
	// 404 is the ONLY "not ours" signal; a failure must not look like one
	if rec := lookup(t, &fakeAddresses{err: storage.ErrNotFound}, "chain-watcher", someAddr); rec.Code != 404 {
		t.Fatalf("not found: %d", rec.Code)
	}
	if rec := lookup(t, &fakeAddresses{err: errors.New("db down")}, "chain-watcher", someAddr); rec.Code != 500 {
		t.Fatalf("db error: %d, want 500 (so chain-watcher retries instead of dropping the transfer)", rec.Code)
	}
	for _, bad := range []string{"0x123", "alice", "70997970C51812dc3A010C7d01b50e0d17dc79C8zz"} {
		f := &fakeAddresses{}
		if rec := lookup(t, f, "chain-watcher", bad); rec.Code != 400 || f.calls != 0 {
			t.Errorf("%q: %d (lookups %d), want 400 and 0", bad, rec.Code, f.calls)
		}
	}
}

func TestHotAddress(t *testing.T) {
	get := func(srv *Server, cn string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/v1/wallet/hot-address", nil)
		if cn != "" {
			req = withPeer(req, cn)
		}
		rec := httptest.NewRecorder()
		srv.MTLSRoutes().ServeHTTP(rec, req)
		return rec
	}
	srv := &Server{hotSigner: &fakeHot{}}
	for cn, want := range map[string]int{"sweeper": 200, "withdrawal-service": 200, "chain-watcher": 200, "api-gateway": 403, "": 403} {
		if got := get(srv, cn).Code; got != want {
			t.Errorf("CN=%q: %d, want %d", cn, got, want)
		}
	}
	rec := get(srv, "sweeper")
	if strings.TrimSpace(rec.Body.String()) != `{"address":"0x3d0290A916B57AE48a2840E7d3547fA317F976c3"}` {
		t.Fatalf("body = %s", rec.Body)
	}
	// Without a hot_wallet seed there is no address to give: 503, never an empty/zero address.
	if rec := get(&Server{}, "sweeper"); rec.Code != 503 || strings.Contains(rec.Body.String(), "0x") {
		t.Fatalf("unconfigured = %d %s", rec.Code, rec.Body)
	}
}

func TestSystemAddresses(t *testing.T) {
	get := func(srv *Server, cn string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/v1/wallet/system-addresses", nil)
		if cn != "" {
			req = withPeer(req, cn)
		}
		rec := httptest.NewRecorder()
		srv.MTLSRoutes().ServeHTTP(rec, req)
		return rec
	}
	srv := (&Server{hotSigner: &fakeHot{}}).WithSystemAddresses(SystemAddresses{
		ChainID: 31337,
		Hot:     common.HexToAddress("0x3d0290A916B57AE48a2840E7d3547fA317F976c3"),
		Warm:    common.HexToAddress("0xa0Ee7A142d267C1f36714E4a8F75612F20a79720"),
	})
	// withdrawal-service only: the hot-address readers (sweeper, chain-watcher) get no new grant.
	for cn, want := range map[string]int{"withdrawal-service": 200, "sweeper": 403, "chain-watcher": 403, "api-gateway": 403, "ledger-service": 403, "": 403} {
		if got := get(srv, cn).Code; got != want {
			t.Errorf("CN=%q: %d, want %d", cn, got, want)
		}
	}
	rec := get(srv, "withdrawal-service")
	if strings.TrimSpace(rec.Body.String()) != `{"chain_id":31337,"hot":"0x3d0290A916B57AE48a2840E7d3547fA317F976c3","warm":"0xa0Ee7A142d267C1f36714E4a8F75612F20a79720"}` {
		t.Fatalf("body = %s", rec.Body)
	}
	// Not configured yet: 503, never a zero/partial answer.
	if rec := get(&Server{}, "withdrawal-service"); rec.Code != 503 || strings.Contains(rec.Body.String(), "0x") {
		t.Fatalf("unconfigured = %d %s", rec.Code, rec.Body)
	}
	// hot-address is unchanged for its existing callers.
	for _, cn := range []string{"sweeper", "chain-watcher", "withdrawal-service"} {
		req := withPeer(httptest.NewRequest("GET", "/v1/wallet/hot-address", nil), cn)
		rec := httptest.NewRecorder()
		srv.MTLSRoutes().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("hot-address as CN=%q: %d", cn, rec.Code)
		}
	}
}
