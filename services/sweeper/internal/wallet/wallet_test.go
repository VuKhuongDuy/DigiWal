package wallet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func serve(t *testing.T, status int, body string) (*Client, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.Path+" "+string(b))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, SweepSignerURL: srv.URL, HTTP: srv.Client()}, &seen
}

func TestSignHotWalletSendsTheDocumentedShape(t *testing.T) {
	c, seen := serve(t, 200, `{"signed_raw_tx":"0x0102","nonce":9}`)
	raw, nonce, err := c.SignHotWallet(context.Background(), HotRequest{
		ToAddress: "0xabc", AssetID: "a", Amount: "1000", ChainID: 1, IdempotencyKey: "gas-topup:1:0xh:0"})
	if err != nil || nonce != 9 || len(raw) != 2 || raw[0] != 1 {
		t.Fatalf("got %x %d %v", raw, nonce, err)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte((*seen)[0][len("POST /v1/sign/hot-wallet "):]), &sent)
	if len(sent) != 5 || sent["idempotency_key"] != "gas-topup:1:0xh:0" || sent["amount"] != "1000" {
		t.Fatalf("body = %v (no gas fields, no user_id, no key selector may be sent)", sent)
	}
}

func TestSignUserAddressSendsUserIDNotAFromAddress(t *testing.T) {
	c, seen := serve(t, 200, `{"signed_raw_tx":"0xff"}`)
	if _, err := c.SignUserAddress(context.Background(), UserRequest{UserID: "alice", ToAddress: "0xhot", AssetID: "a", Amount: "max", ChainID: 1}); err != nil {
		t.Fatal(err)
	}
	if (*seen)[0][:len("POST /v1/sign/user-address ")] != "POST /v1/sign/user-address " {
		t.Fatal((*seen)[0])
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte((*seen)[0][len("POST /v1/sign/user-address "):]), &sent)
	if sent["user_id"] != "alice" || sent["amount"] != "max" || sent["from_address"] != nil || len(sent) != 5 {
		t.Fatalf("body = %v", sent)
	}
}

