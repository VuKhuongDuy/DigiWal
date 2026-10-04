package source

import (
	"context"
	"fmt"

	"digiwal/services/auditor/internal/rules/ledger"
)

// MaxPerRule bounds how many offenders one rule reports per run; a broken ledger should not produce a
// million findings, and fixing the first few shows the pattern.
const MaxPerRule = 200

// LedgerChecks runs L1-L9 against ledger-service's Postgres.
type LedgerChecks struct{ Ledger *PG }

// Check evaluates every ledger rule in one REPEATABLE READ, READ ONLY transaction.
func (c *LedgerChecks) Check(ctx context.Context) ([]ledger.Found, error) {
	var out []ledger.Found
	err := c.Ledger.Snapshot(ctx, func(q Querier) error {
		for _, check := range []func(context.Context, Querier) ([]ledger.Found, error){l1, l2, l3, l4, l5, l6, l7, l8, l9} {
			f, err := check(ctx, q)
			if err != nil {
				return err
			}
			out = append(out, f...)
		}
		return nil
	})
	return out, err
}

// rows runs a query and maps each row to a Found with fn.
func rows(ctx context.Context, q Querier, sql string, nScan int, fn func(v []string) ledger.Found) ([]ledger.Found, error) {
	r, err := q.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []ledger.Found
	for r.Next() {
		v := make([]string, nScan)
		dst := make([]any, nScan)
		for i := range v {
			dst[i] = &v[i]
		}
		if err := r.Scan(dst...); err != nil {
			return nil, err
		}
		out = append(out, fn(v))
	}
	return out, r.Err()
}

func lim() string { return fmt.Sprintf(" LIMIT %d", MaxPerRule) }

// L1: each Ledger Transaction sums to 0 per asset, and has entries.
func l1(ctx context.Context, q Querier) ([]ledger.Found, error) {
	a, err := rows(ctx, q, `
		SELECT e.ledger_transaction_id::text, a.asset_id::text, sum(e.amount)::text
		FROM entries e JOIN accounts a ON a.id = e.account_id
		GROUP BY e.ledger_transaction_id, a.asset_id HAVING sum(e.amount) <> 0`+lim(), 3, func(v []string) ledger.Found {
		return ledger.New("L1", "tx:"+v[0], map[string]any{"problem": "entries do not sum to zero", "asset": v[1], "sum": v[2]})
	})
	if err != nil {
		return nil, err
	}
	b, err := rows(ctx, q, `
		SELECT t.id::text FROM ledger_transactions t LEFT JOIN entries e ON e.ledger_transaction_id = t.id
		WHERE e.id IS NULL`+lim(), 1, func(v []string) ledger.Found {
		return ledger.New("L1", "tx:"+v[0], map[string]any{"problem": "ledger transaction has no entries"})
	})
	return append(a, b...), err
}

// L2: total_balance of every Account equals the sum of its Entries.
func l2(ctx context.Context, q Querier) ([]ledger.Found, error) {
	return rows(ctx, q, `
		SELECT a.id::text, a.total_balance::text, coalesce(sum(e.amount), 0)::text
		FROM accounts a LEFT JOIN entries e ON e.account_id = a.id
		GROUP BY a.id HAVING a.total_balance <> coalesce(sum(e.amount), 0)`+lim(), 3, func(v []string) ledger.Found {
		return ledger.New("L2", "account:"+v[0], map[string]any{"problem": "total_balance differs from the sum of Entries", "total_balance": v[1], "sum_of_entries": v[2]})
	})
}

// L3: locked_balance of every Account equals the sum of its active Locks.
func l3(ctx context.Context, q Querier) ([]ledger.Found, error) {
	return rows(ctx, q, `
		SELECT a.id::text, a.locked_balance::text, coalesce(sum(l.amount) FILTER (WHERE l.status = 'active'), 0)::text
		FROM accounts a LEFT JOIN locks l ON l.account_id = a.id
		GROUP BY a.id HAVING a.locked_balance <> coalesce(sum(l.amount) FILTER (WHERE l.status = 'active'), 0)`+lim(), 3, func(v []string) ledger.Found {
		return ledger.New("L3", "account:"+v[0], map[string]any{"problem": "locked_balance differs from the sum of active Locks", "locked_balance": v[1], "sum_of_active_locks": v[2]})
	})
}

// L4: no user Account's Available balance is below zero.
func l4(ctx context.Context, q Querier) ([]ledger.Found, error) {
	return rows(ctx, q, `
		SELECT id::text, total_balance::text, locked_balance::text FROM accounts
		WHERE kind = 'user' AND total_balance - locked_balance < 0`+lim(), 3, func(v []string) ledger.Found {
		return ledger.New("L4", "account:"+v[0], map[string]any{"problem": "Available balance is negative", "total_balance": v[1], "locked_balance": v[2]})
	})
}

