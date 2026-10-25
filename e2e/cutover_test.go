// The Hot cutover rehearsal (key-custody-hsm.md 5.5, M2c): `make e2e-cutover` drives the procedure around three phases
// of this test, selected by E2E_CUTOVER=before|frozen|after (skipped when it is unset, so `make e2e` never runs it):
//
//	before   on the SOFTWARE Hot: fund it, deposit and sweep, withdraw, then leave it holding native + ERC-20 with a
//	         signed-but-dropped withdrawal (stuck) and an in-flight Gas Top-Up in the mempool
//	frozen   during the freeze: a new withdrawal is accepted but NOT signed
//	after    the cutover is done: the old Hot is empty, the new Hot holds the funds, the stuck tx and the Gas Top-Up
//	         resolved, withdrawals keep working signed by hsm-gateway, and the ledger invariants hold
//
// The Makefile (e2e/Makefile, cutover-rehearse) runs the cutover itself between the phases; this file only sets up
// the state and checks the result. State between phases is the JSON file named by E2E_CUTOVER_STATE.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

type cutoverUser struct{ ID, Key, Dep string }

type cutoverState struct {
	OldHot     string
	Alice, Bob cutoverUser
	Carol      cutoverUser
	DestA      string // alice's whitelisted destinations
	DestS      string
	DestB      string
	StuckKey   string // the withdrawal whose signed tx was dropped from the mempool
	StuckHash  string
	StuckWei   string
	OldTT      string // the old Hot's token balance when `before` ended (carol's deposit is not in it yet)
	CarolTT    string
	FrozenKey  string
}

