package watcher

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"testing"

	"digiwal/pkg/events"
	"digiwal/services/chain-watcher/internal/chain"
	"digiwal/services/chain-watcher/internal/registry"
	"digiwal/services/chain-watcher/internal/store"
	"digiwal/services/chain-watcher/internal/testdb"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testdb.Terminate()
	os.Exit(code)
}

const (
	chainID     = int64(31337)
	nativeAsset = "11111111-1111-1111-1111-111111111111"
	tokenAsset  = "22222222-2222-2222-2222-222222222222"
	depth       = 3
)

var (
	alice     = common.HexToAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")
	bob       = common.HexToAddress("0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC")
	hotWallet = common.HexToAddress("0x9965507D1a55bcC2695C58ba16FB37d819B0A4dc")
	stranger  = common.HexToAddress("0x90F79bf6EB2c4f870365E785982E1f101E93b906")
	sender    = common.HexToAddress("0x15d34AAf54267DB7D7c367839AAf71A00a2C6A65")
	token     = common.HexToAddress("0x5FbDB2315678afecb367f032d93F642f64180aa3")
	otherTok  = common.HexToAddress("0xe7f1725E7734CE288F8367e1Bb143E90bb3F0512")
	oneEther  = new(big.Int).Mul(big.NewInt(1_000_000_000), big.NewInt(1_000_000_000))
	bigAmount = new(big.Int).Mul(oneEther, big.NewInt(100)) // 100 ETH: far beyond int64 wei
)

// ---- a scriptable chain -------------------------------------------------

type fakeBlock struct {
	chain.Block
	failed map[common.Hash]bool // txs that reverted
	logs   []chain.Log          // Index/BlockNumber/BlockHash filled in on read
}

type fakeChain struct {
	mu     sync.Mutex
	blocks []*fakeBlock // index = block number
	salt   int

	// Hooks to inject a reorg, or a misbehaving node, at a precise moment.
	beforeBlock    func(n int64) // runs before Block(n) reads
	beforeLogs     func()        // runs before Logs reads
	ignoreLogQuery bool          // Logs returns every Transfer log, whatever contracts were asked for
}

func newFakeChain() *fakeChain {
	c := &fakeChain{}
	c.blocks = []*fakeBlock{c.mk(0, common.Hash{})}
	return c
}

func (c *fakeChain) mk(n int64, parent common.Hash) *fakeBlock {
	c.salt++
	h := crypto.Keccak256Hash([]byte(fmt.Sprintf("block-%d-%s-%d", n, parent, c.salt)))
	return &fakeBlock{Block: chain.Block{Number: n, Hash: h, ParentHash: parent}, failed: map[common.Hash]bool{}}
}

func (c *fakeChain) tip() *fakeBlock { return c.blocks[len(c.blocks)-1] }

// mine appends a block and lets fill add transactions/logs to it.
func (c *fakeChain) mine(fill func(*fakeBlock)) *fakeBlock {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.mk(c.tip().Number+1, c.tip().Hash)
	if fill != nil {
		fill(b)
	}
	c.blocks = append(c.blocks, b)
	return b
}

func (c *fakeChain) mineEmpty(n int) {
	for i := 0; i < n; i++ {
		c.mine(nil)
	}
}

// reorgTo drops every block above number n and mines replacements.
func (c *fakeChain) reorgTo(n int64, replacements ...func(*fakeBlock)) {
	c.mu.Lock()
	c.blocks = c.blocks[:n+1]
	c.mu.Unlock()
	for _, f := range replacements {
		c.mine(f)
	}
}

func (c *fakeChain) head() int64 { c.mu.Lock(); defer c.mu.Unlock(); return c.tip().Number }

var txCounter int

func nativeTx(from common.Address, to common.Address, v *big.Int) chain.Tx {
	txCounter++
	return chain.Tx{Hash: crypto.Keccak256Hash([]byte(fmt.Sprintf("tx-%d", txCounter))), From: from, To: &to, Value: v}
}

func transferLog(contract, from, to common.Address, amount *big.Int) chain.Log {
	txCounter++
	return chain.Log{
		Address: contract,
		Topics:  []common.Hash{transferTopic, common.BytesToHash(from.Bytes()), common.BytesToHash(to.Bytes())},
		Data:    common.LeftPadBytes(amount.Bytes(), 32),
		TxHash:  crypto.Keccak256Hash([]byte(fmt.Sprintf("tx-%d", txCounter))),
	}
}

