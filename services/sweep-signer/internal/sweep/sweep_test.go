package sweep

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
	"github.com/ethereum/go-ethereum/params"
	hdw "github.com/miguelmota/go-ethereum-hdwallet"

	"digiwal/services/sweep-signer/internal/assets"
	"digiwal/services/sweep-signer/internal/derive"
	"digiwal/services/sweep-signer/internal/store"
	"digiwal/services/sweep-signer/internal/vault"
)

const (
	chainID  = 1337 // the simulated backend's chain id
	nativeID = "11111111-1111-1111-1111-111111111111"
	tokenID  = "22222222-2222-2222-2222-222222222222"
	token    = "0x5FbDB2315678afecb367f032d93F642f64180aa3"
	dest     = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
	// the user_derivation Seed of these tests: a well-known public test mnemonic, never used for anything real. The mnemonic
	// string lives in this test only; nothing under sweep-signer's non-test code makes a string out of a Seed.
	userMnemonic = "legal winner thank year wave sausage worth useful legal winner thank yellow"
)

type simChain struct{ c simulated.Client }

func (s simChain) ChainID(ctx context.Context) (*big.Int, error) { return s.c.ChainID(ctx) }
func (s simChain) PendingNonceAt(ctx context.Context, a common.Address) (uint64, error) {
	return s.c.PendingNonceAt(ctx, a)
}
func (s simChain) BalanceAt(ctx context.Context, a common.Address) (*big.Int, error) {
	return s.c.BalanceAt(ctx, a, nil)
}
func (s simChain) FeeQuote(ctx context.Context) (*big.Int, *big.Int, error) {
	tip, err := s.c.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, nil, err
	}
	head, err := s.c.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	return tip, head.BaseFee, nil
}
func (s simChain) EstimateGas(ctx context.Context, from, to common.Address, v *big.Int, d []byte) (uint64, error) {
	return s.c.EstimateGas(ctx, ethereum.CallMsg{From: from, To: &to, Value: v, Data: d})
}

// fakeChain is a chain with a settable fee market and no EVM (ERC-20 sweeps and fee-ceiling cases).
type fakeChain struct {
	tip, base *big.Int
	balance   *big.Int
}

func (f fakeChain) ChainID(context.Context) (*big.Int, error) { return big.NewInt(chainID), nil }
func (f fakeChain) PendingNonceAt(context.Context, common.Address) (uint64, error) {
	return 4, nil
}
func (f fakeChain) BalanceAt(context.Context, common.Address) (*big.Int, error) {
	return f.balance, nil
}
func (f fakeChain) FeeQuote(context.Context) (*big.Int, *big.Int, error) { return f.tip, f.base, nil }
func (f fakeChain) EstimateGas(context.Context, common.Address, common.Address, *big.Int, []byte) (uint64, error) {
	return 21000, nil
}

type dir map[string]store.Issued

func (d dir) Issued(_ context.Context, user, _ string) (store.Issued, error) {
	r, ok := d[user]
	if !ok {
		return store.Issued{}, store.ErrNotFound
	}
	return r, nil
}

type gate struct {
	mu      sync.Mutex
	err     error
	tripped string
}

func (g *gate) Ready() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.tripped != "" {
		return errors.New("tripped: " + g.tripped)
	}
	return g.err
}
func (g *gate) Trip(reason string) { g.mu.Lock(); g.tripped = reason; g.mu.Unlock() }

type counts struct {
	mu sync.Mutex
	m  map[string]int
}

func (c *counts) Refused(rule string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]int{}
	}
	c.m[rule]++
}
func (c *counts) n(rule string) int { c.mu.Lock(); defer c.mu.Unlock(); return c.m[rule] }

// fakeGW is a TEST-ONLY stand-in for hsm-gateway's unseal (a _test file: the service has no software unseal path).
type fakeGW struct {
	seed  []byte
	err   error
	calls int
}

type rowSrc struct{}

func (rowSrc) ActiveSecret(context.Context) (vault.Row, error) {
	return vault.Row{Role: "user_derivation", Purpose: "sweep", Version: 1, KEKVersion: 1}, nil
}

func (f *fakeGW) UnsealInto(_ context.Context, _ vault.Row, dst []byte) (int, error) {
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	return copy(dst, f.seed), nil
}

