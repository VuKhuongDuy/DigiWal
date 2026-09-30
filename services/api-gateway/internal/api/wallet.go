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

// DepositAddress returns wallet-service's answer for the user's address, creating it if the
// user has none. It calls GET first (the documented endpoint) and only on a 404 the
// idempotent POST that issues the address — the design lists GET alone, but nothing else
// would ever create the first address (a gap reported to the Tech Lead).
func (c *WalletClient) DepositAddress(ctx context.Context, userID string) (int, []byte, error) {
	u := strings.TrimRight(c.BaseURL, "/") + "/v1/users/" + url.PathEscape(userID) + "/deposit-address"
	status, body, err := c.do(ctx, http.MethodGet, u)
	if err != nil || status != http.StatusNotFound {
		return status, body, err
	}
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
