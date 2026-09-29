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
		"withdrawal-service": 200, "sweeper": 200, "api-gateway": 403, "": 403,
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
