package vault

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Gateway is the Unsealer that talks to hsm-gateway: POST /v1/vault/unseal over mTLS as this service's own pinned leaf (CN sweep-signer),
// the one caller the gateway lets unseal. One attempt per call: a Sweep that cannot get the Seed fails and sweeper retries it.
type Gateway struct {
	BaseURL string
	HTTP    *http.Client
}

// NewGateway builds the client from a TLS config that carries the sweep-signer leaf (hotkey.ClientTLS).
func NewGateway(baseURL string, tlsCfg *tls.Config) *Gateway {
	return &Gateway{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{
		Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}}
}

type wireRow struct {
	Role       string `json:"role"`
	Purpose    string `json:"purpose"`
	Version    int    `json:"version"`
	KEKVersion int    `json:"kek_version"`
	WrappedDEK []byte `json:"wrapped_dek"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
	AAD        []byte `json:"aad"`
}

// UnsealInto implements Unsealer. On 200 the body is the raw plaintext (application/octet-stream), read directly into dst; any other
// answer is a JSON error. A body longer than dst, or a 200 that is not octet-stream, is refused and dst is cleared.
func (g *Gateway) UnsealInto(ctx context.Context, row Row, dst []byte) (int, error) {
	body, err := json.Marshal(wireRow(row))
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.BaseURL+"/v1/vault/unseal", strings.NewReader(string(body)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("hsm-gateway unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		var e struct {
			Error string `json:"error"`
			Rule  string `json:"rule"`
		}
		_ = json.Unmarshal(b, &e)
		switch {
		case resp.StatusCode == http.StatusUnprocessableEntity && e.Error == "authentication_failed":
			return 0, ErrAuthFailed
		case resp.StatusCode == http.StatusTooManyRequests:
			return 0, errors.New("hsm-gateway: unseal rate limit reached")
		}
		return 0, fmt.Errorf("hsm-gateway POST /v1/vault/unseal: HTTP %d %s %s", resp.StatusCode, e.Error, e.Rule)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		return 0, fmt.Errorf("hsm-gateway answered 200 with content type %q, not the raw seed", ct)
	}
	n, err := io.ReadFull(resp.Body, dst)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		clear(dst)
		return 0, fmt.Errorf("reading the seed from hsm-gateway: %w", err)
	}
	// anything beyond len(dst) is not a seed
	var extra [1]byte
	if m, _ := resp.Body.Read(extra[:]); m > 0 {
		clear(dst)
		return 0, errors.New("hsm-gateway returned more bytes than a seed")
	}
	return n, nil
}
