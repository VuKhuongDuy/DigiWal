package sweep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"digiwal/pkg/events"
	"digiwal/services/sweeper/internal/chain"
	"digiwal/services/sweeper/internal/registry"
	"digiwal/services/sweeper/internal/screening"
	"digiwal/services/sweeper/internal/wallet"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	nativeAsset = "11111111-1111-1111-1111-111111111111"
	tokenAsset  = "22222222-2222-2222-2222-222222222222"
	chainID     = int64(31337)
	txHash      = "0xabababababababababababababababababababababababababababababababab"
	feeWei      = int64(100_000_000_000_000) // 0.0001 ETH per user-address tx
)

var (
	user     = common.HexToAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")
	hot      = common.HexToAddress("0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC")
	sender   = common.HexToAddress("0x15d34AAf54267DB7D7c367839AAf71A00a2C6A65")
	tokenAdr = common.HexToAddress("0x5FbDB2315678afecb367f032d93F642f64180aa3")
	eth      = func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }
)

// op is what the fake wallet "signs": the fake chain interprets it when it is mined.
type op struct {
	Kind   string // topup | sweep-token | sweep-native
	From   common.Address
	To     common.Address
	Amount *big.Int
}

// world is a tiny chain plus a wallet-service, wired to behave the way the real ones do.
type world struct {
	mu       sync.Mutex
	native   map[common.Address]*big.Int
	tokens   map[common.Address]*big.Int
	latest   map[common.Address]uint64
	pending  map[common.Hash]op
	mined    map[common.Hash]bool // value: success
	autoMine bool
	salt     int

	// wallet side
	userReqs []wallet.UserRequest
	hotReqs  []wallet.HotRequest
	hotByKey map[string][]byte
	hotErr   error
	userErr  error
	// chain side
	broadcasts int
	failMined  bool // mined txs revert
	stuck      bool // txs never mine
}

func newWorld() *world {
	return &world{native: map[common.Address]*big.Int{}, tokens: map[common.Address]*big.Int{}, latest: map[common.Address]uint64{},
		pending: map[common.Hash]op{}, mined: map[common.Hash]bool{}, autoMine: true, hotByKey: map[string][]byte{}}
}

func bal(m map[common.Address]*big.Int, a common.Address) *big.Int {
	if v, ok := m[a]; ok {
		return v
	}
	return new(big.Int)
}

// ---- chain.Chain ----
func (w *world) ChainID(context.Context) (int64, error) { return chainID, nil }
func (w *world) BalanceAt(_ context.Context, a common.Address) (*big.Int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return new(big.Int).Set(bal(w.native, a)), nil
}
func (w *world) TokenBalance(_ context.Context, _, h common.Address) (*big.Int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return new(big.Int).Set(bal(w.tokens, h)), nil
}
func (w *world) LatestNonce(_ context.Context, a common.Address) (uint64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.latest[a], nil
}
func (w *world) PendingNonce(_ context.Context, a common.Address) (uint64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := w.latest[a]
	for _, o := range w.pending {
		if o.From == a {
			n++
		}
	}
	return n, nil
}
func (w *world) Broadcast(_ context.Context, raw []byte) (common.Hash, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.broadcasts++
	h := crypto.Keccak256Hash(raw)
	var o op
	if err := json.Unmarshal(raw, &o); err != nil {
		return common.Hash{}, err
	}
	if _, done := w.mined[h]; !done {
		if _, p := w.pending[h]; !p {
			w.pending[h] = o
		}
	}
	return h, nil
}
func (w *world) Receipt(_ context.Context, h common.Hash) (*chain.Receipt, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ok, done := w.mined[h]; done {
		return &chain.Receipt{Success: ok, Block: 1}, nil
	}
	if o, p := w.pending[h]; p && w.autoMine && !w.stuck {
		w.mine(h, o)
		return &chain.Receipt{Success: !w.failMined, Block: 1}, nil
	}
	return nil, nil
}
func (w *world) mine(h common.Hash, o op) {
	delete(w.pending, h)
	w.mined[h] = !w.failMined
	w.latest[o.From]++
	if w.failMined {
		return
	}
	fee := big.NewInt(feeWei)
	switch o.Kind {
	case "topup":
		w.native[o.From] = new(big.Int).Sub(bal(w.native, o.From), o.Amount)
		w.native[o.To] = new(big.Int).Add(bal(w.native, o.To), o.Amount)
	case "sweep-token":
		w.tokens[o.From] = new(big.Int).Sub(bal(w.tokens, o.From), o.Amount)
		w.tokens[o.To] = new(big.Int).Add(bal(w.tokens, o.To), o.Amount)
		w.native[o.From] = new(big.Int).Sub(bal(w.native, o.From), fee)
	case "sweep-native":
		w.native[o.From] = new(big.Int).Sub(bal(w.native, o.From), new(big.Int).Add(o.Amount, fee))
		w.native[o.To] = new(big.Int).Add(bal(w.native, o.To), o.Amount)
	}
}

