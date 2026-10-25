package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/prometheus/client_golang/prometheus"

	"digiwal/services/hsm-gateway/internal/audit"
	"digiwal/services/hsm-gateway/internal/authz"
	"digiwal/services/hsm-gateway/internal/gateway"
	"digiwal/services/hsm-gateway/internal/hsm"
	"digiwal/services/hsm-gateway/internal/policy"
)

// softBackend: TEST-ONLY token stand-in (see gateway_test.go).
type softBackend struct {
	key      *ecdsa.PrivateKey
	fail     error
	att      *ecdsa.PrivateKey // P-256 attestation key stand-in; nil = token without one
	attSigns atomic.Int64
}

func (s *softBackend) HasAttestKey() bool { return s.att != nil }
func (s *softBackend) AttestPublicKey() []byte {
	if s.att == nil {
		return nil
	}
	return elliptic.Marshal(elliptic.P256(), s.att.X, s.att.Y) //nolint:staticcheck
}
func (s *softBackend) CheckAttestKey(context.Context) error { return nil }
func (s *softBackend) SignAttestation(_ context.Context, d []byte) ([]byte, error) {
	s.attSigns.Add(1)
	if s.fail != nil {
		return nil, s.fail
	}
	r, sv, err := ecdsa.Sign(rand.Reader, s.att, d)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	sv.FillBytes(out[32:])
	return out, nil
}

func (s *softBackend) Address() common.Address        { return crypto.PubkeyToAddress(s.key.PublicKey) }
func (s *softBackend) PublicKey() []byte              { return crypto.FromECDSAPub(&s.key.PublicKey) }
func (s *softBackend) Close()                         {}
func (s *softBackend) CheckKey(context.Context) error { return nil }
func (s *softBackend) SignDigest(_ context.Context, d []byte) ([]byte, error) {
	if s.fail != nil {
		return nil, s.fail
	}
	r, sv, err := ecdsa.Sign(rand.Reader, s.key, d)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	sv.FillBytes(out[32:])
	return out, nil
}

type pki struct {
	caPEM  []byte
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	serial int64
}

func newPKI(t *testing.T) *pki {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	c, _ := x509.ParseCertificate(der)
	return &pki{ca: c, caKey: k, caPEM: pemOf("CERTIFICATE", der), serial: 1}
}

func (p *pki) leaf(t *testing.T, cn string, server bool) (tls.Certificate, *x509.Certificate) {
	p.serial++
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(p.serial), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, p.ca, &k.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k, Leaf: leaf}, leaf
}

func pemOf(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

type env struct {
	url   string
	pki   *pki
	pins  *authz.Pins
	svc   *gateway.Service
	be    *softBackend
	hot   common.Address
	dir   string
	logP  string
	certs map[string]tls.Certificate
}

func setupWithoutAttest(t *testing.T) *env { return setupOpts(t, false) }

func setup(t *testing.T, extraPinned ...string) *env {
	t.Helper()
	return setupOpts(t, true, extraPinned...)
}

func setupOpts(t *testing.T, withAttest bool, extraPinned ...string) *env {
	t.Helper()
	return setupWith(t, withAttest, nil, extraPinned...)
}

// setupWith is setupOpts with a hook that configures the service (vault, ...) before the listener starts.
func setupWith(t *testing.T, withAttest bool, pre func(*gateway.Service), extraPinned ...string) *env {
	t.Helper()
	p := newPKI(t)
	be := &softBackend{}
	be.key, _ = crypto.GenerateKey()
	if withAttest {
		be.att, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	hot := be.Address()
	pol, err := policy.Parse([]byte(fmt.Sprintf(`
enabled: true
chain_id: 1337
hot_address: "%s"
native:
  max_per_tx: "1000000000000000000"
  treasury_max_per_tx: "1"
  window_1h: "5000000000000000000"
  window_24h: "5000000000000000000"
`, hot.Hex())))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logP := filepath.Join(dir, "audit.log")
	l, op, err := audit.Open(logP)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	svc, err := gateway.New(pol, l, op.Entries, be, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if withAttest {
		svc.WithAttestor(be)
	}
	if pre != nil {
		pre(svc)
	}
	srvCert, srvLeaf := p.leaf(t, "hsm-gateway", true)
	certs := map[string]tls.Certificate{}
	var pinned []*x509.Certificate
	for _, cn := range append([]string{authz.CNWalletService, authz.CNSweepSigner, authz.CNAuditor, authz.CNKeyAdmin}, extraPinned...) {
		c, leaf := p.leaf(t, cn, false)
		certs[cn] = c
		pinned = append(pinned, leaf)
	}
	pins := authz.NewPins(pinned...)
	_ = srvLeaf
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(p.caPEM)
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{srvCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool,
		MinVersion: tls.VersionTLS13, SessionTicketsDisabled: true,
		VerifyConnection: func(cs tls.ConnectionState) error { _, err := pins.Check(cs.PeerCertificates[0]); return err }}
	ts := httptest.NewUnstartedServer(New(svc, pins).MTLSRoutes())
	ts.TLS = tlsCfg
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return &env{url: ts.URL, pki: p, pins: pins, svc: svc, be: be, hot: hot, dir: dir, logP: logP, certs: certs}
}

func (e *env) client(cert tls.Certificate) *http.Client {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(e.pki.caPEM)
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13, ServerName: "localhost"}}}
}