func (c *fakeChain) ChainID(context.Context) (int64, error) { return chainID, nil }
func (c *fakeChain) Head(context.Context) (int64, error)    { return c.head(), nil }
func (c *fakeChain) BlockHash(_ context.Context, n int64) (common.Hash, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n < 0 || n >= int64(len(c.blocks)) {
		return common.Hash{}, false, nil
	}
	return c.blocks[n].Hash, true, nil
}
func (c *fakeChain) Block(_ context.Context, n int64) (*chain.Block, error) {
	if c.beforeBlock != nil {
		c.beforeBlock(n)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if n < 0 || n >= int64(len(c.blocks)) {
		return nil, nil
	}
	b := c.blocks[n].Block
	return &b, nil
}
func (c *fakeChain) Logs(_ context.Context, from, to int64, contracts []common.Address, topic0 common.Hash) ([]chain.Log, error) {
	if c.beforeLogs != nil {
		c.beforeLogs()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []chain.Log
	idx := int64(0)
	for n := int64(0); n < int64(len(c.blocks)) && n <= to; n++ {
		b := c.blocks[n]
		for _, l := range b.logs {
			l.BlockNumber, l.BlockHash, l.Index = n, b.Hash, idx
			idx++
			if n < from {
				continue
			}
			match := c.ignoreLogQuery
			for _, a := range contracts {
				match = match || a == l.Address
			}
			if match && len(l.Topics) > 0 && l.Topics[0] == topic0 {
				out = append(out, l)
			}
		}
		idx = 0
	}
	return out, nil
}
func (c *fakeChain) Receipt(_ context.Context, h common.Hash) (*chain.Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, b := range c.blocks {
		for _, tx := range b.Txs {
			if tx.Hash == h {
				return &chain.Receipt{BlockNumber: b.Number, BlockHash: b.Hash, Success: !b.failed[h]}, nil
			}
		}
		for _, l := range b.logs {
			if l.TxHash == h {
				return &chain.Receipt{BlockNumber: b.Number, BlockHash: b.Hash, Success: true}, nil
			}
		}
	}
	return nil, nil
}

// ---- fakes for the collaborators -----------------------------------------

type fakeResolver struct {
	mu     sync.Mutex
	owners map[common.Address]string
	err    error // returned for every lookup when set
	calls  int
}

func (r *fakeResolver) Resolve(_ context.Context, a common.Address) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return "", false, r.err
	}
	u, ok := r.owners[a]
	return u, ok, nil
}

type sent struct {
	Topic, Key string
	Body       []byte
}

type fakePublisher struct {
	mu   sync.Mutex
	sent []sent
	fail error
}

func (p *fakePublisher) Publish(_ context.Context, topic, key string, value []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return p.fail
	}
	p.sent = append(p.sent, sent{topic, key, value})
	return nil
}

func (p *fakePublisher) on(topic string) []sent {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []sent
	for _, s := range p.sent {
		if s.Topic == topic {
			out = append(out, s)
		}
	}
	return out
}

// ---- harness ------------------------------------------------------------

type rig struct {
	t   *testing.T
	ch  *fakeChain
	st  *store.Store
	res *fakeResolver
	pub *fakePublisher
	dsn string
	w   *Watcher
}

func newRig(t *testing.T, start *int64) *rig {
	t.Helper()
	dsn := testdb.New(t)
	st, err := store.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	r := &rig{t: t, ch: newFakeChain(), st: st, dsn: dsn,
		res: &fakeResolver{owners: map[common.Address]string{alice: "alice", bob: "bob"}},
		pub: &fakePublisher{}}
	r.w = r.newWatcher(start)
	return r
}

func (r *rig) newWatcher(start *int64) *Watcher {
	reg := &registry.Registry{Native: nativeAsset, ERC20: map[common.Address]string{token: tokenAsset}}
	return &Watcher{
		Chain: r.ch, Store: r.st, Resolver: r.res, Publisher: r.pub, HotWallet: hotWallet,
		LoadRegistry: func() (*registry.Registry, error) { return reg, nil },
		Config:       Config{ConfirmationDepth: depth, BatchBlocks: 4, StartBlock: start},
	}
}

func (r *rig) step() {
	r.t.Helper()
	if err := r.w.Step(context.Background()); err != nil {
		r.t.Fatalf("step: %v", err)
	}
}

func zero() *int64 { z := int64(0); return &z }

func (r *rig) get(tx common.Hash, logIndex int64) *store.Transfer {
	r.t.Helper()
	tr, err := r.st.Get(context.Background(), chainID, strings.ToLower(tx.Hex()), logIndex)
	if err != nil {
		r.t.Fatal(err)
	}
	return tr
}

