package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"digiwal/services/wallet-service/internal/signing"
	"digiwal/services/wallet-service/internal/storage"
)

type fakeKeys struct {
	lookup  signing.KeyView
	void    signing.VoidResponse
	replace signing.ReplaceResponse
	err     error
	calls   []string // "<method> <caller> <key>"
	req     signing.ReplaceRequest
}

func (f *fakeKeys) Lookup(_ context.Context, caller, key string) (signing.KeyView, error) {
	f.calls = append(f.calls, "lookup "+caller+" "+key)
	return f.lookup, f.err
}

func (f *fakeKeys) Void(_ context.Context, caller, key string) (signing.VoidResponse, error) {
	f.calls = append(f.calls, "void "+caller+" "+key)
	return f.void, f.err
}

func (f *fakeKeys) Replace(_ context.Context, caller, key string, req signing.ReplaceRequest) (signing.ReplaceResponse, error) {
	f.calls = append(f.calls, "replace "+caller+" "+key)
	f.req = req
	return f.replace, f.err
}

func keyCall(f *fakeKeys, method, path, cn, body string) *httptest.ResponseRecorder {
	srv := &Server{}
	if f != nil {
		srv.WithHotKeys(f)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if cn != "" {
		req = withPeer(req, cn)
	}
	rec := httptest.NewRecorder()
	srv.MTLSRoutes().ServeHTTP(rec, req)
	return rec
}

func TestHotKeyEndpointsAuthorization(t *testing.T) {
	const k = "/v1/sign/hot-wallet/withdrawal:abc"
	cases := []struct {
		method, path string
		cn           string
		want         int
	}{
		{"GET", k, "withdrawal-service", 200}, {"GET", k, "sweeper", 200}, {"GET", k, "chain-watcher", 403}, {"GET", k, "api-gateway", 403}, {"GET", k, "", 403},
		{"POST", k + "/void", "withdrawal-service", 200}, {"POST", k + "/void", "sweeper", 403}, {"POST", k + "/void", "api-gateway", 403}, {"POST", k + "/void", "", 403},
		{"POST", k + "/replace", "withdrawal-service", 200}, {"POST", k + "/replace", "sweeper", 200}, {"POST", k + "/replace", "chain-watcher", 403}, {"POST", k + "/replace", "", 403},
	}
	for _, tc := range cases {
		f := &fakeKeys{}
		body := ""
		if strings.HasSuffix(tc.path, "/replace") {
			body = `{"replacement_seq":1}`
		}
		rec := keyCall(f, tc.method, tc.path, tc.cn, body)
		if rec.Code != tc.want {
			t.Errorf("%s %s CN=%q: %d, want %d", tc.method, tc.path, tc.cn, rec.Code, tc.want)
		}
		if tc.want == 403 && len(f.calls) != 0 {
			t.Errorf("%s %s CN=%q: handler logic ran for an unauthorized caller", tc.method, tc.path, tc.cn)
		}
	}
	// There is no endpoint that cancels a signed tx (the Nonce Filler is an operator CLI).
	for _, p := range []string{"/cancel", "/filler"} {
		if rec := keyCall(&fakeKeys{}, "POST", k+p, "withdrawal-service", `{}`); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s → %d, want no such endpoint", p, rec.Code)
		}
	}
}

// The handler hands wallet-service the CN from the verified certificate, never anything from the body.
func TestHotKeyEndpointsScopeByTheVerifiedCN(t *testing.T) {
	f := &fakeKeys{}
	keyCall(f, "GET", "/v1/sign/hot-wallet/withdrawal:abc", "sweeper", "")
	keyCall(f, "POST", "/v1/sign/hot-wallet/withdrawal:abc/void", "withdrawal-service", "")
	keyCall(f, "POST", "/v1/sign/hot-wallet/withdrawal:abc/replace", "sweeper", `{"replacement_seq":3,"max_gas_price":"99"}`)
	want := []string{"lookup sweeper withdrawal:abc", "void withdrawal-service withdrawal:abc", "replace sweeper withdrawal:abc"}
	if strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v", f.calls)
	}
	if f.req.ReplacementSeq != 3 || f.req.MaxGasPrice != "99" {
		t.Fatalf("replace request = %+v", f.req)
	}
}

