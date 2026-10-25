package keyadmin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"digiwal/services/wallet-service/internal/hdwallet"
	"digiwal/services/wallet-service/internal/xpub"
)

// The wire contract with hsm-gateway (POST /v1/attest, GET /v1/attest/key), against a TLS test server that behaves like it,
// and against deviations a compromised or confused gateway could send.
func TestGatewayClient_WireContractAndDeviations(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pub := hex.EncodeToString(elliptic.Marshal(elliptic.P256(), key.X, key.Y)) //nolint:staticcheck
	mn, _ := hdwallet.NewMnemonic()
	xp, _ := hdwallet.AccountXPub(mn)

	mode := "good"
	var gotReq map[string]string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/attest/key":
			_ = json.NewEncoder(w).Encode(map[string]string{"alg": "ecdsa-p256-sha256", "public_key": pub})
		case r.Method == "POST" && r.URL.Path == "/v1/attest":
			_ = json.NewDecoder(r.Body).Decode(&gotReq)
			if mode == "refuse" {
				w.WriteHeader(422)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "attest_refused", "detail": "path"})
				return
			}
			stmt := string(xpub.Statement(gotReq["chain"], gotReq["path"], gotReq["xpub"]))
			if mode == "other-statement" {
				stmt = string(xpub.Statement("evm", "m/44'/60'/1'", gotReq["xpub"]))
			}
			d := sha256.Sum256([]byte(stmt))
			rr, ss, _ := ecdsa.Sign(rand.Reader, key, d[:])
			sig := make([]byte, 64)
			rr.FillBytes(sig[:32])
			ss.FillBytes(sig[32:])
			if mode == "bad-sig" {
				sig[5] ^= 1
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"statement": stmt, "alg": "ecdsa-p256-sha256", "signature": hex.EncodeToString(sig), "public_key": pub, "audit_seq": 7})
		}
	}))
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	c := NewGatewayClient(srv.URL, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13})
	ctx := context.Background()

	if k, err := c.AttestKey(ctx); err != nil || hex.EncodeToString(k) != pub {
		t.Fatalf("AttestKey: %v", err)
	}
	sig, k, err := c.Attest(ctx, "evm", xpub.PathEVM, xp)
	if err != nil || len(sig) != 64 || hex.EncodeToString(k) != pub {
		t.Fatalf("Attest: %v", err)
	}
	if gotReq["chain"] != "evm" || gotReq["path"] != xpub.PathEVM || gotReq["xpub"] != xp || len(gotReq) != 3 {
		t.Fatalf("request body %v: only chain, path and xpub may be sent", gotReq)
	}
	for _, m := range []string{"other-statement", "bad-sig", "refuse"} {
		mode = m
		if _, _, err := c.Attest(ctx, "evm", xpub.PathEVM, xp); err == nil {
			t.Errorf("%s: accepted", m)
		} else if m == "refuse" && !strings.Contains(err.Error(), "attest_refused") {
			t.Errorf("refusal not surfaced: %v", err)
		}
	}
}