func decodeConfirmed(t *testing.T, s sent) events.DepositConfirmed {
	t.Helper()
	ev, err := events.Decode[events.DepositConfirmed](s.Body)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// ---- tests --------------------------------------------------------------

func TestNativeDepositLifecycle(t *testing.T) {
	r := newRig(t, zero())
	tx := nativeTx(sender, alice, bigAmount)
	dep := r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{tx} }) // block 1

	r.step()
	got := r.get(tx.Hash, events.NativeLogIndex)
	if got == nil || got.Status != store.StatusPending || got.UserID != "alice" || got.Amount.Cmp(bigAmount) != 0 || got.BlockNumber != 1 {
		t.Fatalf("after scan: %+v", got)
	}
	det := r.pub.on(events.TopicDepositsDetected)
	if len(det) != 1 || len(r.pub.on(events.TopicDepositsConfirmed)) != 0 {
		t.Fatalf("expected exactly one detected and no confirmed yet, got %d/%d", len(det), len(r.pub.on(events.TopicDepositsConfirmed)))
	}
	ev, err := events.Decode[events.DepositDetected](det[0].Body)
	if err != nil || ev.UserID != "alice" || ev.AssetID != nativeAsset || ev.LogIndex != -1 || ev.ToAddress != alice.Hex() ||
		ev.FromAddress != sender.Hex() || ev.Amount.Int().Cmp(bigAmount) != 0 || ev.BlockNumber != dep.Number || ev.ChainID != chainID {
		t.Fatalf("detected event = %+v (%v)", ev, err)
	}
	if det[0].Key != ev.Key() {
		t.Fatalf("kafka key %q != event key %q", det[0].Key, ev.Key())
	}

	// Depth boundary: block 1 needs head >= 1+depth. Head is 1 now; mine up to one short.
	r.ch.mineEmpty(depth - 1) // head = 3
	r.step()
	if r.get(tx.Hash, -1).Status != store.StatusPending || len(r.pub.on(events.TopicDepositsConfirmed)) != 0 {
		t.Fatal("confirmed one block early")
	}
	r.ch.mineEmpty(1) // head = 4 = 1 + depth
	r.step()
	if r.get(tx.Hash, -1).Status != store.StatusConfirmed {
		t.Fatal("not confirmed at exactly Confirmation Depth")
	}
	conf := r.pub.on(events.TopicDepositsConfirmed)
	if len(conf) != 1 {
		t.Fatalf("confirmed events = %d", len(conf))
	}
	ce := decodeConfirmed(t, conf[0])
	if ce.UserID != "alice" || ce.Amount.Int().Cmp(bigAmount) != 0 || ce.AssetID != nativeAsset || ce.LogIndex != -1 {
		t.Fatalf("confirmed event = %+v", ce)
	}

	// Steady state: further polls change and republish nothing.
	r.ch.mineEmpty(5)
	r.step()
	r.step()
	if len(r.pub.on(events.TopicDepositsConfirmed)) != 1 || len(r.pub.on(events.TopicDepositsDetected)) != 1 {
		t.Fatal("re-published on a later poll")
	}
}

func TestERC20DepositAndFilters(t *testing.T) {
	r := newRig(t, zero())
	good := transferLog(token, sender, bob, big.NewInt(1234))
	notRegistered := transferLog(otherTok, sender, alice, big.NewInt(5))
	notOurs := transferLog(token, sender, stranger, big.NewInt(5))
	zeroAmt := transferLog(token, sender, alice, big.NewInt(0))
	nft := transferLog(token, sender, alice, big.NewInt(7))
	nft.Topics = append(nft.Topics, common.BigToHash(big.NewInt(99))) // ERC-721 shape: 4 topics
	nft.Data = nil
	r.ch.mine(func(b *fakeBlock) { b.logs = []chain.Log{notRegistered, notOurs, good, zeroAmt, nft} })

	r.step()
	det := r.pub.on(events.TopicDepositsDetected)
	if len(det) != 1 {
		t.Fatalf("detected = %d, want only the one registered, ours, non-zero ERC-20 transfer", len(det))
	}
	ev, _ := events.Decode[events.DepositDetected](det[0].Body)
	if ev.UserID != "bob" || ev.AssetID != tokenAsset || ev.Amount != "1234" || ev.ToAddress != bob.Hex() || ev.FromAddress != sender.Hex() {
		t.Fatalf("event = %+v", ev)
	}
	// The natural key uses the real log index: the good log is the 3rd in its block.
	if ev.LogIndex != 2 {
		t.Fatalf("log_index = %d, want 2", ev.LogIndex)
	}
	if r.get(notRegistered.TxHash, 0) != nil || r.get(notOurs.TxHash, 1) != nil {
		t.Fatal("recorded a transfer it must ignore")
	}
}