// ---- Wallet ----
func encodeOp(o op) []byte { b, _ := json.Marshal(o); return b }

func (w *world) SignHotWallet(_ context.Context, req wallet.HotRequest) ([]byte, uint64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.hotReqs = append(w.hotReqs, req)
	if w.hotErr != nil {
		return nil, 0, w.hotErr
	}
	if raw, ok := w.hotByKey[req.IdempotencyKey]; ok { // idempotent per key, like wallet-service
		return raw, 7, nil
	}
	amt, _ := new(big.Int).SetString(req.Amount, 10)
	raw := encodeOp(op{Kind: "topup", From: hot, To: common.HexToAddress(req.ToAddress), Amount: amt})
	w.hotByKey[req.IdempotencyKey] = raw
	return raw, 7, nil
}

func (w *world) SignUserAddress(_ context.Context, req wallet.UserRequest) ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.userReqs = append(w.userReqs, req)
	if w.userErr != nil {
		return nil, w.userErr
	}
	if common.HexToAddress(req.ToAddress) != hot {
		return nil, fmt.Errorf("%w: destination", wallet.ErrPermanent)
	}
	if req.Amount == "max" {
		n := bal(w.native, user)
		amt := new(big.Int).Sub(n, big.NewInt(feeWei))
		if amt.Sign() <= 0 {
			return nil, wallet.ErrBelowGas
		}
		return encodeOp(op{Kind: "sweep-native", From: user, To: hot, Amount: amt}), nil
	}
	amt, _ := new(big.Int).SetString(req.Amount, 10)
	return encodeOp(op{Kind: "sweep-token", From: user, To: hot, Amount: amt}), nil
}

// ---- harness ----
func newSweeper(w *world, blacklist ...string) *Sweeper {
	return &Sweeper{
		Chain: w, Wallet: w, Screening: screening.New(blacklist...),
		Assets: registry.Assets{
			nativeAsset: {Type: registry.AssetNative},
			tokenAsset:  {Type: registry.AssetERC20, Contract: tokenAdr},
		},
		Config: Config{ChainID: chainID, HotWallet: hot, GasTopUp: big.NewInt(1e15), GasReserve: big.NewInt(650_000_000_000_000),
			InclusionTimeout: 300 * time.Millisecond, PollInterval: time.Millisecond},
	}
}

