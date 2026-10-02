package sweep

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"digiwal/services/sweeper/internal/ledger"
	"digiwal/services/sweeper/internal/wallet"
)

type fakeLedger struct {
	mu    sync.Mutex
	posts []ledger.GasFee
	errs  []error // returned, in order, before succeeding
}

func (l *fakeLedger) PostGasFee(_ context.Context, g ledger.GasFee) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.errs) > 0 {
		err := l.errs[0]
		l.errs = l.errs[1:]
		return err
	}
	l.posts = append(l.posts, g)
	return nil
}

// bumpSweeper is a sweeper whose chain grows 10 blocks per look, so "20 blocks" passes in two polls.
func bumpSweeper(w *world) (*Sweeper, *[]string, *fakeLedger) {
	var alerts []string
	led := &fakeLedger{}
	w.stuckTopUpOnly = true
	s := newSweeper(w)
	s.Ledger, s.Alert = led, func(m string) { alerts = append(alerts, m) }
	s.Config.InclusionTimeout = 2 * 1e9
	w.headStep = 10
	return s, &alerts, led
}

func tokenDeposit(w *world) {
	w.tokens[user] = big.NewInt(1234) // no native balance: needs a Gas Top-Up
}

// A stuck, underpriced top-up is re-signed at the same nonce once 20 blocks have passed since it was sent.
func TestStuckUnderpricedTopUpIsFeeBumpedAfter20Blocks(t *testing.T) {
	w := newWorld()
	tokenDeposit(w)
	w.stuck, w.underpriced, w.mineBumpSeq = true, true, 1
	s, alerts, led := bumpSweeper(w)
	ok(t, s.Sweep(context.Background(), deposit(tokenAsset, big.NewInt(1234), 3)))

	if len(w.bumpCalls) != 1 || w.bumpCalls[0] != 1 {
		t.Fatalf("replace calls = %v, want exactly seq 1", w.bumpCalls)
	}
	if len(*alerts) != 0 {
		t.Fatalf("alerts %q", *alerts)
	}
	// The sweep that follows went through, and the gas reported is the MINED version's (the Fee Bump).
	if bal(w.tokens, hot).Cmp(big.NewInt(1234)) != 0 {
		t.Fatal("the token sweep did not happen after the bumped top-up mined")
	}
	key := TopUpKey(chainID, txHash, 3)
	bumped := common.HexToHash(w.reps[key][0].Hash)
	var topupPosts []ledger.GasFee
	for _, g := range led.posts {
		if g.Purpose == "gas_topup" {
			topupPosts = append(topupPosts, g)
		}
	}
	if len(topupPosts) != 1 || common.HexToHash(topupPosts[0].TxHash) != bumped || topupPosts[0].GasUsed != 21000 || topupPosts[0].GasPrice.Int64() != 2_000_000_000 || topupPosts[0].ChainID != chainID {
		t.Fatalf("gas_topup posts = %+v, want the bump %s", topupPosts, bumped.Hex())
	}
}

// Not underpriced, just slow: a bump would only cost more, so nothing is replaced.
func TestSlowButNotUnderpricedTopUpIsNotBumped(t *testing.T) {
	w := newWorld()
	tokenDeposit(w)
	w.stuck, w.underpriced = true, false
	s, _, _ := bumpSweeper(w)
	s.Config.InclusionTimeout = 200 * 1e6
	if err := s.Sweep(context.Background(), deposit(tokenAsset, big.NewInt(1234), 0)); err == nil {
		t.Fatal("a never-mined top-up should time out, not succeed")
	}
	if len(w.bumpCalls) != 0 {
		t.Fatalf("bumped a tx that was not underpriced: %v", w.bumpCalls)
	}
}

// Before 20 blocks have passed nothing is bumped, however underpriced.
func TestNoBumpBefore20Blocks(t *testing.T) {
	w := newWorld()
	tokenDeposit(w)
	w.stuck, w.underpriced = true, true
	s, _, _ := bumpSweeper(w)
	w.headStep = 1
	s.Config.InclusionTimeout = 150 * 1e6 // ~150 polls of 1ms at 1 block each would pass 20; a couple do not
	s.Config.PollInterval = 70 * 1e6
	_ = s.Sweep(context.Background(), deposit(tokenAsset, big.NewInt(1234), 0))
	if len(w.bumpCalls) != 0 {
		t.Fatalf("bumped before 20 blocks: %v", w.bumpCalls)
	}
}

