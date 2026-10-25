package cutover

import (
	"context"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLedgerClientPostsTheGasFeeShape(t *testing.T) {
	var body, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body, path = string(b), r.URL.Path
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := &LedgerClient{BaseURL: srv.URL + "/", HTTP: srv.Client()}
	g := GasFee{ChainID: 31337, TxHash: "0xab", GasUsed: 21000, GasPrice: big.NewInt(7), Purpose: GasPurpose}
	if err := c.PostGasFee(context.Background(), g); err != nil || path != "/v1/ledger/gas-fees" {
		t.Fatalf("%v %s", err, path)
	}
	for _, want := range []string{`"chain_id":31337`, `"gas_used":21000`, `"effective_gas_price":"7"`, `"purpose":"treasury_sweep"`} {
		if !strings.Contains(body, want) {
			t.Errorf("%s lacks %s", body, want)
		}
	}
	for _, code := range []int{400, 403, 409, 500} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		err := (&LedgerClient{BaseURL: s.URL, HTTP: s.Client()}).PostGasFee(context.Background(), g)
		s.Close()
		if err == nil {
			t.Errorf("%d must be an error: the tool must not mark gas posted", code)
		}
	}
}
