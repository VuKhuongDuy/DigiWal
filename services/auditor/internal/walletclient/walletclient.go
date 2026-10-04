// Package walletclient asks wallet-service which addresses are the treasury tiers
// (GET /v1/wallet/system-addresses, over mTLS as CN=auditor). It is the only wallet-service call the auditor
// makes, and the only endpoint wallet-service lets CN=auditor reach.
package walletclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Addresses are the tier addresses, lower case. Cold is empty when wallet-service has none declared.
type Addresses struct {
	ChainID         int64
	Hot, Warm, Cold string
}

type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// TLSConfig is the client side of mTLS: our leaf certificate (CN=auditor) and the project CA.
func TLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading client certificate: %w", err)
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificate in %s", caFile)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, MinVersion: tls.VersionTLS13}, nil
}

func isAddr(s string) bool {
	if len(s) != 42 || !strings.HasPrefix(s, "0x") {
		return false
	}
	for _, c := range strings.ToLower(s[2:]) {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return s != "0x0000000000000000000000000000000000000000"
}

// SystemAddresses asks once. Anything but a clean 200 with distinct, real Hot and Warm addresses (and a real
// Cold one if present) is an error: the auditor must not run with tier addresses it cannot trust.
func (c *Client) SystemAddresses(ctx context.Context) (Addresses, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+"/v1/wallet/system-addresses", nil)
	if err != nil {
		return Addresses{}, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Addresses{}, fmt.Errorf("calling wallet-service: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
	if resp.StatusCode != http.StatusOK {
		return Addresses{}, fmt.Errorf("wallet-service system-addresses answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		ChainID int64  `json:"chain_id"`
		Hot     string `json:"hot"`
		Warm    string `json:"warm"`
		Cold    string `json:"cold"`
	}
	if err := json.Unmarshal(body, &out); err != nil || !isAddr(out.Hot) || !isAddr(out.Warm) || (out.Cold != "" && !isAddr(out.Cold)) {
		return Addresses{}, fmt.Errorf("wallet-service returned unusable system addresses: %q", body)
	}
	a := Addresses{ChainID: out.ChainID, Hot: strings.ToLower(out.Hot), Warm: strings.ToLower(out.Warm), Cold: strings.ToLower(out.Cold)}
	if a.Hot == a.Warm || (a.Cold != "" && (a.Cold == a.Hot || a.Cold == a.Warm)) {
		return Addresses{}, fmt.Errorf("wallet-service returned overlapping tier addresses: %q", body)
	}
	return a, nil
}

// Fetch asks until wallet-service answers (it may still be starting), up to attempts times.
func (c *Client) Fetch(ctx context.Context, attempts int, wait time.Duration, logf func(string, ...any)) (Addresses, error) {
	var last error
	for i := 1; i <= attempts; i++ {
		a, err := c.SystemAddresses(ctx)
		if err == nil {
			return a, nil
		}
		last = err
		logf("asking wallet-service for the treasury addresses (attempt %d/%d): %v", i, attempts, err)
		select {
		case <-ctx.Done():
			return Addresses{}, ctx.Err()
		case <-time.After(wait):
		}
	}
	return Addresses{}, last
}
