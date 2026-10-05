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
	"net/url"
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

// SystemAddresses are the Hot and Warm Wallet addresses, and the Cold Wallet's if the operator declared one
// (GET /v1/wallet/system-addresses). Cold is the zero address when there is none. It is read for the balance
// metrics only: withdrawals to Cold stay allowed (withdrawal-flow.md invariant #18).
type SystemAddresses struct {
	ChainID int64
	Hot     common.Address
	Warm    common.Address
	Cold    common.Address
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
		Cold    string `json:"cold"`
	}
	if err := json.Unmarshal(body, &out); err != nil || !common.IsHexAddress(out.Hot) || !common.IsHexAddress(out.Warm) || (out.Cold != "" && !common.IsHexAddress(out.Cold)) {
		return SystemAddresses{}, fmt.Errorf("wallet-service returned unusable system addresses: %q", body)
	}
	a := SystemAddresses{ChainID: out.ChainID, Hot: common.HexToAddress(out.Hot), Warm: common.HexToAddress(out.Warm)}
	if out.Cold != "" {
		a.Cold = common.HexToAddress(out.Cold)
		if a.Cold == (common.Address{}) || a.Cold == a.Hot || a.Cold == a.Warm {
			return SystemAddresses{}, fmt.Errorf("wallet-service returned unusable system addresses: %q", body)
		}
	}
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

// ErrKeyVoided: wallet-service answered 410 — this key was voided (the Reaper's doing), so it is
// never signed and no nonce was allocated. The request is being (or has been) reaped.
var ErrKeyVoided = errors.New("wallet-service voided this key")

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
	case http.StatusGone:
		return "", 0, fmt.Errorf("%w: %s", ErrKeyVoided, strings.TrimSpace(string(body)))
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

// VoidResult is wallet-service's answer to a void (invariant #22): the key is now never signed
// (Voided), or it had already been signed, and this is that signature.
type VoidResult struct {
	Voided bool
	RawTx  string
	Nonce  uint64
}

// VoidHotWallet is POST /v1/sign/hot-wallet/{key}/void. Done under the lock signing takes, so either
// a sign landed first (already_signed) or any later sign of the key is refused and allocates nothing:
// the only safe basis for Releasing a `reserved` request. Idempotent.
func (c *Client) VoidHotWallet(ctx context.Context, key string) (VoidResult, error) {
	code, body, err := c.do(ctx, http.MethodPost, "/v1/sign/hot-wallet/"+url.PathEscape(key)+"/void", []byte("{}"))
	if err != nil {
		return VoidResult{}, err
	}
	if code != http.StatusOK {
		return VoidResult{}, fmt.Errorf("wallet-service void answered %d: %s", code, strings.TrimSpace(string(body)))
	}
	var out struct {
		Status      string `json:"status"`
		Nonce       uint64 `json:"nonce"`
		SignedRawTx string `json:"signed_raw_tx"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return VoidResult{}, fmt.Errorf("wallet-service returned an unusable void answer: %q", body)
	}
	switch out.Status {
	case "voided":
		return VoidResult{Voided: true}, nil
	case "already_signed":
		if !strings.HasPrefix(out.SignedRawTx, "0x") {
			return VoidResult{}, fmt.Errorf("wallet-service returned an unusable void answer: %q", body)
		}
		return VoidResult{RawTx: out.SignedRawTx, Nonce: out.Nonce}, nil
	}
	return VoidResult{}, fmt.Errorf("wallet-service returned an unknown void status %q", out.Status)
}

// Replacement is a Fee Bump version wallet-service signed: the same tx at the same nonce.
type Replacement struct {
	RawTx string
	Hash  string
	Nonce uint64
	Seq   int
}

// Why a replace was refused. Both mean: alert an operator; neither may be retried blindly.
var (
	// ErrReplaceCeiling: the bump would need a fee above wallet-service's ceiling (422).
	ErrReplaceCeiling = errors.New("wallet-service refused the Fee Bump: fee ceiling")
	// ErrReplaceRefused: 404 (never signed) or 409 (a Nonce Filler is the latest version, a
	// replacement_seq gap, or not our key).
	ErrReplaceRefused = errors.New("wallet-service refused the Fee Bump")
)

// ReplaceHotWallet is POST /v1/sign/hot-wallet/{key}/replace {replacement_seq}: the same tx re-signed
// at the same nonce with higher fees (invariant #21). The same seq replays the stored replacement, so
// a retry after a lost answer is safe. 503 (a Nonce Gap freeze) and anything else is transient.
func (c *Client) ReplaceHotWallet(ctx context.Context, key string, seq int) (Replacement, error) {
	b, _ := json.Marshal(map[string]any{"replacement_seq": seq})
	code, body, err := c.do(ctx, http.MethodPost, "/v1/sign/hot-wallet/"+url.PathEscape(key)+"/replace", b)
	if err != nil {
		return Replacement{}, err
	}
	switch code {
	case http.StatusOK:
		var out struct {
			SignedRawTx    string `json:"signed_raw_tx"`
			TxHash         string `json:"tx_hash"`
			Nonce          uint64 `json:"nonce"`
			ReplacementSeq int    `json:"replacement_seq"`
		}
		if err := json.Unmarshal(body, &out); err != nil || !strings.HasPrefix(out.SignedRawTx, "0x") || out.ReplacementSeq != seq {
			return Replacement{}, fmt.Errorf("wallet-service returned an unusable replacement: %q", body)
		}
		return Replacement{RawTx: out.SignedRawTx, Hash: strings.ToLower(out.TxHash), Nonce: out.Nonce, Seq: out.ReplacementSeq}, nil
	case http.StatusUnprocessableEntity:
		return Replacement{}, fmt.Errorf("%w: %s", ErrReplaceCeiling, strings.TrimSpace(string(body)))
	case http.StatusNotFound, http.StatusConflict:
		return Replacement{}, fmt.Errorf("%w (%d): %s", ErrReplaceRefused, code, strings.TrimSpace(string(body)))
	default:
		return Replacement{}, fmt.Errorf("wallet-service replace answered %d: %s", code, strings.TrimSpace(string(body)))
	}
}

// CancelHashes is the tx hashes of the Nonce Fillers wallet-service signed for a key (kind `cancel`
// in GET /v1/sign/hot-wallet/{key}); empty when the key has none. The Reaper asks for them to see
// whether a filler was mined (its gas is ours to record, ledger.md invariant #12).
func (c *Client) CancelHashes(ctx context.Context, key string) ([]string, error) {
	code, body, err := c.get(ctx, "/v1/sign/hot-wallet/"+url.PathEscape(key))
	if err != nil {
		return nil, err
	}
	switch code {
	case http.StatusOK:
		var out struct {
			Replacements []struct {
				Kind   string `json:"kind"`
				TxHash string `json:"tx_hash"`
			} `json:"replacements"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("wallet-service returned an unusable key view: %q", body)
		}
		var hashes []string
		for _, r := range out.Replacements {
			if r.Kind == "cancel" {
				hashes = append(hashes, strings.ToLower(r.TxHash))
			}
		}
		return hashes, nil
	case http.StatusNotFound:
		return nil, nil
	default:
		return nil, fmt.Errorf("wallet-service key lookup answered %d: %s", code, strings.TrimSpace(string(body)))
	}
}
