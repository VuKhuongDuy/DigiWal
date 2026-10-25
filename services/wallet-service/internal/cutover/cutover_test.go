package cutover

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"digiwal/services/wallet-service/internal/signing"
	"digiwal/services/wallet-service/internal/storage"
	"digiwal/services/wallet-service/internal/testdb"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testdb.Terminate()
	os.Exit(code)
}

const (
	chainID  = int64(31337)
	nativeID = "native-asset"
	tokenID  = "token-asset"
)

var (
	token    = common.HexToAddress("0x5FbDB2315678afecb367f032d93F642f64180aa3")
	basePrce = big.NewInt(1_000_000_000) // 1 gwei base fee
	tipPrice = big.NewInt(100_000_000)
	ether    = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
)

// ---- a small deterministic EVM: native and one ERC-20, mined on demand ------------------------------

type fakeChain struct {
	mu       sync.Mutex
	head     uint64
	mined    map[common.Address]uint64
	pool     []*types.Transaction
	native   map[common.Address]*big.Int
	tokens   map[common.Address]*big.Int
	receipts map[common.Hash]*Receipt
	revert   map[common.Hash]bool
	sent     map[common.Hash]int // broadcasts per tx
	// anvilQuirk: a tx whose tip exceeds fee cap - base fee is charged its fee cap per gas, but its receipt reports
	// base + tip (above the cap), as Anvil does.
	anvilQuirk bool
}

func newFakeChain() *fakeChain {
	return &fakeChain{head: 10, mined: map[common.Address]uint64{}, native: map[common.Address]*big.Int{}, tokens: map[common.Address]*big.Int{},
		receipts: map[common.Hash]*Receipt{}, revert: map[common.Hash]bool{}, sent: map[common.Hash]int{}}
}

func (c *fakeChain) bal(m map[common.Address]*big.Int, a common.Address) *big.Int {
	if m[a] == nil {
		m[a] = new(big.Int)
	}
	return m[a]
}

func (c *fakeChain) credit(a common.Address, wei, tok int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bal(c.native, a).Add(c.bal(c.native, a), new(big.Int).Mul(big.NewInt(wei), big.NewInt(1)))
	c.bal(c.tokens, a).Add(c.bal(c.tokens, a), big.NewInt(tok))
}

func (c *fakeChain) ChainID(context.Context) (*big.Int, error) { return big.NewInt(chainID), nil }
func (c *fakeChain) Head(context.Context) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.head, nil
}
func (c *fakeChain) NonceAt(_ context.Context, a common.Address, _ *big.Int) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mined[a], nil // history is not modelled: "confirmed deep" is covered by the head check on receipts
}

func (c *fakeChain) PendingNonceAt(_ context.Context, a common.Address) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.mined[a]
	for {
		found := false
		for _, tx := range c.pool {
			if from, _ := types.Sender(types.LatestSignerForChainID(big.NewInt(chainID)), tx); from == a && tx.Nonce() == n {
				n++
				found = true
				break
			}
		}
		if !found {
			return n, nil
		}
	}
}

func (c *fakeChain) BalanceAt(_ context.Context, a common.Address) (*big.Int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return new(big.Int).Set(c.bal(c.native, a)), nil
}

func (c *fakeChain) TokenBalance(_ context.Context, _, h common.Address) (*big.Int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return new(big.Int).Set(c.bal(c.tokens, h)), nil
}

func (c *fakeChain) FeeQuote(context.Context) (*big.Int, *big.Int, error) {
	return tipPrice, basePrce, nil
}

func (c *fakeChain) EstimateGas(_ context.Context, _, to common.Address, _ *big.Int, data []byte) (uint64, error) {
	if to == token && len(data) > 0 {
		return 50_000, nil
	}
	return 21_000, nil
}

func (c *fakeChain) Broadcast(_ context.Context, raw []byte) error {
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	from, _ := types.Sender(types.LatestSignerForChainID(big.NewInt(chainID)), &tx)
	c.sent[tx.Hash()]++
	if tx.Nonce() < c.mined[from] {
		return errors.New("nonce too low")
	}
	for _, p := range c.pool {
		if p.Hash() == tx.Hash() {
			return nil
		}
	}
	c.pool = append(c.pool, &tx)
	return nil
}

func (c *fakeChain) Receipt(_ context.Context, h common.Hash) (*Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.receipts[h], nil
}

