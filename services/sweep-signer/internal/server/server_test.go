package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"digiwal/services/sweep-signer/internal/authz"
	"digiwal/services/sweep-signer/internal/sweep"
	"digiwal/services/sweep-signer/internal/vault"
)

type fakeSigner struct {
	err   error
	calls int
}

func (f *fakeSigner) Sign(context.Context, sweep.Request) (sweep.Response, error) {
	f.calls++
	if f.err != nil {
		return sweep.Response{}, f.err
	}
	return sweep.Response{SignedRawTx: "0x02"}, nil
}

func TestStatusMapping_IsTheContractSweeperHasAlwaysSeen(t *testing.T) {
	cases := []struct {
		err  error
		code int
	}{
		{nil, 200},
		{sweep.ErrDestinationNotHotWallet, 403},
		{fmt.Errorf("%w: x", sweep.ErrAddressMismatch), 403},
		{sweep.ErrNotASweep, 403},
		{sweep.ErrUnknownUser, 404},
		{sweep.ErrBalanceBelowGas, 422},
		{fmt.Errorf("%w: bad", sweep.ErrInvalidRequest), 400},
		{sweep.ErrUnknownAsset, 400},
		{sweep.ErrUnsupportedChain, 400},
		{fmt.Errorf("%w: pass pending", sweep.ErrFrozen), 503},
		{fmt.Errorf("%w: 122 > 50", sweep.ErrFeeCeiling), 503},
		{fmt.Errorf("%w: db", vault.ErrSeedUnavailable), 503},
		{errors.New("boom"), 500},
	}
	for _, c := range cases {
		h := New(&fakeSigner{err: c.err}, nil).Handler()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/sign/user-address", strings.NewReader(`{"user_id":"a"}`)))
		if rec.Code != c.code {
			t.Errorf("%v: got %d, want %d (%s)", c.err, rec.Code, c.code, rec.Body)
		}
		if c.code == 500 && strings.Contains(rec.Body.String(), "boom") {
			t.Error("an internal error leaked its text")
		}
		if c.code != 200 && strings.Contains(rec.Body.String(), "122 > 50") {
			t.Error("refusal detail leaked to the caller")
		}
	}
	rec := httptest.NewRecorder()
	New(&fakeSigner{}, nil).Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/sign/user-address", strings.NewReader(`{"user_id":"a","unknown":1}`)))
	if rec.Code != 400 {
		t.Errorf("unknown JSON field: %d", rec.Code)
	}
}

// ---- mTLS: CN allow-list = sweeper, plus the pinned leaf --------------------------------------------------------------

type pki struct {
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caFile string
	dir    string
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	ca, _ := x509.ParseCertificate(der)
	dir := t.TempDir()
	p := &pki{ca: ca, caKey: k, dir: dir, caFile: filepath.Join(dir, "ca.pem")}
	_ = os.WriteFile(p.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	return p
}

func (p *pki) leaf(t *testing.T, cn string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) tls.Certificate {
	t.Helper()
	if parent == nil {
		parent, parentKey = p.ca, p.caKey
	}
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn}, DNSNames: []string{cn, "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, parent, &k.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
}

func writeLeaf(t *testing.T, dir, name string, c tls.Certificate) (certFile, keyFile string) {
	t.Helper()
	certFile, keyFile = filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	_ = os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[0]}), 0o600)
	kb, _ := x509.MarshalECPrivateKey(c.PrivateKey.(*ecdsa.PrivateKey))
	_ = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return
}