func (e *env) do(c *http.Client, method, path, body string) (int, map[string]any, error) {
	req, _ := http.NewRequest(method, e.url+path, strings.NewReader(body))
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return resp.StatusCode, m, nil
}

func body(nonce int, extra string) string {
	return fmt.Sprintf(`{"chain_id":1337,"nonce":%d,"to":"0x5555555555555555555555555555555555555555","value":"1000","data":"0x","gas":21000,`+
		`"max_fee_per_gas":"1000000000","max_priority_fee_per_gas":"1000000000","operation":"payout","idempotency_key":"k%d"%s}`, nonce, nonce, extra)
}

func TestSign_Contract(t *testing.T) {
	e := setup(t)
	wallet := e.client(e.certs[authz.CNWalletService])

	code, m, err := e.do(wallet, "POST", "/v1/hot/sign", body(0, ""))
	if err != nil || code != 200 {
		t.Fatalf("code %d err %v body %v", code, err, m)
	}
	for _, k := range []string{"signed_raw_tx", "tx_hash", "from", "audit_seq", "policy_hash"} {
		if _, ok := m[k]; !ok {
			t.Errorf("200 response lacks %q: %v", k, m)
		}
	}
	if !strings.EqualFold(m["from"].(string), e.hot.Hex()) {
		t.Errorf("from %v", m["from"])
	}

	// refusal: 403 policy_refused with the rule id
	bad := strings.Replace(body(1, ""), `"value":"1000"`, `"value":"9999999999999999999"`, 1)
	code, m, _ = e.do(wallet, "POST", "/v1/hot/sign", bad)
	if code != 403 || m["error"] != "policy_refused" || m["rule"] != policy.RuleMaxPerTx {
		t.Errorf("refusal: %d %v", code, m)
	}
	// approve calldata
	approve := strings.Replace(body(2, ""), `"data":"0x"`, `"data":"0x095ea7b3`+strings.Repeat("00", 64)+`"`, 1)
	if code, m, _ = e.do(wallet, "POST", "/v1/hot/sign", approve); code != 403 || m["rule"] != policy.RuleApproveRefused {
		t.Errorf("approve: %d %v", code, m)
	}
	// malformed / forbidden fields are 400, never signed
	for name, b := range map[string]string{
		"caller digest": body(3, `,"digest":"0x`+strings.Repeat("11", 32)+`"`),
		"unknown field": body(3, `,"foo":1`),
		"not json":      `{`,
		"empty to":      strings.Replace(body(3, ""), `"to":"0x5555555555555555555555555555555555555555"`, `"to":""`, 1),
	} {
		if code, m, _ = e.do(wallet, "POST", "/v1/hot/sign", b); code != 400 || m["error"] != "bad_request" {
			t.Errorf("%s: %d %v", name, code, m)
		}
	}
	// HSM failure: 503 with an explicit class, nothing returned
	e.be.fail = &hsm.Error{Code: hsm.CodeUnavailable, Op: "sign"}
	if code, m, _ = e.do(wallet, "POST", "/v1/hot/sign", body(4, "")); code != 503 || m["error"] != hsm.CodeUnavailable || m["signed_raw_tx"] != nil {
		t.Errorf("hsm failure: %d %v", code, m)
	}
	e.be.fail = nil

	// GET /v1/hot/key
	code, m, _ = e.do(wallet, "GET", "/v1/hot/key", "")
	if code != 200 || !strings.EqualFold(m["address"].(string), e.hot.Hex()) || !strings.HasPrefix(m["public_key"].(string), "0x04") || m["chain_id"].(float64) != 1337 {
		t.Errorf("key: %d %v", code, m)
	}
	if m["serving"] != true { // the Hot cutover tool refuses a gateway that does not say it serves
		t.Errorf("key.serving = %v, want true", m["serving"])
	}
	// GET /v1/audit/head (auditor)
	code, m, _ = e.do(e.client(e.certs[authz.CNAuditor]), "GET", "/v1/audit/head", "")
	if code != 200 || m["seq"].(float64) < 1 || len(m["hash"].(string)) != 64 {
		t.Errorf("head: %d %v", code, m)
	}
}

