package signing

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"digiwal/services/wallet-service/internal/storage"
	"digiwal/services/wallet-service/internal/testdb"
)

// fakeGateway speaks hsm-gateway's HTTP contract with a software key, so the client's behaviour (not the
// gateway's) is what the unit tests exercise. The real gateway is exercised by the container tests.
type fakeGateway struct {
	key     *ecdsa.PrivateKey
	signKey *ecdsa.PrivateKey // what actually signs (differs from key to simulate a swapped key)
	chain   uint64

	mu       sync.Mutex
	mode     string // "", "refuse", "down", "hang", "tamper", "forbidden", "badreq"
	rule     string
	seen     []gatewayWire
	signHits atomic.Int32
}

func newFakeGateway(t *testing.T) (*fakeGateway, *httptest.Server) {
	t.Helper()
	k, _ := crypto.GenerateKey()
	g := &fakeGateway{key: k, signKey: k, chain: chainID, rule: "max_per_tx"}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return g, srv
}

func (g *fakeGateway) setMode(m string) { g.mu.Lock(); g.mode = m; g.mu.Unlock() }
func (g *fakeGateway) operations() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, w := range g.seen {
		out = append(out, w.Operation)
	}
	return out
}
func (g *fakeGateway) address() common.Address { return crypto.PubkeyToAddress(g.key.PublicKey) }

func (g *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	js := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	if r.Method == "GET" && r.URL.Path == "/v1/hot/key" {
		js(200, map[string]any{"address": g.address().Hex(), "public_key": "0x" + hex.EncodeToString(crypto.FromECDSAPub(&g.key.PublicKey)),
			"chain_id": g.chain, "policy_hash": "x"})
		return
	}
	if r.Method != "POST" || r.URL.Path != "/v1/hot/sign" {
		js(404, nil)
		return
	}
	g.signHits.Add(1)
	g.mu.Lock()
	mode, rule := g.mode, g.rule
	g.mu.Unlock()
	var wire gatewayWire
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields() // exactly the gateway's wire: unknown fields are rejected there too
	if err := dec.Decode(&wire); err != nil {
		js(400, map[string]string{"error": "bad_request", "detail": err.Error()})
		return
	}
	g.mu.Lock()
	g.seen = append(g.seen, wire)
	g.mu.Unlock()
	switch mode {
	case "refuse":
		js(403, map[string]string{"error": "policy_refused", "rule": rule})
		return
	case "forbidden":
		js(403, map[string]string{"error": "forbidden"})
		return
	case "badreq":
		js(400, map[string]string{"error": "bad_request", "detail": "nope"})
		return
	case "down":
		js(503, map[string]string{"error": "hsm_unavailable"})
		return
	case "hang":
		time.Sleep(2 * time.Second)
		return
	}
	value, _ := new(big.Int).SetString(wire.Value, 10)
	fee, _ := new(big.Int).SetString(wire.MaxFeePerGas, 10)
	tip, _ := new(big.Int).SetString(wire.MaxPriorityFeePerGas, 10)
	to := common.HexToAddress(wire.To)
	if mode == "tamper" {
		to = common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	}
	data, _ := hex.DecodeString(wire.Data[2:])
	unsigned := types.NewTx(&types.DynamicFeeTx{ChainID: new(big.Int).SetUint64(wire.ChainID), Nonce: wire.Nonce, GasTipCap: tip, GasFeeCap: fee,
		Gas: wire.Gas, To: &to, Value: value, Data: data})
	signed, err := types.SignTx(unsigned, types.LatestSignerForChainID(new(big.Int).SetUint64(wire.ChainID)), g.signKey)
	if err != nil {
		js(500, nil)
		return
	}
	raw, _ := signed.MarshalBinary()
	js(200, map[string]any{"signed_raw_tx": "0x" + hex.EncodeToString(raw), "tx_hash": signed.Hash().Hex(), "from": g.address().Hex()})
}