func TestOnlyWatchedAddressesAndSuccessfulNativeTxs(t *testing.T) {
	r := newRig(t, zero())
	toStranger := nativeTx(sender, stranger, oneEther)
	zeroValue := nativeTx(sender, alice, big.NewInt(0))
	reverted := nativeTx(sender, alice, oneEther)
	ok := nativeTx(sender, alice, oneEther)
	create := chain.Tx{Hash: crypto.Keccak256Hash([]byte("create")), From: sender, To: nil, Value: oneEther}
	r.ch.mine(func(b *fakeBlock) {
		b.Txs = []chain.Tx{toStranger, zeroValue, reverted, ok, create}
		b.failed[reverted.Hash] = true
	})
	r.step()
	if n := len(r.pub.on(events.TopicDepositsDetected)); n != 1 {
		t.Fatalf("detected = %d, want 1 (only the successful transfer to a watched address)", n)
	}
	if r.get(ok.Hash, -1) == nil || r.get(reverted.Hash, -1) != nil || r.get(toStranger.Hash, -1) != nil {
		t.Fatal("wrong set of transfers recorded")
	}
}

// Invariant #1: a resolution failure that is not a clean "not ours" must never drop a transfer.
func TestResolutionFailureDoesNotDropTheDeposit(t *testing.T) {
	r := newRig(t, zero())
	tx := nativeTx(sender, alice, oneEther)
	r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{tx} })

	r.res.err = errors.New("wallet-service is down")
	if err := r.w.Step(context.Background()); err == nil {
		t.Fatal("expected the step to fail while resolution fails")
	}
	if wm, _, _ := r.st.Watermark(context.Background(), chainID); wm != 0 {
		t.Fatalf("watermark advanced to %d past a block with an unresolved transfer", wm)
	}
	if len(r.pub.sent) != 0 {
		t.Fatal("published something despite the failure")
	}

	r.res.err = nil // wallet-service is back
	r.step()
	if tr := r.get(tx.Hash, -1); tr == nil || tr.UserID != "alice" {
		t.Fatalf("deposit lost after the outage: %+v", tr)
	}
	if wm, _, _ := r.st.Watermark(context.Background(), chainID); wm != 1 {
		t.Fatalf("watermark = %d, want 1", wm)
	}
}

func TestRestartResumesFromWatermarkWithoutRepublishing(t *testing.T) {
	r := newRig(t, zero())
	tx1 := nativeTx(sender, alice, oneEther)
	r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{tx1} })
	r.step()

	// "Restart": a new Watcher over the same database. Its StartBlock is irrelevant now.
	r.w = r.newWatcher(nil)
	tx2 := nativeTx(sender, bob, oneEther)
	r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{tx2} })
	r.step()

	det := r.pub.on(events.TopicDepositsDetected)
	if len(det) != 2 {
		t.Fatalf("detected = %d, want 2 (one per deposit, none repeated)", len(det))
	}
}

func TestKafkaOutageIsRepairedAndNothingIsLost(t *testing.T) {
	r := newRig(t, zero())
	tx := nativeTx(sender, alice, oneEther)
	r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{tx} })
	r.ch.mineEmpty(depth)

	r.pub.fail = errors.New("broker unavailable")
	if err := r.w.Step(context.Background()); err == nil {
		t.Fatal("expected a publish error")
	}
	// State is durable even though nothing could be sent: recorded, and already confirmed.
	if tr := r.get(tx.Hash, -1); tr == nil || tr.Status != store.StatusConfirmed {
		t.Fatalf("state after outage: %+v", tr)
	}

	r.pub.fail = nil
	r.step()
	if len(r.pub.on(events.TopicDepositsDetected)) != 1 || len(r.pub.on(events.TopicDepositsConfirmed)) != 1 {
		t.Fatalf("after recovery: detected=%d confirmed=%d, want 1 each",
			len(r.pub.on(events.TopicDepositsDetected)), len(r.pub.on(events.TopicDepositsConfirmed)))
	}
	r.step()
	if len(r.pub.on(events.TopicDepositsConfirmed)) != 1 {
		t.Fatal("republished after being acknowledged")
	}
	// And the detected event went out before the confirmed one.
	if r.pub.sent[0].Topic != events.TopicDepositsDetected {
		t.Fatalf("first event was %s", r.pub.sent[0].Topic)
	}
}