func deposit(asset string, amount *big.Int, logIndex int64) events.DepositConfirmed {
	return events.DepositConfirmed{Version: 1, ChainID: chainID, TxHash: txHash, LogIndex: logIndex, AssetID: asset,
		FromAddress: sender.Hex(), ToAddress: user.Hex(), UserID: "alice", Amount: events.AmountFromInt(amount), BlockNumber: 5}
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ---- tests ----

func TestNativeDepositIsSweptWithMax(t *testing.T) {
	w := newWorld()
	w.native[user] = eth(5)
	s := newSweeper(w)
	ok(t, s.Sweep(context.Background(), deposit(nativeAsset, eth(5), -1)))

	if len(w.userReqs) != 1 || w.userReqs[0].Amount != "max" || w.userReqs[0].UserID != "alice" ||
		common.HexToAddress(w.userReqs[0].ToAddress) != hot || w.userReqs[0].AssetID != nativeAsset || w.userReqs[0].ChainID != chainID {
		t.Fatalf("sign request = %+v", w.userReqs)
	}
	if len(w.hotReqs) != 0 {
		t.Fatal("a native sweep must not get a Gas Top-Up: gas is paid in the asset being moved")
	}
	want := new(big.Int).Sub(eth(5), big.NewInt(feeWei))
	if bal(w.native, hot).Cmp(want) != 0 || bal(w.native, user).Sign() != 0 {
		t.Fatalf("hot %s user %s", bal(w.native, hot), bal(w.native, user))
	}
}

func TestRedeliveryAfterASuccessfulSweepDoesNothing(t *testing.T) {
	w := newWorld()
	w.native[user] = eth(5)
	s := newSweeper(w)
	ev := deposit(nativeAsset, eth(5), -1)
	ok(t, s.Sweep(context.Background(), ev))
	ok(t, s.Sweep(context.Background(), ev))
	if len(w.userReqs) != 1 || w.broadcasts != 1 {
		t.Fatalf("sign requests %d, broadcasts %d: a redelivered event swept again", len(w.userReqs), w.broadcasts)
	}
}

func TestTokenDepositWithNoGasGetsOneTopUpThenSweepsEverything(t *testing.T) {
	w := newWorld()
	w.tokens[user] = big.NewInt(1234)
	s := newSweeper(w)
	ev := deposit(tokenAsset, big.NewInt(1234), 3)
	ok(t, s.Sweep(context.Background(), ev))

	if len(w.hotReqs) != 1 {
		t.Fatalf("top-ups = %d", len(w.hotReqs))
	}
	h := w.hotReqs[0]
	if h.IdempotencyKey != "gas-topup:31337:"+txHash+":3" || h.Amount != "1000000000000000" || common.HexToAddress(h.ToAddress) != user || h.AssetID != nativeAsset || h.ChainID != chainID {
		t.Fatalf("top-up request = %+v", h)
	}
	if len(w.userReqs) != 1 || w.userReqs[0].Amount != "1234" || w.userReqs[0].AssetID != tokenAsset {
		t.Fatalf("sweep request = %+v", w.userReqs)
	}
	if bal(w.tokens, hot).Cmp(big.NewInt(1234)) != 0 || bal(w.tokens, user).Sign() != 0 {
		t.Fatal("tokens did not move to the hot wallet")
	}

	// Redelivery: nothing left to move, and no second top-up.
	ok(t, s.Sweep(context.Background(), ev))
	if len(w.hotReqs) != 1 || len(w.userReqs) != 1 {
		t.Fatalf("redelivery re-funded or re-swept: %d top-ups, %d sweeps", len(w.hotReqs), len(w.userReqs))
	}
}

func TestTokenDepositWithEnoughGasIsNotToppedUp(t *testing.T) {
	w := newWorld()
	w.tokens[user] = big.NewInt(50)
	w.native[user] = big.NewInt(700_000_000_000_000) // above the reserve
	s := newSweeper(w)
	ok(t, s.Sweep(context.Background(), deposit(tokenAsset, big.NewInt(50), 0)))
	if len(w.hotReqs) != 0 {
		t.Fatal("topped up an address that already had gas (invariant #8)")
	}
	if len(w.userReqs) != 1 {
		t.Fatal("did not sweep")
	}
}

// Invariant #10: the amount swept is the live balance, not what the event said.
func TestSweepsTheLiveBalanceNotTheEventAmount(t *testing.T) {
	w := newWorld()
	w.native[user] = big.NewInt(1e15)
	w.tokens[user] = big.NewInt(300) // two deposits landed; the event for the first says 100
	s := newSweeper(w)
	ok(t, s.Sweep(context.Background(), deposit(tokenAsset, big.NewInt(100), 0)))
	if w.userReqs[0].Amount != "300" {
		t.Fatalf("swept %s, want the live balance 300", w.userReqs[0].Amount)
	}
	// The second deposit's event then finds nothing left.
	ok(t, s.Sweep(context.Background(), deposit(tokenAsset, big.NewInt(200), 1)))
	if len(w.userReqs) != 1 {
		t.Fatal("swept a balance that was already moved")
	}
}

// If the top-up took a while, the balance is read again before signing.
func TestBalanceIsReReadAfterTheTopUp(t *testing.T) {
	w := newWorld()
	w.tokens[user] = big.NewInt(100)
	s := newSweeper(w)
	s.Wallet = &hookWallet{world: w, afterHot: func() { w.mu.Lock(); w.tokens[user] = big.NewInt(0); w.mu.Unlock() }}
	ok(t, s.Sweep(context.Background(), deposit(tokenAsset, big.NewInt(100), 0)))
	if len(w.userReqs) != 0 {
		t.Fatal("signed a sweep for a balance that vanished while the top-up was mining")
	}
}

type hookWallet struct {
	*world
	afterHot func()
}

func (h *hookWallet) SignHotWallet(ctx context.Context, r wallet.HotRequest) ([]byte, uint64, error) {
	raw, n, err := h.world.SignHotWallet(ctx, r)
	h.afterHot()
	return raw, n, err
}

func TestTopUpKeyIsDeterministicAndCaseInsensitive(t *testing.T) {
	a := TopUpKey(1, "0xABCDEF", 2)
	if a != TopUpKey(1, "0xabcdef", 2) || a != "gas-topup:1:0xabcdef:2" {
		t.Fatalf("key = %q", a)
	}
	if TopUpKey(1, "0xab", 0) == TopUpKey(1, "0xab", 1) || TopUpKey(1, "0xab", 0) == TopUpKey(2, "0xab", 0) {
		t.Fatal("keys of different deposits collide")
	}
}

func TestRetryAfterLostTopUpResponseReusesTheSameSignedTx(t *testing.T) {
	w := newWorld()
	w.tokens[user] = big.NewInt(9)
	s := newSweeper(w)
	ev := deposit(tokenAsset, big.NewInt(9), 0)
	w.stuck = true // top-up is broadcast but never mines: the attempt times out
	if err := s.Sweep(context.Background(), ev); err == nil {
		t.Fatal("expected a timeout")
	}
	w.stuck = false
	ok(t, s.Sweep(context.Background(), ev))
	if len(w.hotReqs) != 2 || w.hotReqs[0].IdempotencyKey != w.hotReqs[1].IdempotencyKey {
		t.Fatalf("hot requests = %+v: the retry must reuse the key so the nonce is not claimed twice", w.hotReqs)
	}
	if len(w.hotByKey) != 1 {
		t.Fatal("two different top-up transactions were signed for one deposit")
	}
	if bal(w.tokens, hot).Cmp(big.NewInt(9)) != 0 {
		t.Fatal("did not finish sweeping on the retry")
	}
}

func TestScreenedSenderIsNeverSwept(t *testing.T) {
	w := newWorld()
	w.native[user] = eth(5)
	w.tokens[user] = big.NewInt(10)
	s := newSweeper(w, strings.ToUpper(sender.Hex()[2:]), sender.Hex()) // any casing
	for _, ev := range []events.DepositConfirmed{deposit(nativeAsset, eth(5), -1), deposit(tokenAsset, big.NewInt(10), 0)} {
		ok(t, s.Sweep(context.Background(), ev))
	}
	if len(w.userReqs)+len(w.hotReqs) != 0 || w.broadcasts != 0 {
		t.Fatal("swept (or funded) a deposit from a Screening List address")
	}
}

func TestDustIsSkippedNotFailed(t *testing.T) {
	w := newWorld()
	w.native[user] = big.NewInt(feeWei / 2) // cannot even pay its own fee
	s := newSweeper(w)
	ok(t, s.Sweep(context.Background(), deposit(nativeAsset, big.NewInt(feeWei/2), -1)))
	if w.broadcasts != 0 {
		t.Fatal("broadcast a sweep that costs more than it moves")
	}
}

func TestDoesNotStackASecondTxBehindAPendingOne(t *testing.T) {
	w := newWorld()
	w.native[user] = eth(5)
	w.autoMine = false
	// An earlier attempt's sweep is in the mempool.
	w.pending[common.HexToHash("0x01")] = op{Kind: "sweep-native", From: user, To: hot, Amount: eth(1)}
	s := newSweeper(w)
	err := s.Sweep(context.Background(), deposit(nativeAsset, eth(5), -1))
	if err == nil || errors.Is(err, ErrPermanent) {
		t.Fatalf("err = %v, want a transient 'pending' error", err)
	}
	if len(w.userReqs) != 0 {
		t.Fatal("signed a second transaction while one from the same address was pending")
	}
	// Once it clears, the retry proceeds.
	w.mu.Lock()
	w.pending = map[common.Hash]op{}
	w.mu.Unlock()
	w.autoMine = true
	ok(t, s.Sweep(context.Background(), deposit(nativeAsset, eth(5), -1)))
	if len(w.userReqs) != 1 {
		t.Fatal("did not sweep once the mempool was clear")
	}
}

func TestRevertedTransactionsAreTransientErrors(t *testing.T) {
	w := newWorld()
	w.native[user] = eth(5)
	w.failMined = true
	s := newSweeper(w)
	err := s.Sweep(context.Background(), deposit(nativeAsset, eth(5), -1))
	if err == nil || errors.Is(err, ErrPermanent) || !strings.Contains(err.Error(), "reverted") {
		t.Fatalf("err = %v", err)
	}

	// A reverted top-up stops the sweep: it must not go on to sign against a still-empty address.
	w2 := newWorld()
	w2.tokens[user] = big.NewInt(5)
	w2.failMined = true
	if err := newSweeper(w2).Sweep(context.Background(), deposit(tokenAsset, big.NewInt(5), 0)); err == nil {
		t.Fatal("expected the failed top-up to fail the attempt")
	}
	if len(w2.userReqs) != 0 {
		t.Fatal("went on to sweep after a failed top-up")
	}
}

func TestWalletServiceFailuresAreClassified(t *testing.T) {
	permanent := fmt.Errorf("%w: 403", wallet.ErrPermanent)
	transient := errors.New("connection refused")
	for name, tc := range map[string]struct {
		asset   string
		amount  *big.Int
		hotErr  error
		userErr error
		perm    bool
	}{
		"user sign refused":     {nativeAsset, eth(1), nil, permanent, true},
		"user sign unavailable": {nativeAsset, eth(1), nil, transient, false},
		"top-up refused":        {tokenAsset, big.NewInt(5), permanent, nil, true},
		"top-up unavailable":    {tokenAsset, big.NewInt(5), transient, nil, false},
	} {
		w := newWorld()
		w.native[user] = eth(1)
		if tc.asset == tokenAsset {
			w.native[user] = big.NewInt(0)
			w.tokens[user] = tc.amount
		}
		w.hotErr, w.userErr = tc.hotErr, tc.userErr
		err := newSweeper(w).Sweep(context.Background(), deposit(tc.asset, tc.amount, 0))
		if err == nil || errors.Is(err, ErrPermanent) != tc.perm {
			t.Errorf("%s: err = %v, permanent = %v", name, err, errors.Is(err, ErrPermanent))
		}
	}
}

func TestEventsThatCanNeverBeSweptArePermanent(t *testing.T) {
	w := newWorld()
	s := newSweeper(w)
	wrongChain := deposit(nativeAsset, eth(1), -1)
	wrongChain.ChainID = 1
	unknown := deposit("99999999-9999-9999-9999-999999999999", eth(1), -1)
	for name, ev := range map[string]events.DepositConfirmed{"wrong chain": wrongChain, "unknown asset": unknown} {
		if err := s.Sweep(context.Background(), ev); !errors.Is(err, ErrPermanent) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// Garbage on the topic.
	if err := s.Handle(context.Background(), Message{Value: []byte(`{"version":1}`)}); !errors.Is(err, ErrPermanent) {
		t.Errorf("undecodable record: %v", err)
	}
}

// ---- Run ----

type scripted struct {
	mu        sync.Mutex
	msgs      []Message
	committed []Message
	done      chan struct{}
}

func (r *scripted) Fetch(ctx context.Context) (Message, error) {
	r.mu.Lock()
	if len(r.msgs) > 0 {
		m := r.msgs[0]
		r.msgs = r.msgs[1:]
		r.mu.Unlock()
		return m, nil
	}
	r.mu.Unlock()
	select {
	case <-r.done:
	case <-ctx.Done():
	}
	return Message{}, ctx.Err()
}
func (r *scripted) Commit(_ context.Context, m Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.committed = append(r.committed, m)
	return nil
}
func (r *scripted) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.committed) }

