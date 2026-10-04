package audit

import (
	"context"
	"testing"
	"time"

	"digiwal/services/auditor/internal/finding"
	"digiwal/services/auditor/internal/job"
	"digiwal/services/auditor/internal/source"
)

type ledEnv struct {
	*env
	job *LedgerIntegrity
}

const (
	userAcct  = "bbbbbbbb-0000-0000-0000-000000000001"
	chainAcct = "bbbbbbbb-0000-0000-0000-000000000002"
	depTx     = "cccccccc-0000-0000-0000-000000000001"
	depEntry  = "dddddddd-0000-0000-0000-000000000001"
)

// newLedEnv seeds a consistent ledger: one deposit of 500, 100 of it under an active Lock.
func newLedEnv(t *testing.T) *ledEnv {
	e := newEnv(t)
	e.exec(e.led, `INSERT INTO assets (id, asset_type, contract_address, symbol, decimals, hot_floor, hot_ceiling, min_withdrawal_amount)
		VALUES ($1, 'erc20', $2, 'TKN', 6, 0, 0, 0)`, assetID, token)
	e.exec(e.led, `INSERT INTO accounts (id, asset_id, kind, user_id, total_balance, locked_balance) VALUES ($1, $2, 'user', 'u1', 500, 100)`, userAcct, assetID)
	e.exec(e.led, `INSERT INTO accounts (id, asset_id, kind, total_balance) VALUES ($1, $2, 'chain', -500)`, chainAcct, assetID)
	e.exec(e.led, `INSERT INTO ledger_transactions (id, source_type, idempotency_key, request_hash, chain_id, tx_hash, log_index)
		VALUES ($1, 'deposit', 'k-dep', 'h', 31337, '0xaa', 0)`, depTx)
	e.exec(e.led, `INSERT INTO entries (id, ledger_transaction_id, account_id, amount) VALUES ($1, $2, $3, 500)`, depEntry, depTx, userAcct)
	e.exec(e.led, `INSERT INTO entries (ledger_transaction_id, account_id, amount) VALUES ($1, $2, -500)`, depTx, chainAcct)
	e.exec(e.led, `INSERT INTO locks (idempotency_key, account_id, amount, status) VALUES ('withdrawal:w1', $1, 100, 'active')`, userAcct)
	return &ledEnv{env: e, job: &LedgerIntegrity{
		Src: &source.LedgerChecks{Ledger: e.ro["ledger"].(*source.PG)}, Store: e.st, Now: func() time.Time { return e.now }}}
}

// tamper runs SQL as a superuser with triggers off, the way a bug or a manual edit would get past the
// ledger's own guards. CHECK constraints still apply.
func (l *ledEnv) tamper(sql ...string) {
	l.t.Helper()
	ctx := context.Background()
	c, err := l.led.Acquire(ctx)
	if err != nil {
		l.t.Fatal(err)
	}
	defer c.Release()
	if _, err := c.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		l.t.Fatal(err)
	}
	for _, s := range sql {
		if _, err := c.Exec(ctx, s); err != nil {
			l.t.Fatalf("%v\n%s", err, s)
		}
	}
}

func (l *ledEnv) run() job.Result {
	l.t.Helper()
	res, err := job.RunOnce(context.Background(), l.st, l.job, func() time.Time { return l.now })
	if err != nil {
		l.t.Fatal(err)
	}
	l.now = l.now.Add(5 * time.Minute)
	return res
}

// open returns the rules with an open finding.
func (l *ledEnv) open() map[string]bool {
	l.t.Helper()
	rows, err := l.st.Findings(context.Background(), finding.FlowLedger)
	if err != nil {
		l.t.Fatal(err)
	}
	out := map[string]bool{}
	for _, r := range rows {
		if r.Status == "open" {
			out[r.Rule] = true
		}
	}
	return out
}

func TestConsistentLedgerIsOk(t *testing.T) {
	l := newLedEnv(t)
	for i := 0; i < 2; i++ {
		if res := l.run(); res != job.OK {
			t.Fatalf("run %d: %s %v", i+1, res, l.open())
		}
	}
}

