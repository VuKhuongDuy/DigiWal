package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// LedgerClient reads balances from ledger-service over mTLS as CN=api-gateway
// (GET /v1/ledger/balances/{user_id}, integration-contracts.md §2).
type LedgerClient struct {
	BaseURL string
	HTTP    *http.Client
}

// Balances returns ledger-service's answer for the user, verbatim. assetID may be empty.
func (c *LedgerClient) Balances(ctx context.Context, userID, assetID string) (int, []byte, error) {
	u := strings.TrimRight(c.BaseURL, "/") + "/v1/ledger/balances/" + url.PathEscape(userID)
	if assetID != "" {
		u += "?asset_id=" + url.QueryEscape(assetID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("reading ledger-service response: %w", err)
	}
	return resp.StatusCode, body, nil
}
