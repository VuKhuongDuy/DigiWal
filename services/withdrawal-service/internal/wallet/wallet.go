// Package wallet is withdrawal-service's client for wallet-service, over mTLS as
// CN=withdrawal-service (service-auth-mtls.md), and the invariant #18 destination check built on it.
package wallet

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"digiwal/services/withdrawal-service/internal/validate"
)

type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// ClientTLSConfig: our own leaf certificate (CN=withdrawal-service) and the project CA as the
// only trust root.
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

// SystemAddresses are the Hot and Warm Wallet addresses (GET /v1/wallet/system-addresses).
type SystemAddresses struct {
	ChainID int64
	Hot     common.Address
	Warm    common.Address
}

func (c *Client) get(ctx context.Context, path string) (int, []byte, error) {
	return c.do(ctx, http.MethodGet, path, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("calling wallet-service %s: %w", path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
	return resp.StatusCode, out, nil
}

// SystemAddresses is asked once at startup and cached by the caller (invariant #18); anything but
// a clean 200 with two real, distinct addresses is an error.
func (c *Client) SystemAddresses(ctx context.Context) (SystemAddresses, error) {
	code, body, err := c.get(ctx, "/v1/wallet/system-addresses")
	if err != nil {
		return SystemAddresses{}, err
	}
	if code != http.StatusOK {
		return SystemAddresses{}, fmt.Errorf("wallet-service system-addresses answered %d: %s", code, strings.TrimSpace(string(body)))
	}
	var out struct {
		ChainID int64  `json:"chain_id"`
		Hot     string `json:"hot"`
		Warm    string `json:"warm"`
	}
	if err := json.Unmarshal(body, &out); err != nil || !common.IsHexAddress(out.Hot) || !common.IsHexAddress(out.Warm) {
		return SystemAddresses{}, fmt.Errorf("wallet-service returned unusable system addresses: %q", body)
	}
	a := SystemAddresses{ChainID: out.ChainID, Hot: common.HexToAddress(out.Hot), Warm: common.HexToAddress(out.Warm)}
	if a.Hot == (common.Address{}) || a.Warm == (common.Address{}) || a.Hot == a.Warm {
		return SystemAddresses{}, fmt.Errorf("wallet-service returned unusable system addresses: %q", body)
	}
	return a, nil
}

// SignRequest is what to pay, never which key nor how to build the tx (withdrawal-flow.md #5).
type SignRequest struct {
	ToAddress      string `json:"to_address"`
	AssetID        string `json:"asset_id"`
	Amount         string `json:"amount"`
	ChainID        int64  `json:"chain_id"`
	IdempotencyKey string `json:"idempotency_key"` // the Withdrawal Key (invariant #20)
}

// ErrSignRefused: wallet-service refused the request itself (400/422) — it allocated no nonce, so
// the Lock can be Released. Anything else (503 while a Nonce Gap is open, 5xx, no response) is
// transient: the same key is asked again, and wallet-service returns the stored tx if it had
// already signed (invariant #16), so a lost response can never strand a nonce.
var ErrSignRefused = errors.New("wallet-service refused to sign")

// ErrKeyConflict: wallet-service answered 409 — the Withdrawal Key already has a signature for a
// different request. Should be impossible (the key is unique and the request deterministic).
var ErrKeyConflict = errors.New("wallet-service already signed this key for a different request")

// SignHotWallet is POST /v1/sign/hot-wallet → the signed raw tx and the Hot Wallet nonce it claims.
func (c *Client) SignHotWallet(ctx context.Context, r SignRequest) (rawTx string, nonce uint64, err error) {
	b, _ := json.Marshal(r)
	code, body, err := c.do(ctx, http.MethodPost, "/v1/sign/hot-wallet", b)
	if err != nil {
		return "", 0, err
	}
	switch code {
	case http.StatusOK:
		var out struct {
			SignedRawTx string `json:"signed_raw_tx"`
			Nonce       uint64 `json:"nonce"`
		}
		if err := json.Unmarshal(body, &out); err != nil || !strings.HasPrefix(out.SignedRawTx, "0x") {
			return "", 0, fmt.Errorf("wallet-service returned an unusable signature: %q", body)
		}
		return out.SignedRawTx, out.Nonce, nil
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "", 0, fmt.Errorf("%w (%d): %s", ErrSignRefused, code, strings.TrimSpace(string(body)))
	case http.StatusConflict:
		// The key was already signed for a DIFFERENT request: a nonce IS allocated under it, so
		// releasing would strand it. Never a refusal; an operator must look.
		return "", 0, fmt.Errorf("%w: %s", ErrKeyConflict, strings.TrimSpace(string(body)))
	default:
		return "", 0, fmt.Errorf("wallet-service sign answered %d: %s", code, strings.TrimSpace(string(body)))
	}
}

// ReportNonceGap is POST /v1/wallet/nonce-gap (invariant #19): wallet-service stops allocating Hot
// Wallet nonces from fromNonce on, for every caller. Idempotent; the caller retries until nil.
func (c *Client) ReportNonceGap(ctx context.Context, chainID int64, fromNonce uint64) error {
	b, _ := json.Marshal(map[string]any{"chain_id": chainID, "from_nonce": fromNonce})
	code, body, err := c.do(ctx, http.MethodPost, "/v1/wallet/nonce-gap", b)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("wallet-service nonce-gap answered %d: %s", code, strings.TrimSpace(string(body)))
	}
	return nil
}

// ErrUnknown means wallet-service gave no clean answer: never to be read as "not ours".
var ErrUnknown = errors.New("could not tell whether the address is ours")

// IsDepositAddress asks GET /v1/addresses/{address}: 200 = one of ours, 404 = not ours, anything
// else = ErrUnknown. A live call every time, no cache: a stale "no" is the one answer that could
// let a withdrawal into a deposit address through (invariant #18).
func (c *Client) IsDepositAddress(ctx context.Context, addr common.Address) (bool, error) {
	code, body, err := c.get(ctx, "/v1/addresses/"+addr.Hex())
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrUnknown, err)
	}
	switch code {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("%w: wallet-service answered %d: %s", ErrUnknown, code, strings.TrimSpace(string(body)))
	}
}

// DepositLookup is the slice of Client the destination check needs.
type DepositLookup interface {
	IsDepositAddress(ctx context.Context, addr common.Address) (bool, error)
}

// Destinations implements validate.Destinations: the Hot/Warm comparison is local and runs first;
// the deposit-address lookup is remote and runs only if that passes. Cold is not refused.
type Destinations struct {
	System   SystemAddresses
	Deposits DepositLookup
}

func (d *Destinations) Refuse(ctx context.Context, addr common.Address) (string, error) {
	if addr == d.System.Hot || addr == d.System.Warm {
		return validate.DestinationIsSystem, nil
	}
	ours, err := d.Deposits.IsDepositAddress(ctx, addr)
	if err != nil {
		return "", err
	}
	if ours {
		return validate.DestinationIsDeposit, nil
	}
	return "", nil
}