func gatewaySetup(t *testing.T) (*Signer, *fakeChain, *fakeGateway, *GatewayHot, *storage.Store) {
	t.Helper()
	gw, srv := newFakeGateway(t)
	g, err := NewGatewayHot(context.Background(), GatewayConfig{BaseURL: srv.URL, CallerCN: "wallet-service"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Connect(context.Background(), testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	chain := newFakeChain()
	s, err := NewWithHot(context.Background(), store, g, chain, Assets{nativeID: {Type: AssetNative}})
	if err != nil {
		t.Fatal(err)
	}
	return s, chain, gw, g, store
}

func TestGatewayHot_PinsTheAddressAndSignsPayoutBumpAndCancel(t *testing.T) {
	s, _, gw, _, _ := gatewaySetup(t)
	ctx := context.Background()
	if s.HotWalletAddress() != gw.address() {
		t.Fatalf("pinned %s, gateway key is %s", s.HotWalletAddress(), gw.address())
	}
	if s.HotKey() != "0x"+hex.EncodeToString(gw.address().Bytes()) {
		t.Fatalf("nonce-state key %q", s.HotKey())
	}
	resp, err := s.Sign(ctx, wdCN, nativeReq("withdrawal:a"))
	if err != nil {
		t.Fatal(err)
	}
	tx := decode(t, resp)
	if from, _ := types.Sender(types.LatestSignerForChainID(big.NewInt(chainID)), tx); from != gw.address() || resp.Nonce != 4 {
		t.Fatalf("sender %s nonce %d", from, resp.Nonce)
	}
	if _, err := s.Replace(ctx, wdCN, "withdrawal:a", ReplaceRequest{ReplacementSeq: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(ctx, "withdrawal:a"); err != nil {
		t.Fatal(err)
	}
	if got := gw.operations(); len(got) != 3 || got[0] != OpPayout || got[1] != OpPayout || got[2] != OpCancel {
		t.Fatalf("operation classes sent: %v, want [payout payout cancel]", got)
	}
	last := gw.seen[2]
	if last.IdempotencyKey != "withdrawal:a" || last.CallerCN != "wallet-service" || last.To != lower(gw.address()) || last.Value != "0" || last.Gas != 21000 {
		t.Fatalf("cancel intent: %+v", last)
	}
	// The software backend is untouched: its path never sends anything to a gateway.
}

func lower(a common.Address) string { return "0x" + hex.EncodeToString(a.Bytes()) }

func TestGatewayHot_PolicyRefusalIsTypedAndConsumesNoNonce(t *testing.T) {
	s, chain, gw, g, _ := gatewaySetup(t)
	ctx := context.Background()
	gw.rule = "window_1h"
	gw.setMode("refuse")
	for i := 0; i < 6; i++ { // refusals are not strikes: the breaker stays closed however many there are
		_, err := s.Sign(ctx, wdCN, nativeReq("withdrawal:r"))
		var pr *PolicyRefusedError
		if !errors.As(err, &pr) || pr.Rule != "window_1h" {
			t.Fatalf("got %v, want PolicyRefusedError{window_1h}", err)
		}
		if errors.Is(err, ErrSignerUnavailable) {
			t.Fatal("a policy refusal must not look like an outage")
		}
	}
	if err := g.Available(); err != nil {
		t.Fatalf("breaker opened on refusals: %v", err)
	}
	gw.setMode("")
	resp, err := s.Sign(ctx, wdCN, nativeReq("withdrawal:r"))
	if err != nil || resp.Nonce != chain.pending {
		t.Fatalf("after refusals the same key signs the first nonce: %d %v (want %d)", resp.Nonce, err, chain.pending)
	}
}

// Invariant 6: an HSM / gateway failure consumes no nonce, and a retry with the same key signs the same nonce.
func TestGatewayHot_FailingGatewayConsumesNoNonce(t *testing.T) {
	for _, mode := range []string{"down", "forbidden", "badreq", "tamper", "hang"} {
		t.Run(mode, func(t *testing.T) {
			s, chain, gw, g, store := gatewaySetup(t)
			g.cfg.Timeout = 300 * time.Millisecond
			ctx := context.Background()
			gw.setMode(mode)
			_, err := s.Sign(ctx, wdCN, nativeReq("withdrawal:f"))
			if !errors.Is(err, ErrSignerUnavailable) {
				t.Fatalf("got %v, want ErrSignerUnavailable", err)
			}
			var pr *PolicyRefusedError
			if errors.As(err, &pr) {
				t.Fatal("not a policy refusal")
			}
			// nothing stored, nothing consumed
			if _, err := store.LookupHotSignature(ctx, wdCN, "withdrawal:f", requestHash(nativeReq("withdrawal:f"))); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("a signature was stored: %v", err)
			}
			if st, _ := store.NonceStates(ctx, s.HotKey()); len(st) != 0 && st[0].NextNonce != chain.pending {
				t.Fatalf("next_nonce moved to %d on a failed signature", st[0].NextNonce)
			}
			gw.setMode("")
			g.br = breaker{now: time.Now} // recovery
			resp, err := s.Sign(ctx, wdCN, nativeReq("withdrawal:f"))
			if err != nil || resp.Nonce != chain.pending {
				t.Fatalf("retry with the same key: nonce %d err %v, want %d", resp.Nonce, err, chain.pending)
			}
			if next, err := s.Sign(ctx, wdCN, nativeReq("withdrawal:g")); err != nil || next.Nonce != chain.pending+1 {
				t.Fatalf("the next key gets the next nonce: %d %v", next.Nonce, err)
			}
		})
	}
}

func TestGatewayHot_CircuitBreakerIsCheckedBeforeAnyRPCOrLock(t *testing.T) {
	s, chain, gw, g, _ := gatewaySetup(t)
	ctx := context.Background()
	now := time.Now()
	g.br.now = func() time.Time { return now }
	gw.setMode("down")
	for i := 0; i < 3; i++ {
		if _, err := s.Sign(ctx, wdCN, nativeReq("withdrawal:b")); !errors.Is(err, ErrSignerUnavailable) {
			t.Fatal(err)
		}
	}
	hits, pend, fees := gw.signHits.Load(), chain.pendingCalls.Load(), chain.feeCalls.Load()
	if hits != 3 {
		t.Fatalf("gateway hits %d, want 3 strikes", hits)
	}
	// open: fails fast, no gateway call, no RPC, so no nonce-row lock either (SignHotWallet is never reached)
	_, err := s.Sign(ctx, wdCN, nativeReq("withdrawal:b"))
	if !errors.Is(err, ErrSignerUnavailable) {
		t.Fatalf("open breaker: %v", err)
	}
	if gw.signHits.Load() != hits || chain.pendingCalls.Load() != pend || chain.feeCalls.Load() != fees {
		t.Fatal("an open breaker must not touch the gateway, the RPC or the DB lock")
	}
	if _, err := s.Replace(ctx, wdCN, "withdrawal:b", ReplaceRequest{ReplacementSeq: 1}); !errors.Is(err, ErrSignerUnavailable) || chain.feeCalls.Load() != fees {
		t.Fatalf("replace with an open breaker: %v (fee calls %d)", err, chain.feeCalls.Load())
	}
	// a replay of an already-stored signature needs no signer at all
	gw.setMode("")
	now = now.Add(6 * time.Second) // window over: one probe is allowed, and succeeds
	if _, err := s.Sign(ctx, wdCN, nativeReq("withdrawal:ok")); err != nil {
		t.Fatalf("probe after the window: %v", err)
	}
	gw.setMode("down")
	if _, err := s.Sign(ctx, wdCN, nativeReq("withdrawal:ok")); err != nil {
		t.Fatalf("replay must not need the signer: %v", err)
	}
}

// The gauge behind the HotSignerBreakerOpen alert: tripped after the third failure, and it stays tripped across the
// 5 s windows until a call succeeds (an alert on "open for a minute" must not flap with the probe windows).
func TestGatewayHot_BreakerOpenGaugeStaysUntilASuccess(t *testing.T) {
	s, _, gw, g, _ := gatewaySetup(t)
	ctx := context.Background()
	now := time.Now()
	g.br.now = func() time.Time { return now }
	if g.BreakerOpen() {
		t.Fatal("open before any failure")
	}
	gw.setMode("down")
	for i := 0; i < 3; i++ {
		_, _ = s.Sign(ctx, wdCN, nativeReq("withdrawal:gauge"))
	}
	if !g.BreakerOpen() {
		t.Fatal("not open after 3 consecutive failures")
	}
	now = now.Add(6 * time.Second) // window over, no call yet: still tripped
	if !g.BreakerOpen() {
		t.Fatal("must stay tripped until a call succeeds")
	}
	gw.setMode("")
	if _, err := s.Sign(ctx, wdCN, nativeReq("withdrawal:gauge-ok")); err != nil {
		t.Fatal(err)
	}
	if g.BreakerOpen() {
		t.Fatal("still open after a success")
	}
}

// Invariant 5: wallet-service re-verifies the sender against the pinned address; a swapped key is refused and nothing is stored.
func TestGatewayHot_SignatureFromAnotherKeyIsRefusedAndNotStored(t *testing.T) {
	s, chain, gw, _, store := gatewaySetup(t)
	ctx := context.Background()
	other, _ := crypto.GenerateKey()
	gw.signKey = other
	_, err := s.Sign(ctx, wdCN, nativeReq("withdrawal:x"))
	if !errors.Is(err, ErrSenderMismatch) || !errors.Is(err, ErrSignerUnavailable) {
		t.Fatalf("got %v, want ErrSenderMismatch", err)
	}
	if _, err := store.LookupHotSignature(ctx, wdCN, "withdrawal:x", requestHash(nativeReq("withdrawal:x"))); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a foreign signature was stored: %v", err)
	}
	gw.signKey = gw.key
	if resp, err := s.Sign(ctx, wdCN, nativeReq("withdrawal:x")); err != nil || resp.Nonce != chain.pending {
		t.Fatalf("after the key is right again: %d %v", resp.Nonce, err)
	}
}

func TestNewWithHot_RefusesAChainMismatchAndABadKeyAnswer(t *testing.T) {
	gw, srv := newFakeGateway(t)
	g, err := NewGatewayHot(context.Background(), GatewayConfig{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	chain := newFakeChain()
	chain.id = 1
	if _, err := NewWithHot(context.Background(), nil, g, chain, nil); err == nil {
		t.Fatal("the gateway is pinned to chain 31337, the RPC is chain 1: must refuse")
	}
	// public key that does not hash to the advertised address
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		other, _ := crypto.GenerateKey()
		_ = json.NewEncoder(w).Encode(map[string]any{"address": gw.address().Hex(), "public_key": "0x" + hex.EncodeToString(crypto.FromECDSAPub(&other.PublicKey)), "chain_id": 31337})
	}))
	defer bad.Close()
	if _, err := NewGatewayHot(context.Background(), GatewayConfig{BaseURL: bad.URL}); err == nil {
		t.Fatal("a public key that does not match the address must not be pinned")
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer down.Close()
	if _, err := NewGatewayHot(context.Background(), GatewayConfig{BaseURL: down.URL}); err == nil {
		t.Fatal("an unreachable gateway at start is an error: no software fallback")
	}
}

// mTLS: the client presents its leaf and only trusts a gateway certificate for the name hsm-gateway from the project CA.
func TestGatewayTLS_ClientCertAndServerNameAreEnforced(t *testing.T) {
	dir := t.TempDir()
	ca, caCert, caPriv := newTestCA(t)
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	clientCert, clientKey := ca.issue(t, caCert, caPriv, "wallet-service")
	cf, kf, caf := write("c.pem", clientCert), write("k.pem", clientKey), write("ca.pem", ca.certPEM)

	serve := func(serverName string) *httptest.Server {
		crt, key := ca.issue(t, caCert, caPriv, serverName)
		pair, _ := tls.X509KeyPair(crt, key)
		pool := x509.NewCertPool()
		pool.AddCert(caCert)
		var sawClient atomic.Bool
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(r.TLS.PeerCertificates) == 1 && r.TLS.PeerCertificates[0].Subject.CommonName == "wallet-service" {
				sawClient.Store(true)
			}
			gw := &fakeGateway{}
			gw.key, _ = crypto.GenerateKey()
			gw.chain = chainID
			gw.ServeHTTP(w, r)
		}))
		srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, MinVersion: tls.VersionTLS13}
		srv.StartTLS()
		t.Cleanup(srv.Close)
		return srv
	}
	cfg, err := GatewayTLSConfig(cf, kf, caf)
	if err != nil {
		t.Fatal(err)
	}
	good := serve("hsm-gateway")
	if _, err := NewGatewayHot(context.Background(), GatewayConfig{BaseURL: good.URL, TLS: cfg}); err != nil {
		t.Fatalf("a gateway with the right name and CA: %v", err)
	}
	wrongName := serve("not-the-gateway")
	if _, err := NewGatewayHot(context.Background(), GatewayConfig{BaseURL: wrongName.URL, TLS: cfg}); err == nil {
		t.Fatal("a CA-signed certificate for another name must be refused")
	}
	// a client without a certificate is refused by the server
	noCert := cfg.Clone()
	noCert.Certificates = nil
	if _, err := NewGatewayHot(context.Background(), GatewayConfig{BaseURL: good.URL, TLS: noCert}); err == nil {
		t.Fatal("the server requires a client certificate")
	}
}