func loadState(t *testing.T) cutoverState {
	t.Helper()
	var s cutoverState
	b, err := os.ReadFile(os.Getenv("E2E_CUTOVER_STATE"))
	if err != nil {
		t.Fatalf("reading the rehearsal state: %v", err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s cutoverState) save(t *testing.T) {
	t.Helper()
	b, _ := json.MarshalIndent(s, "", "  ")
	if err := os.WriteFile(os.Getenv("E2E_CUTOVER_STATE"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (c cutoverUser) user() *user { return &user{id: c.ID, key: c.Key} }

func TestCutover(t *testing.T) {
	phase := os.Getenv("E2E_CUTOVER")
	if os.Getenv("E2E") != "1" || phase == "" {
		t.Skip("the Hot cutover rehearsal: make e2e-cutover")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	eth, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		t.Fatal(err)
	}
	rpcc, _ := rpc.DialContext(ctx, rpcURL)
	switch phase {
	case "before":
		cutoverBefore(ctx, t, eth, rpcc)
	case "frozen":
		cutoverFrozen(ctx, t)
	case "after":
		cutoverAfter(ctx, t, eth, rpcc)
	default:
		t.Fatalf("unknown E2E_CUTOVER %q", phase)
	}
}

func cutoverBefore(ctx context.Context, t *testing.T, eth *ethclient.Client, rpcc *rpc.Client) {
	key0, key1 := mustKey(t, anvilKey0), mustKey(t, anvilKey1)
	token := ensureToken(ctx, t, eth, key0)
	oldHot := hotWalletAddress(ctx, t)
	send(ctx, t, eth, key1, &oldHot, eth1(40), nil)
	send(ctx, t, eth, key0, &token, big.NewInt(0), transferCalldata(t, oldHot, eth1(3000)))

	alice, bob, carol := newUser(t), newUser(t), newUser(t)
	aliceDep, bobDep, carolDep := common.HexToAddress(alice.depositAddress(t)), common.HexToAddress(bob.depositAddress(t)), common.HexToAddress(carol.depositAddress(t))
	st := cutoverState{OldHot: oldHot.Hex(), Alice: cutoverUser{alice.id, alice.key, aliceDep.Hex()}, Bob: cutoverUser{bob.id, bob.key, bobDep.Hex()},
		Carol: cutoverUser{carol.id, carol.key, carolDep.Hex()}, CarolTT: eth1(300).String()}

	// Deposits through the real flow, swept into the old Hot (alice native, bob token: his sweep needs a Gas Top-Up).
	send(ctx, t, eth, key1, &aliceDep, eth1(3), nil)
	send(ctx, t, eth, key0, &token, big.NewInt(0), transferCalldata(t, bobDep, eth1(600)))
	mine(ctx, t, rpcc, 5)
	waitFor(t, 120*time.Second, "deposits credited", func() (bool, string) {
		a, b := alice.ledger(t), bob.ledger(t)
		return a[ethAssetID].total == eth1(3).String() && b[tokenAssetID].total == eth1(600).String(), fmt.Sprint(a, b)
	})
	waitFor(t, 150*time.Second, "deposit addresses swept", func() (bool, string) {
		mine(ctx, t, rpcc, 1)
		tt, a := tokenBalance(ctx, t, eth, token, bobDep), balance(ctx, t, eth, aliceDep)
		return tt.Sign() == 0 && a.Cmp(finney(1)) < 0, fmt.Sprintf("bob TT %s, alice %s", tt, a)
	})

	// Withdrawals that complete normally, signed by the software Hot.
	destA, destB, destS := freshAddress(t), freshAddress(t), freshAddress(t)
	st.DestA, st.DestB, st.DestS = destA.Hex(), destB.Hex(), destS.Hex()
	for _, w := range []struct {
		u    *user
		dest common.Address
	}{{alice, destA}, {alice, destS}, {bob, destB}} {
		if code, body := w.u.do(t, "POST", "/v1/whitelist", map[string]string{"address": w.dest.Hex()}); code != 201 {
			t.Fatalf("whitelist: %d %s", code, body)
		}
	}
	for _, w := range []struct {
		u    *user
		dest common.Address
	}{{alice, destA}, {alice, destS}, {bob, destB}} {
		waitFor(t, 30*time.Second, "whitelist entry active", func() (bool, string) { s := w.u.whitelistStatus(t, w.dest); return s == "active", s })
	}
	if code, body := alice.withdraw(t, "cut-alice-1", ethAssetID, finney(500), destA); code != 202 {
		t.Fatalf("withdraw: %d %s", code, body)
	}
	h := waitConfirmed(ctx, t, rpcc, alice, "cut-alice-1")
	assertPaid(ctx, t, eth, h, oldHot, destA, finney(500))
	if code, body := bob.withdraw(t, "cut-bob-1", tokenAssetID, eth1(100), destB); code != 202 {
		t.Fatalf("withdraw: %d %s", code, body)
	}
	waitConfirmed(ctx, t, rpcc, bob, "cut-bob-1")
	st.OldTT = tokenBalance(ctx, t, eth, token, oldHot).String()

	// An in-flight Gas Top-Up: carol's token deposit is mined and confirmed, automine is then switched off, so the
	// sweeper's top-up (Hot -> carol, native) sits in the mempool unmined.
	send(ctx, t, eth, key0, &token, big.NewInt(0), transferCalldata(t, carolDep, eth1(300)))
	setAutomine(ctx, t, rpcc, false)
	mine(ctx, t, rpcc, 5)
	waitFor(t, 90*time.Second, "the Gas Top-Up in the mempool", func() (bool, string) {
		p, _ := eth.PendingNonceAt(ctx, oldHot)
		l, _ := eth.NonceAt(ctx, oldHot, nil)
		return p > l, fmt.Sprintf("pending nonce %d, mined %d", p, l)
	})
	// A stuck withdrawal: signed and broadcast, then forgotten by the node (anvil_dropTransaction). It stays signed in
	// wallet-service's database and `broadcast` in withdrawal-service, and every later nonce queues behind it.
	if code, body := alice.withdraw(t, "cut-alice-stuck", ethAssetID, finney(200), destS); code != 202 {
		t.Fatalf("withdraw: %d %s", code, body)
	}
	var stuck string
	waitFor(t, 60*time.Second, "the withdrawal's tx in the mempool", func() (bool, string) {
		_, h := alice.status(t, "cut-alice-stuck")
		stuck = h
		return h != "", h
	})
	if err := rpcc.CallContext(ctx, nil, "anvil_dropTransaction", stuck); err != nil {
		t.Fatalf("anvil_dropTransaction: %v", err)
	}
	if st2, _ := alice.status(t, "cut-alice-stuck"); st2 == "confirmed" {
		t.Fatalf("the stuck withdrawal confirmed by itself")
	}
	st.StuckKey, st.StuckHash, st.StuckWei = "cut-alice-stuck", stuck, finney(200).String()
	p, _ := eth.PendingNonceAt(ctx, oldHot)
	l, _ := eth.NonceAt(ctx, oldHot, nil)
	t.Logf("BEFORE ends: old Hot %s holds %s wei and %s TT; chain nonce %d, pending %d (top-up in the pool), withdrawal tx %s dropped (stuck)",
		oldHot, balance(ctx, t, eth, oldHot), tokenBalance(ctx, t, eth, token, oldHot), l, p, stuck)
	st.save(t)
}

func cutoverFrozen(ctx context.Context, t *testing.T) {
	st := loadState(t)
	alice := st.Alice.user()
	code, body := alice.withdraw(t, "cut-frozen-1", ethAssetID, finney(50), common.HexToAddress(st.DestA))
	if code != 202 {
		t.Fatalf("a withdrawal during the freeze must be accepted: %d %s", code, body)
	}
	// It is not signed while signing is frozen: no tx hash, no signature row.
	time.Sleep(8 * time.Second)
	if status, hash := alice.status(t, "cut-frozen-1"); hash != "" {
		t.Fatalf("a withdrawal was signed during the freeze: %s %s", status, hash)
	}
	id := withdrawalID(t, alice, "cut-frozen-1")
	if n := psql(t, "custody", fmt.Sprintf(`SELECT count(*) FROM hot_wallet_signatures WHERE idempotency_key = 'withdrawal:%s'`, id)); n != "0" {
		t.Fatalf("%s signature rows for a withdrawal requested during the freeze", n)
	}
	if state := psql(t, "custody", `SELECT state FROM hot_cutover ORDER BY frozen_at DESC LIMIT 1`); state != "frozen" {
		t.Fatalf("hot_cutover state %q, want frozen", state)
	}
	st.FrozenKey = "cut-frozen-1"
	st.save(t)
}

func cutoverAfter(ctx context.Context, t *testing.T, eth *ethclient.Client, rpcc *rpc.Client) {
	st := loadState(t)
	key0 := mustKey(t, anvilKey0)
	token := ensureToken(ctx, t, eth, key0)
	oldHot := common.HexToAddress(st.OldHot)
	newHot := hotWalletAddress(ctx, t)
	alice, bob, carol := st.Alice.user(), st.Bob.user(), st.Carol.user()

	t.Run("the consumers serve the new Hot address", func(t *testing.T) {
		if newHot == oldHot {
			t.Fatalf("GET /v1/wallet/hot-address still answers the old Hot address %s", oldHot)
		}
		logs := compose(t, "logs", "wallet-service")
		if !strings.Contains(logs, "Hot Key: hsm-gateway at") || !strings.Contains(logs, "the hot_wallet seed is not loaded") {
			t.Fatalf("wallet-service does not run on the gateway backend:\n%s", logs)
		}
	})

	t.Run("the stuck withdrawal and the in-flight Gas Top-Up resolved, from the old Hot", func(t *testing.T) {
		waitFor(t, 90*time.Second, st.StuckKey+" confirmed", func() (bool, string) {
			s, h := alice.status(t, st.StuckKey)
			return s == "confirmed" && h != "", s + " " + h
		})
		_, h := alice.status(t, st.StuckKey)
		if !strings.EqualFold(h, st.StuckHash) {
			t.Errorf("the stuck withdrawal confirmed with tx %s, not the dropped one %s", h, st.StuckHash)
		}
		assertPaid(ctx, t, eth, h, oldHot, common.HexToAddress(st.DestS), finney(200))
		// carol's deposit is credited and swept into the NEW Hot (the sweeper re-reads the address at start)
		waitFor(t, 150*time.Second, "carol's deposit credited and swept", func() (bool, string) {
			mine(ctx, t, rpcc, 1)
			b := carol.balances(t)
			tt := tokenBalance(ctx, t, eth, token, common.HexToAddress(st.Carol.Dep))
			return b[tokenAssetID] == st.CarolTT && tt.Sign() == 0, fmt.Sprint(b, " on chain ", tt)
		})
		if b := carol.balances(t); b[ethAssetID] != "0" {
			t.Errorf("the Gas Top-Up was credited to carol as a deposit: %v", b)
		}
	})

	t.Run("the old Hot is empty and the new Hot holds the funds", func(t *testing.T) {
		if n := balance(ctx, t, eth, oldHot); n.Sign() != 0 {
			t.Errorf("old Hot still holds %s wei", n)
		}
		if tt := tokenBalance(ctx, t, eth, token, oldHot); tt.Sign() != 0 {
			t.Errorf("old Hot still holds %s TT", tt)
		}
		oldTT, _ := new(big.Int).SetString(st.OldTT, 10)
		carolTT, _ := new(big.Int).SetString(st.CarolTT, 10)
		if got, want := tokenBalance(ctx, t, eth, token, newHot), new(big.Int).Add(oldTT, carolTT); got.Cmp(want) != 0 {
			t.Errorf("new Hot holds %s TT, want %s (the old Hot's %s plus carol's sweep)", got, want, oldTT)
		}
		if n := balance(ctx, t, eth, newHot); n.Cmp(eth1(40)) <= 0 {
			t.Errorf("new Hot holds only %s wei of native", n)
		}
	})

	t.Run("withdrawals keep working, signed by hsm-gateway", func(t *testing.T) {
		// a withdrawal made during the freeze: never signed by the old key; if it went through, it is the new Hot's
		s, h := alice.status(t, st.FrozenKey)
		t.Logf("the withdrawal made during the freeze ended %q %s", s, h)
		if s == "confirmed" {
			assertPaid(ctx, t, eth, h, newHot, common.HexToAddress(st.DestA), finney(50))
		}
		if code, body := alice.withdraw(t, "cut-alice-after", ethAssetID, finney(300), common.HexToAddress(st.DestA)); code != 202 {
			t.Fatalf("withdraw: %d %s", code, body)
		}
		assertPaid(ctx, t, eth, waitConfirmed(ctx, t, rpcc, alice, "cut-alice-after"), newHot, common.HexToAddress(st.DestA), finney(300))
		if code, body := bob.withdraw(t, "cut-bob-after", tokenAssetID, eth1(50), common.HexToAddress(st.DestB)); code != 202 {
			t.Fatalf("withdraw: %d %s", code, body)
		}
		hash := waitConfirmed(ctx, t, rpcc, bob, "cut-bob-after")
		tx, _, err := eth.TransactionByHash(ctx, common.HexToHash(hash))
		if err != nil {
			t.Fatal(err)
		}
		rc, _ := eth.TransactionReceipt(ctx, tx.Hash())
		sender, err := eth.TransactionSender(ctx, tx, rc.BlockHash, rc.TransactionIndex)
		if err != nil || sender != newHot || tx.To() == nil || *tx.To() != token || len(tx.Data()) != 68 ||
			common.BytesToAddress(tx.Data()[16:36]) != common.HexToAddress(st.DestB) || new(big.Int).SetBytes(tx.Data()[36:]).Cmp(eth1(50)) != 0 {
			t.Errorf("bob's token withdrawal tx %s: sender %s (%v), want the new Hot %s paying 50 TT to %s", hash, sender, err, newHot, st.DestB)
		}
	})

	t.Run("nonce state and ledger invariants hold", func(t *testing.T) {
		if s := psql(t, "custody", `SELECT state FROM hot_cutover`); s != "done" {
			t.Errorf("hot_cutover state %q, want done", s)
		}
		old := strings.ToLower(oldHot.Hex())
		nw := strings.ToLower(newHot.Hex())
		if n := psql(t, "custody", fmt.Sprintf(`SELECT count(*) FROM hot_wallet_signatures WHERE hot_address = '%s' AND caller_cn = 'hot-cutover'`, old)); n != "2" {
			t.Errorf("%s drain signature rows on the old address, want 2 (token, native)", n)
		}
		if row := psql(t, "custody", fmt.Sprintf(`SELECT min(nonce) || '-' || max(nonce) || '/' || count(*) FROM hot_wallet_signatures WHERE hot_address = '%s'`, nw)); !strings.HasPrefix(row, "0-") {
			t.Errorf("the new Hot address's first nonce is not 0: %s", row)
		}
		oldNext := psql(t, "custody", fmt.Sprintf(`SELECT next_nonce FROM hot_wallet_nonces WHERE hot_address = '%s'`, old))
		if on, _ := eth.NonceAt(ctx, oldHot, nil); oldNext != fmt.Sprint(on) {
			t.Errorf("old next_nonce %s, chain nonce %d", oldNext, on)
		}
		for _, c := range []struct{ name, db, sql string }{
			{"W1 dense per Hot address", "custody", `SELECT count(*) FROM (SELECT chain_id, hot_address FROM hot_wallet_signatures GROUP BY chain_id, hot_address HAVING count(DISTINCT nonce)<>count(*) OR max(nonce)-min(nonce)+1<>count(*)) x`},
			{"W2 next_nonce = max signed + 1", "custody", `SELECT count(*) FROM hot_wallet_nonces n WHERE n.next_nonce <> COALESCE((SELECT max(nonce)+1 FROM hot_wallet_signatures s WHERE s.chain_id=n.chain_id AND s.hot_address=n.hot_address), n.next_nonce)`},
			{"W2b every signature's address has a row", "custody", `SELECT count(*) FROM hot_wallet_signatures s WHERE NOT EXISTS (SELECT 1 FROM hot_wallet_nonces n WHERE n.chain_id=s.chain_id AND n.hot_address=s.hot_address)`},
			{"I1 entries sum to zero per asset", "ledger", `SELECT count(*) FROM (SELECT a.asset_id FROM entries e JOIN accounts a ON a.id=e.account_id GROUP BY a.asset_id HAVING sum(e.amount)<>0) x`},
			{"I2 total_balance = sum(entries)", "ledger", `SELECT count(*) FROM accounts a WHERE a.total_balance <> COALESCE((SELECT sum(amount) FROM entries WHERE account_id=a.id),0)`},
			{"I3 locked_balance = sum(active locks)", "ledger", `SELECT count(*) FROM accounts a WHERE a.locked_balance <> COALESCE((SELECT sum(amount) FROM locks WHERE account_id=a.id AND status='active'),0)`},
			// I4 as in tests/e2e, plus the Gas Expense Account (ledger migration 000002): a posted gas fee moves value from
			// gas_expense to chain, so chain + gas_expense == -(users). Any drift in the drain's gas shows up here.
			{"I4 chain + gas expense = -(sum of user totals)", "ledger", `SELECT count(*) FROM (SELECT c.asset_id FROM accounts c WHERE c.kind='chain' AND c.total_balance + COALESCE((SELECT sum(g.total_balance) FROM accounts g WHERE g.kind='gas_expense' AND g.asset_id=c.asset_id),0) <> -COALESCE((SELECT sum(u.total_balance) FROM accounts u WHERE u.kind='user' AND u.asset_id=c.asset_id),0)) x`},
			{"I5 every ledger tx has >=2 entries summing to 0", "ledger", `SELECT count(*) FROM ledger_transactions lt WHERE (SELECT count(*) FROM entries WHERE ledger_transaction_id=lt.id)<2 OR (SELECT sum(amount) FROM entries WHERE ledger_transaction_id=lt.id)<>0`},
			{"I5b settled lock <=> withdrawal_settle tx", "ledger", `SELECT count(*) FROM locks l WHERE (l.status='settled') <> EXISTS (SELECT 1 FROM ledger_transactions WHERE idempotency_key=l.idempotency_key AND source_type='withdrawal_settle')`},
			{"I6 released/active locks post no entry", "ledger", `SELECT count(*) FROM locks l JOIN ledger_transactions t ON t.idempotency_key=l.idempotency_key WHERE l.status<>'settled'`},
			{"I7 deposit natural key unique", "ledger", `SELECT count(*) FROM (SELECT 1 FROM ledger_transactions WHERE tx_hash IS NOT NULL GROUP BY chain_id,tx_hash,log_index HAVING count(*)>1) x`},
			{"the drain's gas is in the ledger, once per drain tx", "ledger", `SELECT count(*) FROM (SELECT 1) x WHERE (SELECT count(*) FROM ledger_transactions WHERE source_type='gas_fee' AND purpose='treasury_sweep') <> 2`},
		} {
			if got := psql(t, c.db, c.sql); got != "0" {
				t.Errorf("INVARIANT VIOLATED: %s (%s)", c.name, got)
			}
		}
	})
}
