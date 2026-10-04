package audit

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"digiwal/services/auditor/internal/finding"
	"digiwal/services/auditor/internal/job"
	"digiwal/services/auditor/internal/rules/treasury"
	"digiwal/services/auditor/internal/rules/withdrawal"
	"digiwal/services/auditor/internal/source"
)

const nativeID = "eeeeeeee-0000-0000-0000-000000000001"

type fakeReconChain struct {
	head int64
	bal  map[string]*big.Int // addr|token
}

func (c *fakeReconChain) Head(context.Context) (int64, error) { return c.head, nil }
func (c *fakeReconChain) Balance(_ context.Context, addr, token string, _ int64) (*big.Int, error) {
	if v, ok := c.bal[addr+"|"+token]; ok {
		return v, nil
	}
	return new(big.Int), nil
}

type rcEnv struct {
	*env
	chain *fakeReconChain
	job   *Reconciliation
}

func eth(n float64) *big.Int {
	v, _ := new(big.Float).Mul(big.NewFloat(n), big.NewFloat(1e18)).Int(nil)
	return v
}

// newRcEnv: one user holds 500 TKN (one credited deposit, observed at block 10), all of it in the Hot Wallet.
// The native coin has a Chain Account at 0 and nothing on chain.
func newRcEnv(t *testing.T) *rcEnv {
	e := newEnv(t)
	r := &rcEnv{env: e, chain: &fakeReconChain{head: 100, bal: map[string]*big.Int{tHot + "|" + token: big.NewInt(500)}}}
	e.exec(e.led, `INSERT INTO assets (id, asset_type, contract_address, symbol, decimals, hot_floor, hot_ceiling, min_withdrawal_amount) VALUES
		($1, 'erc20', $2, 'TKN', 6, 0, 0, 0), ($3, 'native', NULL, 'ETH', 18, 0, 0, 0)`, assetID, token, nativeID)
	e.exec(e.led, `INSERT INTO accounts (id, asset_id, kind, user_id, total_balance, locked_balance) VALUES ($1, $2, 'user', 'u1', 500, 0)`, userAcct, assetID)
	e.exec(e.led, `INSERT INTO accounts (id, asset_id, kind, total_balance) VALUES ($1, $2, 'chain', -500), ('bbbbbbbb-0000-0000-0000-000000000003', $3, 'chain', 0),
		('bbbbbbbb-0000-0000-0000-000000000004', $3, 'gas_expense', 0)`, chainAcct, assetID, nativeID)
	e.exec(e.led, `INSERT INTO ledger_transactions (id, source_type, idempotency_key, request_hash, chain_id, tx_hash, log_index, created_at)
		VALUES ($1, 'deposit', 'k-dep', 'h', 31337, '0xaa', 0, $2)`, depTx, t0)
	e.exec(e.led, `INSERT INTO entries (ledger_transaction_id, account_id, amount) VALUES ($1, $2, 500), ($1, $3, -500)`, depTx, userAcct, chainAcct)
	e.exec(e.cw, `INSERT INTO observed_transfers (chain_id, tx_hash, log_index, asset_id, from_address, to_address, user_id, amount, block_number, block_hash, status, confirmed_at)
		VALUES (31337, '0xaa', 0, $1, '0x00000000000000000000000000000000000000f1', $2, 'u1', 500, 10, '0xb', 'confirmed', $3)`, assetID, tDep, t0)
	e.exec(e.wal, `INSERT INTO wallet_addresses (user_id, derivation_index, address) VALUES ('u1', 1, $1)`, tDep)

	r.job = &Reconciliation{
		Src: &source.Recon{Ledger: e.ro["ledger"].(*source.PG), Watcher: e.ro["chain-watcher"], Withdrawal: e.ro["withdrawal"], Wallet: e.ro["wallet"],
			Chain: r.chain, TxChain: e.chain, ChainID: 31337},
		Store: e.st, Tiers: treasury.Addresses{Hot: tHot, Warm: tWarm}, Confirms: 1,
		Capital: map[string]*big.Int{}, Tolerance: map[string]*big.Int{}, Now: func() time.Time { return e.now },
	}
	return r
}

func (r *rcEnv) run() job.Result {
	r.t.Helper()
	res, err := job.RunOnce(context.Background(), r.st, r.job, func() time.Time { return r.now })
	if err != nil {
		r.t.Fatal(err)
	}
	r.now = r.now.Add(5 * time.Minute)
	return res
}