// A crash after Kafka acked but before the row was marked republishes: at-least-once.
func TestCrashBeforeMarkPublishedRepublishes(t *testing.T) {
	r := newRig(t, zero())
	tx := nativeTx(sender, alice, oneEther)
	r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{tx} })
	r.ch.mineEmpty(depth)
	r.step() // everything published and marked

	// Simulate the crash window: forget that confirmed was published.
	pool := r.st.Pool()
	if _, err := pool.Exec(context.Background(), `UPDATE observed_transfers SET confirmed_published_at = NULL`); err != nil {
		t.Fatal(err)
	}
	r.step()
	conf := r.pub.on(events.TopicDepositsConfirmed)
	if len(conf) != 2 || string(conf[0].Body) != string(conf[1].Body) || conf[0].Key != conf[1].Key {
		t.Fatalf("expected the identical confirmed event twice (same key), got %d", len(conf))
	}
}

func TestReorgDropsPendingTransferThatLeftTheChain(t *testing.T) {
	r := newRig(t, zero())
	r.ch.mineEmpty(1) // block 1
	tx := nativeTx(sender, alice, oneEther)
	r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{tx} }) // block 2
	r.ch.mineEmpty(1)                                        // block 3
	r.step()
	if r.get(tx.Hash, -1) == nil {
		t.Fatal("not recorded")
	}

	// The chain forks at block 1; the new branch does not contain the transfer, and is longer.
	r.ch.reorgTo(1, nil, nil, nil, nil, nil, nil)
	r.step()
	if r.get(tx.Hash, -1) != nil {
		t.Fatal("pending transfer from an abandoned branch is still recorded")
	}
	r.ch.mineEmpty(depth + 2)
	r.step()
	if len(r.pub.on(events.TopicDepositsConfirmed)) != 0 {
		t.Fatal("confirmed a deposit that was reorged away")
	}
	wm, _, _ := r.st.Watermark(context.Background(), chainID)
	if wm != r.ch.head() {
		t.Fatalf("watermark %d, head %d", wm, r.ch.head())
	}
}

func TestReorgThatReincludesTheTransferConfirmsItOnTheNewBranch(t *testing.T) {
	r := newRig(t, zero())
	tx := nativeTx(sender, alice, oneEther)
	r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{tx} }) // block 1
	r.ch.mineEmpty(1)                                        // block 2
	r.step()
	oldHash := r.get(tx.Hash, -1).BlockHash

	// Fork below block 1; the transaction lands in a different block (2) of the new branch.
	r.ch.reorgTo(0, nil, func(b *fakeBlock) { b.Txs = []chain.Tx{tx} })
	r.ch.mineEmpty(depth + 1)
	r.step()

	got := r.get(tx.Hash, -1)
	if got == nil || got.Status != store.StatusConfirmed || got.BlockNumber != 2 || got.BlockHash == oldHash {
		t.Fatalf("after re-inclusion: %+v (old hash %s)", got, oldHash)
	}
	if n := len(r.pub.on(events.TopicDepositsConfirmed)); n != 1 {
		t.Fatalf("confirmed events = %d, want exactly 1", n)
	}
	// It is detected twice (once per branch) — same key, informational, consumers dedupe.
	if n := len(r.pub.on(events.TopicDepositsDetected)); n != 2 {
		t.Fatalf("detected events = %d, want 2", n)
	}
}

func TestReorgNeverConfirmsATransferNoLongerInItsBlock(t *testing.T) {
	// Even when the reorg lands between scanning and confirming, the receipt check holds it back.
	r := newRig(t, zero())
	tx := nativeTx(sender, alice, oneEther)
	r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{tx} })
	r.step()

	r.ch.reorgTo(0, nil, nil, nil, nil, nil) // new branch without the tx, deep enough to "confirm"
	// Call confirm directly, skipping reconcile: the previous scan's row still points at the old block.
	if err := r.w.confirm(context.Background(), r.ch.head()); err != nil {
		t.Fatal(err)
	}
	if r.get(tx.Hash, -1).Status != store.StatusPending {
		t.Fatal("confirmed a transfer whose block is no longer canonical")
	}
}