// The node saying "underpriced" on the first broadcast bumps straight away.
func TestUnderpricedBroadcastBumpsImmediately(t *testing.T) {
	w := newWorld()
	tokenDeposit(w)
	w.stuck, w.mineBumpSeq = true, 1
	first := true
	w.broadcastErr = func([]byte) error {
		if first {
			first = false
			return errors.New("server returned an error response: error code -32003: max fee per gas less than block base fee")
		}
		return nil
	}
	s, _, _ := bumpSweeper(w)
	w.headStep = 0 // no blocks pass: only the node's refusal can trigger it
	ok(t, s.Sweep(context.Background(), deposit(tokenAsset, big.NewInt(1234), 0)))
	if len(w.bumpCalls) != 1 {
		t.Fatalf("replace calls = %v", w.bumpCalls)
	}
}

// At most 5 bumps, then an ALERT and no more.
func TestAtMostFiveBumpsThenAnAlert(t *testing.T) {
	w := newWorld()
	tokenDeposit(w)
	w.stuck, w.underpriced = true, true // never mined at any price
	s, alerts, _ := bumpSweeper(w)
	s.Config.InclusionTimeout = 600 * 1e6
	if err := s.Sweep(context.Background(), deposit(tokenAsset, big.NewInt(1234), 0)); err == nil {
		t.Fatal("expected a timeout")
	}
	if len(w.bumpCalls) != 5 {
		t.Fatalf("replace calls = %v, want exactly 5 (seq 1..5)", w.bumpCalls)
	}
	for i, c := range w.bumpCalls {
		if c != i+1 {
			t.Fatalf("replace calls = %v: seqs must be consecutive", w.bumpCalls)
		}
	}
	if len(*alerts) != 1 || !strings.Contains((*alerts)[0], "5 Fee Bumps") {
		t.Fatalf("alerts = %q", *alerts)
	}
}

// wallet-service refusing (the 50 gwei ceiling, a Nonce Filler) is an ALERT, and the sweeper stops asking.
func TestARefusedBumpIsAnAlertAndStopsTrying(t *testing.T) {
	w := newWorld()
	tokenDeposit(w)
	w.stuck, w.underpriced = true, true
	w.bumpErr = fmt.Errorf("%w: /v1/sign/hot-wallet/k/replace 422 fee ceiling", wallet.ErrPermanent)
	s, alerts, _ := bumpSweeper(w)
	s.Config.InclusionTimeout = 400 * 1e6
	_ = s.Sweep(context.Background(), deposit(tokenAsset, big.NewInt(1234), 0))
	if len(w.bumpCalls) != 1 || len(*alerts) != 1 || !strings.Contains((*alerts)[0], "refused") {
		t.Fatalf("calls %v alerts %q", w.bumpCalls, *alerts)
	}
}

// A transient failure of the replace call is just tried again, with the same seq.
func TestATransientBumpFailureIsRetriedWithTheSameSeq(t *testing.T) {
	w := newWorld()
	tokenDeposit(w)
	w.stuck, w.underpriced, w.mineBumpSeq = true, true, 1
	w.bumpErr = errors.New("wallet-service /replace answered 503")
	s, _, _ := bumpSweeper(w)
	var once sync.Once
	s.Wallet = &flakyReplace{world: w, heal: func() { once.Do(func() { w.mu.Lock(); w.bumpErr = nil; w.mu.Unlock() }) }}
	ok(t, s.Sweep(context.Background(), deposit(tokenAsset, big.NewInt(1234), 0)))
	for _, c := range w.bumpCalls {
		if c != 1 {
			t.Fatalf("replace calls = %v: a retry must ask for seq 1 again", w.bumpCalls)
		}
	}
}

type flakyReplace struct {
	*world
	heal func()
}

func (f *flakyReplace) ReplaceHotWallet(ctx context.Context, key string, seq int) ([]byte, string, error) {
	raw, h, err := f.world.ReplaceHotWallet(ctx, key, seq)
	if err != nil {
		f.heal()
	}
	return raw, h, err
}

// A retry (after a crash or a timeout) carries on from the key's latest replacement and watches every
// hash: here the ORIGINAL is the one that ends up mined, and that older version counts.
func TestARetryWatchesEveryVersionAndAnOlderMinedOneCounts(t *testing.T) {
	w := newWorld()
	tokenDeposit(w)
	w.stuck, w.underpriced = true, true
	s, _, led := bumpSweeper(w)
	s.Config.InclusionTimeout = 300 * 1e6
	ev := deposit(tokenAsset, big.NewInt(1234), 0)
	_ = s.Sweep(context.Background(), ev) // first attempt: bumps, never mined, times out
	if len(w.bumpCalls) == 0 {
		t.Fatal("setup: nothing was bumped")
	}
	key := TopUpKey(chainID, txHash, 0)
	original := crypto32(w.hotByKey[key])
	w.minable[original] = true // the network finally takes the ORIGINAL
	before := len(w.bumpCalls)
	s.Config.InclusionTimeout = 2 * 1e9
	ok(t, s.Sweep(context.Background(), ev))
	if len(w.bumpCalls) != before {
		t.Fatalf("the retry bumped again although the original was mineable: %v", w.bumpCalls)
	}
	var topup []ledger.GasFee
	for _, g := range led.posts {
		if g.Purpose == "gas_topup" {
			topup = append(topup, g)
		}
	}
	if len(topup) != 1 || common.HexToHash(topup[0].TxHash) != original {
		t.Fatalf("gas_topup posts = %+v, want the original %s", topup, original.Hex())
	}
}