// drop removes a pending tx (a node that forgot it).
func (c *fakeChain) drop(h common.Hash) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, p := range c.pool {
		if p.Hash() == h {
			c.pool = append(c.pool[:i], c.pool[i+1:]...)
			return
		}
	}
}

// mine is one block holding every pool tx that is executable now.
func (c *fakeChain) mine() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.head++
	for progressed := true; progressed; {
		progressed = false
		for i, tx := range c.pool {
			from, _ := types.Sender(types.LatestSignerForChainID(big.NewInt(chainID)), tx)
			if tx.Nonce() != c.mined[from] {
				continue
			}
			c.pool = append(c.pool[:i], c.pool[i+1:]...)
			c.mined[from]++
			used := uint64(21_000)
			ok := !c.revert[tx.Hash()]
			if tx.To() != nil && *tx.To() == token && len(tx.Data()) == 68 {
				used = 50_000
				if ok {
					amt := new(big.Int).SetBytes(tx.Data()[36:68])
					to := common.BytesToAddress(tx.Data()[16:36])
					c.bal(c.tokens, from).Sub(c.bal(c.tokens, from), amt)
					c.bal(c.tokens, to).Add(c.bal(c.tokens, to), amt)
				}
			} else if ok && tx.To() != nil {
				c.bal(c.native, from).Sub(c.bal(c.native, from), tx.Value())
				c.bal(c.native, *tx.To()).Add(c.bal(c.native, *tx.To()), tx.Value())
			}
			price := new(big.Int).Add(basePrce, tx.GasTipCap())
			reported := new(big.Int).Set(price)
			if price.Cmp(tx.GasFeeCap()) > 0 {
				price = tx.GasFeeCap()
				if !c.anvilQuirk {
					reported = new(big.Int).Set(price)
				}
			}
			c.bal(c.native, from).Sub(c.bal(c.native, from), new(big.Int).Mul(new(big.Int).SetUint64(used), price))
			c.receipts[tx.Hash()] = &Receipt{Success: ok, Block: c.head, GasUsed: used, GasPrice: reported}
			progressed = true
			break
		}
	}
}

// ---- the other collaborators --------------------------------------------------------------------

type fakeGateway struct {
	key GatewayKey
	err error
}

func (g *fakeGateway) Key(context.Context) (GatewayKey, error) { return g.key, g.err }

type fakeLedger struct {
	mu    sync.Mutex
	posts map[string]GasFee // by tx hash: idempotent, like ledger-service
	calls int
	fail  int // fail this many calls first
	// refuse answers ErrLedgerRefused for these purposes (the ledger holds that Gas Fee with other values)
	refuse map[string]bool
}

func (l *fakeLedger) PostGasFee(_ context.Context, g GasFee) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.fail > 0 {
		l.fail--
		return errors.New("ledger-service unavailable")
	}
	if l.refuse[g.Purpose] {
		return fmt.Errorf("%w (409)", ErrLedgerRefused)
	}
	if l.posts == nil {
		l.posts = map[string]GasFee{}
	}
	l.posts[g.TxHash] = g
	return nil
}

func (l *fakeLedger) totalFee() *big.Int {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := new(big.Int)
	for _, g := range l.posts {
		t.Add(t, new(big.Int).Mul(new(big.Int).SetUint64(g.GasUsed), g.GasPrice))
	}
	return t
}

type softKey struct{ k *ecdsa.PrivateKey }

func (s softKey) Address() common.Address { return crypto.PubkeyToAddress(s.k.PublicKey) }
func (s softKey) SignTx(tx *types.Transaction, id *big.Int) (*types.Transaction, error) {
	return types.SignTx(tx, types.LatestSignerForChainID(id), s.k)
}

// ---- the rig --------------------------------------------------------------------------------------

type rig struct {
	t      *testing.T
	store  *storage.Store
	chain  *fakeChain
	gw     *fakeGateway
	ledger *fakeLedger
	old    softKey
	newHot common.Address
	out    *bytes.Buffer
	cfg    Config
}