func TestDeepReorgBelowAConfirmedTransferStopsAndKeepsIt(t *testing.T) {
	r := newRig(t, zero())
	tx := nativeTx(sender, alice, oneEther)
	r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{tx} })
	r.ch.mineEmpty(depth)
	r.step()
	if r.get(tx.Hash, -1).Status != store.StatusConfirmed {
		t.Fatal("setup: not confirmed")
	}

	r.ch.reorgTo(0, nil, nil, nil, nil, nil, nil, nil)
	err := r.w.Step(context.Background())
	if !errors.Is(err, store.ErrDeepReorg) {
		t.Fatalf("err = %v, want ErrDeepReorg", err)
	}
	if got := r.get(tx.Hash, -1); got == nil || got.Status != store.StatusConfirmed {
		t.Fatalf("a confirmed transfer was altered: %+v", got)
	}
	if wm, _, _ := r.st.Watermark(context.Background(), chainID); wm == 0 {
		t.Fatal("watermark was rewound despite the refusal")
	}
}

func TestFirstRunStartsAtHeadByDefault(t *testing.T) {
	r := newRig(t, nil)
	old := nativeTx(sender, alice, oneEther)
	r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{old} })
	r.ch.mineEmpty(2) // head = 3
	r.step()
	if r.get(old.Hash, -1) != nil {
		t.Fatal("scanned history it was not asked to")
	}
	fresh := nativeTx(sender, alice, oneEther)
	r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{fresh} })
	r.step()
	if r.get(fresh.Hash, -1) == nil {
		t.Fatal("missed a deposit made after the first run")
	}
}

func TestStartBlockScansHistory(t *testing.T) {
	r := newRig(t, nil)
	old := nativeTx(sender, alice, oneEther)
	r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{old} }) // block 1
	r.ch.mineEmpty(9)
	one := int64(1)
	r.w = r.newWatcher(&one)
	r.step()
	if r.get(old.Hash, -1) == nil {
		t.Fatal("START_BLOCK=1 should include block 1")
	}
}

func TestLargeBacklogIsScannedInBatches(t *testing.T) {
	r := newRig(t, zero())
	var txs []chain.Tx
	for i := 0; i < 10; i++ {
		tx := nativeTx(sender, alice, oneEther)
		txs = append(txs, tx)
		r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{tx} })
	}
	r.step() // BatchBlocks is 4: needs three ranges in one Step
	for _, tx := range txs {
		if r.get(tx.Hash, -1) == nil {
			t.Fatal("missed a deposit across a batch boundary")
		}
	}
	if wm, _, _ := r.st.Watermark(context.Background(), chainID); wm != 10 {
		t.Fatalf("watermark = %d", wm)
	}
}

func TestBadRegistryReloadKeepsThePreviousOne(t *testing.T) {
	r := newRig(t, zero())
	calls := 0
	good := &registry.Registry{Native: nativeAsset, ERC20: map[common.Address]string{}}
	r.w.LoadRegistry = func() (*registry.Registry, error) {
		calls++
		if calls == 1 {
			return good, nil
		}
		return nil, errors.New("file half-written")
	}
	r.step()
	tx := nativeTx(sender, alice, oneEther)
	r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{tx} })
	r.step()
	if r.get(tx.Hash, -1) == nil {
		t.Fatal("a failed registry reload broke scanning")
	}
}

func TestStoreConfirmIsGuarded(t *testing.T) {
	r := newRig(t, zero())
	tx := nativeTx(sender, alice, oneEther)
	r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{tx} })
	r.step()
	tr := r.get(tx.Hash, -1)
	first, err := r.st.Confirm(context.Background(), *tr)
	second, err2 := r.st.Confirm(context.Background(), *tr)
	if err != nil || err2 != nil || !first || second {
		t.Fatalf("Confirm twice = %v, %v (errs %v %v): only the first may win", first, second, err, err2)
	}
}

func TestCommitRangeRefusesWhenTheWatermarkMoved(t *testing.T) {
	r := newRig(t, zero())
	r.ch.mineEmpty(3)
	r.step() // watermark 3
	// A second instance believes the watermark is still at 0 and tries to commit block 1..2.
	err := r.st.CommitRange(context.Background(), chainID, 1, 2, nil, nil)
	if !errors.Is(err, store.ErrLostWatermark) {
		t.Fatalf("err = %v, want ErrLostWatermark", err)
	}
}