func msgFor(t *testing.T, ev events.DepositConfirmed) Message {
	t.Helper()
	b, err := events.Encode(ev)
	if err != nil {
		t.Fatal(err)
	}
	return Message{Topic: ev.Topic(), Key: []byte(ev.Key()), Value: b}
}

func runFor(t *testing.T, s *Sweeper, msgs []Message, attempts int, until func() bool) (*scripted, func()) {
	t.Helper()
	rd := &scripted{msgs: msgs, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	fin := make(chan error, 1)
	go func() { fin <- Run(ctx, rd, s, attempts) }()
	deadline := time.Now().Add(10 * time.Second)
	for !until() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return rd, func() { cancel(); <-fin }
}

func TestRunCommitsOnlyAfterTheSweepAndGivesUpAfterMaxAttempts(t *testing.T) {
	w := newWorld()
	w.native[user] = eth(5)
	w.userErr = errors.New("wallet-service down")
	s := newSweeper(w)
	var rd *scripted
	rd, stop := runFor(t, s, []Message{msgFor(t, deposit(nativeAsset, eth(5), -1))}, 3, func() bool { return true })
	// Not committed while it is still being retried.
	time.Sleep(300 * time.Millisecond)
	if rd.count() != 0 {
		t.Fatal("committed while the sweep was failing")
	}
	stop()

	// Now with a cap: after 3 attempts the record is given up on (ALERT) and committed.
	w2 := newWorld()
	w2.native[user] = eth(5)
	w2.userErr = errors.New("wallet-service down")
	rd2, stop2 := runFor(t, newSweeper(w2), []Message{msgFor(t, deposit(nativeAsset, eth(5), -1))}, 3, func() bool { return true })
	deadline := time.Now().Add(15 * time.Second)
	for rd2.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	stop2()
	if rd2.count() != 1 || len(w2.userReqs) != 3 {
		t.Fatalf("committed %d, attempts %d: want a commit after exactly 3 attempts", rd2.count(), len(w2.userReqs))
	}
}

func TestRunCommitsAPermanentFailureAtOnceAndKeepsGoing(t *testing.T) {
	w := newWorld()
	w.native[user] = eth(5)
	s := newSweeper(w)
	bad := Message{Topic: events.TopicDepositsConfirmed, Key: []byte("k"), Value: []byte(`nonsense`)}
	good := msgFor(t, deposit(nativeAsset, eth(5), -1))
	rd, stop := runFor(t, s, []Message{bad, good}, 5, func() bool { return len(w.userReqsSafe()) == 1 })
	deadline := time.Now().Add(5 * time.Second)
	for rd.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	if rd.count() != 2 || len(w.userReqsSafe()) != 1 {
		t.Fatalf("committed %d, sweeps %d: a poison record must not block the ones behind it", rd.count(), len(w.userReqsSafe()))
	}
}

func (w *world) userReqsSafe() []wallet.UserRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]wallet.UserRequest(nil), w.userReqs...)
}
