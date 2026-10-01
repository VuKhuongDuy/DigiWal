// Package wallet is withdrawal-service's client for wallet-service, over mTLS as
// CN=withdrawal-service (service-auth-mtls.md), and the invariant #18 destination check built on it.
package wallet

import (
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+path, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("calling wallet-service %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
	return resp.StatusCode, body, nil
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
