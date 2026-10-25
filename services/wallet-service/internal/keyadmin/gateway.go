// Package keyadmin is the logic of the operator ceremonies (key-custody-hsm.md: Ceremony, `key-admin`). It holds no secret:
// the mnemonic stays in the !prod command, which hands this package only the PUBLIC account xpub.
package keyadmin

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"time"

	"digiwal/services/wallet-service/internal/xpub"
)

// Gateway is what the migration needs from hsm-gateway.
type Gateway interface {
	// AttestKey is the attestation public key the gateway says it holds (65-byte uncompressed P-256 point).
	AttestKey(ctx context.Context) ([]byte, error)
	// Attest asks the gateway to sign the statement over (chain, path, xpub): the raw 64-byte r||s and the key that signed.
	Attest(ctx context.Context, chain, path, xpub string) (sig, pub []byte, err error)
}

// GatewayClient talks to hsm-gateway over mTLS as CN key-admin (the leaf the gateway pins).
type GatewayClient struct {
	BaseURL string
	HTTP    *http.Client
}

// NewGatewayClient builds a client from a TLS config that carries the key-admin leaf, the project CA and ServerName hsm-gateway.
func NewGatewayClient(baseURL string, cfg *tls.Config) *GatewayClient {
	return &GatewayClient{BaseURL: baseURL, HTTP: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}}
}

func (c *GatewayClient) do(ctx context.Context, method, path string, body []byte, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error  string `json:"error"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(b, &e)
		return resp.StatusCode, fmt.Errorf("hsm-gateway %s %s: %d %s %s", method, path, resp.StatusCode, e.Error, e.Detail)
	}
	return resp.StatusCode, json.Unmarshal(b, out)
}

// AttestKey implements Gateway (GET /v1/attest/key).
func (c *GatewayClient) AttestKey(ctx context.Context) ([]byte, error) {
	var r struct {
		Alg       string `json:"alg"`
		PublicKey string `json:"public_key"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/v1/attest/key", nil, &r); err != nil {
		return nil, err
	}
	if r.Alg != xpub.Alg {
		return nil, fmt.Errorf("hsm-gateway attestation alg %q, want %q", r.Alg, xpub.Alg)
	}
	pk, err := hex.DecodeString(r.PublicKey)
	if err != nil || len(pk) != 65 {
		return nil, errors.New("hsm-gateway returned a malformed attestation public key")
	}
	return pk, nil
}

// Attest implements Gateway (POST /v1/attest). It checks the response against what it asked for: the statement must be
// byte-identical to the canonical one and the signature must verify under the returned key.
func (c *GatewayClient) Attest(ctx context.Context, chain, path, xp string) ([]byte, []byte, error) {
	body, _ := json.Marshal(map[string]string{"chain": chain, "path": path, "xpub": xp})
	var r struct {
		Statement string `json:"statement"`
		Alg       string `json:"alg"`
		Signature string `json:"signature"`
		PublicKey string `json:"public_key"`
	}
	if _, err := c.do(ctx, http.MethodPost, "/v1/attest", body, &r); err != nil {
		return nil, nil, err
	}
	if r.Alg != xpub.Alg || r.Statement != string(xpub.Statement(chain, path, xp)) {
		return nil, nil, errors.New("hsm-gateway signed a statement other than the one requested")
	}
	sig, err1 := hex.DecodeString(r.Signature)
	pub, err2 := hex.DecodeString(r.PublicKey)
	if err1 != nil || err2 != nil || len(sig) != 64 || len(pub) != 65 {
		return nil, nil, errors.New("hsm-gateway returned a malformed attestation")
	}
	x, y := elliptic.Unmarshal(elliptic.P256(), pub) //nolint:staticcheck
	if x == nil {
		return nil, nil, errors.New("hsm-gateway returned an attestation key that is not on P-256")
	}
	d := xpub.Digest(chain, path, xp)
	if !ecdsa.Verify(&ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, d[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return nil, nil, errors.New("hsm-gateway's signature does not verify under its own key")
	}
	return sig, pub, nil
}