func newRig(t *testing.T) *rig {
	t.Helper()
	store, err := storage.Connect(context.Background(), testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	oldKey, _ := crypto.GenerateKey()
	newKey, _ := crypto.GenerateKey()
	r := &rig{t: t, store: store, chain: newFakeChain(), ledger: &fakeLedger{}, old: softKey{oldKey}, newHot: crypto.PubkeyToAddress(newKey.PublicKey), out: &bytes.Buffer{}}
	r.gw = &fakeGateway{key: GatewayKey{Address: r.newHot, ChainID: uint64(chainID), Serving: true}}
	r.cfg = Config{ChainID: chainID, NewHot: r.newHot, Confirmations: 2, Poll: time.Millisecond, Timeout: 2 * time.Second, Out: r.out,
		Assets: signing.Assets{nativeID: {Type: signing.AssetNative}, tokenID: {Type: signing.AssetERC20, Contract: token}},
		Sleep:  func(context.Context, time.Duration) error { r.chain.mine(); return nil }}
	return r
}

func (r *rig) tool() *Tool {
	cfg := r.cfg
	cfg.Out = r.out
	return New(cfg, r.chain, r.store, r.gw, r.old, r.ledger)
}

func (r *rig) must(err error) {
	r.t.Helper()
	if err != nil {
		r.t.Fatalf("%v\n--- output ---\n%s", err, r.out)
	}
}

// signHot stores a Hot signature of the old address the way wallet-service would, returns the signed tx.
func (r *rig) signHot(key string, to common.Address, value int64) *types.Transaction {
	r.t.Helper()
	ctx := context.Background()
	var out *types.Transaction
	_, err := r.store.SignHotWallet(ctx, chainID, strings.ToLower(r.old.Address().Hex()), "withdrawal-service", key, "h",
		func(ctx context.Context) (uint64, error) { return r.chain.PendingNonceAt(ctx, r.old.Address()) },
		func(nonce uint64) ([]byte, error) {
			tx := types.NewTx(&types.DynamicFeeTx{ChainID: big.NewInt(chainID), Nonce: nonce, GasTipCap: tipPrice, GasFeeCap: big.NewInt(5_000_000_000), Gas: 21000, To: &to, Value: big.NewInt(value)})
			signed, err := r.old.SignTx(tx, big.NewInt(chainID))
			out = signed
			if err != nil {
				return nil, err
			}
			return signed.MarshalBinary()
		})
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

func (r *rig) oldBal() (*big.Int, *big.Int) {
	n, _ := r.chain.BalanceAt(context.Background(), r.old.Address())
	tk, _ := r.chain.TokenBalance(context.Background(), token, r.old.Address())
	return n, tk
}

func (r *rig) newBal() (*big.Int, *big.Int) {
	n, _ := r.chain.BalanceAt(context.Background(), r.newHot)
	tk, _ := r.chain.TokenBalance(context.Background(), token, r.newHot)
	return n, tk
}

func lowerAddr(a common.Address) string { return strings.ToLower(a.Hex()) }

// ---- tests ------------------------------------------------------------------------------------------

func TestPreflightRefusals(t *testing.T) {
	for name, mutate := range map[string]func(*rig){
		"gateway not serving": func(r *rig) { r.gw.key.Serving = false },
		"gateway down":        func(r *rig) { r.gw.err = errors.New("connection refused") },
		"new address differs": func(r *rig) { r.cfg.NewHot = common.HexToAddress("0x00000000000000000000000000000000000000aa") },
		"gateway other chain": func(r *rig) { r.gw.key.ChainID = 1 },
		"no expected address": func(r *rig) { r.cfg.NewHot = common.Address{} },
		"old equals new": func(r *rig) {
			r.gw.key.Address, r.cfg.NewHot = r.old.Address(), r.old.Address()
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			mutate(r)
			for _, step := range []func(*Tool) error{
				func(x *Tool) error { return x.Preflight(context.Background()) },
				func(x *Tool) error { return x.Freeze(context.Background()) },
				func(x *Tool) error { return x.Drain(context.Background()) },
			} {
				if err := step(r.tool()); err == nil {
					t.Fatal("expected a refusal")
				}
			}
			// A refused freeze froze nothing.
			if _, err := r.store.GetCutover(context.Background(), chainID, lowerAddr(r.old.Address())); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("a refused run left a cutover row: %v", err)
			}
		})
	}
}

