package authz

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

func (ca *testCA) leaf(t *testing.T, cn string) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		DNSNames:  []string{cn, "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// newServer starts an mTLS server (same tls settings as ServerTLSConfig)
// with the two sign endpoints stubbed behind their real policies.
func newServer(t *testing.T, ca *testCA) *httptest.Server {
	t.Helper()
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux := http.NewServeMux()
	mux.Handle("POST /v1/sign/hot-wallet", Require(HotWalletSigners, ok))
	mux.Handle("POST /v1/sign/user-address", Require(UserAddressSigners, ok))
	mux.Handle("POST /v1/sign/hot-wallet/{key}/void", Require(HotWalletVoiders, ok))
	mux.Handle("POST /v1/sign/hot-wallet/{key}/replace", Require(HotWalletKeyReaders, ok))
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{ca.leaf(t, "wallet-service")},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.pool,
		MinVersion:   tls.VersionTLS13,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func post(srv *httptest.Server, serverCA *testCA, clientCert *tls.Certificate, path string) (int, error) {
	cfg := &tls.Config{RootCAs: serverCA.pool, ServerName: "localhost", MinVersion: tls.VersionTLS13}
	if clientCert != nil {
		cfg.Certificates = []tls.Certificate{*clientCert}
	}
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
	resp, err := c.Post(srv.URL+path, "application/json", nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func TestCNAuthorizationMatrix(t *testing.T) {
	ca := newCA(t, "digiwal-internal-ca")
	srv := newServer(t, ca)

	cases := []struct {
		cn, path string
		want     int
	}{
		{"withdrawal-service", "/v1/sign/hot-wallet", 200},
		{"sweeper", "/v1/sign/hot-wallet", 200},
		{"sweeper", "/v1/sign/user-address", 200},
		// invariant #4: a compromised withdrawal-service can never pick a user key
		{"withdrawal-service", "/v1/sign/user-address", 403},
		{"api-gateway", "/v1/sign/hot-wallet", 403},
		{"api-gateway", "/v1/sign/user-address", 403},
		// invariants #21, #22: void is withdrawal-service's alone; replace is also sweeper's (its
		// Gas Top-Ups); nobody else touches either
		{"withdrawal-service", "/v1/sign/hot-wallet/k/void", 200},
		{"sweeper", "/v1/sign/hot-wallet/k/void", 403},
		{"chain-watcher", "/v1/sign/hot-wallet/k/void", 403},
		{"api-gateway", "/v1/sign/hot-wallet/k/void", 403},
		{"withdrawal-service", "/v1/sign/hot-wallet/k/replace", 200},
		{"sweeper", "/v1/sign/hot-wallet/k/replace", 200},
		{"chain-watcher", "/v1/sign/hot-wallet/k/replace", 403},
		{"api-gateway", "/v1/sign/hot-wallet/k/replace", 403},
	}
	for _, tc := range cases {
		cert := ca.leaf(t, tc.cn)
		got, err := post(srv, ca, &cert, tc.path)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.cn, tc.path, err)
		}
		if got != tc.want {
			t.Errorf("CN=%s %s: got %d, want %d", tc.cn, tc.path, got, tc.want)
		}
	}
}

func TestHandshakeRejectsUnknownOrMissingClientCert(t *testing.T) {
	ca := newCA(t, "digiwal-internal-ca")
	srv := newServer(t, ca)

	// Right CN, but signed by a different CA: must fail at the TLS layer.
	rogue := newCA(t, "rogue-ca")
	forged := rogue.leaf(t, "sweeper")
	if code, err := post(srv, ca, &forged, "/v1/sign/user-address"); err == nil {
		t.Fatalf("forged cert accepted, status %d", code)
	}
	// No client cert at all.
	if code, err := post(srv, ca, nil, "/v1/sign/hot-wallet"); err == nil {
		t.Fatalf("anonymous client accepted, status %d", code)
	}
}

func TestRequireDeniesWithoutTLS(t *testing.T) {
	h := Require([]string{"sweeper"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not run")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
}