func TestSameDepositNeverRecordedTwice(t *testing.T) {
	r := newRig(t, zero())
	tx := nativeTx(sender, alice, oneEther)
	r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{tx} })
	r.step()
	// Force a rescan of the same block (as after a crash between record and publish elsewhere).
	if _, err := r.st.Pool().Exec(context.Background(), `UPDATE watermark SET block_number = 0`); err != nil {
		t.Fatal(err)
	}
	r.step()
	var n int
	if err := r.st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM observed_transfers`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d (%v), want 1", n, err)
	}
	if len(r.pub.on(events.TopicDepositsDetected)) != 1 {
		t.Fatal("re-detected a transfer already known")
	}
}

// A transfer re-included in a different block by a reorg must not be confirmed from its old row.
func TestReorgReincludedInAnotherBlockIsNotConfirmedFromTheOldRow(t *testing.T) {
	r := newRig(t, zero())
	tx := nativeTx(sender, alice, oneEther)
	r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{tx} })
	r.step()
	r.ch.reorgTo(0, nil, func(b *fakeBlock) { b.Txs = []chain.Tx{tx} }) // same tx, now block 2
	r.ch.mineEmpty(depth + 1)
	// confirm alone (no reconcile): the row still says block 1's old hash, the receipt says otherwise.
	if err := r.w.confirm(context.Background(), r.ch.head()); err != nil {
		t.Fatal(err)
	}
	if r.get(tx.Hash, -1).Status != store.StatusPending {
		t.Fatal("confirmed against a block that is no longer canonical")
	}
}

// Defense in depth for invariant #2: even if the node hands back logs from contracts we did
// not ask for, an unregistered contract never produces a deposit.
func TestUnregisteredContractLogsAreIgnoredEvenIfTheNodeReturnsThem(t *testing.T) {
	r := newRig(t, zero())
	r.ch.ignoreLogQuery = true
	rogue := transferLog(otherTok, sender, alice, big.NewInt(1_000_000))
	r.ch.mine(func(b *fakeBlock) { b.logs = []chain.Log{rogue} })
	r.step()
	if n := len(r.pub.sent); n != 0 {
		t.Fatalf("published %d event(s) for an unregistered token", n)
	}
}

func TestOnlyExactERC20TransferShapeCounts(t *testing.T) {
	r := newRig(t, zero())
	// ERC-721 style: same signature, tokenId indexed => 4 topics, even with non-empty data.
	nft := transferLog(token, sender, alice, big.NewInt(42))
	nft.Topics = append(nft.Topics, common.BigToHash(big.NewInt(7)))
	// 3 topics but no value in data.
	short := transferLog(token, sender, alice, big.NewInt(42))
	short.Data = nil
	r.ch.mine(func(b *fakeBlock) { b.logs = []chain.Log{nft, short} })
	r.step()
	if n := len(r.pub.sent); n != 0 {
		t.Fatalf("published %d event(s) for a malformed/ERC-721 Transfer", n)
	}
}

// The node reorgs while a range is being read: blocks from two branches must never be committed.
func TestReorgWhileReadingARangeCommitsNothing(t *testing.T) {
	r := newRig(t, zero())
	tx := nativeTx(sender, alice, oneEther)
	r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{tx} })
	r.ch.mineEmpty(3) // head 4: one range (BatchBlocks = 4)
	forked := false
	r.ch.beforeBlock = func(n int64) {
		if n == 3 && !forked {
			forked = true
			// blocks 1-2 were already read from the old branch; the fork replaces 2 onward
			r.ch.blocks = r.ch.blocks[:2]
			for i := 0; i < 4; i++ {
				r.ch.blocks = append(r.ch.blocks, r.ch.mk(r.ch.tip().Number+1, r.ch.tip().Hash))
			}
		}
	}
	if err := r.w.Step(context.Background()); err == nil {
		t.Fatal("expected the mixed-branch range to be rejected")
	}
	if wm, _, _ := r.st.Watermark(context.Background(), chainID); wm != 0 {
		t.Fatalf("watermark advanced to %d on a range that straddled a reorg", wm)
	}
	var blocks int
	_ = r.st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM scanned_blocks`).Scan(&blocks)
	if blocks != 0 {
		t.Fatalf("%d block hashes were recorded from a torn read", blocks)
	}
	r.ch.beforeBlock = nil
	r.step() // and the next poll simply works on the new branch
}