// The whole procedure on a Hot address that holds native + ERC-20, with a signed-but-not-broadcast tx, an
// in-flight one and a gas top-up, then every step run twice.
func TestFullCutoverAndEveryStepIsIdempotent(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	old := r.old.Address()
	r.chain.native[old] = new(big.Int).Mul(big.NewInt(5), ether)
	r.chain.tokens[old] = new(big.Int).Mul(big.NewInt(1234), ether)

	dest := common.HexToAddress("0x00000000000000000000000000000000000000d1")
	stuck := r.signHot("withdrawal:w1", dest, 1000) // nonce 0: signed, never broadcast
	inflight := r.signHot("gas-topup:g1", dest, 2000)
	if err := r.chain.Broadcast(ctx, mustMarshal(t, inflight)); err != nil { // nonce 1 is in the pool, behind the gap
		t.Fatal(err)
	}

	// Step 2, twice.
	r.must(r.tool().Freeze(ctx))
	r.must(r.tool().Freeze(ctx))
	if _, err := r.store.SignHotWallet(ctx, chainID, lowerAddr(old), "withdrawal-service", "new-while-frozen", "h", nil, func(uint64) ([]byte, error) { return nil, nil }); !errors.Is(err, storage.ErrHotCutover) {
		t.Fatalf("a new sign while frozen: %v", err)
	}
	// Drain before settle is refused: nonce 0 is not mined.
	if err := r.tool().Drain(ctx); err == nil || !strings.Contains(err.Error(), "settle") {
		t.Fatalf("drain before settle: %v", err)
	}
	if ds, _ := r.store.Drains(ctx, chainID, lowerAddr(old)); len(ds) != 0 {
		t.Fatal("a refused drain stored something")
	}

	// Step 3, twice: it rebroadcasts the stuck tx from the stored signature and waits.
	r.must(r.tool().Settle(ctx))
	if r.chain.sent[stuck.Hash()] == 0 {
		t.Fatal("settle did not rebroadcast the signed-but-not-broadcast tx")
	}
	r.must(r.tool().Settle(ctx))
	if n, _ := r.chain.NonceAt(ctx, old, nil); n != 2 {
		t.Fatalf("old chain nonce %d, want 2", n)
	}

	// Dry-run drain changes nothing and says what it would do.
	dry := r.rigWith(func(c *Config) { c.DryRun = true })
	r.must(dry.Drain(ctx))
	if ds, _ := r.store.Drains(ctx, chainID, lowerAddr(old)); len(ds) != 0 {
		t.Fatal("dry-run planned a drain")
	}
	if !strings.Contains(r.out.String(), "[dry-run] would drain "+tokenID) || !strings.Contains(r.out.String(), "POINT OF NO RETURN") {
		t.Fatalf("dry-run output lacks the plan:\n%s", r.out)
	}

	// Steps 5-7, twice.
	r.must(r.tool().Drain(ctx))
	r.must(r.tool().Drain(ctx))
	ds, err := r.store.Drains(ctx, chainID, lowerAddr(old))
	if err != nil || len(ds) != 2 {
		t.Fatalf("want exactly 2 drain txs (token, native), got %d: %v", len(ds), err)
	}
	if ds[0].AssetID != tokenID || ds[1].AssetID != nativeID || ds[0].Nonce != 2 || ds[1].Nonce != 3 {
		t.Fatalf("the ERC-20 drains first and the native one last, at the next dense nonces: %+v", ds)
	}
	on, ot := r.oldBal()
	nn, nt := r.newBal()
	if on.Sign() != 0 || ot.Sign() != 0 {
		t.Fatalf("old Hot not empty: %s wei, %s TT", on, ot)
	}
	if nt.Cmp(new(big.Int).Mul(big.NewInt(1234), ether)) != 0 {
		t.Fatalf("new Hot TT = %s", nt)
	}
	// Everything the old address had left the old address: new + gas of every tx the old address sent == 5 ETH.
	spent := new(big.Int)
	for _, h := range []common.Hash{stuck.Hash(), inflight.Hash(), common.HexToHash(ds[0].TxHash), common.HexToHash(ds[1].TxHash)} {
		rc, _ := r.chain.Receipt(ctx, h)
		spent.Add(spent, new(big.Int).Mul(new(big.Int).SetUint64(rc.GasUsed), rc.GasPrice))
	}
	spent.Add(spent, big.NewInt(1000+2000)) // the two withdrawals' values went to dest
	if got := new(big.Int).Add(nn, spent); got.Cmp(new(big.Int).Mul(big.NewInt(5), ether)) != 0 {
		t.Fatalf("native is not conserved: new %s + out %s = %s", nn, spent, got)
	}
	// Step 7: the ledger holds the gas of the two drain txs (purpose treasury_sweep) and of the Gas Top-Up that was in
	// flight while the sweeper was stopped (purpose gas_topup), once each, equal to what the chain charged.
	if len(r.ledger.posts) != 3 {
		t.Fatalf("ledger posts: %d, want 3 (two drain txs and the in-flight top-up)", len(r.ledger.posts))
	}
	want := new(big.Int)
	for _, h := range []string{ds[0].TxHash, ds[1].TxHash} {
		rc, _ := r.chain.Receipt(ctx, common.HexToHash(h))
		want.Add(want, new(big.Int).Mul(new(big.Int).SetUint64(rc.GasUsed), rc.GasPrice))
		if g := r.ledger.posts[h]; g.Purpose != GasPurpose || g.ChainID != chainID {
			t.Fatalf("posted %+v", g)
		}
	}
	rcTop, _ := r.chain.Receipt(ctx, inflight.Hash())
	want.Add(want, new(big.Int).Mul(new(big.Int).SetUint64(rcTop.GasUsed), rcTop.GasPrice))
	if g := r.ledger.posts[strings.ToLower(inflight.Hash().Hex())]; g.Purpose != "gas_topup" {
		t.Fatalf("the in-flight Gas Top-Up's gas: %+v", g)
	}
	if r.ledger.posts[strings.ToLower(stuck.Hash().Hex())].TxHash != "" {
		t.Fatal("the withdrawal's gas is withdrawal-service's to post, not this tool's")
	}
	if r.ledger.totalFee().Cmp(want) != 0 {
		t.Fatalf("ledger fee %s, chain %s", r.ledger.totalFee(), want)
	}
	if c, _ := r.store.GetCutover(ctx, chainID, lowerAddr(old)); c.State != storage.CutoverDrained || c.PonrAt == nil {
		t.Fatalf("state %s", c.State)
	}
	// Past the point of no return there is no abort.
	if err := r.tool().Abort(ctx); !errors.Is(err, storage.ErrCutoverState) {
		t.Fatalf("abort after the point of no return: %v", err)
	}

	// Verify, twice. The nonce row of the old address equals its chain nonce.
	if _, err := r.tool().Verify(ctx); err != nil {
		t.Fatalf("%v\n%s", err, r.out)
	}
	if _, err := r.tool().Verify(ctx); err != nil {
		t.Fatal(err)
	}
	if next, _, _, _ := r.store.NonceRow(ctx, chainID, lowerAddr(old)); next != 4 {
		t.Fatalf("old next_nonce %d, want 4", next)
	}

	// Step 9: refused without the affirmation, then done, twice. Signing then works, on the new address's own nonce.
	if err := r.tool().Complete(ctx, false); err == nil {
		t.Fatal("complete without -switched must refuse")
	}
	r.must(r.tool().Complete(ctx, true))
	r.must(r.tool().Complete(ctx, true))
	sig, err := r.store.SignHotWallet(ctx, chainID, lowerAddr(r.newHot), "withdrawal-service", "first-on-new", "h",
		func(ctx context.Context) (uint64, error) { return r.chain.PendingNonceAt(ctx, r.newHot) },
		func(uint64) ([]byte, error) { return []byte("x"), nil })
	if err != nil || sig.Nonce != 0 {
		t.Fatalf("first signature of the new Hot address: %+v %v", sig, err)
	}
	// And a re-run of freeze after the cutover is done does not freeze again.
	r.must(r.tool().Freeze(ctx))
	if _, err := r.store.SignHotWallet(ctx, chainID, lowerAddr(r.newHot), "withdrawal-service", "second-on-new", "h", nil, func(uint64) ([]byte, error) { return []byte("y"), nil }); err != nil {
		t.Fatalf("a repeated freeze of a finished cutover froze signing: %v", err)
	}
}