func crypto32(raw []byte) common.Hash { return hashOf(raw) }

// invariant #12: each mined Sweep and Gas Top-Up posts its gas, once, with the receipt's numbers.
func TestMinedSweepAndTopUpPostTheirGas(t *testing.T) {
	w := newWorld()
	tokenDeposit(w)
	s, alerts, led := bumpSweeper(w)
	w.headStep = 0
	ok(t, s.Sweep(context.Background(), deposit(tokenAsset, big.NewInt(1234), 0)))
	if len(led.posts) != 2 || len(*alerts) != 0 {
		t.Fatalf("posts %+v alerts %q", led.posts, *alerts)
	}
	purposes := led.posts[0].Purpose + "," + led.posts[1].Purpose
	if purposes != "gas_topup,sweep" {
		t.Fatalf("purposes = %s", purposes)
	}
	for _, g := range led.posts {
		if g.ChainID != chainID || g.GasUsed != 21000 || g.GasPrice.Int64() != 2_000_000_000 || !strings.HasPrefix(g.TxHash, "0x") || g.TxHash != strings.ToLower(g.TxHash) {
			t.Fatalf("%+v", g)
		}
	}
	// A redelivery moves nothing and posts nothing new.
	ok(t, s.Sweep(context.Background(), deposit(tokenAsset, big.NewInt(1234), 0)))
	if len(led.posts) != 2 {
		t.Fatalf("a redelivery posted gas again: %d", len(led.posts))
	}
}

func TestNativeSweepPostsItsGas(t *testing.T) {
	w := newWorld()
	w.native[user] = eth(5)
	s, _, led := bumpSweeper(w)
	w.headStep = 0
	ok(t, s.Sweep(context.Background(), deposit(nativeAsset, eth(5), -1)))
	if len(led.posts) != 1 || led.posts[0].Purpose != "sweep" {
		t.Fatalf("%+v", led.posts)
	}
}

// Best effort: a ledger outage is retried a few times, then an ALERT — and never fails the Sweep, which
// a redelivery could not repeat (nothing is left to move).
func TestGasPostingIsBestEffortAndNeverFailsTheSweep(t *testing.T) {
	w := newWorld()
	w.native[user] = eth(5)
	s, alerts, led := bumpSweeper(w)
	w.headStep = 0
	led.errs = []error{errors.New("503"), errors.New("503"), errors.New("503")}
	ok(t, s.Sweep(context.Background(), deposit(nativeAsset, eth(5), -1)))
	if len(led.posts) != 0 || len(*alerts) != 1 || !strings.Contains((*alerts)[0], "could not record") {
		t.Fatalf("posts %+v alerts %q", led.posts, *alerts)
	}
	if bal(w.native, user).Sign() != 0 {
		t.Fatal("the sweep did not happen")
	}
	// A transient error that clears within the attempts is simply retried.
	w2 := newWorld()
	w2.native[user] = eth(5)
	s2, alerts2, led2 := bumpSweeper(w2)
	w2.headStep = 0
	led2.errs = []error{errors.New("503")}
	ok(t, s2.Sweep(context.Background(), deposit(nativeAsset, eth(5), -1)))
	if len(led2.posts) != 1 || len(*alerts2) != 0 {
		t.Fatalf("posts %+v alerts %q", led2.posts, *alerts2)
	}
}

func TestARefusedGasFeeIsAnAlertWithoutRetries(t *testing.T) {
	w := newWorld()
	w.native[user] = eth(5)
	s, alerts, led := bumpSweeper(w)
	w.headStep = 0
	led.errs = []error{fmt.Errorf("%w (409): different values", ledger.ErrRefused)}
	ok(t, s.Sweep(context.Background(), deposit(nativeAsset, eth(5), -1)))
	if len(*alerts) != 1 || !strings.Contains((*alerts)[0], "refused") || len(led.errs) != 0 {
		t.Fatalf("alerts %q, errs left %v", *alerts, led.errs)
	}
}
