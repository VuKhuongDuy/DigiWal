package source

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"digiwal/services/auditor/internal/rules/deposit"

	"github.com/jackc/pgx/v5"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Deposits reads everything the D rules need. A deposit is identified by "chain_id:tx_hash:log_index"
// (log_index -1 for the native coin), the same key the ledger and history-service use.
type Deposits struct {
	Watcher, Ledger, Wallet Querier
	History                 HistoryReader
	Chain                   DepositChain
	Hot                     string // wallet-service's Hot Wallet address; its Gas Top-Ups are not deposits (D7)
}

// DepositKey is the reference of a deposit.
func DepositKey(chainID int64, txHash string, logIndex int64) string {
	return fmt.Sprintf("%d:%s:%d", chainID, strings.ToLower(txHash), logIndex)
}

func parseDepositKey(ref string) (chainID int64, tx string, log int64, err error) {
	p := strings.Split(ref, ":")
	if len(p) != 3 {
		return 0, "", 0, fmt.Errorf("bad deposit reference %q", ref)
	}
	if chainID, err = strconv.ParseInt(p[0], 10, 64); err != nil {
		return 0, "", 0, fmt.Errorf("bad deposit reference %q", ref)
	}
	if log, err = strconv.ParseInt(p[2], 10, 64); err != nil {
		return 0, "", 0, fmt.Errorf("bad deposit reference %q", ref)
	}
	return chainID, strings.ToLower(p[1]), log, nil
}

// splitCursor turns a Ref into the key tuple a keyset query resumes after; the zero Ref sorts first.
func splitCursor(r Ref) (chainID int64, tx string, log int64) {
	if r.ID == "" {
		return 0, "", -2
	}
	c, t, l, err := parseDepositKey(r.ID)
	if err != nil {
		return 0, "", -2
	}
	return c, t, l
}

// ListConfirmed returns up to limit observed transfers confirmed after `after` and no later than notAfter,
// oldest first.
func (d *Deposits) ListConfirmed(ctx context.Context, after Ref, notAfter time.Time, limit int) ([]Ref, error) {
	c, t, l := splitCursor(after)
	rows, err := d.Watcher.Query(ctx, `
		SELECT chain_id, lower(tx_hash), log_index, confirmed_at FROM observed_transfers
		WHERE status = 'confirmed' AND confirmed_at IS NOT NULL
		  AND (confirmed_at, chain_id, lower(tx_hash), log_index) > ($1, $2, $3, $4) AND confirmed_at <= $5
		ORDER BY confirmed_at, chain_id, lower(tx_hash), log_index LIMIT $6`, after.At, c, t, l, notAfter, limit)
	if err != nil {
		return nil, err
	}
	return scanKeys(rows)
}

// ListLedgerDeposits returns up to limit deposit Ledger Transactions after `after`, no newer than notAfter.
// It finds credits that have no observed transfer behind them (D2).
func (d *Deposits) ListLedgerDeposits(ctx context.Context, after Ref, notAfter time.Time, limit int) ([]Ref, error) {
	c, t, l := splitCursor(after)
	rows, err := d.Ledger.Query(ctx, `
		SELECT chain_id, lower(tx_hash), log_index, created_at FROM ledger_transactions
		WHERE source_type = 'deposit' AND chain_id IS NOT NULL
		  AND (created_at, chain_id, lower(tx_hash), log_index) > ($1, $2, $3, $4) AND created_at <= $5
		ORDER BY created_at, chain_id, lower(tx_hash), log_index LIMIT $6`, after.At, c, t, l, notAfter, limit)
	if err != nil {
		return nil, err
	}
	return scanKeys(rows)
}

func scanKeys(rows pgx.Rows) ([]Ref, error) {
	defer rows.Close()
	var out []Ref
	for rows.Next() {
		var (
			chainID, log int64
			tx           string
			at           time.Time
		)
		if err := rows.Scan(&chainID, &tx, &log, &at); err != nil {
			return nil, err
		}
		out = append(out, Ref{ID: DepositKey(chainID, tx, log), At: at})
	}
	return out, rows.Err()
}

// Observed reports whether chain-watcher has any record of the transfer.
func (d *Deposits) Observed(ctx context.Context, ref string) (bool, error) {
	c, t, l, err := parseDepositKey(ref)
	if err != nil {
		return false, nil
	}
	var n int
	err = d.Watcher.QueryRow(ctx, `SELECT count(*) FROM observed_transfers WHERE chain_id = $1 AND lower(tx_hash) = $2 AND log_index = $3`, c, t, l).Scan(&n)
	return n > 0, err
}

