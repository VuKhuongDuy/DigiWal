// Package wallet is sweeper's mTLS client for wallet-service's two signing endpoints, with
// sweeper's own certificate (CN=sweeper, service-auth-mtls.md). It sorts every failure into
// the classes the sweep logic needs: an answer, "retry later", or "this can never work".
package wallet

import (
	"bytes"
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

	"github.com/ethereum/go-ethereum/common"
)

var (
	// ErrBelowGas: a native "max" Sweep has nothing left after reserving its own gas — dust.
	ErrBelowGas = errors.New("balance does not cover the sweep's gas")
	// ErrPermanent: wallet-service refused in a way retrying cannot fix (bad request, unknown
	// user or asset, destination not the Hot Wallet, key reused with a different request).
	ErrPermanent = errors.New("wallet-service refused the request")
)

type Client struct {
	BaseURL string
	HTTP    *http.Client
}

type HotRequest struct {
	ToAddress      string `json:"to_address"`
	AssetID        string `json:"asset_id"`
	Amount         string `json:"amount"`
	ChainID        int64  `json:"chain_id"`
	IdempotencyKey string `json:"idempotency_key"`
}

type UserRequest struct {
	UserID    string `json:"user_id"`
	ToAddress string `json:"to_address"`
	AssetID   string `json:"asset_id"`
	Amount    string `json:"amount"` // decimal minor units, or "max" for a native-coin sweep
	ChainID   int64  `json:"chain_id"`
}

// SignHotWallet: POST /v1/sign/hot-wallet (Gas Top-Up: Hot Wallet -> user address). Returns
// the signed raw transaction and the Hot Wallet nonce it claimed.
func (c *Client) SignHotWallet(ctx context.Context, req HotRequest) ([]byte, uint64, error) {
	var out struct {
		SignedRawTx string `json:"signed_raw_tx"`
		Nonce       uint64 `json:"nonce"`
	}
	if err := c.post(ctx, "/v1/sign/hot-wallet", req, &out); err != nil {
		return nil, 0, err
	}
	raw, err := decodeTx(out.SignedRawTx)
	return raw, out.Nonce, err
}

// SignUserAddress: POST /v1/sign/user-address (the Sweep: user address -> Hot Wallet).
func (c *Client) SignUserAddress(ctx context.Context, req UserRequest) ([]byte, error) {
	var out struct {
		SignedRawTx string `json:"signed_raw_tx"`
	}
	if err := c.post(ctx, "/v1/sign/user-address", req, &out); err != nil {
		return nil, err
	}
	return decodeTx(out.SignedRawTx)
}

// HotAddress: GET /v1/wallet/hot-address — the address wallet-service itself derived for the
// Hot Wallet (deposit-flow.md invariant #15). Asked once at startup and cached by the caller:
// a locally configured copy could drift from the real one.
func (c *Client) HotAddress(ctx context.Context) (common.Address, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+"/v1/wallet/hot-address", nil)
	if err != nil {
		return common.Address{}, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return common.Address{}, fmt.Errorf("calling wallet-service hot-address: %w", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
	if resp.StatusCode != http.StatusOK {
		return common.Address{}, fmt.Errorf("wallet-service hot-address answered %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	var out struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(payload, &out); err != nil || !common.IsHexAddress(out.Address) {
		return common.Address{}, fmt.Errorf("wallet-service returned an unusable hot address: %q", payload)
	}
	return common.HexToAddress(out.Address), nil
}

func decodeTx(s string) ([]byte, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil || len(raw) == 0 {
		return nil, fmt.Errorf("wallet-service returned an unusable signed transaction")
	}
	return raw, nil
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("calling wallet-service %s: %w", path, err) // transient
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	switch {
	case resp.StatusCode == http.StatusOK:
		if err := json.Unmarshal(payload, out); err != nil {
			return fmt.Errorf("wallet-service %s: unreadable response: %w", path, err)
		}
		return nil
	case resp.StatusCode == http.StatusUnprocessableEntity && path == "/v1/sign/user-address":
		return ErrBelowGas
	case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusForbidden,
		resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusConflict, resp.StatusCode == http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: %s %d %s", ErrPermanent, path, resp.StatusCode, strings.TrimSpace(string(payload)))
	default: // 5xx, 503 (frozen / not configured), 429, ...: try again later
		return fmt.Errorf("wallet-service %s answered %d: %s", path, resp.StatusCode, strings.TrimSpace(string(payload)))
	}
}

// ClientTLSConfig is sweeper's mTLS client config: its own leaf certificate and the project CA
// as the only trust root.
func ClientTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading client key pair: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading CA cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no certificates found in %s", caFile)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, MinVersion: tls.VersionTLS13}, nil
}
