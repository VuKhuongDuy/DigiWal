package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type ep struct{ name, base, method, path, body string; allowed []string }

// SEC-01: real TLS, real CA-signed certs for every service CN, every mTLS endpoint.
func TestSEC01_CNMatrixOverRealTLS(t *testing.T) {
	qa(t)
	u := uid("sec01")
	depositAddress(t, u)
	key := uid("sec")
	eps := []ep{
		{"hot-wallet", walletTLS, "POST", "/v1/sign/hot-wallet", `{"to_address":"` + randAddr().Hex() + `","asset_id":"` + ethAsset + `","amount":"1","chain_id":31337,"idempotency_key":"` + key + `"}`, []string{"withdrawal-service", "sweeper"}},
		{"user-address", walletTLS, "POST", "/v1/sign/user-address", `{"user_id":"` + u + `","to_address":"` + hotAddr.Hex() + `","asset_id":"` + ethAsset + `","amount":"max","chain_id":31337}`, []string{"sweeper"}},
		{"addresses", walletTLS, "GET", "/v1/addresses/" + randAddr().Hex(), ``, []string{"chain-watcher"}},
		{"ledger-transactions", ledgerTLS, "POST", "/v1/ledger/transactions", `{"idempotency_key":"x","source_type":"correction","entries":[{"corrects_entry_id":"00000000-0000-0000-0000-000000000000"},{"corrects_entry_id":"00000000-0000-0000-0000-000000000001"}]}`, []string{"withdrawal-service"}},
		{"ledger-lock", ledgerTLS, "POST", "/v1/ledger/locks", `{"idempotency_key":"` + uid("sl") + `","user_id":"` + u + `","asset_id":"` + ethAsset + `","amount":"1"}`, []string{"withdrawal-service"}},
		{"ledger-settle", ledgerTLS, "POST", "/v1/ledger/locks/nope/settle", ``, []string{"withdrawal-service"}},
		{"ledger-release", ledgerTLS, "POST", "/v1/ledger/locks/nope/release", ``, []string{"withdrawal-service"}},
	}
	cns := []string{"withdrawal-service", "sweeper", "chain-watcher", "ledger-service", "wallet-service", "api-gateway", "evil-service"}
	for _, e := range eps {
		for _, cn := range cns {
			r := do(client(cn), e.method, e.base+e.path, e.body)
			allowed := false
			for _, a := range e.allowed {
				allowed = allowed || a == cn
			}
			if allowed && r.Code == 403 {
				t.Errorf("%s: allowed CN %s got 403", e.name, cn)
			}
			if !allowed && r.Code != 403 {
				t.Errorf("AUTHZ BREACH %s: CN %s got %d (want 403) %s", e.name, cn, r.Code, r.Body)
			}
		}
	}
}

func rawTLS(addr string, cfg *tls.Config, req string) (string, error) {
	c, err := tls.DialWithDialer(&net_Dialer, "tcp", addr, cfg)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte(req))
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	return string(buf[:n]), err
}

func TestSEC02_HandshakeRules(t *testing.T) {
	qa(t)
	ca, _ := os.ReadFile(repoRoot + "/tests/e2e/.state/ca/ca-cert.pem")
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	base := &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS13}
	req := "POST /v1/ledger/locks HTTP/1.1\r\nHost: localhost\r\nContent-Length: 2\r\n\r\n{}"

	// no client certificate
	if out, err := rawTLS("localhost:18444", base, req); err == nil && strings.Contains(out, "HTTP/1.1 4") || (err == nil && strings.Contains(out, "HTTP/1.1 2")) {
		t.Errorf("request without client cert was served: %q", out)
	}
	// cert from a foreign CA with an allowed CN
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "withdrawal-service"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	foreign := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
	cfg := base.Clone()
	cfg.Certificates = []tls.Certificate{foreign}
	for _, addr := range []string{"localhost:18443", "localhost:18444"} {
		out, err := rawTLS(addr, cfg, req)
		if err == nil && strings.HasPrefix(out, "HTTP/1.1 ") && !strings.Contains(out, "400") {
			t.Errorf("%s accepted a self-signed cert with CN=withdrawal-service: %q", addr, out)
		}
	}
	// TLS 1.2 refused
	old := base.Clone()
	old.MinVersion, old.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
	if _, err := rawTLS("localhost:18444", old, req); err == nil {
		t.Errorf("TLS1.2 accepted")
	}
	// plain HTTP on the TLS port
	r := do(http.DefaultClient, "GET", "http://localhost:18444/v1/ledger/locks", nil)
	if r.Code == 200 {
		t.Errorf("plain http served on TLS port")
	}
}

// SEC-03: the plain listeners expose nothing but /healthz (+ wallet's deposit-address, see SEC-04).
func TestSEC03_PlainListenersExposeNoMoneyEndpoints(t *testing.T) {
	qa(t)
	for _, c := range []struct{ base, method, path string }{
		{ledgerHTP, "POST", "/v1/ledger/locks"}, {ledgerHTP, "POST", "/v1/ledger/transactions"}, {ledgerHTP, "POST", "/v1/ledger/locks/x/settle"},
		{walletURL, "POST", "/v1/sign/hot-wallet"}, {walletURL, "POST", "/v1/sign/user-address"}, {walletURL, "GET", "/v1/addresses/0x0000000000000000000000000000000000000001"},
	} {
		r := do(http.DefaultClient, c.method, c.base+c.path, `{}`)
		if r.Code != 404 && r.Code != 405 {
			t.Errorf("%s %s%s on plain listener -> %d", c.method, c.base, c.path, r.Code)
		}
	}
}

// SEC-04: wallet-service's deposit-address endpoints are on the PLAIN listener, unauthenticated
// (service-auth-mtls.md Open Question #4). Anyone who can reach :8080 can create addresses for arbitrary user ids
// and enumerate which ids exist.
func TestSEC04_DepositAddressEndpointIsUnauthenticated(t *testing.T) {
	qa(t)
	u := uid("sec04")
	if r := do(http.DefaultClient, "POST", walletURL+"/v1/users/"+u+"/deposit-address", nil); r.Code == 201 {
		t.Logf("OBSERVATION S-2 (MEDIUM, known open question): unauthenticated caller created a deposit address for arbitrary user %q — enables unbounded row/index creation (DoS of the derivation space) and user-id enumeration via GET 200/404", u)
	}
}

// SEC-05: no secret material in container logs after the whole run.
func TestSEC05_NoSecretsInLogs(t *testing.T) {
	qa(t)
	mn := loadMnemonics()
	master, _ := os.ReadFile(repoRoot + "/tests/e2e/.state/master.env")
	for _, svc := range []string{"digiwal-qa-wallet-service-1", "digiwal-qa-ledger-service-1"} {
		out, _ := execCmd("logs", svc)
		for role, m := range mn {
			for _, w := range strings.Fields(m)[:3] {
				_ = w
			}
			if strings.Contains(out, m) || strings.Contains(out, strings.Join(strings.Fields(m)[:6], " ")) {
				t.Errorf("%s logs contain the %s mnemonic", svc, role)
			}
		}
		if k := strings.TrimPrefix(strings.TrimSpace(string(master)), "MASTER_KEY_HEX="); k != "" && strings.Contains(out, k) {
			t.Errorf("%s logs contain MASTER_KEY_HEX", svc)
		}
		if strings.Contains(out, faucetKeyHex) || strings.Contains(strings.ToLower(out), "private key") {
			t.Errorf("%s logs mention key material", svc)
		}
	}
}
