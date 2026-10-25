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
	// Hot is the CURRENT Hot address (lower case): the one wallet-service signs with now. Hot Wallet nonce
	// state is per (chain_id, hot_address) (wallet-service migration 000005, key-custody-hsm.md invariant 24),
	// so T1/T2/T4 are reconciled for this address only. Required: an empty Hot is an error, never "all rows".
	Hot string
}

// hotScope is the WHERE fragment that selects the rows of ONE Hot address. $1 is the chain id, $2 the Hot
// address (lower case); alias is the table alias of the rows to filter.
//
// Legacy rows (hot_address is empty, written before the migration) belong to the address that signed them under
// the software backend. wallet-service attributes them to its own address at start-up when it runs the
// software backend (storage.AdoptLegacyHotAddress), after which this rule never fires. Until that happens, and
// forever on a pkcs11 wallet-service that never adopts, the auditor applies this rule: unattributed rows belong
// to the current Hot address if and only if it has no attributed nonce row of its own. Once the current Hot
// has its own row (the first signature under a new key), the unattributed rows are the previous key's and are
// excluded, so a Hot key change never mixes two counters. Consequence, accepted: between a cutover and the
// first signature of the new key the legacy rows are read as the new address's, which can only make T1 more
// lenient (it fires on chain count > next_nonce), never raise a false alarm.
func hotScope(alias string) string {
	return `(` + alias + `.chain_id = $1 AND (lower(` + alias + `.hot_address) = $2 OR (` + alias + `.hot_address = '' AND NOT EXISTS (
		SELECT 1 FROM hot_wallet_nonces hn WHERE hn.chain_id = $1 AND lower(hn.hot_address) = $2))))`
}

func (t *Treasury) hot() (string, error) {
	if t.Hot == "" {
		return "", errors.New("treasury source: no Hot address configured, cannot scope the Hot Wallet nonce state")
	}
	return strings.ToLower(t.Hot), nil
}

// NextNonce is wallet-service's next Hot Wallet nonce for the current Hot address on the chain (see
// hotScope for legacy rows); ok is false if none was ever allocated.
func (t *Treasury) NextNonce(ctx context.Context) (n uint64, ok bool, err error) {
	hot, err := t.hot()
	if err != nil {
		return 0, false, err
	}
	var v int64
	err = t.Wallet.QueryRow(ctx, `SELECT s.next_nonce FROM hot_wallet_nonces s WHERE `+hotScope("s"), t.ChainID, hot).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return uint64(v), err == nil, err
}

// Known loads every tx hash wallet-service ever signed for the current Hot address on this chain: the hash of
// each original signed tx, and of each Fee Bump and cancel. Nonces are per address, so another Hot address's
// signatures never make a nonce "known" here.
func (t *Treasury) Known(ctx context.Context) (treasury.Known, error) {
	k := treasury.Known{Hashes: map[string]bool{}, Nonces: map[uint64]bool{}}
	hot, err := t.hot()
	if err != nil {
		return k, err
	}
	rows, err := t.Wallet.Query(ctx, `SELECT s.nonce, s.signed_raw_tx FROM hot_wallet_signatures s WHERE `+hotScope("s"), t.ChainID, hot)
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
		JOIN hot_wallet_signatures s ON s.idempotency_key = r.idempotency_key WHERE `+hotScope("s"), t.ChainID, hot)
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

// Signatures lists every Hot Wallet signature on the chain, of every Hot address (T3 is about purpose and
// caller, not nonces: a signature by a previous Hot key must still have a legitimate purpose), with whether a withdrawal key has its request.
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