func TestHotKeyResponses(t *testing.T) {
	f := &fakeKeys{void: signing.VoidResponse{Status: "already_signed", Nonce: 4, SignedRawTx: "0xab", TxHash: "0xcd"},
		lookup:  signing.KeyView{Nonce: 4, SignedRawTx: "0xab", TxHash: "0xcd", Replacements: []signing.ReplacementView{}},
		replace: signing.ReplaceResponse{SignedRawTx: "0xef", TxHash: "0x12", Nonce: 4, ReplacementSeq: 1, MaxFeePerGas: "5", MaxPriorityFeePerGas: "1"}}
	v := keyCall(f, "POST", "/v1/sign/hot-wallet/k/void", "withdrawal-service", "")
	if v.Code != 200 || !strings.Contains(v.Body.String(), `"status":"already_signed"`) || !strings.Contains(v.Body.String(), `"signed_raw_tx":"0xab"`) {
		t.Fatalf("%d %s", v.Code, v.Body)
	}
	g := keyCall(f, "GET", "/v1/sign/hot-wallet/k", "withdrawal-service", "")
	if g.Code != 200 || !strings.Contains(g.Body.String(), `"replacements":[]`) {
		t.Fatalf("%d %s", g.Code, g.Body)
	}
	r := keyCall(f, "POST", "/v1/sign/hot-wallet/k/replace", "withdrawal-service", `{"replacement_seq":1}`)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"max_priority_fee_per_gas":"1"`) || !strings.Contains(r.Body.String(), `"replacement_seq":1`) {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
}

func TestHotKeyErrorMapping(t *testing.T) {
	type tc struct {
		path, method, body string
		err                error
		want               int
	}
	lk, vd, rp := "/v1/sign/hot-wallet/k", "/v1/sign/hot-wallet/k/void", "/v1/sign/hot-wallet/k/replace"
	body := `{"replacement_seq":1}`
	for _, c := range []tc{
		{lk, "GET", "", storage.ErrNotFound, 404},
		{lk, "GET", "", errors.New("db"), 500},
		{vd, "POST", "", storage.ErrNotCallers, 409},
		{vd, "POST", "", errors.New("db"), 500},
		{rp, "POST", body, storage.ErrNotFound, 404},
		{rp, "POST", body, storage.ErrNotCallers, 409},
		{rp, "POST", body, storage.ErrReplacementGap, 409},
		{rp, "POST", body, storage.ErrKeyCancelled, 409},
		{rp, "POST", body, signing.ErrGasCeilingExceeded, 422},
		{rp, "POST", body, storage.ErrNonceGapOpen, 503},
		{rp, "POST", body, signing.ErrInvalidRequest, 400},
		{rp, "POST", body, signing.ErrSignerUnavailable, 503},
		{rp, "POST", body, &signing.PolicyRefusedError{Rule: "replacement_not_bump"}, 422},
		{rp, "POST", body, storage.ErrOtherHotKey, 409},
		{rp, "POST", body, errors.New("db"), 500},
	} {
		if got := keyCall(&fakeKeys{err: c.err}, c.method, c.path, "withdrawal-service", c.body).Code; got != c.want {
			t.Errorf("%s %s: %v → %d, want %d", c.method, c.path, c.err, got, c.want)
		}
	}
	// A replace body has no field that could change the destination or the amount.
	for _, b := range []string{`{"replacement_seq":1,"to_address":"0x01"}`, `{"replacement_seq":1,"amount":"5"}`, `nope`, ``} {
		if got := keyCall(&fakeKeys{}, "POST", rp, "withdrawal-service", b).Code; got != 400 {
			t.Errorf("body %q → %d, want 400", b, got)
		}
	}
	if got := keyCall(nil, "GET", lk, "withdrawal-service", "").Code; got != 503 {
		t.Errorf("unconfigured → %d, want 503", got)
	}
}

func TestSignOfAVoidedKeyIs410(t *testing.T) {
	if got := call(t, &fakeHot{err: storage.ErrKeyVoided}, "withdrawal-service", `{}`).Code; got != http.StatusGone {
		t.Fatalf("voided key → %d, want 410", got)
	}
	h := &fakeHot{}
	call(t, h, "sweeper", `{"idempotency_key":"k"}`)
	if h.cn != "sweeper" {
		t.Fatalf("Sign got caller %q, want the verified CN", h.cn)
	}
}
