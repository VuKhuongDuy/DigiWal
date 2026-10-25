// Package hotkey learns the Hot address a Sweep may go to (key-custody-hsm.md invariant 15): from hsm-gateway
// GET /v1/hot/key, over mTLS as CN sweep-signer (a leaf the gateway pins). wallet-service is not trusted for it. The answer
// is read ONCE at start and pinned in memory; the request never carries it, and a Hot cutover needs a sweep-signer restart.
package hotkey

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Pinned is what sweep-signer pins about the Hot Wallet.
type Pinned struct {
	Address common.Address
	ChainID uint64
}

// Source says where the pinned Hot address comes from.
type Source interface {
	Pin(ctx context.Context) (Pinned, error)
	Name() string
}

// ClientTLS is sweep-signer's client config for hsm-gateway: its own leaf, the project CA, ServerName hsm-gateway.
func ClientTLS(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading client certificate: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("CA file holds no certificate")
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, MinVersion: tls.VersionTLS13, ServerName: "hsm-gateway"}, nil
}

// Gateway is the production source: hsm-gateway's GET /v1/hot/key.
type Gateway struct {
	BaseURL string
	HTTP    *http.Client
}

// NewGateway builds the client from a TLS config that carries the sweep-signer leaf.
func NewGateway(baseURL string, tlsCfg *tls.Config) *Gateway {
	return &Gateway{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{
		Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}}
}

func (g *Gateway) Name() string { return "hsm-gateway " + g.BaseURL }

// Pin fetches the Hot key and checks that the advertised public key hashes to the advertised address and that the gateway
// says it is serving (its self-test passed).
func (g *Gateway) Pin(ctx context.Context) (Pinned, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.BaseURL+"/v1/hot/key", nil)
	if err != nil {
		return Pinned{}, err
	}
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return Pinned{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return Pinned{}, fmt.Errorf("GET /v1/hot/key: HTTP %d", resp.StatusCode)
	}
	var k struct {
		Address   string `json:"address"`
		PublicKey string `json:"public_key"`
		ChainID   uint64 `json:"chain_id"`
		Serving   bool   `json:"serving"`
	}
	if err := json.Unmarshal(body, &k); err != nil {
		return Pinned{}, fmt.Errorf("GET /v1/hot/key: %w", err)
	}
	if !common.IsHexAddress(k.Address) || k.ChainID == 0 {
		return Pinned{}, errors.New("GET /v1/hot/key: malformed answer")
	}
	pub, err := hex.DecodeString(strings.TrimPrefix(k.PublicKey, "0x"))
	if err != nil {
		return Pinned{}, fmt.Errorf("GET /v1/hot/key: public_key: %w", err)
	}
	pk, err := crypto.UnmarshalPubkey(pub)
	if err != nil {
		return Pinned{}, fmt.Errorf("GET /v1/hot/key: public_key: %w", err)
	}
	if crypto.PubkeyToAddress(*pk) != common.HexToAddress(k.Address) {
		return Pinned{}, errors.New("GET /v1/hot/key: public key does not match the address")
	}
	if !k.Serving {
		return Pinned{}, errors.New("GET /v1/hot/key: the gateway is not serving (self-test not passed)")
	}
	return Pinned{Address: common.HexToAddress(k.Address), ChainID: k.ChainID}, nil
}

// PinWithRetry keeps asking until the answer is good or wait has passed (compose starts the gateway in parallel).
func PinWithRetry(ctx context.Context, s Source, wait time.Duration) (Pinned, error) {
	deadline := time.Now().Add(wait)
	for {
		p, err := s.Pin(ctx)
		if err == nil {
			return p, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return Pinned{}, fmt.Errorf("pinning the Hot address from %s: %w", s.Name(), err)
		}
		time.Sleep(2 * time.Second)
	}
}
