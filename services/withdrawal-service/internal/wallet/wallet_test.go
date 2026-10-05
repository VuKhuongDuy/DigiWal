package wallet

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"digiwal/services/withdrawal-service/internal/validate"
)

var (
	hot     = common.HexToAddress("0x3d0290A916B57AE48a2840E7d3547fA317F976c3")
	warm    = common.HexToAddress("0xa0Ee7A142d267C1f36714E4a8F75612F20a79720")
	deposit = common.HexToAddress("0x8f4e15D37Be2B4D3539A4f14b2450f5bE0d884e4")
	cold    = common.HexToAddress("0x14dC79964da2C08b23698B3D3cc7Ca32193d9955") // not listed anywhere
)

func serve(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL + "/", HTTP: srv.Client()}
}

// Cold is optional: absent means no Cold Wallet is declared, and the answer parses as before.
func TestSystemAddressesWithAndWithoutCold(t *testing.T) {
	cold := common.HexToAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"chain_id":31337,"cold":"` + cold.Hex() + `","hot":"` + hot.Hex() + `","warm":"` + warm.Hex() + `"}`))
	})
	if got, err := c.SystemAddresses(context.Background()); err != nil || got.Cold != cold || got.Hot != hot || got.Warm != warm {
		t.Fatalf("with cold: %+v, %v", got, err)
	}
	c = serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"chain_id":31337,"hot":"` + hot.Hex() + `","warm":"` + warm.Hex() + `"}`))
	})
	if got, err := c.SystemAddresses(context.Background()); err != nil || got.Cold != (common.Address{}) {
		t.Fatalf("without cold: %+v, %v", got, err)
	}
}

func TestSystemAddresses(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/wallet/system-addresses" {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"chain_id":31337,"hot":"` + hot.Hex() + `","warm":"` + strings.ToLower(warm.Hex()) + `"}`))
	})
	got, err := c.SystemAddresses(context.Background())
	if err != nil || got.ChainID != 31337 || got.Hot != hot || got.Warm != warm {
		t.Fatalf("got %+v, %v", got, err)
	}
	// Never start with a partial or nonsensical answer.
	for name, resp := range map[string]struct {
		code int
		body string
	}{
		"503":             {503, `{"error":"hot wallet not configured"}`},
		"403":             {403, `{"error":"forbidden"}`},
		"garbage":         {200, `{"hot":"nope","warm":"nope"}`},
		"zero warm":       {200, `{"hot":"` + hot.Hex() + `","warm":"0x0000000000000000000000000000000000000000"}`},
		"hot equals warm": {200, `{"hot":"` + hot.Hex() + `","warm":"` + hot.Hex() + `"}`},
		"bad cold":        {200, `{"hot":"` + hot.Hex() + `","warm":"` + warm.Hex() + `","cold":"0x12"}`},
		"zero cold":       {200, `{"hot":"` + hot.Hex() + `","warm":"` + warm.Hex() + `","cold":"0x0000000000000000000000000000000000000000"}`},
		"cold equals hot": {200, `{"hot":"` + hot.Hex() + `","warm":"` + warm.Hex() + `","cold":"` + hot.Hex() + `"}`},
	} {
		c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(resp.code)
			_, _ = w.Write([]byte(resp.body))
		})
		if a, err := c.SystemAddresses(context.Background()); err == nil {
			t.Errorf("%s: accepted %+v", name, a)
		}
	}
}

// 404 is the only "not ours"; every other outcome is ErrUnknown, never false.
func TestIsDepositAddress(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/addresses/" + deposit.Hex():
			_, _ = w.Write([]byte(`{"user_id":"alice","chain":"evm"}`))
		case "/v1/addresses/" + cold.Hex():
			w.WriteHeader(404)
		default:
			w.WriteHeader(500)
		}
	})
	if ours, err := c.IsDepositAddress(context.Background(), deposit); err != nil || !ours {
		t.Fatalf("deposit: %v %v", ours, err)
	}
	if ours, err := c.IsDepositAddress(context.Background(), cold); err != nil || ours {
		t.Fatalf("not ours: %v %v", ours, err)
	}
	if _, err := c.IsDepositAddress(context.Background(), hot); !errors.Is(err, ErrUnknown) {
		t.Fatalf("5xx: %v, want ErrUnknown", err)
	}
	down := &Client{BaseURL: "http://127.0.0.1:1", HTTP: http.DefaultClient}
	if _, err := down.IsDepositAddress(context.Background(), cold); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unreachable: %v, want ErrUnknown", err)
	}
}

type fakeLookup struct {
	ours  map[common.Address]bool
	err   error
	calls int
}

func (f *fakeLookup) IsDepositAddress(_ context.Context, a common.Address) (bool, error) {
	f.calls++
	return f.ours[a], f.err
}