func TestAuthz_CNAllowListPerEndpoint(t *testing.T) {
	e := setup(t)
	for _, c := range []struct {
		cn, method, path string
		want             int
	}{
		{authz.CNWalletService, "POST", "/v1/hot/sign", 200},
		{authz.CNSweepSigner, "POST", "/v1/hot/sign", 403}, // sweep-signer may read the key, never sign Hot
		{authz.CNAuditor, "POST", "/v1/hot/sign", 403},
		{authz.CNKeyAdmin, "POST", "/v1/hot/sign", 403},
		{authz.CNWalletService, "GET", "/v1/hot/key", 200},
		{authz.CNSweepSigner, "GET", "/v1/hot/key", 200},
		{authz.CNAuditor, "GET", "/v1/hot/key", 403},
		{authz.CNAuditor, "GET", "/v1/audit/head", 200},
		{authz.CNKeyAdmin, "GET", "/v1/audit/head", 200},
		{authz.CNWalletService, "GET", "/v1/audit/head", 403},
		{authz.CNSweepSigner, "GET", "/v1/audit/head", 403},
	} {
		b := ""
		if c.method == "POST" {
			b = body(int(time.Now().UnixNano()%1_000_000), "")
		}
		code, _, err := e.do(e.client(e.certs[c.cn]), c.method, c.path, b)
		if err != nil || code != c.want {
			t.Errorf("%s %s as %s: %d (%v), want %d", c.method, c.path, c.cn, code, err, c.want)
		}
	}
}

// K15: a certificate the project CA signed is not enough. A second wallet-service certificate (same CN,
// same CA, e.g. minted with a stolen CA key) is rejected in the handshake; a different CN is too.
func TestAuthz_FingerprintPinningRejectsCASignedButUnpinnedLeaves(t *testing.T) {
	e := setup(t)
	impostor, _ := e.pki.leaf(t, authz.CNWalletService, false)
	if code, _, err := e.do(e.client(impostor), "POST", "/v1/hot/sign", body(0, "")); err == nil {
		t.Fatalf("CA-signed, right CN, unpinned leaf was served (status %d)", code)
	}
	stranger, _ := e.pki.leaf(t, "some-other-service", false)
	if _, _, err := e.do(e.client(stranger), "GET", "/v1/hot/key", ""); err == nil {
		t.Fatal("CA-signed unknown CN was served")
	}
	// no client certificate at all
	noCert := e.client(tls.Certificate{})
	noCert.Transport.(*http.Transport).TLSClientConfig.Certificates = nil
	if _, _, err := e.do(noCert, "GET", "/v1/hot/key", ""); err == nil {
		t.Fatal("a client without a certificate was served")
	}
	// a certificate from a different CA
	other := newPKI(t)
	foreign, _ := other.leaf(t, authz.CNWalletService, false)
	if _, _, err := e.do(e.client(foreign), "GET", "/v1/hot/key", ""); err == nil {
		t.Fatal("a foreign-CA certificate was served")
	}
	// the genuine pinned leaf still works
	if code, _, err := e.do(e.client(e.certs[authz.CNWalletService]), "GET", "/v1/hot/key", ""); err != nil || code != 200 {
		t.Fatalf("pinned leaf: %d %v", code, err)
	}
	// the middleware pins independently of the handshake: a request carrying a verified but unpinned chain gets 403
	req := httptest.NewRequest("GET", "/v1/hot/key", nil)
	_, leaf := e.pki.leaf(t, authz.CNWalletService, false)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf}}}
	rec := httptest.NewRecorder()
	New(e.svc, e.pins).MTLSRoutes().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("middleware accepted an unpinned leaf: %d", rec.Code)
	}
}

// Invariant 3: no route accepts a digest, hash or signature.
func TestContract_NoRouteTakesADigest(t *testing.T) {
	forbidden := []string{"digest", "hash", "sighash", "signature", "sig", "raw", "raw_tx", "message", "r", "s", "v"}
	for route, typ := range RequestSchemas {
		for i := 0; i < typ.NumField(); i++ {
			name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
			for _, f := range forbidden {
				if strings.EqualFold(name, f) {
					t.Errorf("%s: request field %q looks like a caller-supplied digest", route, name)
				}
			}
		}
	}
	if _, ok := RequestSchemas["POST /v1/hot/sign"]; !ok {
		t.Fatal("missing schema for POST /v1/hot/sign")
	}
}

func TestPlain_HealthAndReadiness(t *testing.T) {
	e := setup(t)
	rec := httptest.NewRecorder()
	New(e.svc, e.pins).PlainRoutes().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	rec = httptest.NewRecorder()
	New(e.svc, e.pins).PlainRoutes().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	e.be.fail = &hsm.Error{Code: hsm.CodeUnavailable, Op: "sign"}
	if err := e.svc.SelfTest(context.Background()); err == nil {
		t.Fatal("self-test passed with a failing HSM")
	}
	rec = httptest.NewRecorder()
	New(e.svc, e.pins).PlainRoutes().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 503 {
		t.Fatalf("readyz after a failed self-test: %d", rec.Code)
	}
}
