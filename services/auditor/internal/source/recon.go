package source

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ReconAsset is a registered asset.
type ReconAsset struct {
	ID, Type, Contract, Symbol string
	Decimals                   int
}

// LedgerFigures are one asset's numbers from ledger-service, read in one snapshot.
type LedgerFigures struct {
	ChainAccount, UserTotal, ActiveLocks, Credited, FlaggedHeld *big.Int
}

// Recon reads what the aggregate reconciliation needs from every store.
type Recon struct {
	Ledger     *PG
	Watcher    Querier
	Withdrawal Querier
	Wallet     Querier
	Chain      ReconChain
	TxChain    Chain // withdrawal transactions in flight
	ChainID    int64
}

func zero() *big.Int { return new(big.Int) }

func (r *Recon) Assets(ctx context.Context) ([]ReconAsset, error) {
	rows, err := r.Ledger.Query(ctx, `SELECT id::text, asset_type, lower(coalesce(contract_address, '')), symbol, decimals FROM assets ORDER BY symbol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReconAsset
	for rows.Next() {
		var a ReconAsset
		if err := rows.Scan(&a.ID, &a.Type, &a.Contract, &a.Symbol, &a.Decimals); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DepositAddresses are our users' deposit addresses (lower case). Every one is queried on chain each run:
// fine for the dev environment this tool is limited to.
func (r *Recon) DepositAddresses(ctx context.Context) ([]string, error) {
	rows, err := r.Wallet.Query(ctx, `SELECT lower(address) FROM wallet_addresses ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func bigMap(ctx context.Context, q Querier, sql string, args ...any) (map[string]*big.Int, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*big.Int{}
	for rows.Next() {
		var id, v string
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		n, ok := new(big.Int).SetString(v, 10)
		if !ok {
			return nil, fmt.Errorf("not a number: %q", v)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// LedgerFigures reads, per asset, the Chain Account, user balances, active Locks, credited deposits and held
// Flagged Deposits, in one REPEATABLE READ snapshot.
func (r *Recon) LedgerFigures(ctx context.Context) (map[string]*LedgerFigures, error) {
	out := map[string]*LedgerFigures{}
	get := func(id string) *LedgerFigures {
		if out[id] == nil {
			out[id] = &LedgerFigures{zero(), zero(), zero(), zero(), zero()}
		}
		return out[id]
	}
	err := r.Ledger.Snapshot(ctx, func(q Querier) error {
		for _, f := range []struct {
			sql string
			set func(*LedgerFigures, *big.Int)
		}{
			{`SELECT asset_id::text, sum(total_balance)::text FROM accounts WHERE kind = 'chain' GROUP BY asset_id`, func(l *LedgerFigures, v *big.Int) { l.ChainAccount = v }},
			{`SELECT asset_id::text, sum(total_balance)::text FROM accounts WHERE kind = 'user' GROUP BY asset_id`, func(l *LedgerFigures, v *big.Int) { l.UserTotal = v }},
			{`SELECT a.asset_id::text, sum(l.amount)::text FROM locks l JOIN accounts a ON a.id = l.account_id WHERE l.status = 'active' GROUP BY a.asset_id`, func(l *LedgerFigures, v *big.Int) { l.ActiveLocks = v }},
			{`SELECT a.asset_id::text, sum(e.amount)::text FROM entries e JOIN accounts a ON a.id = e.account_id
				JOIN ledger_transactions t ON t.id = e.ledger_transaction_id WHERE t.source_type = 'deposit' AND a.kind = 'user' GROUP BY a.asset_id`, func(l *LedgerFigures, v *big.Int) { l.Credited = v }},
			{`SELECT asset_id::text, sum(amount)::text FROM flagged_deposits WHERE status IN ('pending', 'rejected') GROUP BY asset_id`, func(l *LedgerFigures, v *big.Int) { l.FlaggedHeld = v }},
		} {
			m, err := bigMap(ctx, q, f.sql)
			if err != nil {
				return err
			}
			for id, v := range m {
				f.set(get(id), v)
			}
		}
		return nil
	})
	return out, err
}

// ObservedUpTo is, per asset, the sum of transfers to deposit addresses chain-watcher saw mined at or before block.
func (r *Recon) ObservedUpTo(ctx context.Context, block int64) (map[string]*big.Int, error) {
	return bigMap(ctx, r.Watcher, `SELECT asset_id::text, sum(amount)::text FROM observed_transfers
		WHERE chain_id = $1 AND block_number <= $2 AND status IN ('pending', 'confirmed') GROUP BY asset_id`, r.ChainID, block)
}

// InFlight returns, per asset, the amount of withdrawals whose tx is mined at or before block but not yet Settled,
// and the native gas those txs and the not-yet-posted Gas Fees account for.
func (r *Recon) InFlight(ctx context.Context, block int64) (unsettled map[string]*big.Int, gas *big.Int, err error) {
	unsettled, gas = map[string]*big.Int{}, zero()

	rows, err := r.Withdrawal.Query(ctx, `SELECT withdrawal_id::text, asset_id, amount::text, coalesce(tx_hash, '') FROM withdrawal_requests WHERE status = 'broadcast'`)
	if err != nil {
		return nil, nil, err
	}
	type req struct {
		id, asset, amount string
		hashes            []string
	}
	var reqs []*req
	var ids []string
	for rows.Next() {
		q := &req{}
		var h string
		if err := rows.Scan(&q.id, &q.asset, &q.amount, &h); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if h != "" {
			q.hashes = append(q.hashes, strings.ToLower(h))
		}
		reqs = append(reqs, q)
		ids = append(ids, q.id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(ids) > 0 {
		vr, err := r.Withdrawal.Query(ctx, `SELECT withdrawal_id::text, lower(tx_hash) FROM withdrawal_txs WHERE withdrawal_id = ANY($1::uuid[])`, ids)
		if err != nil {
			return nil, nil, err
		}
		byID := map[string]*req{}
		for _, q := range reqs {
			byID[q.id] = q
		}
		for vr.Next() {
			var id, h string
			if err := vr.Scan(&id, &h); err != nil {
				vr.Close()
				return nil, nil, err
			}
			byID[id].hashes = append(byID[id].hashes, h)
		}
		vr.Close()
		if err := vr.Err(); err != nil {
			return nil, nil, err
		}
	}
	for _, q := range reqs {
		seen := map[string]bool{}
		for _, h := range q.hashes {
			if seen[h] {
				continue
			}
			seen[h] = true
			c, err := r.TxChain.Tx(ctx, h)
			if err != nil {
				return nil, nil, err
			}
			if !c.Found || !c.Mined || c.Block > block {
				continue
			}
			if c.Success {
				amt, _ := new(big.Int).SetString(q.amount, 10)
				if unsettled[q.asset] == nil {
					unsettled[q.asset] = zero()
				}
				unsettled[q.asset].Add(unsettled[q.asset], amt)
			}
			if c.GasFee != nil {
				gas.Add(gas, c.GasFee)
			}
			break // only one version of a tx is ever mined
		}
	}

	// gas of mined requests that withdrawal-service has recorded but not yet posted to the ledger
	var pending *string
	err = r.Withdrawal.QueryRow(ctx, `SELECT coalesce(sum(gas_used * gas_price), 0)::text FROM withdrawal_requests
		WHERE gas_tx_hash IS NOT NULL AND gas_posted_at IS NULL`).Scan(&pending)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, err
	}
	if pending != nil {
		if n, ok := new(big.Int).SetString(*pending, 10); ok {
			gas.Add(gas, n)
		}
	}
	return unsettled, gas, nil
}