type env struct {
	signer  *Signer
	gate    *gate
	obs     *counts
	hot     common.Address
	userAdr common.Address
	backend *simulated.Backend
	chain   simChain
	dir     dir
	seeds   *vault.Provider
	seed    []byte
}

func newSeed(t *testing.T) []byte {
	t.Helper()
	s, err := hdw.NewSeedFromMnemonic(userMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newEnv(t *testing.T) *env {
	t.Helper()
	seed := newSeed(t)
	gw := &fakeGW{seed: seed}
	seeds := &vault.Provider{Source: rowSrc{}, Unsealer: gw}
	userAddr, err := derive.AddressAt(seed, 0)
	if err != nil {
		t.Fatal(err)
	}
	hot := common.HexToAddress(dest)
	backend := simulated.NewBackend(types.GenesisAlloc{userAddr: {Balance: new(big.Int).Mul(big.NewInt(2), big.NewInt(params.Ether))}})
	t.Cleanup(func() { _ = backend.Close() })
	chain := simChain{backend.Client()}
	id, _ := chain.ChainID(context.Background())
	d := dir{"alice": {UserID: "alice", Index: 0, Address: userAddr.Hex()}}
	g, o := &gate{}, &counts{}
	s, err := New(context.Background(), d, seeds, chain, assets.Assets{nativeID: {Type: assets.Native}}, hot, id.Uint64(), g)
	if err != nil {
		t.Fatal(err)
	}
	s.WithObserver(o)
	return &env{signer: s, gate: g, obs: o, hot: hot, userAdr: userAddr, backend: backend, chain: chain, dir: d, seeds: seeds, seed: seed}
}

func (e *env) req() Request {
	return Request{UserID: "alice", ToAddress: e.hot.Hex(), AssetID: nativeID, Amount: "1000000000000000000", ChainID: chainID}
}

func decode(t *testing.T, resp Response) *types.Transaction {
	t.Helper()
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(common.FromHex(resp.SignedRawTx)); err != nil {
		t.Fatalf("response is not a valid raw tx: %v", err)
	}
	return tx
}

// ---- ported from wallet-service's useraddress_test.go -------------------------------------------------------------

// End to end against a real EVM: the sweep is signed by alice's derived key, mined, and lands in the Hot Wallet.
func TestSweep_SignedByTheUsersKeyAndMinedByARealEVM(t *testing.T) {
	e := newEnv(t)
	resp, err := e.signer.Sign(context.Background(), e.req())
	if err != nil {
		t.Fatal(err)
	}
	tx := decode(t, resp)
	sender, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx)
	if err != nil || sender != e.userAdr {
		t.Fatalf("recovered %s (%v), want alice's address %s", sender, err, e.userAdr)
	}
	if *tx.To() != e.hot {
		t.Fatalf("destination %s, want hot wallet", tx.To())
	}
	if err := e.backend.Client().SendTransaction(context.Background(), tx); err != nil {
		t.Fatalf("EVM rejected the sweep: %v", err)
	}
	e.backend.Commit()
	bal, _ := e.backend.Client().BalanceAt(context.Background(), e.hot, nil)
	if bal.String() != "1000000000000000000" {
		t.Fatalf("hot wallet balance %s", bal)
	}
	// A retry after the sweep signs the *next* live nonce, not a stale one.
	r := e.req()
	r.Amount = "1000"
	again, err := e.signer.Sign(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if decode(t, again).Nonce() != 1 {
		t.Fatal("nonce must be the live pending nonce (1 after one mined sweep)")
	}
}

func TestSweep_DestinationIsNeverTakenFromTheCaller(t *testing.T) {
	e := newEnv(t)
	for _, to := range []string{"0x1111111111111111111111111111111111111111", e.userAdr.Hex(), "0x0000000000000000000000000000000000000000"} {
		r := e.req()
		r.ToAddress = to
		if _, err := e.signer.Sign(context.Background(), r); !errors.Is(err, ErrDestinationNotHotWallet) {
			t.Errorf("to=%s: want ErrDestinationNotHotWallet, got %v", to, err)
		}
	}
	// hex case must not matter for the legitimate destination
	r := e.req()
	r.ToAddress = "0x" + strings.ToUpper(e.hot.Hex()[2:])
	if _, err := e.signer.Sign(context.Background(), r); err != nil {
		t.Errorf("checksum/case variants of the hot wallet address must be accepted: %v", err)
	}
}

func TestSweep_Validation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	mut := func(f func(*Request)) Request { r := e.req(); f(&r); return r }
	cases := []struct {
		name string
		req  Request
		want error
	}{
		{"unknown user", mut(func(r *Request) { r.UserID = "nobody" }), ErrUnknownUser},
		{"no user", mut(func(r *Request) { r.UserID = "" }), ErrInvalidRequest},
		{"zero amount", mut(func(r *Request) { r.Amount = "0" }), ErrInvalidRequest},
		{"negative amount", mut(func(r *Request) { r.Amount = "-5" }), ErrInvalidRequest},
		{"bad address", mut(func(r *Request) { r.ToAddress = "nope" }), ErrInvalidRequest},
		{"other chain", mut(func(r *Request) { r.ChainID = 1 }), ErrUnsupportedChain},
		{"unknown asset", mut(func(r *Request) { r.AssetID = tokenID }), ErrUnknownAsset},
	}
	for _, tc := range cases {
		if _, err := e.signer.Sign(ctx, tc.req); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
}

// "max": sweep everything except the tx's own gas. The EVM only accepts a tx whose value + gas*maxFee fits the balance, so
// mining it proves the reservation is right.
func TestSweep_MaxReservesGasAndIsMinedByARealEVM(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := e.req()
	r.Amount = AmountMax
	resp, err := e.signer.Sign(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	tx := decode(t, resp)
	before, _ := e.chain.BalanceAt(ctx, e.userAdr)
	maxFee := new(big.Int).Mul(new(big.Int).SetUint64(tx.Gas()), tx.GasFeeCap())
	if new(big.Int).Add(tx.Value(), maxFee).Cmp(before) != 0 {
		t.Fatalf("value %s + max fee %s must equal balance %s exactly", tx.Value(), maxFee, before)
	}
	if err := e.backend.Client().SendTransaction(ctx, tx); err != nil {
		t.Fatalf("EVM rejected the max sweep: %v", err)
	}
	e.backend.Commit()
	if bal, _ := e.chain.BalanceAt(ctx, e.hot); bal.Cmp(tx.Value()) != 0 {
		t.Fatalf("hot wallet got %s, want %s", bal, tx.Value())
	}
	left, _ := e.chain.BalanceAt(ctx, e.userAdr)
	if left.Sign() < 0 || left.Cmp(maxFee) >= 0 {
		t.Fatalf("leftover dust %s should be below the reserved fee %s", left, maxFee)
	}
}

func TestSweep_MaxRules(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// "max" is a native-only concept
	erc := e.req()
	erc.Amount, erc.AssetID = AmountMax, tokenID
	e.signer.assets[tokenID] = assets.Asset{Type: assets.ERC20, Contract: common.HexToAddress(token)}
	if _, err := e.signer.Sign(ctx, erc); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("max on erc20: got %v, want ErrInvalidRequest", err)
	}
	// nothing left after gas: refuse instead of signing a tx the chain would reject
	poor := simulated.NewBackend(types.GenesisAlloc{e.userAdr: {Balance: big.NewInt(1000)}})
	t.Cleanup(func() { _ = poor.Close() })
	e.signer.chain = simChain{poor.Client()}
	r := e.req()
	r.Amount = AmountMax
	if _, err := e.signer.Sign(ctx, r); !errors.Is(err, ErrBalanceBelowGas) {
		t.Errorf("dust balance: got %v, want ErrBalanceBelowGas", err)
	}
}

// ---- invariant 15: one refusal test per rule, each also counted and (but for a malformed request) alerted ----------

func (e *env) mustRefuse(t *testing.T, r Request, want error, rule string) {
	t.Helper()
	before := e.obs.n(rule)
	_, err := e.signer.Sign(context.Background(), r)
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
	if e.obs.n(rule) != before+1 {
		t.Fatalf("refusal counter for rule %q: %d, want %d", rule, e.obs.n(rule), before+1)
	}
}

func TestInvariant15_DestinationMustBeThePinnedHot(t *testing.T) {
	e := newEnv(t)
	r := e.req()
	r.ToAddress = e.userAdr.Hex() // a request that tries to send it elsewhere
	e.mustRefuse(t, r, ErrDestinationNotHotWallet, RuleDest)
}

func TestInvariant15_ChainMustBeThePinnedChain(t *testing.T) {
	e := newEnv(t)
	r := e.req()
	r.ChainID = 1
	e.mustRefuse(t, r, ErrUnsupportedChain, RuleChain)
	// and at start: an RPC on another chain than the gateway's pin is a startup error, not a signer
	id, _ := e.chain.ChainID(context.Background())
	if _, err := New(context.Background(), e.dir, e.seeds, e.chain, e.signer.assets, e.hot, id.Uint64()+1, e.gate); err == nil {
		t.Fatal("New accepted an RPC on a different chain than the pinned one")
	}
}

func TestInvariant15_AssetMustBeRegistered(t *testing.T) {
	e := newEnv(t)
	r := e.req()
	r.AssetID = "99999999-9999-9999-9999-999999999999"
	e.mustRefuse(t, r, ErrUnknownAsset, RuleAsset)
}

func TestInvariant15_IndexMustHaveBeenIssued(t *testing.T) {
	e := newEnv(t)
	r := e.req()
	r.UserID = "mallory" // no wallet_addresses row, so no index was ever issued to this user
	e.mustRefuse(t, r, ErrUnknownUser, RuleIndex)
}

func TestInvariant15_FeeMustNotExceedTheCeiling(t *testing.T) {
	e := newEnv(t)
	e.signer.chain = fakeChain{tip: big.NewInt(2e9), base: big.NewInt(60e9), balance: big.NewInt(1e18)} // cap = 2*60+2 = 122 gwei > 50
	e.mustRefuse(t, e.req(), ErrFeeCeiling, RuleFee)
	// just under the ceiling signs
	e.signer.chain = fakeChain{tip: big.NewInt(2e9), base: big.NewInt(24e9), balance: big.NewInt(1e18)} // 50 gwei exactly
	if _, err := e.signer.Sign(context.Background(), e.req()); err != nil {
		t.Fatalf("a cap equal to the ceiling must sign: %v", err)
	}
	e.signer.WithFeeCeiling(big.NewInt(49e9))
	e.mustRefuse(t, e.req(), ErrFeeCeiling, RuleFee)
}

// Invariant 11: the key derived at the user's index must be the address that was issued. A stored row that the Seed does
// not derive (a swapped xpub issued it) is refused, alerted, and freezes every later Sweep.
func TestInvariant15_DerivedAddressMustEqualTheIssuedRow(t *testing.T) {
	e := newEnv(t)
	e.dir["alice"] = store.Issued{UserID: "alice", Index: 0, Address: "0x000000000000000000000000000000000000dEaD"}
	e.signer.chain = fakeChain{tip: big.NewInt(2e9), base: big.NewInt(10e9), balance: big.NewInt(1e18)} // the planted address has no funds on the EVM
	e.mustRefuse(t, e.req(), ErrAddressMismatch, RuleMismatch)
	if e.gate.tripped == "" {
		t.Fatal("a derived-address mismatch did not trip the audit gate")
	}
	// an honest user is refused now too: the whole signer is frozen
	e.dir["bob"] = store.Issued{UserID: "bob", Index: 0, Address: e.userAdr.Hex()}
	r := e.req()
	r.UserID = "bob"
	e.mustRefuse(t, r, ErrFrozen, RuleFrozen)
}

// A row at the right index but of another user's address (the index/address pair does not belong together) is the same refusal.
func TestInvariant15_RowWithTheWrongIndexForItsAddress(t *testing.T) {
	e := newEnv(t)
	e.dir["alice"] = store.Issued{UserID: "alice", Index: 1, Address: e.userAdr.Hex()} // index 1 does not derive index 0's address
	e.mustRefuse(t, e.req(), ErrAddressMismatch, RuleMismatch)
}

func TestInvariant15_AuditFrozenRefusesEverything(t *testing.T) {
	e := newEnv(t)
	e.gate.err = errors.New("the first address audit pass has not completed")
	e.mustRefuse(t, e.req(), ErrFrozen, RuleFrozen)
}

// "no other operation": only a native transfer to Hot, or ERC-20 transfer(hot, amount) on the registered contract, is ever
// signed. The shape check is the last gate before a key is touched.
func TestInvariant15_OnlyASweepShapeIsEverSigned(t *testing.T) {
	e := newEnv(t)
	contract := common.HexToAddress(token)
	erc20 := assets.Asset{Type: assets.ERC20, Contract: contract}
	native := assets.Asset{Type: assets.Native}
	mk := func(to *common.Address, value int64, data []byte) *types.Transaction {
		return types.NewTx(&types.DynamicFeeTx{ChainID: big.NewInt(chainID), GasFeeCap: big.NewInt(1), Gas: 21000, To: to, Value: big.NewInt(value), Data: data})
	}
	good := erc20TransferData(e.hot, big.NewInt(5))
	approve := append([]byte{0x09, 0x5e, 0xa7, 0xb3}, good[4:]...) // approve(spender, amount)
	toOther := erc20TransferData(common.HexToAddress("0x1111111111111111111111111111111111111111"), big.NewInt(5))
	other := common.HexToAddress("0x1111111111111111111111111111111111111111")
	cases := []struct {
		name  string
		tx    *types.Transaction
		asset assets.Asset
		ok    bool
	}{
		{"native to hot", mk(&e.hot, 5, nil), native, true},
		{"erc20 transfer to hot", mk(&contract, 0, good), erc20, true},
		{"contract creation", mk(nil, 5, []byte{0x60}), native, false},
		{"native to someone else", mk(&other, 5, nil), native, false},
		{"native with calldata", mk(&e.hot, 5, []byte{1, 2, 3}), native, false},
		{"native of zero value", mk(&e.hot, 0, nil), native, false},
		{"erc20 approve", mk(&contract, 0, approve), erc20, false},
		{"erc20 transfer to someone else", mk(&contract, 0, toOther), erc20, false},
		{"erc20 on an unregistered contract", mk(&other, 0, good), erc20, false},
		{"erc20 that also sends value", mk(&contract, 1, good), erc20, false},
		{"erc20 with trailing calldata", mk(&contract, 0, append(append([]byte(nil), good...), 0)), erc20, false},
	}
	for _, c := range cases {
		err := e.signer.checkShape(c.tx, c.asset)
		if c.ok != (err == nil) {
			t.Errorf("%s: ok=%v err=%v", c.name, c.ok, err)
		}
		if !c.ok && !errors.Is(err, ErrNotASweep) {
			t.Errorf("%s: want ErrNotASweep, got %v", c.name, err)
		}
	}
}

// An ERC-20 Sweep end to end (no EVM contract: a fake chain): the signed tx is transfer(hot, balance) on the registered
// token contract, from alice's key.
func TestSweep_ERC20SignsATransferToHot(t *testing.T) {
	e := newEnv(t)
	e.signer.assets[tokenID] = assets.Asset{Type: assets.ERC20, Contract: common.HexToAddress(token)}
	e.signer.chain = fakeChain{tip: big.NewInt(2e9), base: big.NewInt(10e9), balance: big.NewInt(0)}
	r := e.req()
	r.AssetID, r.Amount = tokenID, "1500"
	resp, err := e.signer.Sign(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	tx := decode(t, resp)
	if *tx.To() != common.HexToAddress(token) || tx.Value().Sign() != 0 || !bytes.Equal(tx.Data(), erc20TransferData(e.hot, big.NewInt(1500))) {
		t.Fatalf("not transfer(hot, 1500) on the token contract: to=%s data=%x", tx.To(), tx.Data())
	}
	if from, _ := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx); from != e.userAdr {
		t.Fatalf("signed by %s, want %s", from, e.userAdr)
	}
}

// The signer asks the Seed provider once per Sweep and never for a refused request: no unseal on any refusal path that
// precedes signing.
func TestSweep_SeedIsOnlyOpenedToSign(t *testing.T) {
	e := newEnv(t)
	r := e.req()
	r.ToAddress = e.userAdr.Hex()
	_, _ = e.signer.Sign(context.Background(), r)
	r = e.req()
	r.UserID = "nobody"
	_, _ = e.signer.Sign(context.Background(), r)
	if n := e.seeds.Unseals(); n != 0 {
		t.Fatalf("the Seed was opened %d times for refused requests", n)
	}
	if _, err := e.signer.Sign(context.Background(), e.req()); err != nil {
		t.Fatal(err)
	}
	if n := e.seeds.Unseals(); n != 1 {
		t.Fatalf("the Seed was opened %d times for one Sweep, want 1", n)
	}
}
