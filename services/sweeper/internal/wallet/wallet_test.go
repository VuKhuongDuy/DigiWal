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
	return &Client{BaseURL: srv.URL, HTTP: srv.Client()}, &seen
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
	c.BaseURL = "http://127.0.0.1:1"
	_, err := c.SignUserAddress(context.Background(), UserRequest{})
	if err == nil || errors.Is(err, ErrPermanent) || errors.Is(err, ErrBelowGas) {
		t.Fatalf("err = %v", err)
	}
}