// key-custody-hsm.md M3b: the Sweep signature is asked of sweep-signer and never of wallet-service (which has no such endpoint),
// while the Hot Wallet calls keep going to wallet-service. Two servers prove each call lands where it should.
func TestSweepGoesToSweepSignerAndHotCallsToWalletService(t *testing.T) {
	hit := map[string][]string{}
	mk := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hit[name] = append(hit[name], r.Method+" "+r.URL.Path)
			_, _ = w.Write([]byte(`{"signed_raw_tx":"0x01","nonce":1}`))
		}))
	}
	ws, ss := mk("wallet-service"), mk("sweep-signer")
	defer ws.Close()
	defer ss.Close()
	c := &Client{BaseURL: ws.URL, SweepSignerURL: ss.URL, HTTP: http.DefaultClient}
	if _, err := c.SignUserAddress(context.Background(), UserRequest{UserID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.SignHotWallet(context.Background(), HotRequest{IdempotencyKey: "k"}); err != nil {
		t.Fatal(err)
	}
	if len(hit["sweep-signer"]) != 1 || hit["sweep-signer"][0] != "POST /v1/sign/user-address" {
		t.Fatalf("sweep-signer saw %v", hit["sweep-signer"])
	}
	if len(hit["wallet-service"]) != 1 || hit["wallet-service"][0] != "POST /v1/sign/hot-wallet" {
		t.Fatalf("wallet-service saw %v (it must not be asked for a Sweep)", hit["wallet-service"])
	}
	// no sweep-signer configured: a transient error, never a fallback to wallet-service
	c.SweepSignerURL = ""
	if _, err := c.SignUserAddress(context.Background(), UserRequest{}); err == nil || errors.Is(err, ErrPermanent) {
		t.Fatalf("err = %v", err)
	}
	if len(hit["wallet-service"]) != 1 {
		t.Fatal("a Sweep fell back to wallet-service")
	}
}

func TestFailureClasses(t *testing.T) {
	for _, tc := range []struct {
		status                                  int
		path                                    string
		wantBelow, wantPermanent, wantTransient bool
	}{
		{422, "user", true, false, false}, // wallet-service: sweep has nothing left after gas
		{422, "hot", false, true, false},  // hot wallet: e.g. gas ceiling — retrying cannot fix it
		{400, "user", false, true, false},
		{403, "user", false, true, false}, // destination is not the hot wallet: misconfiguration
		{404, "user", false, true, false}, // unknown user
		{409, "hot", false, true, false},  // idempotency key reused with a different request
		{500, "user", false, false, true},
		{503, "hot", false, false, true}, // nonce gap freeze / not configured: will clear
		{429, "user", false, false, true},
	} {
		c, _ := serve(t, tc.status, `{"error":"x"}`)
		var err error
		if tc.path == "user" {
			_, err = c.SignUserAddress(context.Background(), UserRequest{})
		} else {
			_, _, err = c.SignHotWallet(context.Background(), HotRequest{})
		}
		below, perm := errors.Is(err, ErrBelowGas), errors.Is(err, ErrPermanent)
		if err == nil || below != tc.wantBelow || perm != tc.wantPermanent || (!below && !perm) != tc.wantTransient {
			t.Errorf("%d on %s: err=%v below=%v permanent=%v", tc.status, tc.path, err, below, perm)
		}
	}
}

func TestUnusableSuccessResponsesAreNotAnswers(t *testing.T) {
	for _, body := range []string{`{}`, `{"signed_raw_tx":""}`, `{"signed_raw_tx":"0xzz"}`, `not json`} {
		c, _ := serve(t, 200, body)
		if _, err := c.SignUserAddress(context.Background(), UserRequest{}); err == nil || errors.Is(err, ErrPermanent) {
			t.Errorf("body %q: err = %v, want a transient error", body, err)
		}
	}
}

func TestUnreachableWalletServiceIsTransient(t *testing.T) {
	c, _ := serve(t, 200, "")
	c.SweepSignerURL = "http://127.0.0.1:1"
	_, err := c.SignUserAddress(context.Background(), UserRequest{})
	if err == nil || errors.Is(err, ErrPermanent) || errors.Is(err, ErrBelowGas) {
		t.Fatalf("err = %v", err)
	}
}

func TestHotAddress(t *testing.T) {
	c, seen := serve(t, 200, `{"address":"0x3d0290A916B57AE48a2840E7d3547fA317F976c3"}`)
	got, err := c.HotAddress(context.Background())
	if err != nil || got.Hex() != "0x3d0290A916B57AE48a2840E7d3547fA317F976c3" || (*seen)[0][:len("GET /v1/wallet/hot-address")] != "GET /v1/wallet/hot-address" {
		t.Fatalf("got %s %v %v", got, err, *seen)
	}
	for _, tc := range []struct {
		status int
		body   string
	}{{503, `{"error":"hot wallet not configured"}`}, {403, `{}`}, {200, `{}`}, {200, `{"address":"nope"}`}, {200, `not json`}} {
		c, _ := serve(t, tc.status, tc.body)
		if a, err := c.HotAddress(context.Background()); err == nil {
			t.Errorf("%d %q accepted as %s", tc.status, tc.body, a)
		}
	}
}

func TestLookupHotKey(t *testing.T) {
	c, seen := serve(t, 200, `{"nonce":7,"signed_raw_tx":"0x0102","tx_hash":"0xAA","replacements":[{"replacement_seq":1,"kind":"fee_bump","signed_raw_tx":"0x03","tx_hash":"0xBB"},{"replacement_seq":2,"kind":"cancel","signed_raw_tx":"0x04","tx_hash":"0xCC"}]}`)
	k, err := c.LookupHotKey(context.Background(), "gas-topup:1:0xh:0")
	if err != nil || k == nil || len(k.Raw) != 2 || k.Hash != "0xaa" || len(k.Replacements) != 2 {
		t.Fatalf("%+v %v", k, err)
	}
	if k.Replacements[0].Seq != 1 || k.Replacements[0].Kind != "fee_bump" || k.Replacements[0].Hash != "0xbb" || k.Replacements[1].Kind != "cancel" || k.Replacements[1].Raw[0] != 4 {
		t.Fatalf("%+v", k.Replacements)
	}
	if (*seen)[0] != "GET /v1/sign/hot-wallet/gas-topup:1:0xh:0 " {
		t.Fatalf("request %q", (*seen)[0])
	}
	// Never signed: nil, not an error. Anything else is an error, never "no versions".
	if c, _ := serve(t, 404, `{}`); true {
		if k, err := c.LookupHotKey(context.Background(), "k"); k != nil || err != nil {
			t.Fatalf("404: %+v %v", k, err)
		}
	}
	if c, _ := serve(t, 503, `{}`); true {
		if _, err := c.LookupHotKey(context.Background(), "k"); err == nil {
			t.Fatal("a 503 read as 'never signed'")
		}
	}
}

func TestReplaceHotWallet(t *testing.T) {
	c, seen := serve(t, 200, `{"signed_raw_tx":"0x0a0b","tx_hash":"0xAB","replacement_seq":2}`)
	raw, hash, err := c.ReplaceHotWallet(context.Background(), "gas-topup:1:0xh:0", 2)
	if err != nil || len(raw) != 2 || hash != "0xab" || (*seen)[0] != `POST /v1/sign/hot-wallet/gas-topup:1:0xh:0/replace {"replacement_seq":2}` {
		t.Fatalf("%x %s %v %q", raw, hash, err, *seen)
	}
	// 422 (the fee ceiling), 409 (a Nonce Filler, a seq gap) and 404 are refusals; 503 (a freeze) is transient.
	for code, permanent := range map[int]bool{422: true, 409: true, 404: true, 403: true, 503: false, 500: false} {
		c, _ := serve(t, code, `{}`)
		_, _, err := c.ReplaceHotWallet(context.Background(), "k", 1)
		if err == nil || errors.Is(err, ErrPermanent) != permanent {
			t.Errorf("%d: %v permanent=%v", code, err, permanent)
		}
	}
	if c, _ := serve(t, 200, `{"signed_raw_tx":"0x0a","tx_hash":"0xab","replacement_seq":9}`); true {
		if _, _, err := c.ReplaceHotWallet(context.Background(), "k", 1); err == nil {
			t.Fatal("accepted an answer for another replacement_seq")
		}
	}
}
