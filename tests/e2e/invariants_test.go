package e2e

import (
	"github.com/ethereum/go-ethereum/core/types"
	"context"
	"testing"
)

// checkInvariants is the property check from docs/qa/TEST_PLAN.md §9. Every test calls it on cleanup,
// so a test cannot pass while P3 (conservation) is broken anywhere in the ledger.
func checkInvariants(t testing.TB) {
	t.Helper()
	ctx := context.Background()
	type chk struct{ name, sql string }
	checks := []chk{
		{"I1 entries sum to zero per asset", `SELECT count(*) FROM (SELECT a.asset_id FROM entries e JOIN accounts a ON a.id=e.account_id GROUP BY a.asset_id HAVING sum(e.amount)<>0) x`},
		{"I2 total_balance = sum(entries)", `SELECT count(*) FROM accounts a WHERE a.total_balance <> COALESCE((SELECT sum(amount) FROM entries WHERE account_id=a.id),0)`},
		{"I3 locked_balance = sum(active locks)", `SELECT count(*) FROM accounts a WHERE a.locked_balance <> COALESCE((SELECT sum(amount) FROM locks WHERE account_id=a.id AND status='active'),0)`},
		{"I3b no user available < 0", `SELECT count(*) FROM accounts WHERE kind='user' AND total_balance-locked_balance<0`},
		{"I4 chain account = -(sum of user totals)", `SELECT count(*) FROM (SELECT c.asset_id FROM accounts c WHERE c.kind='chain' AND c.total_balance <> -COALESCE((SELECT sum(u.total_balance) FROM accounts u WHERE u.kind='user' AND u.asset_id=c.asset_id),0)) x`},
		{"I5 every ledger tx has >=2 entries summing to 0", `SELECT count(*) FROM ledger_transactions lt WHERE (SELECT count(*) FROM entries WHERE ledger_transaction_id=lt.id)<2 OR (SELECT sum(amount) FROM entries WHERE ledger_transaction_id=lt.id)<>0`},
		{"I5b settled lock <=> withdrawal_settle tx", `SELECT count(*) FROM locks l WHERE (l.status='settled') <> EXISTS (SELECT 1 FROM ledger_transactions WHERE idempotency_key=l.idempotency_key AND source_type='withdrawal_settle')`},
		{"I6 released/active locks post no entry", `SELECT count(*) FROM locks l JOIN ledger_transactions t ON t.idempotency_key=l.idempotency_key WHERE l.status<>'settled'`},
		{"I7 deposit natural key unique", `SELECT count(*) FROM (SELECT 1 FROM ledger_transactions WHERE tx_hash IS NOT NULL GROUP BY chain_id,tx_hash,log_index HAVING count(*)>1) x`},
	}
	for _, c := range checks {
		var n int
		if err := ldb.QueryRow(ctx, c.sql).Scan(&n); err != nil {
			t.Errorf("invariant %q: query failed: %v", c.name, err)
			continue
		}
		if n != 0 {
			t.Errorf("INVARIANT VIOLATED: %s (%d rows)", c.name, n)
		}
	}
	// wallet side (I8)
	wchecks := []chk{
		{"W1 hot-wallet nonces unique and dense", `SELECT count(*) FROM (SELECT chain_id FROM hot_wallet_signatures GROUP BY chain_id HAVING count(DISTINCT nonce)<>count(*) OR max(nonce)-min(nonce)+1<>count(*)) x`},
		{"W2 next_nonce = max signed + 1", `SELECT count(*) FROM hot_wallet_nonces n WHERE n.next_nonce <> COALESCE((SELECT max(nonce)+1 FROM hot_wallet_signatures s WHERE s.chain_id=n.chain_id), n.next_nonce)`},
		{"W3 derivation indices unique and < next_index", `SELECT count(*) FROM (SELECT derivation_index FROM wallet_addresses GROUP BY derivation_index HAVING count(*)>1) x`},
		{"W3b indices < next_index", `SELECT count(*) FROM wallet_addresses a, wallet_seeds s WHERE s.seed_role='user_derivation' AND a.derivation_index >= s.next_index`},
	}
	for _, c := range wchecks {
		var n int
		if err := wdb.QueryRow(ctx, c.sql).Scan(&n); err != nil {
			t.Errorf("invariant %q: query failed: %v", c.name, err)
			continue
		}
		if n != 0 {
			t.Errorf("INVARIANT VIOLATED: %s (%d rows)", c.name, n)
		}
	}
}

// qa wraps a test: invariants are checked when it ends, pass or fail.
func qa(t *testing.T) *testing.T {
	t.Helper()
	syncHot(t)
	t.Cleanup(func() { syncHot(t); checkInvariants(t) })
	return t
}

// syncHot is what a recovery job would do: rebroadcast every stored Hot Wallet signature the chain hasn't seen.
// Tests that sign without broadcasting would otherwise leave a real nonce gap for the tests after them.
func syncHot(t testing.TB) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		on, err := cl.NonceAt(ctx, hotAddr, nil)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := wdb.Query(ctx, `SELECT signed_raw_tx FROM hot_wallet_signatures WHERE nonce >= $1 ORDER BY nonce`, int64(on))
		if err != nil {
			t.Fatal(err)
		}
		var raws [][]byte
		for rows.Next() {
			var b []byte
			rows.Scan(&b)
			raws = append(raws, b)
		}
		rows.Close()
		if len(raws) == 0 {
			return
		}
		for _, b := range raws {
			var tx types.Transaction
			if tx.UnmarshalBinary(b) == nil {
				_ = cl.SendTransaction(ctx, &tx)
			}
		}
		mine(t, 1)
	}
}
