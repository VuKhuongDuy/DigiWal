// Package ledger is sweeper's client for ledger-service's POST /v1/ledger/gas-fees (ledger.md invariant
// #12; its first and only ledger-service call), over mTLS as CN=sweeper. sweeper never touches a
// balance: it only reports the gas its mined Gas Top-Ups and Sweeps spent.
package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
)

// ErrRefused: ledger-service will never accept this Gas Fee as sent (400, or 409: the same tx was
// reported with different values). Retrying cannot help; an operator must look.
var ErrRefused = errors.New("ledger-service refused the Gas Fee")

type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// GasFee is a mined tx's gas as its receipt reports it; ledger-service computes the fee.
type GasFee struct {
	ChainID  int64
	TxHash   string
	GasUsed  uint64
	GasPrice *big.Int // effective gas price, wei
	Purpose  string   // gas_topup | sweep
}

// PostGasFee is idempotent on the tx hash, so a retry after a lost response is safe.
func (c *Client) PostGasFee(ctx context.Context, g GasFee) error {
	b, _ := json.Marshal(map[string]any{
		"chain_id": g.ChainID, "tx_hash": g.TxHash, "gas_used": g.GasUsed,
		"effective_gas_price": g.GasPrice.String(), "purpose": g.Purpose,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/v1/ledger/gas-fees", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("calling ledger-service gas-fees: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusBadRequest, http.StatusConflict:
		return fmt.Errorf("%w (%d): %s", ErrRefused, resp.StatusCode, strings.TrimSpace(string(body)))
	default:
		return fmt.Errorf("ledger-service gas-fees answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}