func TestMTLS_OnlyThePinnedSweeperLeafReachesTheEndpoint(t *testing.T) {
	p := newPKI(t)
	serverCert := p.leaf(t, "sweep-signer", nil, nil)
	sweeper := p.leaf(t, "sweeper", nil, nil)
	walletSvc := p.leaf(t, "wallet-service", nil, nil)
	unpinnedSweeper := p.leaf(t, "sweeper", nil, nil) // a second, valid, CA-signed leaf with CN sweeper that is NOT pinned

	// a "CA" the attacker made up, signing a leaf with CN sweeper
	rk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rtpl := &x509.Certificate{SerialNumber: big.NewInt(9), Subject: pkix.Name{CommonName: "rogue"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rder, _ := x509.CreateCertificate(rand.Reader, rtpl, rtpl, &rk.PublicKey, rk)
	rogueCA, _ := x509.ParseCertificate(rder)
	rogue := p.leaf(t, "sweeper", rogueCA, rk)

	sc, _ := x509.ParseCertificate(sweeper.Certificate[0])
	wc, _ := x509.ParseCertificate(walletSvc.Certificate[0])
	// the callers file pins sweeper AND wallet-service, so the wallet-service case proves the CN allow-list, not just the pin
	callers := filepath.Join(p.dir, "callers.yaml")
	_ = os.WriteFile(callers, []byte(fmt.Sprintf("callers:\n  - cn: sweeper\n    sha256: %q\n  - cn: wallet-service\n    sha256: %q\n",
		authz.Fingerprint(sc.Raw), authz.Fingerprint(wc.Raw))), 0o600)
	pins, err := authz.LoadPins(callers)
	if err != nil {
		t.Fatal(err)
	}
	crt, key := writeLeaf(t, p.dir, "server", serverCert)
	tlsCfg, err := authz.ServerTLSConfig(crt, key, p.caFile, pins)
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeSigner{}
	ts := httptest.NewUnstartedServer(New(fs, pins).MTLSRoutes())
	ts.TLS = tlsCfg
	ts.StartTLS()
	defer ts.Close()

	pool := x509.NewCertPool()
	pool.AddCert(p.ca)
	call := func(c tls.Certificate, method, path string) (int, error) {
		cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{c}, RootCAs: pool, ServerName: "sweep-signer", MinVersion: tls.VersionTLS13}}}
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(`{"user_id":"alice"}`))
		resp, err := cl.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}

	if code, err := call(sweeper, "POST", "/v1/sign/user-address"); err != nil || code != 200 {
		t.Fatalf("the pinned sweeper leaf: %d %v", code, err)
	}
	if code, err := call(walletSvc, "POST", "/v1/sign/user-address"); err != nil || code != 403 {
		t.Fatalf("a pinned leaf with another CN: %d %v (want 403)", code, err)
	}
	for name, c := range map[string]tls.Certificate{"unpinned sweeper leaf": unpinnedSweeper, "leaf of a rogue CA": rogue} {
		if code, err := call(c, "POST", "/v1/sign/user-address"); err == nil {
			t.Errorf("%s was accepted (%d)", name, code)
		}
	}
	if code, err := call(sweeper, "GET", "/v1/sign/user-address"); err != nil || code == 200 {
		t.Errorf("GET on the signing path: %d %v", code, err)
	}
	// no other operation exists
	for _, path := range []string{"/v1/sign/hot-wallet", "/v1/wallet/hot-address", "/v1/sign/user-address/x", "/metrics", "/v1/seed"} {
		if code, err := call(sweeper, "POST", path); err != nil || code != 404 {
			t.Errorf("POST %s: %d %v (want 404)", path, code, err)
		}
	}
	if fs.calls != 1 {
		t.Fatalf("the signer was called %d times, want exactly 1 (the pinned sweeper's)", fs.calls)
	}

	// no client certificate at all
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "sweep-signer", MinVersion: tls.VersionTLS13}}}
	if resp, err := cl.Post(ts.URL+"/v1/sign/user-address", "application/json", strings.NewReader("{}")); err == nil {
		resp.Body.Close()
		t.Error("a connection without a client certificate was accepted")
	}
}

func TestPlainListenerServesOnlyHealthz(t *testing.T) {
	h := New(&fakeSigner{}, nil).PlainRoutes()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthz %d", rec.Code)
	}
	for _, path := range []string{"/v1/sign/user-address", "/metrics"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader("{}")))
		if rec.Code == 200 {
			t.Errorf("the plain listener answers %s", path)
		}
	}
}