func TestDestinations(t *testing.T) {
	lookup := &fakeLookup{ours: map[common.Address]bool{deposit: true}}
	d := &Destinations{System: SystemAddresses{Hot: hot, Warm: warm}, Deposits: lookup}
	for addr, want := range map[common.Address]string{
		hot: validate.DestinationIsSystem, warm: validate.DestinationIsSystem,
	} {
		if got, err := d.Refuse(context.Background(), addr); err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", addr.Hex(), got, err, want)
		}
	}
	if lookup.calls != 0 {
		t.Fatalf("the remote lookup ran %d times for Hot/Warm; the local check must decide first", lookup.calls)
	}
	if got, err := d.Refuse(context.Background(), deposit); err != nil || got != validate.DestinationIsDeposit {
		t.Fatalf("deposit: %q %v", got, err)
	}
	// Cold (or any outside address) is allowed.
	if got, err := d.Refuse(context.Background(), cold); err != nil || got != "" {
		t.Fatalf("cold: %q %v, want allowed", got, err)
	}
	lookup.err = ErrUnknown
	if got, err := d.Refuse(context.Background(), cold); err == nil || got != "" {
		t.Fatalf("lookup failed: %q %v, want an error (fail closed)", got, err)
	}
}

func TestVoidHotWallet(t *testing.T) {
	var path, method string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		path, method = r.URL.Path, r.Method
		_, _ = w.Write([]byte(`{"status":"voided"}`))
	})
	got, err := c.VoidHotWallet(context.Background(), "withdrawal:abc")
	if err != nil || !got.Voided || method != "POST" || path != "/v1/sign/hot-wallet/withdrawal:abc/void" {
		t.Fatalf("%+v %v %s %s", got, err, method, path)
	}
	c = serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"already_signed","nonce":7,"signed_raw_tx":"0xabcd","tx_hash":"0x01"}`))
	})
	got, err = c.VoidHotWallet(context.Background(), "k")
	if err != nil || got.Voided || got.Nonce != 7 || got.RawTx != "0xabcd" {
		t.Fatalf("%+v %v", got, err)
	}
	// Anything but a clean answer is NOT "voided": the caller must not Release on it.
	for name, resp := range map[string]struct {
		code int
		body string
	}{"503": {503, `{}`}, "403": {403, `{}`}, "409": {409, `{}`}, "garbage": {200, `nope`}, "unknown status": {200, `{"status":"maybe"}`},
		"signed without a tx": {200, `{"status":"already_signed"}`}} {
		c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(resp.code)
			_, _ = w.Write([]byte(resp.body))
		})
		if r, err := c.VoidHotWallet(context.Background(), "k"); err == nil {
			t.Errorf("%s: accepted %+v", name, r)
		}
	}
}

func TestReplaceHotWallet(t *testing.T) {
	var body string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 200)
		n, _ := r.Body.Read(b)
		body = string(b[:n])
		if r.URL.Path != "/v1/sign/hot-wallet/withdrawal:abc/replace" {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"signed_raw_tx":"0xbeef","tx_hash":"0xAB","nonce":3,"replacement_seq":2}`))
	})
	got, err := c.ReplaceHotWallet(context.Background(), "withdrawal:abc", 2)
	if err != nil || got.RawTx != "0xbeef" || got.Hash != "0xab" || got.Nonce != 3 || got.Seq != 2 || body != `{"replacement_seq":2}` {
		t.Fatalf("%+v %v %q", got, err, body)
	}
	for name, tc := range map[string]struct {
		code int
		want error
	}{"422": {422, ErrReplaceCeiling}, "409": {409, ErrReplaceRefused}, "404": {404, ErrReplaceRefused}} {
		c := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.code) })
		if _, err := c.ReplaceHotWallet(context.Background(), "k", 1); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	// 503 (a Nonce Gap freeze) and a wrong seq in the answer are transient/unusable, never "refused".
	c = serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
	if _, err := c.ReplaceHotWallet(context.Background(), "k", 1); err == nil || errors.Is(err, ErrReplaceRefused) || errors.Is(err, ErrReplaceCeiling) {
		t.Fatalf("503: %v", err)
	}
	c = serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"signed_raw_tx":"0xbeef","tx_hash":"0xab","nonce":3,"replacement_seq":9}`))
	})
	if _, err := c.ReplaceHotWallet(context.Background(), "k", 1); err == nil {
		t.Fatal("accepted an answer for another replacement_seq")
	}
}

func TestSignOfAVoidedKeyIsErrKeyVoided(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(410) })
	if _, _, err := c.SignHotWallet(context.Background(), SignRequest{IdempotencyKey: "k"}); !errors.Is(err, ErrKeyVoided) {
		t.Fatalf("%v", err)
	}
}

func TestCancelHashes(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sign/hot-wallet/withdrawal:abc" || r.Method != "GET" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"nonce":3,"replacements":[{"kind":"fee_bump","tx_hash":"0x01"},{"kind":"cancel","tx_hash":"0xAB"}]}`))
	})
	got, err := c.CancelHashes(context.Background(), "withdrawal:abc")
	if err != nil || len(got) != 1 || got[0] != "0xab" {
		t.Fatalf("%v %v", got, err)
	}
	// 404 = never signed = no filler; any other failure is an error (the Reaper must not guess).
	c = serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
	if got, err := c.CancelHashes(context.Background(), "k"); err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
	c = serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
	if _, err := c.CancelHashes(context.Background(), "k"); err == nil {
		t.Fatal("a 503 must not read as 'no filler'")
	}
}
