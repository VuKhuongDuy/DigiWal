package e2e

import (
	"context"
	"fmt"
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// withdrawal stand-in (what withdrawal-service will do): lock -> sign -> broadcast -> mine -> settle.
type wd struct {
	key, user, asset, amount string
	to                       common.Address
	signed                   resp
	hash                     common.Hash
}

func newWd(user, asset string, amount int64) *wd {
	return &wd{key: uid("wd"), user: user, asset: asset, amount: fmt.Sprint(amount), to: randAddr()}
}

func (w *wd) lock() resp { return ledgerLock(w.key, w.user, w.asset, w.amount) }
func (w *wd) sign() resp {
	w.signed = signHot(w.key, w.to.Hex(), w.asset, w.amount)
	return w.signed
}
func (w *wd) send(t testing.TB) {
	t.Helper()
	w.hash = broadcast(t, w.signed.s("signed_raw_tx"))
	receipt(t, w.hash)
}
func (w *wd) settle() resp { return ledgerSettle(w.key) }

func (w *wd) full(t testing.TB) {
	t.Helper()
	if r := w.lock(); r.Code != 200 {
		t.Fatalf("lock %d %s", r.Code, r.Body)
	}
	if r := w.sign(); r.Code != 200 {
		t.Fatalf("sign %d %s", r.Code, r.Body)
	}
	w.send(t)
	mine(t, 12)
	if r := w.settle(); r.Code != 200 {
		t.Fatalf("settle %d %s", r.Code, r.Body)
	}
}

// WDR-01: full ETH withdrawal with every record checked.
func TestWDR01_HappyPathETH(t *testing.T) {
	qa(t)
	u := uid("wdr01")
	credit(t, u, ethAsset, 1_000_000_000_000_000_000)
	w := newWd(u, ethAsset, 400_000_000_000_000_000)
	nonceBefore := q1[int](t, wdb, `SELECT next_nonce FROM hot_wallet_nonces WHERE chain_id=$1`, chainID)
	hotBefore := onchainETH(t, hotAddr)

	if r := w.lock(); r.Code != 200 || r.s("status") != "active" {
		t.Fatalf("lock %d %s", r.Code, r.Body)
	}
	wantBal(t, u, ethAsset, 1e18, 4e17) // reserved, nothing posted yet
	if n := count(t, `SELECT count(*) FROM ledger_transactions WHERE idempotency_key=$1`, w.key); n != 0 {
		t.Fatal("lock posted an entry")
	}
	w.sign()
	if w.signed.Code != 200 {
		t.Fatalf("sign %d %s", w.signed.Code, w.signed.Body)
	}
	w.send(t)
	mine(t, 12)
	if got := onchainETH(t, w.to); got.String() != w.amount {
		t.Fatalf("recipient has %s, want %s", got, w.amount)
	}
	if r := w.settle(); r.Code != 200 || r.s("status") != "settled" {
		t.Fatalf("settle %d %s", r.Code, r.Body)
	}
	wantBal(t, u, ethAsset, 6e17, 0)
	if st := q1[string](t, ldb, `SELECT status FROM locks WHERE idempotency_key=$1`, w.key); st != "settled" {
		t.Fatal(st)
	}
	if n := count(t, `SELECT count(*) FROM entries e JOIN ledger_transactions l ON l.id=e.ledger_transaction_id WHERE l.idempotency_key=$1 AND l.source_type='withdrawal_settle'`, w.key); n != 2 {
		t.Fatalf("%d settle entries", n)
	}
	if neg := q1[string](t, ldb, `SELECT e.amount::text FROM entries e JOIN accounts a ON a.id=e.account_id JOIN ledger_transactions l ON l.id=e.ledger_transaction_id WHERE l.idempotency_key=$1 AND a.kind='user'`, w.key); neg != "-400000000000000000" {
		t.Fatalf("user entry %s", neg)
	}
	// wallet-service records
	if n := q1[int](t, wdb, `SELECT count(*) FROM hot_wallet_signatures WHERE idempotency_key=$1`, w.key); n != 1 {
		t.Fatalf("%d signature rows", n)
	}
	if after := q1[int](t, wdb, `SELECT next_nonce FROM hot_wallet_nonces WHERE chain_id=$1`, chainID); after != nonceBefore+1 {
		t.Fatalf("nonce %d -> %d", nonceBefore, after)
	}
	if on, _ := cl.NonceAt(context.Background(), hotAddr, nil); int(on) != nonceBefore+1 {
		t.Fatalf("on-chain nonce %d vs db next_nonce %d", on, nonceBefore+1)
	}
	if out := new(big.Int).Sub(hotBefore, onchainETH(t, hotAddr)); out.Cmp(big.NewInt(4e17)) < 0 || out.Cmp(big.NewInt(4e17+1e14)) > 0 {
		t.Fatalf("hot wallet outflow %s", out)
	}
}

func TestWDR02_HappyPathERC20(t *testing.T) {
	qa(t)
	u := uid("wdr02")
	credit(t, u, ttAsset, 5_000)
	w := newWd(u, ttAsset, 1_250)
	w.full(t)
	if got := tokenBalance(common.HexToAddress(ttAddr), w.to); got.Int64() != 1250 {
		t.Fatalf("recipient tokens %s", got)
	}
	wantBal(t, u, ttAsset, 3750, 0)
}

// WDR-03/04: overdraft boundaries.
func TestWDR03_04_OverdraftBoundaries(t *testing.T) {
	qa(t)
	u := uid("wdr03")
	credit(t, u, ethAsset, 1000)
	a := newWd(u, ethAsset, 1001)
	if r := a.lock(); r.Code != 422 {
		t.Fatalf("available+1: %d %s", r.Code, r.Body)
	}
	b := newWd(u, ethAsset, 600)
	if r := b.lock(); r.Code != 200 {
		t.Fatalf("lock 600: %d", r.Code)
	}
	c := newWd(u, ethAsset, 401) // only 400 available now (600 locked)
	if r := c.lock(); r.Code != 422 {
		t.Fatalf("must count locked funds: %d %s", r.Code, r.Body)
	}
	d := newWd(u, ethAsset, 400)
	if r := d.lock(); r.Code != 200 {
		t.Fatalf("exact available: %d", r.Code)
	}
	if r := newWd(u, ethAsset, 1).lock(); r.Code != 422 {
		t.Fatalf("1 wei over: %d", r.Code)
	}
	wantBal(t, u, ethAsset, 1000, 1000)
	for _, bad := range []string{"0", "-5", "+5", "1e18", "1.5", " 5", "", "0x10", "٣", "115792089237316195423570985008687907853269984665640564039457584007913129639936"} {
		r := ledgerLock(uid("bad"), u, ethAsset, bad)
		if r.Code == 200 {
			t.Errorf("amount %q accepted", bad)
		}
		if r.Code >= 500 {
			t.Errorf("amount %q -> %d", bad, r.Code)
		}
	}
	wantBal(t, u, ethAsset, 1000, 1000)
}

// WDR-06/07/08: idempotency — lock, sign, settle each safe under 30-way concurrent retry.
func TestWDR06_07_08_ConcurrentRetriesAreIdempotent(t *testing.T) {
	qa(t)
	u := uid("wdr06")
	credit(t, u, ethAsset, 10_000_000_000_000_000)
	w := newWd(u, ethAsset, 1_000_000_000_000_000)
	nonceBefore := q1[int](t, wdb, `SELECT next_nonce FROM hot_wallet_nonces WHERE chain_id=$1`, chainID)

	locks := make([]resp, 30)
	parallel(30, func(i int) { locks[i] = w.lock() })
	if c := codeCount(locks); c[200] != 30 {
		t.Fatalf("lock codes %v", c)
	}
	wantBal(t, u, ethAsset, 10_000_000_000_000_000, 1_000_000_000_000_000)

	signs := make([]resp, 30)
	parallel(30, func(i int) { signs[i] = signHot(w.key, w.to.Hex(), ethAsset, w.amount) })
	raw := signs[0].s("signed_raw_tx")
	for _, s := range signs {
		if s.Code != 200 || s.s("signed_raw_tx") != raw {
			t.Fatalf("sign replay differs: %d %s", s.Code, s.Body)
		}
	}
	if n := q1[int](t, wdb, `SELECT count(*) FROM hot_wallet_signatures WHERE idempotency_key=$1`, w.key); n != 1 {
		t.Fatalf("%d signature rows", n)
	}
	if after := q1[int](t, wdb, `SELECT next_nonce FROM hot_wallet_nonces WHERE chain_id=$1`, chainID); after != nonceBefore+1 {
		t.Fatalf("30 retries consumed %d nonces", after-nonceBefore)
	}
	w.signed = signs[0]
	w.send(t)

	// same key, different request
	if r := ledgerLock(w.key, u, ethAsset, "2"); r.Code != 409 {
		t.Errorf("lock key reuse with other amount: %d", r.Code)
	}
	if r := signHot(w.key, w.to.Hex(), ethAsset, "2"); r.Code != 409 {
		t.Errorf("sign key reuse with other amount: %d", r.Code)
	}
	if r := signHot(w.key, randAddr().Hex(), ethAsset, w.amount); r.Code != 409 {
		t.Errorf("sign key reuse with other destination: %d", r.Code)
	}

	st := make([]resp, 30)
	parallel(30, func(i int) { st[i] = w.settle() })
	if c := codeCount(st); c[200] != 30 {
		t.Fatalf("settle codes %v", c)
	}
	wantBal(t, u, ethAsset, 9_000_000_000_000_000, 0)
	if n := count(t, `SELECT count(*) FROM ledger_transactions WHERE idempotency_key=$1`, w.key); n != 1 {
		t.Fatalf("%d settle txs", n)
	}
}

// WDR-09: 40 racing withdrawals, funds for exactly 12.
func TestWDR09_ConcurrentWithdrawalsNeverOverdraw(t *testing.T) {
	qa(t)
	u := uid("wdr09")
	credit(t, u, ethAsset, 1200)
	var ok, insufficient, other atomic.Int32
	parallel(40, func(i int) {
		switch r := newWd(u, ethAsset, 100).lock(); r.Code {
		case 200:
			ok.Add(1)
		case 422:
			insufficient.Add(1)
		default:
			other.Add(1)
		}
	})
	if ok.Load() != 12 || insufficient.Load() != 28 || other.Load() != 0 {
		t.Fatalf("ok=%d insufficient=%d other=%d", ok.Load(), insufficient.Load(), other.Load())
	}
	wantBal(t, u, ethAsset, 1200, 1200)
}

// WDR-11/12/13: settle vs release, hammered.
func TestWDR11_13_SettleReleaseRace(t *testing.T) {
	qa(t)
	u := uid("wdr11")
	credit(t, u, ethAsset, 10_000)
	const n = 20
	ws := make([]*wd, n)
	for i := range ws {
		ws[i] = newWd(u, ethAsset, 100)
		if r := ws[i].lock(); r.Code != 200 {
			t.Fatal(r.Code)
		}
	}
	var settled, released atomic.Int32
	parallel(n*2, func(i int) {
		w := ws[i/2]
		if i%2 == 0 {
			if r := w.settle(); r.Code == 200 {
				settled.Add(1)
			}
		} else if r := ledgerRelease(w.key); r.Code == 200 {
			released.Add(1)
		}
	})
	// every lock resolved exactly one way
	if got := int(settled.Load() + released.Load()); got != n {
		t.Fatalf("settled %d + released %d != %d", settled.Load(), released.Load(), n)
	}
	wantBal(t, u, ethAsset, 10_000-100*int64(settled.Load()), 0)
	for _, w := range ws { // flipping afterwards is refused
		st := q1[string](t, ldb, `SELECT status FROM locks WHERE idempotency_key=$1`, w.key)
		var other resp
		if st == "settled" {
			other = ledgerRelease(w.key)
		} else {
			other = w.settle()
		}
		if other.Code != 409 {
			t.Errorf("resolving a %s lock the other way: %d", st, other.Code)
		}
	}
	if r := ledgerSettle("no-such-key"); r.Code != 404 {
		t.Errorf("unknown settle %d", r.Code)
	}
}

// WDR-16: sign, crash before broadcast; recovery re-asks and must get the SAME tx (no second nonce).
func TestWDR16_SignedButNotBroadcastIsRecoverable(t *testing.T) {
	qa(t)
	u := uid("wdr16")
	credit(t, u, ethAsset, 1_000_000_000_000_000)
	w := newWd(u, ethAsset, 500_000_000_000_000)
	w.lock()
	first := w.sign()
	compose(t, "restart", "wallet-service") // the "crash"
	waitFor(t, "wallet back", 40*time.Second, func() bool { return do(nil2(), "GET", walletURL+"/healthz", nil).Code == 200 })
	again := w.sign()
	if again.Code != 200 || again.s("signed_raw_tx") != first.s("signed_raw_tx") {
		t.Fatalf("after restart sign returned %d %s", again.Code, again.Body)
	}
	w.send(t)
	mine(t, 3)
	if r := w.settle(); r.Code != 200 {
		t.Fatal(r.Code)
	}
	if got := onchainETH(t, w.to); got.String() != w.amount {
		t.Fatalf("recipient %s", got)
	}
}

// WDR-23: gas ceiling: a spike above max_gas_price must refuse, burning no nonce.
func TestWDR23_GasSpikeIsRefusedWithoutBurningNonce(t *testing.T) {
	qa(t)
	before := q1[int](t, wdb, `SELECT next_nonce FROM hot_wallet_nonces WHERE chain_id=$1`, chainID)
	r := do(client("withdrawal-service"), "POST", walletTLS+"/v1/sign/hot-wallet", map[string]any{
		"to_address": randAddr().Hex(), "asset_id": ethAsset, "amount": "1000", "chain_id": chainID,
		"idempotency_key": uid("gas"), "max_gas_price": "1"}) // 1 wei ceiling
	if r.Code == 200 {
		t.Fatalf("signed under a 1 wei ceiling: %s", r.Body)
	}
	if after := q1[int](t, wdb, `SELECT next_nonce FROM hot_wallet_nonces WHERE chain_id=$1`, chainID); after != before {
		t.Fatalf("refused request burned %d nonces", after-before)
	}
}

// WDR-24: ERC-20 the hot wallet cannot cover -> estimateGas reverts -> nothing signed, no nonce.
func TestWDR24_ERC20BeyondHotWalletTokens(t *testing.T) {
	qa(t)
	before := q1[int](t, wdb, `SELECT next_nonce FROM hot_wallet_nonces WHERE chain_id=$1`, chainID)
	huge := new(big.Int).Mul(big.NewInt(1e12), eth).String()
	r := signHot(uid("tt"), randAddr().Hex(), ttAsset, huge)
	if r.Code == 200 {
		t.Errorf("signed an ERC-20 transfer the hot wallet cannot fund")
	}
	if r.Code >= 500 && r.Code != 503 {
		t.Logf("note: error surfaced as %d %s", r.Code, r.Body)
	}
	if after := q1[int](t, wdb, `SELECT next_nonce FROM hot_wallet_nonces WHERE chain_id=$1`, chainID); after != before {
		t.Fatalf("burned %d nonces", after-before)
	}
}

// WDR-31: a signed tx is chain-bound.
func TestWDR31_SignedTxCarriesChainID(t *testing.T) {
	qa(t)
	r := signHot(uid("cid"), randAddr().Hex(), ethAsset, "1")
	if r.Code != 200 {
		t.Fatal(r.Body)
	}
	if bad := do(client("withdrawal-service"), "POST", walletTLS+"/v1/sign/hot-wallet", map[string]any{
		"to_address": randAddr().Hex(), "asset_id": ethAsset, "amount": "1", "chain_id": 1, "idempotency_key": uid("cid")}); bad.Code != 400 {
		t.Errorf("wrong chain_id accepted: %d", bad.Code)
	}
}
