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
