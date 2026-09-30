package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// WalletClient calls wallet-service's deposit-address endpoints. Those are still on
// wallet-service's plain listener: moving them behind mTLS with CN=api-gateway is deferred
// (service-auth-mtls.md Open Question #4), so there is no client certificate yet.
type WalletClient struct {
	BaseURL string
	HTTP    *http.Client
}

// DepositAddress returns the user's deposit address via wallet-service's POST, which is
// idempotent get-or-create: an existing address comes back as-is, a first-time user's is
// derived on the spot, so the client never sees a 404 (integration-contracts.md §2, decided).
func (c *WalletClient) DepositAddress(ctx context.Context, userID string) (int, []byte, error) {
	u := strings.TrimRight(c.BaseURL, "/") + "/v1/users/" + url.PathEscape(userID) + "/deposit-address"
	return c.do(ctx, http.MethodPost, u)
}

func (c *WalletClient) do(ctx context.Context, method, u string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return 0, nil, fmt.Errorf("reading wallet-service response: %w", err)
	}
	return resp.StatusCode, body, nil
}
