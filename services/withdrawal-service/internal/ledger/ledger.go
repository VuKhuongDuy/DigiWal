// Package ledger is withdrawal-service's client for ledger-service's Lock/Settle/Release
// (ledger.md, integration-contracts.md §2), over mTLS as CN=withdrawal-service. Every call is
// keyed on the Withdrawal Key, never the client's idempotency_key (withdrawal-flow.md #20), and
// every call is idempotent on the ledger side, so a retry after a lost response is safe.
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
	"net/url"
	"strings"
)

var (
	// ErrInsufficientFunds: the user's Available Balance cannot cover the Lock. Nothing was locked.
	ErrInsufficientFunds = errors.New("insufficient available funds")
	// ErrConflict: the Lock was already resolved the other way (settled vs released), or the key was
	// reused for a different request. Never retried: a human must look.
	ErrConflict = errors.New("lock already resolved differently")
	// ErrLockNotFound: no Lock with that key exists.
	ErrLockNotFound = errors.New("no such lock")
)

type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func (c *Client) call(ctx context.Context, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("calling ledger-service %s: %w", path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
	return resp.StatusCode, out, nil
}

func unexpected(op string, code int, body []byte) error {
	return fmt.Errorf("ledger-service %s answered %d: %s", op, code, strings.TrimSpace(string(body)))
}

// Lock reserves amount of the user's Available Balance under key (the Reservation). No Entry is
// posted (withdrawal-flow.md invariant #1).
func (c *Client) Lock(ctx context.Context, key, userID, assetID string, amount *big.Int) error {
	code, body, err := c.call(ctx, "/v1/ledger/locks", map[string]string{
		"idempotency_key": key, "user_id": userID, "asset_id": assetID, "amount": amount.String(),
	})
	switch {
	case err != nil:
		return err
	case code == http.StatusOK:
		var out struct{ Status string }
		if json.Unmarshal(body, &out) != nil || out.Status != "active" {
			// A replay of a key that is already settled/released: not a fresh Reservation.
			return fmt.Errorf("%w: lock %s is %q", ErrConflict, key, out.Status)
		}
		return nil
	case code == http.StatusUnprocessableEntity:
		return ErrInsufficientFunds
	case code == http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrConflict, strings.TrimSpace(string(body)))
	default:
		return unexpected("lock", code, body)
	}
}

// Settled is what a Settle posted.
type Settled struct {
	LedgerTransactionID string
	EntryIDs            []string // the debit and the chain-account credit: what a refund corrects
}

// Settle turns the Lock into the real debit (withdrawal-flow.md invariant #3: the only point where
// an Entry is posted). Idempotent: a replay returns the same transaction and entries.
func (c *Client) Settle(ctx context.Context, key string) (Settled, error) {
	code, body, err := c.call(ctx, "/v1/ledger/locks/"+url.PathEscape(key)+"/settle", nil)
	switch {
	case err != nil:
		return Settled{}, err
	case code == http.StatusOK:
		var out struct {
			LedgerTransactionID string   `json:"ledger_transaction_id"`
			EntryIDs            []string `json:"entry_ids"`
		}
		if json.Unmarshal(body, &out) != nil || out.LedgerTransactionID == "" || len(out.EntryIDs) == 0 {
			return Settled{}, fmt.Errorf("ledger-service settle returned %q", body)
		}
		return Settled{LedgerTransactionID: out.LedgerTransactionID, EntryIDs: out.EntryIDs}, nil
	case code == http.StatusConflict:
		return Settled{}, fmt.Errorf("%w: %s", ErrConflict, strings.TrimSpace(string(body)))
	case code == http.StatusNotFound:
		return Settled{}, ErrLockNotFound
	default:
		return Settled{}, unexpected("settle", code, body)
	}
}

// Refund posts the Compensating Entry for a Settled withdrawal whose tx never landed (task 3.5,
// withdrawal-flow.md invariant #9): a correction of exactly the Settle's entries. withdrawal-service
// names only which entries; ledger-service derives accounts and amounts as their exact negation
// (ledger.md invariant #11). Keyed {Withdrawal Key}:refund, so a retry can never refund twice.
func (c *Client) Refund(ctx context.Context, key string, settleEntryIDs []string) error {
	entries := make([]map[string]string, 0, len(settleEntryIDs))
	for _, id := range settleEntryIDs {
		entries = append(entries, map[string]string{"corrects_entry_id": id})
	}
	code, body, err := c.call(ctx, "/v1/ledger/transactions", map[string]any{
		"idempotency_key": key + ":refund", "source_type": "correction", "entries": entries,
	})
	switch {
	case err != nil:
		return err
	case code == http.StatusOK, code == http.StatusCreated:
		return nil
	case code == http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrConflict, strings.TrimSpace(string(body)))
	default:
		return unexpected("refund", code, body)
	}
}

// Release gives the Lock's capacity back; nothing was ever posted, so nothing is compensated
// (withdrawal-flow.md invariant #3). A Lock that never existed is fine: there is nothing to give back.
func (c *Client) Release(ctx context.Context, key string) error {
	code, body, err := c.call(ctx, "/v1/ledger/locks/"+url.PathEscape(key)+"/release", nil)
	switch {
	case err != nil:
		return err
	case code == http.StatusOK, code == http.StatusNotFound:
		return nil
	case code == http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrConflict, strings.TrimSpace(string(body)))
	default:
		return unexpected("release", code, body)
	}
}