func mustMarshal(t *testing.T, tx *types.Transaction) []byte {
	b, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (r *rig) rigWith(f func(*Config)) *Tool {
	cfg := r.cfg
	cfg.Out = r.out
	f(&cfg)
	return New(cfg, r.chain, r.store, r.gw, r.old, r.ledger)
}

func TestDrainResumesAfterALedgerOutageWithoutADoubleDrain(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.chain.native[r.old.Address()] = new(big.Int).Mul(big.NewInt(3), ether)
	r.chain.tokens[r.old.Address()] = big.NewInt(77)
	r.must(r.tool().Freeze(ctx))
	r.must(r.tool().Settle(ctx))

	r.ledger.fail = 1
	if err := r.tool().Drain(ctx); err == nil || !strings.Contains(err.Error(), "ledger") {
		t.Fatalf("want the ledger error: %v", err)
	}
	// The tx was mined; the gas was not posted yet. The crash left a durable plan.
	ds, _ := r.store.Drains(ctx, chainID, lowerAddr(r.old.Address()))
	if len(ds) != 1 || ds[0].ConfirmedAt == nil || ds[0].GasPostedAt != nil {
		t.Fatalf("after the outage: %+v", ds)
	}
	r.must(r.tool().Drain(ctx))
	ds, _ = r.store.Drains(ctx, chainID, lowerAddr(r.old.Address()))
	if len(ds) != 2 || len(r.ledger.posts) != 2 {
		t.Fatalf("drains %d posts %d", len(ds), len(r.ledger.posts))
	}
	for _, d := range ds {
		if r.chain.sent[common.HexToHash(d.TxHash)] == 0 {
			t.Fatal("never broadcast")
		}
	}
	if n, tk := r.oldBal(); n.Sign() != 0 || tk.Sign() != 0 {
		t.Fatalf("old not empty: %s %s", n, tk)
	}
}

func TestLateArrivalPlansAnotherRound(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.chain.native[r.old.Address()] = new(big.Int).Mul(big.NewInt(3), ether)
	r.chain.tokens[r.old.Address()] = big.NewInt(10)
	r.must(r.tool().Freeze(ctx))
	r.must(r.tool().Settle(ctx))
	r.must(r.tool().Drain(ctx))

	// A late Sweep lands on the old address after the drain completed.
	r.chain.credit(r.old.Address(), 4e17, 5)
	if _, err := r.tool().Verify(ctx); err == nil {
		t.Fatal("verify must fail while the old Hot address is not empty")
	}
	r.must(r.tool().Drain(ctx))
	ds, _ := r.store.Drains(ctx, chainID, lowerAddr(r.old.Address()))
	if len(ds) != 4 || ds[2].Round != 2 || ds[3].Round != 2 {
		t.Fatalf("want a second round of two txs: %+v", ds)
	}
	if n, tk := r.oldBal(); n.Sign() != 0 || tk.Sign() != 0 {
		t.Fatalf("old not empty: %s %s", n, tk)
	}
	if _, nt := r.newBal(); nt.Int64() != 15 {
		t.Fatalf("new TT %s", nt)
	}
	r.must(func() error { _, err := r.tool().Verify(ctx); return err }())
	if next, _, _, _ := r.store.NonceRow(ctx, chainID, lowerAddr(r.old.Address())); next != 4 {
		t.Fatalf("old next_nonce %d, want 4", next)
	}
}

func TestNativeOnlyAndAnAlreadyEmptyAddress(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.chain.native[r.old.Address()] = big.NewInt(5e18)
	r.must(r.tool().Freeze(ctx))
	r.must(r.tool().Settle(ctx))
	r.must(r.tool().Drain(ctx))
	if ds, _ := r.store.Drains(ctx, chainID, lowerAddr(r.old.Address())); len(ds) != 1 || ds[0].AssetID != nativeID {
		t.Fatalf("%+v", ds)
	}
	if n, _ := r.oldBal(); n.Sign() != 0 {
		t.Fatalf("old %s", n)
	}

	e := newRig(t) // nothing on the old address at all
	e.must(e.tool().Freeze(ctx))
	e.must(e.tool().Settle(ctx))
	e.must(e.tool().Drain(ctx))
	if c, _ := e.store.GetCutover(ctx, chainID, lowerAddr(e.old.Address())); c.State != storage.CutoverDrained || c.PonrAt != nil {
		t.Fatalf("an empty old address is drained without a point of no return: %+v", c)
	}
	if _, err := e.tool().Verify(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestNotEnoughGasRefusesBeforeSigningAndAbortStillWorks(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.chain.native[r.old.Address()] = big.NewInt(1000) // dust
	r.chain.tokens[r.old.Address()] = big.NewInt(50)
	r.must(r.tool().Freeze(ctx))
	r.must(r.tool().Settle(ctx))
	err := r.tool().Drain(ctx)
	if err == nil || !strings.Contains(err.Error(), "send at least") || !strings.Contains(err.Error(), "nothing was signed") {
		t.Fatalf("want the funding instruction: %v", err)
	}
	if ds, _ := r.store.Drains(ctx, chainID, lowerAddr(r.old.Address())); len(ds) != 0 {
		t.Fatal("something was signed")
	}
	r.must(r.tool().Abort(ctx))
	r.must(r.tool().Abort(ctx))
	if _, err := r.store.SignHotWallet(ctx, chainID, lowerAddr(r.old.Address()), "c", "k", "h", seed0, func(uint64) ([]byte, error) { return []byte("x"), nil }); err != nil {
		t.Fatalf("signing after the abort: %v", err)
	}
}

func seed0(context.Context) (uint64, error) { return 0, nil }

func TestSettleBlockersAreNamed(t *testing.T) {
	ctx := context.Background()
	t.Run("a tx that is never mined times out and names the key", func(t *testing.T) {
		r := newRig(t)
		r.cfg.Timeout = 100 * time.Millisecond
		r.cfg.Sleep = func(context.Context, time.Duration) error { time.Sleep(10 * time.Millisecond); return nil } // chain never mines
		r.signHot("withdrawal:stuck", common.HexToAddress("0x00000000000000000000000000000000000000d2"), 1)
		r.must(r.tool().Freeze(ctx))
		err := r.tool().Settle(ctx)
		if err == nil || !strings.Contains(err.Error(), "withdrawal:stuck") || !strings.Contains(err.Error(), "hot-wallet cancel") {
			t.Fatalf("%v", err)
		}
		if err := r.tool().Drain(ctx); err == nil {
			t.Fatal("drain must refuse while something is in flight")
		}
	})
	t.Run("a Nonce Gap is a blocker", func(t *testing.T) {
		r := newRig(t)
		r.signHot("withdrawal:g", common.HexToAddress("0x00000000000000000000000000000000000000d3"), 1)
		if _, err := r.store.FreezeNonceGap(ctx, chainID, lowerAddr(r.old.Address()), 0); err != nil {
			t.Fatal(err)
		}
		r.must(r.tool().Freeze(ctx))
		if err := r.tool().Settle(ctx); err == nil || !strings.Contains(err.Error(), "Nonce Gap") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("the chain ahead of wallet-service is a hard stop", func(t *testing.T) {
		r := newRig(t)
		r.chain.mined[r.old.Address()] = 3 // someone signed outside wallet-service
		r.must(r.tool().Freeze(ctx))
		if err := r.tool().Settle(ctx); err == nil || !strings.Contains(err.Error(), "outside wallet-service") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("dry-run settle changes and rebroadcasts nothing", func(t *testing.T) {
		r := newRig(t)
		tx := r.signHot("withdrawal:d", common.HexToAddress("0x00000000000000000000000000000000000000d4"), 1)
		r.must(r.tool().Freeze(ctx))
		r.must(r.rigWith(func(c *Config) { c.DryRun = true }).Settle(ctx))
		if r.chain.sent[tx.Hash()] != 0 {
			t.Fatal("dry-run broadcast")
		}
		if !strings.Contains(r.out.String(), "[dry-run] would rebroadcast nonce 0") {
			t.Fatal(r.out.String())
		}
	})
}

func TestARevertedDrainStopsEverything(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.chain.native[r.old.Address()] = new(big.Int).Mul(big.NewInt(3), ether)
	r.chain.tokens[r.old.Address()] = big.NewInt(10)
	r.must(r.tool().Freeze(ctx))
	r.must(r.tool().Settle(ctx))
	// Plan, then make the token drain revert.
	r.cfg.Sleep = func(context.Context, time.Duration) error {
		r.chain.mu.Lock()
		for _, p := range r.chain.pool {
			r.chain.revert[p.Hash()] = true
		}
		r.chain.mu.Unlock()
		r.chain.mine()
		return nil
	}
	err := r.tool().Drain(ctx)
	if err == nil || !strings.Contains(err.Error(), "REVERTED") {
		t.Fatalf("%v", err)
	}
	if len(r.ledger.posts) != 0 {
		t.Fatal("the gas of an unconfirmed drain must not be posted")
	}
	if c, _ := r.store.GetCutover(ctx, chainID, lowerAddr(r.old.Address())); c.State != storage.CutoverDraining {
		t.Fatalf("state %s", c.State)
	}
}

func TestCompleteBeforeDrainedIsRefused(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.must(r.tool().Freeze(ctx))
	if err := r.tool().Complete(ctx, true); !errors.Is(err, storage.ErrCutoverState) {
		t.Fatalf("%v", err)
	}
	if err := r.tool().Settle(ctx); err != nil { // an empty address settles at once
		t.Fatal(err)
	}
	if err := newRig(t).tool().Settle(ctx); err == nil || !strings.Contains(err.Error(), "freeze") {
		t.Fatalf("settle without a freeze: %v", err)
	}
}

func TestDryRunFreezeWritesNothing(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.must(r.rigWith(func(c *Config) { c.DryRun = true }).Freeze(ctx))
	if _, err := r.store.GetCutover(ctx, chainID, lowerAddr(r.old.Address())); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("dry-run froze: %v", err)
	}
	if !strings.Contains(r.out.String(), "[dry-run] would freeze") {
		t.Fatal(r.out.String())
	}
}

func TestAGasTopUpTheLedgerAlreadyHasIsNotAnError(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.chain.native[r.old.Address()] = new(big.Int).Mul(big.NewInt(3), ether)
	topup := r.signHot("gas-topup:x", common.HexToAddress("0x00000000000000000000000000000000000000d5"), 5)
	r.must(r.chain.Broadcast(ctx, mustMarshal(t, topup)))
	r.must(r.tool().Freeze(ctx))
	r.must(r.tool().Settle(ctx))
	r.ledger.refuse = map[string]bool{"gas_topup": true} // the sweeper posted it with its own values
	r.must(r.tool().Drain(ctx))
	if _, ok := r.ledger.posts[strings.ToLower(topup.Hash().Hex())]; ok {
		t.Fatal("a refused post must not be recorded as posted")
	}
	if c, _ := r.store.GetCutover(ctx, chainID, lowerAddr(r.old.Address())); c.State != storage.CutoverDrained {
		t.Fatalf("state %s", c.State)
	}
	// dry-run reports the top-ups it would post and posts nothing
	e := newRig(t)
	tp := e.signHot("gas-topup:y", common.HexToAddress("0x00000000000000000000000000000000000000d6"), 5)
	e.must(e.chain.Broadcast(ctx, mustMarshal(t, tp)))
	e.chain.credit(e.old.Address(), 3e18, 0)
	e.must(e.tool().Freeze(ctx))
	e.must(e.tool().Settle(ctx))
	e.must(e.rigWith(func(c *Config) { c.DryRun = true }).Drain(ctx))
	if e.ledger.calls != 0 || !strings.Contains(e.out.String(), "[dry-run] would post to the ledger the gas of 1 mined Gas Top-Up") {
		t.Fatalf("calls %d\n%s", e.ledger.calls, e.out)
	}
}

// The ledger gets what the chain charged even when a node's receipt reports a price above the tx's fee cap (Anvil does,
// for the drain's tip == cap): the posted gas of every drain tx equals the balance the old Hot actually lost.
func TestTheDrainsGasIsClampedToTheFeeCap(t *testing.T) {
	r := newRig(t)
	r.chain.anvilQuirk = true
	ctx := context.Background()
	r.chain.native[r.old.Address()] = new(big.Int).Mul(big.NewInt(3), ether)
	r.chain.tokens[r.old.Address()] = big.NewInt(10)
	r.must(r.tool().Freeze(ctx))
	r.must(r.tool().Settle(ctx))
	r.must(r.tool().Drain(ctx))
	ds, _ := r.store.Drains(ctx, chainID, lowerAddr(r.old.Address()))
	if len(ds) != 2 {
		t.Fatalf("%d drains", len(ds))
	}
	for _, d := range ds {
		var tx types.Transaction
		if err := tx.UnmarshalBinary(d.Raw); err != nil {
			t.Fatal(err)
		}
		rc, _ := r.chain.Receipt(ctx, common.HexToHash(d.TxHash))
		if rc.GasPrice.Cmp(tx.GasFeeCap()) <= 0 {
			t.Fatal("the fake did not reproduce the quirk")
		}
		if g := r.ledger.posts[d.TxHash]; g.GasPrice.Cmp(tx.GasFeeCap()) != 0 {
			t.Errorf("posted price %s, want the fee cap %s", g.GasPrice, tx.GasFeeCap())
		}
	}
	if n, tk := r.oldBal(); n.Sign() != 0 || tk.Sign() != 0 {
		t.Fatalf("old not empty: %s %s", n, tk)
	}
}