// Gather reads one deposit from every store and the chain.
func (d *Deposits) Gather(ctx context.Context, ref string) (deposit.Facts, error) {
	var f deposit.Facts
	chainID, tx, log, err := parseDepositKey(ref)
	if err != nil {
		return f, nil // a reference we cannot parse has nothing behind it
	}

	f.Observed, err = d.observed(ctx, chainID, tx, log)
	if err != nil {
		return f, err
	}
	if err := d.ledgerFacts(ctx, chainID, tx, log, &f); err != nil {
		return f, err
	}
	if f.Chain, err = d.Chain.Transfer(ctx, tx, log); err != nil {
		return f, err
	}
	if f.Chain.Found {
		if f.Chain.AssetID, err = d.assetOf(ctx, f.Chain.Token); err != nil {
			return f, err
		}
	}
	to := ""
	switch {
	case f.Observed != nil:
		to = f.Observed.To
	case f.Chain.Found:
		to = f.Chain.To
	}
	if to != "" {
		if f.Owner, err = d.owner(ctx, to); err != nil {
			return f, err
		}
	}
	f.Hot = d.Hot
	f.History, err = d.history(ctx, "deposit:"+ref)
	return f, err
}

func (d *Deposits) observed(ctx context.Context, chainID int64, tx string, log int64) (*deposit.Observed, error) {
	var o deposit.Observed
	var amount string
	err := d.Watcher.QueryRow(ctx, `
		SELECT asset_id::text, to_address, user_id, amount::text, block_number, block_hash, status FROM observed_transfers
		WHERE chain_id = $1 AND lower(tx_hash) = $2 AND log_index = $3`, chainID, tx, log).
		Scan(&o.AssetID, &o.To, &o.UserID, &amount, &o.Block, &o.BlockHash, &o.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("observed transfer: %w", err)
	}
	o.Amount = parseBig(amount)
	return &o, nil
}

func (d *Deposits) ledgerFacts(ctx context.Context, chainID int64, tx string, log int64, f *deposit.Facts) error {
	var id string
	err := d.Ledger.QueryRow(ctx, `SELECT id::text FROM ledger_transactions
		WHERE source_type = 'deposit' AND chain_id = $1 AND lower(tx_hash) = $2 AND log_index = $3`, chainID, tx, log).Scan(&id)
	switch {
	case err == nil:
		l := &deposit.Ledger{ID: id}
		var amount string
		err := d.Ledger.QueryRow(ctx, `SELECT a.user_id, a.asset_id::text, e.amount::text
			FROM entries e JOIN accounts a ON a.id = e.account_id
			WHERE e.ledger_transaction_id = $1::uuid AND a.kind = 'user'`, id).Scan(&l.UserID, &l.AssetID, &amount)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("deposit entries: %w", err)
		}
		l.Amount = parseBig(amount)
		f.Ledger = l
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("deposit transaction: %w", err)
	}
	var n int
	if err := d.Ledger.QueryRow(ctx, `SELECT count(*) FROM flagged_deposits WHERE chain_id = $1 AND lower(tx_hash) = $2 AND log_index = $3`, chainID, tx, log).Scan(&n); err != nil {
		return fmt.Errorf("flagged deposits: %w", err)
	}
	f.Flagged = n > 0
	return nil
}

// assetOf maps a token contract ("" = the native coin) to the ledger's asset id, "" if unregistered.
func (d *Deposits) assetOf(ctx context.Context, token string) (string, error) {
	var id string
	var err error
	if token == "" {
		err = d.Ledger.QueryRow(ctx, `SELECT id::text FROM assets WHERE asset_type = 'native'`).Scan(&id)
	} else {
		err = d.Ledger.QueryRow(ctx, `SELECT id::text FROM assets WHERE asset_type = 'erc20' AND lower(contract_address) = $1`, token).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (d *Deposits) owner(ctx context.Context, addr string) (string, error) {
	var u string
	err := d.Wallet.QueryRow(ctx, `SELECT user_id FROM wallet_addresses WHERE lower(address) = $1`, strings.ToLower(addr)).Scan(&u)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return u, err
}

func (d *Deposits) history(ctx context.Context, id string) (*deposit.History, error) {
	doc, err := d.History.FindOne(ctx, "history", id)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("history %s: %w", id, err)
	}
	s := func(k string) string { v, _ := doc[k].(string); return v }
	return &deposit.History{Status: s("status"), Amount: s("amount"), AssetID: s("asset_id")}, nil
}

// Watermark is chain-watcher's last fully scanned block; ok is false before its first scan.
func (d *Deposits) Watermark(ctx context.Context, chainID int64) (n int64, ok bool, err error) {
	err = d.Watcher.QueryRow(ctx, `SELECT block_number FROM watermark WHERE chain_id = $1`, chainID).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return n, err == nil, err
}

// ScanTargets returns the ERC-20 contracts to watch and the set of deposit addresses (lower case). The
// whole address set is loaded: fine for the dev environment this tool is limited to.
func (d *Deposits) ScanTargets(ctx context.Context) (tokens []string, addrs map[string]bool, err error) {
	rows, err := d.Ledger.Query(ctx, `SELECT lower(contract_address) FROM assets WHERE asset_type = 'erc20' AND contract_address IS NOT NULL`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return nil, nil, err
		}
		tokens = append(tokens, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	rows, err = d.Wallet.Query(ctx, `SELECT lower(address) FROM wallet_addresses`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	addrs = map[string]bool{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, nil, err
		}
		addrs[a] = true
	}
	return tokens, addrs, rows.Err()
}
