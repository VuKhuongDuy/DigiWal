package source

import (
	"context"
	"errors"
	"strings"

	"digiwal/services/auditor/internal/rules/treasury"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v5"
)

// Treasury reads what the T rules need: wallet-service's record of every Hot Wallet signature, the
// registry of assets, our users' deposit addresses, and the chain.
type Treasury struct {
	Wallet, Ledger, Withdrawal Querier
	Chain                      TreasuryChain
	ChainID                    int64
}

// NextNonce is wallet-service's next Hot Wallet nonce for the chain; ok is false if none was ever allocated.
func (t *Treasury) NextNonce(ctx context.Context) (n uint64, ok bool, err error) {
	var v int64
	err = t.Wallet.QueryRow(ctx, `SELECT next_nonce FROM hot_wallet_nonces WHERE chain_id = $1`, t.ChainID).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return uint64(v), err == nil, err
}

// Known loads every tx hash wallet-service ever signed for the Hot Wallet on this chain: the hash of each
// original signed tx, and of each Fee Bump and cancel.
func (t *Treasury) Known(ctx context.Context) (treasury.Known, error) {
	k := treasury.Known{Hashes: map[string]bool{}, Nonces: map[uint64]bool{}}
	rows, err := t.Wallet.Query(ctx, `SELECT nonce, signed_raw_tx FROM hot_wallet_signatures WHERE chain_id = $1`, t.ChainID)
	if err != nil {
		return k, err
	}
	for rows.Next() {
		var nonce int64
		var raw []byte
		if err := rows.Scan(&nonce, &raw); err != nil {
			rows.Close()
			return k, err
		}
		k.Nonces[uint64(nonce)] = true
		var tx types.Transaction
		if err := tx.UnmarshalBinary(raw); err != nil {
			continue // an unreadable raw tx proves nothing; its nonce is still known
		}
		k.Hashes[strings.ToLower(tx.Hash().Hex())] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return k, err
	}
	rows, err = t.Wallet.Query(ctx, `SELECT r.nonce, r.tx_hash FROM hot_wallet_replacements r
		JOIN hot_wallet_signatures s ON s.idempotency_key = r.idempotency_key WHERE s.chain_id = $1`, t.ChainID)
	if err != nil {
		return k, err
	}
	defer rows.Close()
	for rows.Next() {
		var nonce int64
		var h string
		if err := rows.Scan(&nonce, &h); err != nil {
			return k, err
		}
		k.Nonces[uint64(nonce)] = true
		k.Hashes[strings.ToLower(h)] = true
	}
	return k, rows.Err()
}

// Signatures lists every Hot Wallet signature on the chain, with whether a withdrawal key has its request.
func (t *Treasury) Signatures(ctx context.Context) ([]treasury.Signature, error) {
	rows, err := t.Wallet.Query(ctx, `SELECT idempotency_key, caller_cn FROM hot_wallet_signatures WHERE chain_id = $1 ORDER BY created_at, idempotency_key`, t.ChainID)
	if err != nil {
		return nil, err
	}
	var out []treasury.Signature
	var ids []string
	for rows.Next() {
		var s treasury.Signature
		var cn *string
		if err := rows.Scan(&s.Key, &cn); err != nil {
			rows.Close()
			return nil, err
		}
		s.CallerCN = deref(cn)
		out = append(out, s)
		if id := strings.TrimPrefix(s.Key, "withdrawal:"); id != s.Key && uuidRe.MatchString(id) {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	have := map[string]bool{}
	if len(ids) > 0 {
		r, err := t.Withdrawal.Query(ctx, `SELECT withdrawal_id::text FROM withdrawal_requests WHERE withdrawal_id = ANY($1::uuid[])`, ids)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		for r.Next() {
			var id string
			if err := r.Scan(&id); err != nil {
				return nil, err
			}
			have[id] = true
		}
		if err := r.Err(); err != nil {
			return nil, err
		}
	}
	for i := range out {
		out[i].WithdrawalExists = have[strings.TrimPrefix(out[i].Key, "withdrawal:")]
	}
	return out, nil
}

// Assets maps each ERC-20 contract (lower case) to its asset id, and gives the native asset's id.
func (t *Treasury) Assets(ctx context.Context) (tokens map[string]string, native string, err error) {
	tokens = map[string]string{}
	rows, err := t.Ledger.Query(ctx, `SELECT id::text, asset_type, lower(contract_address) FROM assets`)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var id, typ string
		var contract *string
		if err := rows.Scan(&id, &typ, &contract); err != nil {
			return nil, "", err
		}
		if typ == "native" {
			native = id
		} else if contract != nil {
			tokens[*contract] = id
		}
	}
	return tokens, native, rows.Err()
}

// DepositAddresses is the set of our users' deposit addresses (lower case).
func (t *Treasury) DepositAddresses(ctx context.Context) (map[string]bool, error) {
	rows, err := t.Wallet.Query(ctx, `SELECT lower(address) FROM wallet_addresses`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out[a] = true
	}
	return out, rows.Err()
}
