package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A user id is one path segment, whatever it contains: it can never add path or query to the request.
func TestLedgerClientEscapesTheUserID(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.URL.EscapedPath() + "?" + r.URL.RawQuery }))
	defer srv.Close()
	c := &LedgerClient{BaseURL: srv.URL, HTTP: srv.Client()}
	if _, _, err := c.Balances(context.Background(), "a/../b?asset_id=x", ""); err != nil {
		t.Fatal(err)
	}
	if got != "/v1/ledger/balances/a%2F..%2Fb%3Fasset_id=x?" {
		t.Fatalf("request = %q", got)
	}
}