type testCA struct{ certPEM []byte }

func newTestCA(t *testing.T) (testCA, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return testCA{certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, cert, priv
}

// issue makes a leaf with CN and DNS SAN name (and 127.0.0.1 for httptest), valid for client and server auth.
func (testCA) issue(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, name string) (certPEM, keyPEM []byte) {
	t.Helper()
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &priv.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(priv)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

// The Hot cutover tool refuses a gateway that does not say it is serving (key-custody-hsm.md 5.5).
func TestNewGatewayHot_ReportsTheGatewaysServingState(t *testing.T) {
	gw, _ := newFakeGateway(t)
	for _, tc := range []struct {
		name string
		body func() map[string]any
		want bool
	}{
		{"serving", func() map[string]any { return map[string]any{"serving": true} }, true},
		{"not serving", func() map[string]any { return map[string]any{"serving": false} }, false},
		{"an older gateway that says nothing counts as not serving", func() map[string]any { return map[string]any{} }, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			body := map[string]any{"address": gw.address().Hex(), "public_key": "0x" + hex.EncodeToString(crypto.FromECDSAPub(&gw.key.PublicKey)), "chain_id": 31337}
			for k, v := range tc.body() {
				body[k] = v
			}
			_ = json.NewEncoder(w).Encode(body)
		}))
		g, err := NewGatewayHot(context.Background(), GatewayConfig{BaseURL: srv.URL})
		srv.Close()
		if err != nil || g.Serving() != tc.want {
			t.Errorf("%s: serving=%v err=%v, want %v", tc.name, g != nil && g.Serving(), err, tc.want)
		}
	}
}