func TestLogsFromAnotherBranchThanTheScannedBlocksAreRejected(t *testing.T) {
	r := newRig(t, zero())
	tok := transferLog(token, sender, alice, big.NewInt(9))
	r.ch.mine(func(b *fakeBlock) { b.logs = []chain.Log{tok} })
	r.ch.mineEmpty(1) // head 2
	forked := false
	r.ch.beforeLogs = func() {
		if !forked {
			forked = true // the branch changes after the blocks were read, before the logs are
			r.ch.blocks = r.ch.blocks[:1]
			r.ch.blocks = append(r.ch.blocks, r.ch.mk(1, r.ch.tip().Hash), nil)
			r.ch.blocks[1].logs = []chain.Log{tok}
			r.ch.blocks[2] = r.ch.mk(2, r.ch.blocks[1].Hash)
		}
	}
	if err := r.w.Step(context.Background()); err == nil {
		t.Fatal("expected logs from a different branch to be rejected")
	}
	if wm, _, _ := r.st.Watermark(context.Background(), chainID); wm != 0 {
		t.Fatalf("watermark = %d", wm)
	}
}

// ---- Hot Wallet Outflow (deposit-flow.md invariant #17) ------------------------------------

func TestHotWalletOutflowIsNotADeposit(t *testing.T) {
	r := newRig(t, zero())
	gasTopUp := nativeTx(hotWallet, alice, oneEther)
	realNative := nativeTx(sender, alice, oneEther)
	outTT := transferLog(token, hotWallet, bob, big.NewInt(50)) // ERC-20 sent from the Hot Wallet
	realTT := transferLog(token, sender, bob, big.NewInt(60))
	r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{gasTopUp, realNative}; b.logs = []chain.Log{outTT, realTT} })

	r.step()
	if r.get(gasTopUp.Hash, events.NativeLogIndex) != nil || r.get(outTT.TxHash, 0) != nil {
		t.Fatal("recorded Hot Wallet Outflow as a deposit")
	}
	if r.get(realNative.Hash, events.NativeLogIndex) == nil || r.get(realTT.TxHash, 1) == nil /* log index 1 in its block */ {
		t.Fatal("dropped a real deposit from another sender")
	}
	if n := len(r.pub.on(events.TopicDepositsDetected)); n != 2 {
		t.Fatalf("detected events = %d, want only the 2 real deposits", n)
	}
	// The outflow is skipped before Address Resolution: only the two real senders' recipients were asked.
	if r.res.calls != 2 {
		t.Fatalf("resolver calls = %d, want 2 (alice, bob)", r.res.calls)
	}

	// Past Confirmation Depth still nothing for the outflow, and the watermark advanced.
	r.ch.mineEmpty(depth + 1)
	r.step()
	if n := len(r.pub.on(events.TopicDepositsConfirmed)); n != 2 {
		t.Fatalf("confirmed events = %d, want 2", n)
	}
	if wm, ok, err := r.st.Watermark(context.Background(), chainID); err != nil || !ok || wm < r.ch.head() {
		t.Fatalf("watermark = %d ok=%v (%v), want it at the head %d", wm, ok, err, r.ch.head())
	}
}

func TestHotWalletOutflowSkipSurvivesRescanAndReorg(t *testing.T) {
	r := newRig(t, zero())
	gasTopUp := nativeTx(hotWallet, alice, oneEther)
	r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{gasTopUp} })
	r.step()
	// A restarted watcher rescans nothing new; a reorg re-including the same tx in another block
	// is decided the same way, because the decision depends only on the sender.
	r.w = r.newWatcher(nil)
	r.step()
	r.ch.reorgTo(0, func(b *fakeBlock) { b.Txs = []chain.Tx{gasTopUp} })
	r.ch.mineEmpty(depth + 1)
	r.step()
	if r.get(gasTopUp.Hash, events.NativeLogIndex) != nil || len(r.pub.on(events.TopicDepositsDetected)) != 0 {
		t.Fatal("Hot Wallet Outflow was recorded after a rescan/reorg")
	}
}

func TestNoScanUntilTheHotWalletAddressIsKnown(t *testing.T) {
	r := newRig(t, zero())
	r.w.HotWallet = common.Address{}
	tx := nativeTx(hotWallet, alice, oneEther)
	r.ch.mine(func(b *fakeBlock) { b.Txs = []chain.Tx{tx} })
	if err := r.w.Step(context.Background()); err == nil {
		t.Fatal("Step scanned without a Hot Wallet address")
	}
	if wm, ok, err := r.st.Watermark(context.Background(), chainID); err != nil || ok {
		t.Fatalf("watermark = %d ok=%v (%v): a watermark exists though nothing was scanned", wm, ok, err)
	}
	if r.res.calls != 0 {
		t.Fatalf("resolver was called %d times", r.res.calls)
	}
}
