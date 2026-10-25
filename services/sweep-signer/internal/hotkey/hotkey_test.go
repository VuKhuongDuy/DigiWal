package hotkey

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

func gatewayAnswering(t *testing.T, body func() (int, string)) *Gateway {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/hot/key" || r.Method != "GET" {
			http.NotFound(w, r)
			return
		}
		code, b := body()
		w.WriteHeader(code)
		fmt.Fprint(w, b)
	}))
	t.Cleanup(ts.Close)
	return NewGateway(ts.URL, nil)
}

func keyJSON(t *testing.T, addrOf string, serving bool, chain int) string {
	t.Helper()
	k, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(k.PublicKey).Hex()
	if addrOf != "" {
		addr = addrOf
	}
	return fmt.Sprintf(`{"address":%q,"public_key":"0x%s","chain_id":%d,"serving":%v}`, addr, hex.EncodeToString(crypto.FromECDSAPub(&k.PublicKey)), chain, serving)
}

func TestGateway_PinsTheAddressTheGatewayReports(t *testing.T) {
	j := keyJSON(t, "", true, 31337)
	g := gatewayAnswering(t, func() (int, string) { return 200, j })
	p, err := g.Pin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.ChainID != 31337 || p.Address.Hex() == "0x0000000000000000000000000000000000000000" {
		t.Fatalf("pinned %+v", p)
	}
}

func TestGateway_RefusesAnythingButAConsistentServingAnswer(t *testing.T) {
	other, _ := crypto.GenerateKey()
	for name, c := range map[string]struct {
		code int
		body string
	}{
		"403":            {403, `{"error":"forbidden"}`},
		"not serving":    {200, keyJSON(t, "", false, 1)},
		"address != key": {200, keyJSON(t, crypto.PubkeyToAddress(other.PublicKey).Hex(), true, 1)},
		"no chain":       {200, keyJSON(t, "", true, 0)},
		"garbage":        {200, `{"address":"nope"}`},
		"not json":       {200, `<html>`},
		"bad pubkey":     {200, `{"address":"0x0000000000000000000000000000000000000001","public_key":"0x01","chain_id":1,"serving":true}`},
	} {
		c := c
		g := gatewayAnswering(t, func() (int, string) { return c.code, c.body })
		if _, err := g.Pin(context.Background()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPinWithRetry_GivesUpAfterTheWait(t *testing.T) {
	g := gatewayAnswering(t, func() (int, string) { return 503, "" })
	start := time.Now()
	if _, err := PinWithRetry(context.Background(), g, 100*time.Millisecond); err == nil {
		t.Fatal("pinned from a failing gateway")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("did not give up")
	}
}