func (r *rcEnv) open() map[string]string {
	r.t.Helper()
	rows, err := r.st.Findings(context.Background(), finding.FlowCustody)
	if err != nil {
		r.t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range rows {
		if f.Status == "open" {
			out[f.Rule+" "+f.Reference] = f.Severity
		}
	}
	return out
}

func (r *rcEnv) residual(asset string) string {
	r.t.Helper()
	s, err := r.st.Snapshots(context.Background(), asset)
	if err != nil || len(s) == 0 {
		r.t.Fatalf("no snapshot for %s: %v", asset, err)
	}
	return s[0].Residual
}

func hasRule(m map[string]string, rule, sev string) bool {
	for k, v := range m {
		if strings.HasPrefix(k, rule+" ") && v == sev {
			return true
		}
	}
	return false
}

func TestCustodyMatchesTheLedger(t *testing.T) {
	r := newRcEnv(t)
	for i := 0; i < 3; i++ {
		if res := r.run(); res != job.OK {
			t.Fatalf("run %d: %s %v", i+1, res, r.open())
		}
	}
	if r.residual(assetID) != "0" || r.residual(nativeID) != "0" {
		t.Fatalf("residuals %s %s", r.residual(assetID), r.residual(nativeID))
	}
	if s, _ := r.st.Snapshots(context.Background(), assetID); len(s) != 3 || s[0].Block != 99 {
		t.Fatalf("a snapshot per run at block head-confirms: %+v", s)
	}
}

func TestLagAndHeldDepositsAreExplained(t *testing.T) {
	r := newRcEnv(t)
	// 200 more arrived on chain and chain-watcher saw it, but the ledger has not credited it yet
	r.exec(r.cw, `INSERT INTO observed_transfers (chain_id, tx_hash, log_index, asset_id, from_address, to_address, user_id, amount, block_number, block_hash, status)
		VALUES (31337, '0xbb', 0, $1, '0x00000000000000000000000000000000000000f1', $2, 'u1', 200, 12, '0xc', 'pending')`, assetID, tDep)
	r.chain.bal[tDep+"|"+token] = big.NewInt(200)
	// ...and one from a Screening List sender is held: observed, never credited, flagged
	r.exec(r.cw, `INSERT INTO observed_transfers (chain_id, tx_hash, log_index, asset_id, from_address, to_address, user_id, amount, block_number, block_hash, status, confirmed_at)
		VALUES (31337, '0xcc', 0, $1, '0x00000000000000000000000000000000000000f2', $2, 'u1', 300, 13, '0xd', 'confirmed', $3)`, assetID, tDep, t0)
	r.exec(r.led, `INSERT INTO flagged_deposits (chain_id, tx_hash, log_index, asset_id, from_address, to_address, amount, block_number)
		VALUES (31337, '0xcc', 0, $1, '0x00000000000000000000000000000000000000f2', $2, 300, 13)`, assetID, tDep)
	r.chain.bal[tWarm+"|"+token] = big.NewInt(300)
	for i := 0; i < 2; i++ {
		if res := r.run(); res != job.OK {
			t.Fatalf("run %d: %s %v", i+1, res, r.open())
		}
	}
	// a transfer mined after block B is not on the chain at B, so it is not expected there either
	r.exec(r.cw, `INSERT INTO observed_transfers (chain_id, tx_hash, log_index, asset_id, from_address, to_address, user_id, amount, block_number, block_hash, status)
		VALUES (31337, '0xdd', 0, $1, '0x00000000000000000000000000000000000000f1', $2, 'u1', 999, 100, '0xe', 'pending')`, assetID, tDep)
	if res := r.run(); res != job.OK {
		t.Fatalf("a transfer newer than B: %s %v", res, r.open())
	}
}

func TestMissingFundsAreCriticalOnTheSecondRun(t *testing.T) {
	r := newRcEnv(t)
	r.chain.bal[tHot+"|"+token] = big.NewInt(400) // 100 gone
	if res := r.run(); res != job.OK {
		t.Fatalf("first sighting is only remembered: %s", res)
	}
	if res := r.run(); res != job.Fail {
		t.Fatalf("second run: %s %v", res, r.open())
	}
	o := r.open()
	if !hasRule(o, "C1", "critical") || !hasRule(o, "C2", "critical") {
		t.Fatalf("a shortfall is C1 and, since users are owed 500, C2: %v", o)
	}
	if r.residual(assetID) != "-100" {
		t.Fatalf("residual %s", r.residual(assetID))
	}
	r.chain.bal[tHot+"|"+token] = big.NewInt(500)
	if res := r.run(); res != job.OK || len(r.open()) != 0 {
		t.Fatalf("after: %s %v", res, r.open())
	}
}

func TestMinedButUnsettledWithdrawalIsExplained(t *testing.T) {
	r := newRcEnv(t)
	const tx = "0xabc0000000000000000000000000000000000000000000000000000000000001"
	r.exec(r.wd, `INSERT INTO withdrawal_requests (withdrawal_id, user_id, idempotency_key, asset_id, amount, to_address, status, tx_hash, nonce, signed_raw_tx, chain_id, updated_at)
		VALUES ($1, 'u1', 'k1', $2, 100, $3, 'broadcast', $4, 0, '0x00', 31337, $5)`, wid, assetID, tExt, tx, t0)
	r.chain.bal[tHot+"|"+token] = big.NewInt(400) // the 100 already left
	// not mined yet: the chain still holds 500, so a drop to 400 is unexplained
	r.job.Src.TxChain = fakeChain{}
	r.run()
	if res := r.run(); res != job.Fail {
		t.Fatalf("funds left Hot with no mined withdrawal: %s %v", res, r.open())
	}
	// mined at block 50 (<= B): explained
	r.job.Src.TxChain = fakeChain{tx: withdrawal.ChainTx{Found: true, Mined: true, Success: true, Block: 50}}
	if res := r.run(); res != job.OK {
		t.Fatalf("mined and not settled: %s %v", res, r.open())
	}
	// mined after B: not on chain at B, so the 100 is still there at B
	r.job.Src.TxChain = fakeChain{tx: withdrawal.ChainTx{Found: true, Mined: true, Success: true, Block: 100}}
	r.chain.bal[tHot+"|"+token] = big.NewInt(500)
	if res := r.run(); res != job.OK {
		t.Fatalf("mined after B: %s %v", res, r.open())
	}
}

// Gas: ETH capital in the Hot Wallet pays for it; the gas paid by a mined, unconfirmed withdrawal is explained
// until it is posted, and a drift beyond the tolerance is not.
func TestNativeCapitalGasAndTolerance(t *testing.T) {
	r := newRcEnv(t)
	const tx = "0xabc0000000000000000000000000000000000000000000000000000000000002"
	r.exec(r.wd, `INSERT INTO withdrawal_requests (withdrawal_id, user_id, idempotency_key, asset_id, amount, to_address, status, tx_hash, nonce, signed_raw_tx, chain_id, updated_at)
		VALUES ($1, 'u1', 'k1', $2, 100, $3, 'broadcast', $4, 0, '0x00', 31337, $5)`, wid, assetID, tExt, tx, t0)
	r.chain.bal[tHot+"|"+token] = big.NewInt(400)
	r.job.Src.TxChain = fakeChain{tx: withdrawal.ChainTx{Found: true, Mined: true, Success: true, Block: 50, GasFee: eth(0.001)}}

	// the Hot Wallet was funded with 10 ETH by the operator, and paid 0.001 of gas
	r.chain.bal[tHot+"|"] = new(big.Int).Sub(eth(10), eth(0.001))
	r.run()
	if res := r.run(); res != job.Fail { // capital not configured yet: 9.999 ETH nobody accounts for
		t.Fatalf("unaccounted ETH should be reported: %s %v", res, r.open())
	}
	r.job.Capital["ETH"] = eth(10)
	if res := r.run(); res != job.OK {
		t.Fatalf("with the capital configured: %s %v", res, r.open())
	}

	// a small unposted-gas drift (0.005 ETH <= the 0.01 tolerance) is a warn that does not escalate
	r.chain.bal[tHot+"|"] = new(big.Int).Sub(r.chain.bal[tHot+"|"], eth(0.005))
	r.run()
	for i := 0; i < 6; i++ {
		if res := r.run(); res != job.Warn {
			t.Fatalf("within tolerance should stay a warn: %s %v", res, r.open())
		}
	}
	if !hasRule(r.open(), "C1", "warn") {
		t.Fatalf("%v", r.open())
	}
	// beyond it: critical
	r.chain.bal[tHot+"|"] = new(big.Int).Sub(r.chain.bal[tHot+"|"], eth(0.02))
	r.run()
	if res := r.run(); res != job.Fail || !hasRule(r.open(), "C1", "critical") {
		t.Fatalf("beyond tolerance: %s %v", res, r.open())
	}
}

func TestHotWalletCannotCoverTheLocks(t *testing.T) {
	r := newRcEnv(t)
	r.exec(r.led, `UPDATE accounts SET locked_balance = 300 WHERE id = $1`, userAcct)
	r.exec(r.led, `INSERT INTO locks (idempotency_key, account_id, amount, status) VALUES ('withdrawal:w9', $1, 300, 'active')`, userAcct)
	r.chain.bal[tHot+"|"+token] = big.NewInt(200) // the rest is in Warm
	r.chain.bal[tWarm+"|"+token] = big.NewInt(300)
	r.run()
	if res := r.run(); res != job.Warn || !hasRule(r.open(), "C3", "warn") {
		t.Fatalf("%s %v", res, r.open())
	}
	r.chain.bal[tHot+"|"+token] = big.NewInt(300)
	r.chain.bal[tWarm+"|"+token] = big.NewInt(200)
	if res := r.run(); res != job.OK {
		t.Fatalf("after a rebalance: %s %v", res, r.open())
	}
}
