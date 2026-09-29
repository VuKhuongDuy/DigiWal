package signing

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
	"github.com/ethereum/go-ethereum/params"

	"digiwal/services/wallet-service/internal/hdwallet"
	"digiwal/services/wallet-service/internal/storage"
	"digiwal/services/wallet-service/internal/testdb"
)

// The user_derivation and hot_wallet seeds are different mnemonics: a user
// address's key must never be the Hot Wallet's.
const userMnemonic = "legal winner thank year wave sausage worth useful legal winner thank yellow"

type sweepEnv struct {
	sweep   *UserSigner
	users   *hdwallet.Wallet
	hot     common.Address
	userAdr common.Address
	backend *simulated.Backend
	chain   simChain
}

func newSweepEnv(t *testing.T) sweepEnv {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Connect(ctx, testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.InitSeed(ctx, storage.RoleUserDerivation, []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	users, _ := hdwallet.FromMnemonic(userMnemonic)
	hotW, _ := hdwallet.FromMnemonic(testMnemonic)
	hotHex, _ := hotW.AddressAt(hdwallet.HotWalletIndex)

	userHex, err := store.CreateAddress(ctx, "alice", "evm", users.AddressAt)
	if err != nil {
		t.Fatal(err)
	}
	userAddr := common.HexToAddress(userHex)

	backend := simulated.NewBackend(types.GenesisAlloc{userAddr: {Balance: new(big.Int).Mul(big.NewInt(2), big.NewInt(params.Ether))}})
	t.Cleanup(func() { _ = backend.Close() })
	chain := simChain{backend.Client()}
	sweep, err := NewUserSigner(ctx, store, users, chain, Assets{nativeID: {Type: AssetNative}}, common.HexToAddress(hotHex))
	if err != nil {
		t.Fatal(err)
	}
	return sweepEnv{sweep: sweep, users: users, hot: common.HexToAddress(hotHex), userAdr: userAddr, backend: backend, chain: chain}
}

func (e sweepEnv) req() UserRequest {
	id, _ := e.chain.ChainID(context.Background())
	return UserRequest{UserID: "alice", ToAddress: e.hot.Hex(), AssetID: nativeID, Amount: "1000000000000000000", ChainID: id.Int64()}
}

// End to end against a real EVM: the sweep is signed by alice's derived key,
// mined, and lands in the Hot Wallet.
func TestSweep_SignedByTheUsersKeyAndMinedByARealEVM(t *testing.T) {
	e := newSweepEnv(t)
	resp, err := e.sweep.Sign(context.Background(), e.req())
	if err != nil {
		t.Fatal(err)
	}
	tx := decode(t, Response{SignedRawTx: resp.SignedRawTx})
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
	again, err := e.sweep.Sign(context.Background(), func() UserRequest { r := e.req(); r.Amount = "1000"; return r }())
	if err != nil {
		t.Fatal(err)
	}
	if decode(t, Response{SignedRawTx: again.SignedRawTx}).Nonce() != 1 {
		t.Fatal("nonce must be the live pending nonce (1 after one mined sweep)")
	}
}

func TestSweep_DestinationIsNeverTakenFromTheCaller(t *testing.T) {
	e := newSweepEnv(t)
	for _, dest := range []string{dest, e.userAdr.Hex(), "0x0000000000000000000000000000000000000000"} {
		r := e.req()
		r.ToAddress = dest
		if _, err := e.sweep.Sign(context.Background(), r); !errors.Is(err, ErrDestinationNotHotWallet) {
			t.Errorf("to=%s: want ErrDestinationNotHotWallet, got %v", dest, err)
		}
	}
	// hex case must not matter for the legitimate destination
	r := e.req()
	r.ToAddress = "0x" + upper(e.hot.Hex()[2:])
	if _, err := e.sweep.Sign(context.Background(), r); err != nil {
		t.Errorf("checksum/case variants of the hot wallet address must be accepted: %v", err)
	}
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 32
		}
	}
	return string(b)
}

func TestSweep_Validation(t *testing.T) {
	e := newSweepEnv(t)
	ctx := context.Background()
	mut := func(f func(*UserRequest)) UserRequest { r := e.req(); f(&r); return r }
	cases := []struct {
		name string
		req  UserRequest
		want error
	}{
		{"unknown user", mut(func(r *UserRequest) { r.UserID = "nobody" }), ErrUnknownUser},
		{"no user", mut(func(r *UserRequest) { r.UserID = "" }), ErrInvalidRequest},
		{"zero amount", mut(func(r *UserRequest) { r.Amount = "0" }), ErrInvalidRequest},
		{"bad address", mut(func(r *UserRequest) { r.ToAddress = "nope" }), ErrInvalidRequest},
		{"other chain", mut(func(r *UserRequest) { r.ChainID = 1 }), ErrUnsupportedChain},
		{"unknown asset", mut(func(r *UserRequest) { r.AssetID = tokenID }), ErrUnknownAsset},
	}
	for _, tc := range cases {
		if _, err := e.sweep.Sign(ctx, tc.req); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
}

// "max": sweep everything except the tx's own gas. The EVM only accepts a tx whose
// value + gas*maxFee fits the balance, so mining it proves the reservation is right.
func TestSweep_MaxReservesGasAndIsMinedByARealEVM(t *testing.T) {
	e := newSweepEnv(t)
	ctx := context.Background()
	r := e.req()
	r.Amount = AmountMax
	resp, err := e.sweep.Sign(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	tx := decode(t, Response{SignedRawTx: resp.SignedRawTx})
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
	e := newSweepEnv(t)
	ctx := context.Background()
	// "max" is a native-only concept
	erc := e.req()
	erc.Amount, erc.AssetID = AmountMax, tokenID
	e.sweep.assets[tokenID] = Asset{Type: AssetERC20, Contract: common.HexToAddress(token)}
	if _, err := e.sweep.Sign(ctx, erc); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("max on erc20: got %v, want ErrInvalidRequest", err)
	}
	// nothing left after gas: refuse instead of signing a tx the chain would reject
	poor := simChain{poorBackend(t, e)}
	e.sweep.chain = poor
	r := e.req()
	r.Amount = AmountMax
	if _, err := e.sweep.Sign(ctx, r); !errors.Is(err, ErrBalanceBelowGas) {
		t.Errorf("dust balance: got %v, want ErrBalanceBelowGas", err)
	}
}

// poorBackend is a chain where alice's address holds less than one transfer's gas.
func poorBackend(t *testing.T, e sweepEnv) simulated.Client {
	t.Helper()
	b := simulated.NewBackend(types.GenesisAlloc{e.userAdr: {Balance: big.NewInt(1000)}})
	t.Cleanup(func() { _ = b.Close() })
	return b.Client()
}