// Every rule: a baseline that is clean, one targeted corruption, the finding on the FIRST run (the rules
// are Immediate), and resolution when it is undone.
func TestEachRuleDetectsAndResolves(t *testing.T) {
	cases := []struct {
		rule         string
		corrupt, fix []string
	}{
		{"L1", []string{`INSERT INTO entries (ledger_transaction_id, account_id, amount) VALUES ('` + depTx + `', '` + userAcct + `', 7)`,
			`UPDATE accounts SET total_balance = total_balance + 7 WHERE id = '` + userAcct + `'`, `UPDATE accounts SET total_balance = total_balance - 7 WHERE id = '` + chainAcct + `'`},
			[]string{`DELETE FROM entries WHERE amount = 7`, `UPDATE accounts SET total_balance = 500 WHERE id = '` + userAcct + `'`, `UPDATE accounts SET total_balance = -500 WHERE id = '` + chainAcct + `'`}},
		{"L2", []string{`UPDATE accounts SET total_balance = 501 WHERE id = '` + userAcct + `'`}, []string{`UPDATE accounts SET total_balance = 500 WHERE id = '` + userAcct + `'`}},
		{"L3", []string{`UPDATE accounts SET locked_balance = 50 WHERE id = '` + userAcct + `'`}, []string{`UPDATE accounts SET locked_balance = 100 WHERE id = '` + userAcct + `'`}},
		{"L4", []string{`ALTER TABLE accounts DROP CONSTRAINT accounts_check1`, `UPDATE accounts SET locked_balance = 600 WHERE id = '` + userAcct + `'`, `UPDATE locks SET amount = 600`},
			[]string{`UPDATE locks SET amount = 100`, `UPDATE accounts SET locked_balance = 100 WHERE id = '` + userAcct + `'`}},
		{"L5", []string{`UPDATE accounts SET total_balance = -400 WHERE id = '` + chainAcct + `'`}, []string{`UPDATE accounts SET total_balance = -500 WHERE id = '` + chainAcct + `'`}},
		{"L6", []string{`INSERT INTO ledger_transactions (id, source_type, idempotency_key, request_hash) VALUES ('cccccccc-0000-0000-0000-000000000002', 'correction', 'k-corr', 'h')`,
			`INSERT INTO entries (ledger_transaction_id, account_id, amount, corrects_entry_id) VALUES ('cccccccc-0000-0000-0000-000000000002', '` + userAcct + `', -499, '` + depEntry + `')`,
			`INSERT INTO entries (ledger_transaction_id, account_id, amount) VALUES ('cccccccc-0000-0000-0000-000000000002', '` + chainAcct + `', 499)`},
			[]string{`DELETE FROM entries WHERE ledger_transaction_id = 'cccccccc-0000-0000-0000-000000000002'`, `DELETE FROM ledger_transactions WHERE id = 'cccccccc-0000-0000-0000-000000000002'`}},
		{"L7", []string{`UPDATE locks SET status = 'settled', resolved_at = now()`, `UPDATE accounts SET locked_balance = 0 WHERE id = '` + userAcct + `'`},
			[]string{`UPDATE locks SET status = 'active', resolved_at = NULL`, `UPDATE accounts SET locked_balance = 100 WHERE id = '` + userAcct + `'`}},
		{"L8", []string{`INSERT INTO ledger_transactions (source_type, idempotency_key, request_hash, purpose) VALUES ('gas_fee', 'gas:31337:0xBAD', 'h', 'withdrawal')`},
			[]string{`DELETE FROM ledger_transactions WHERE idempotency_key = 'gas:31337:0xBAD'`}},
		{"L9", []string{`ALTER TABLE entries DISABLE TRIGGER entries_no_update_delete`}, []string{`ALTER TABLE entries ENABLE TRIGGER entries_no_update_delete`}},
	}
	for _, c := range cases {
		t.Run(c.rule, func(t *testing.T) {
			l := newLedEnv(t)
			if res := l.run(); res != job.OK {
				t.Fatalf("baseline: %s %v", res, l.open())
			}
			l.tamper(c.corrupt...)
			if res := l.run(); res != job.Fail {
				t.Fatalf("first run after the corruption: %s, want fail", res)
			}
			if !l.open()[c.rule] {
				t.Fatalf("%s not reported, open: %v", c.rule, l.open())
			}
			l.tamper(c.fix...)
			if res := l.run(); res != job.OK {
				t.Fatalf("after the fix: %s %v", res, l.open())
			}
			if l.open()[c.rule] {
				t.Fatalf("%s should be resolved", c.rule)
			}
		})
	}
}

// A settled Lock with its Settle, and a Gas Fee with a good key, are fine (L7, L8 positive cases).
func TestSettledLockAndGasFeeAreClean(t *testing.T) {
	l := newLedEnv(t)
	l.exec(l.led, `INSERT INTO ledger_transactions (id, source_type, idempotency_key, request_hash) VALUES ('cccccccc-0000-0000-0000-000000000009', 'withdrawal_settle', 'withdrawal:w1', 'h')`)
	l.tamper(
		`INSERT INTO entries (ledger_transaction_id, account_id, amount) VALUES ('cccccccc-0000-0000-0000-000000000009', '`+userAcct+`', -100), ('cccccccc-0000-0000-0000-000000000009', '`+chainAcct+`', 100)`,
		`UPDATE accounts SET total_balance = 400, locked_balance = 0 WHERE id = '`+userAcct+`'`,
		`UPDATE accounts SET total_balance = -400 WHERE id = '`+chainAcct+`'`,
		`UPDATE locks SET status = 'settled', resolved_at = now()`,
		`INSERT INTO assets (id, asset_type, symbol, decimals, hot_floor, hot_ceiling, min_withdrawal_amount) VALUES ('eeeeeeee-0000-0000-0000-000000000001', 'native', 'ETH', 18, 0, 0, 0)`,
		`INSERT INTO accounts (id, asset_id, kind, total_balance) VALUES ('bbbbbbbb-0000-0000-0000-000000000003', 'eeeeeeee-0000-0000-0000-000000000001', 'chain', 21), ('bbbbbbbb-0000-0000-0000-000000000004', 'eeeeeeee-0000-0000-0000-000000000001', 'gas_expense', -21)`,
		`INSERT INTO ledger_transactions (id, source_type, idempotency_key, request_hash, purpose) VALUES ('cccccccc-0000-0000-0000-000000000010', 'gas_fee', 'gas:31337:0x`+fill64+`', 'h', 'withdrawal')`,
		`INSERT INTO entries (ledger_transaction_id, account_id, amount) VALUES ('cccccccc-0000-0000-0000-000000000010', 'bbbbbbbb-0000-0000-0000-000000000003', 21), ('cccccccc-0000-0000-0000-000000000010', 'bbbbbbbb-0000-0000-0000-000000000004', -21)`)
	if res := l.run(); res != job.OK {
		t.Fatalf("%s %v", res, l.open())
	}
}

const fill64 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