// L5: per asset, all Accounts together sum to zero.
func l5(ctx context.Context, q Querier) ([]ledger.Found, error) {
	return rows(ctx, q, `
		SELECT asset_id::text, sum(total_balance)::text FROM accounts
		GROUP BY asset_id HAVING sum(total_balance) <> 0`+lim(), 2, func(v []string) ledger.Found {
		return ledger.New("L5", "asset:"+v[0], map[string]any{"problem": "accounts of this asset do not sum to zero", "sum": v[1]})
	})
}

// L6: a correction exactly negates its target, only corrections correct, and a target is reversed at most once.
func l6(ctx context.Context, q Querier) ([]ledger.Found, error) {
	a, err := rows(ctx, q, `
		SELECT c.id::text, c.ledger_transaction_id::text, c.amount::text, t.amount::text
		FROM entries c JOIN entries t ON t.id = c.corrects_entry_id
		WHERE c.account_id <> t.account_id OR c.amount <> -t.amount`+lim(), 4, func(v []string) ledger.Found {
		return ledger.New("L6", "tx:"+v[1], map[string]any{"problem": "a correction does not exactly negate its target", "entry": v[0], "amount": v[2], "target_amount": v[3]})
	})
	if err != nil {
		return nil, err
	}
	b, err := rows(ctx, q, `
		SELECT corrects_entry_id::text, count(*)::text FROM entries
		WHERE corrects_entry_id IS NOT NULL GROUP BY corrects_entry_id HAVING count(*) > 1`+lim(), 2, func(v []string) ledger.Found {
		return ledger.New("L6", "entry:"+v[0], map[string]any{"problem": "an entry was reversed more than once", "reversals": v[1]})
	})
	if err != nil {
		return nil, err
	}
	c, err := rows(ctx, q, `
		SELECT DISTINCT t.id::text FROM ledger_transactions t JOIN entries e ON e.ledger_transaction_id = t.id
		WHERE (t.source_type = 'correction') <> (e.corrects_entry_id IS NOT NULL)`+lim(), 1, func(v []string) ledger.Found {
		return ledger.New("L6", "tx:"+v[0], map[string]any{"problem": "a correction transaction has entries that correct nothing, or another transaction has correcting entries"})
	})
	return append(append(a, b...), c...), err
}

// L7: a settled Lock has exactly one withdrawal_settle, and a withdrawal_settle has a settled Lock.
func l7(ctx context.Context, q Querier) ([]ledger.Found, error) {
	a, err := rows(ctx, q, `
		SELECT l.idempotency_key, count(t.id)::text FROM locks l
		LEFT JOIN ledger_transactions t ON t.idempotency_key = l.idempotency_key AND t.source_type = 'withdrawal_settle'
		WHERE l.status = 'settled' GROUP BY l.idempotency_key HAVING count(t.id) <> 1`+lim(), 2, func(v []string) ledger.Found {
		return ledger.New("L7", "lock:"+v[0], map[string]any{"problem": "a settled Lock without exactly one withdrawal_settle", "settles": v[1]})
	})
	if err != nil {
		return nil, err
	}
	b, err := rows(ctx, q, `
		SELECT t.idempotency_key, coalesce(l.status, 'none') FROM ledger_transactions t
		LEFT JOIN locks l ON l.idempotency_key = t.idempotency_key
		WHERE t.source_type = 'withdrawal_settle' AND coalesce(l.status, 'none') <> 'settled'`+lim(), 2, func(v []string) ledger.Found {
		return ledger.New("L7", "lock:"+v[0], map[string]any{"problem": "a withdrawal_settle whose Lock is not settled", "lock": v[1]})
	})
	return append(a, b...), err
}

// L8: Gas Fee keys are well formed (gas:{chain_id}:{lower-case tx hash}), a Gas Fee has a purpose, and
// nothing else uses the gas: prefix.
func l8(ctx context.Context, q Querier) ([]ledger.Found, error) {
	return rows(ctx, q, `
		SELECT idempotency_key, source_type FROM ledger_transactions
		WHERE (source_type = 'gas_fee' AND (idempotency_key !~ '^gas:[0-9]+:0x[0-9a-f]{64}$' OR purpose IS NULL))
		   OR (source_type <> 'gas_fee' AND idempotency_key LIKE 'gas:%')`+lim(), 2, func(v []string) ledger.Found {
		return ledger.New("L8", "gas:"+v[0], map[string]any{"problem": "malformed Gas Fee key, missing purpose, or the gas: prefix on another kind of transaction", "source_type": v[1]})
	})
}

// L9: the immutability triggers on entries exist and are enabled.
func l9(ctx context.Context, q Querier) ([]ledger.Found, error) {
	var out []ledger.Found
	for _, name := range ledger.TriggerNames {
		var n int
		err := q.QueryRow(ctx, `SELECT count(*) FROM pg_trigger
			WHERE tgrelid = 'public.entries'::regclass AND NOT tgisinternal AND tgname = $1 AND tgenabled IN ('O', 'A')`, name).Scan(&n)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			out = append(out, ledger.New("L9", "trigger:"+name, map[string]any{"problem": "the immutability trigger on entries is missing or disabled"}))
		}
	}
	return out, nil
}
