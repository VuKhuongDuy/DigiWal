package chaos

import (
	"testing"
	"time"
)

// checkLedger is the conservation check (docs/qa/TEST_PLAN.md §9) run against the real ledger DB after every test.
func checkLedger(t testing.TB) {
	t.Helper()
	checks := []struct{ name, sql string }{
		{"entries sum to zero per asset", `SELECT count(*) FROM (SELECT a.asset_id FROM entries e JOIN accounts a ON a.id=e.account_id GROUP BY a.asset_id HAVING sum(e.amount)<>0) x`},
		{"total_balance = sum(entries)", `SELECT count(*) FROM accounts a WHERE a.total_balance <> COALESCE((SELECT sum(amount) FROM entries WHERE account_id=a.id),0)`},
		{"locked_balance = sum(active locks)", `SELECT count(*) FROM accounts a WHERE a.locked_balance <> COALESCE((SELECT sum(amount) FROM locks WHERE account_id=a.id AND status='active'),0)`},
		{"no negative available", `SELECT count(*) FROM accounts WHERE kind='user' AND total_balance-locked_balance<0`},
		{"every ledger tx has >=2 entries summing to 0", `SELECT count(*) FROM ledger_transactions lt WHERE (SELECT count(*) FROM entries WHERE ledger_transaction_id=lt.id)<2 OR (SELECT sum(amount) FROM entries WHERE ledger_transaction_id=lt.id)<>0`},
		{"deposit natural key unique", `SELECT count(*) FROM (SELECT 1 FROM ledger_transactions WHERE tx_hash IS NOT NULL GROUP BY chain_id,tx_hash,log_index HAVING count(*)>1) x`},
	}
	for _, c := range checks {
		if got := psql(t, "ledger", c.sql); got != "0" {
			t.Errorf("INVARIANT VIOLATED: %s (%s rows)", c.name, got)
		}
	}
}

// ensureUp restarts any service that a previous fault left dead and says so loudly: a service that stays dead
// after a transient fault is itself a finding (K-2), but must not cascade into every later test.
func ensureUp(t testing.TB) {
	t.Helper()
	for _, s := range []string{"wallet-service", "ledger-service", "chain-watcher", "sweeper", "api-gateway", "withdrawal-service", "history-service", "postgres", "kafka", "redis", "anvil"} {
		if st := dc(t, "inspect", "-f", "{{.State.Status}}", ctr(s)); st != "running" {
			t.Logf("ENV RECOVERY: %s was %q after the previous fault, starting it", s, st)
			if st == "paused" {
				dc(t, "unpause", ctr(s))
			} else {
				dc(t, "start", ctr(s))
			}
			time.Sleep(6 * time.Second)
		}
	}
}

func qa(t *testing.T) { t.Helper(); ensureUp(t); t.Cleanup(func() { checkLedger(t) }) }
