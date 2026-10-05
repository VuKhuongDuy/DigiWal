package walletclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	hot  = "0x3d0290A916B57AE48a2840E7d3547fA317F976c3"
	warm = "0xa0Ee7A142d267C1f36714E4a8F75612F20a79720"
	cold = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
)

func serve(t *testing.T, code int, body string) *Client {
	t.Helper()
	var gotPath string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(code)
		w.Write([]byte(body))
	}))
	t.Cleanup(func() {
		srv.Close()
		if gotPath != "" && gotPath != "/v1/wallet/system-addresses" {
			t.Errorf("called %s; system-addresses is the only endpoint the auditor may use", gotPath)
		}
	})
	return &Client{BaseURL: srv.URL, HTTP: srv.Client()}
}

func TestSystemAddresses(t *testing.T) {
	a, err := serve(t, 200, `{"chain_id":31337,"hot":"`+hot+`","warm":"`+warm+`"}`).SystemAddresses(context.Background())
	if err != nil || a.ChainID != 31337 || a.Hot != "0x3d0290a916b57ae48a2840e7d3547fa317f976c3" || a.Cold != "" {
		t.Fatalf("without cold: %+v %v", a, err)
	}
	a, err = serve(t, 200, `{"chain_id":31337,"hot":"`+hot+`","warm":"`+warm+`","cold":"`+cold+`"}`).SystemAddresses(context.Background())
	if err != nil || a.Cold != "0x70997970c51812dc3a010c7d01b50e0d17dc79c8" {
		t.Fatalf("with cold: %+v %v", a, err)
	}
}

func TestSystemAddressesRefusesAnythingUnusable(t *testing.T) {
	for name, c := range map[string]*Client{
		"503":          serve(t, 503, `{"error":"hot wallet not configured"}`),
		"403":          serve(t, 403, ``),
		"not json":     serve(t, 200, `nope`),
		"no hot":       serve(t, 200, `{"chain_id":1,"warm":"`+warm+`"}`),
		"zero warm":    serve(t, 200, `{"chain_id":1,"hot":"`+hot+`","warm":"0x0000000000000000000000000000000000000000"}`),
		"bad cold":     serve(t, 200, `{"chain_id":1,"hot":"`+hot+`","warm":"`+warm+`","cold":"0x12"}`),
		"hot == warm":  serve(t, 200, `{"chain_id":1,"hot":"`+hot+`","warm":"`+hot+`"}`),
		"cold == warm": serve(t, 200, `{"chain_id":1,"hot":"`+hot+`","warm":"`+warm+`","cold":"`+warm+`"}`),
	} {
		if _, err := c.SystemAddresses(context.Background()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFetchRetries(t *testing.T) {
	n := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n++; n < 3 {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte(`{"chain_id":31337,"hot":"` + hot + `","warm":"` + warm + `"}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if a, err := c.Fetch(context.Background(), 5, time.Millisecond, func(string, ...any) {}); err != nil || a.ChainID != 31337 || n != 3 {
		t.Fatalf("%+v %v after %d calls", a, err, n)
	}
}

func TestTLSConfigNeedsRealFiles(t *testing.T) {
	d := t.TempDir()
	if _, err := TLSConfig(filepath.Join(d, "c.pem"), filepath.Join(d, "k.pem"), filepath.Join(d, "ca.pem")); err == nil {
		t.Error("missing files accepted")
	}
	os.WriteFile(filepath.Join(d, "ca.pem"), []byte("not pem"), 0o600)
	if _, err := TLSConfig(filepath.Join(d, "c.pem"), filepath.Join(d, "k.pem"), filepath.Join(d, "ca.pem")); err == nil {
		t.Error("garbage accepted")
	}
}
