package signing

import (
	"context"
	"errors"
	"math/big"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"digiwal/services/wallet-service/internal/hdwallet"
	"digiwal/services/wallet-service/internal/storage"
	"digiwal/services/wallet-service/internal/testdb"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testdb.Terminate()
	os.Exit(code)
}

const (
	chainID  = 31337
	nativeID = "11111111-1111-1111-1111-111111111111"
	tokenID  = "22222222-2222-2222-2222-222222222222"
	token    = "0x5FbDB2315678afecb367f032d93F642f64180aa3"
	dest     = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
	// a well-known public test mnemonic — never used for anything real
	testMnemonic = "test test test test test test test test test test test junk"
)

type fakeChain struct {
	id                     int64
	pending                uint64
	tip, base              *big.Int
	gas                    uint64
	pendingCalls, feeCalls atomic.Int32
}

func newFakeChain() *fakeChain {
	return &fakeChain{id: chainID, pending: 4, tip: big.NewInt(2e9), base: big.NewInt(10e9), gas: 21000}
}
func (f *fakeChain) ChainID(context.Context) (*big.Int, error) { return big.NewInt(f.id), nil }
func (f *fakeChain) PendingNonceAt(context.Context, common.Address) (uint64, error) {
	f.pendingCalls.Add(1)
	return f.pending, nil
}
func (f *fakeChain) BalanceAt(context.Context, common.Address) (*big.Int, error) {
	return big.NewInt(0), nil
}
func (f *fakeChain) FeeQuote(context.Context) (*big.Int, *big.Int, error) {
	f.feeCalls.Add(1)
	return f.tip, f.base, nil
}
func (f *fakeChain) EstimateGas(context.Context, common.Address, common.Address, *big.Int, []byte) (uint64, error) {
	return f.gas, nil
}

func setup(t *testing.T) (*Signer, *fakeChain, *hdwallet.Wallet) {
	t.Helper()
	store, err := storage.Connect(context.Background(), testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	hot, err := hdwallet.FromMnemonic(testMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	chain := newFakeChain()
	assets := Assets{
		nativeID: {Type: AssetNative},
		tokenID:  {Type: AssetERC20, Contract: common.HexToAddress(token)},
	}
	s, err := New(context.Background(), store, hot, chain, assets)
	if err != nil {
		t.Fatal(err)
	}
	return s, chain, hot
}

func nativeReq(key string) Request {
	return Request{ToAddress: dest, AssetID: nativeID, Amount: "1000000000000000", ChainID: chainID, IdempotencyKey: key}
}

func decode(t *testing.T, resp Response) *types.Transaction {
	t.Helper()
	raw := common.FromHex(resp.SignedRawTx)
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(raw); err != nil {
		t.Fatalf("response is not a valid raw tx: %v", err)
	}
	return tx
}

// The signature must recover to the configured Hot Wallet address — proves the
// endpoint signs with the hot key and nothing else.
func TestSign_NativeTransfer_SignatureRecoversToHotWallet(t *testing.T) {
	s, _, hot := setup(t)
	resp, err := s.Sign(context.Background(), nativeReq("w1"))
	if err != nil {
		t.Fatal(err)
	}
	tx := decode(t, resp)

	sender, err := types.Sender(types.LatestSignerForChainID(big.NewInt(chainID)), tx)
	if err != nil {
		t.Fatal(err)
	}
	wantAddr, _ := hot.AddressAt(hdwallet.HotWalletIndex)
	if sender != common.HexToAddress(wantAddr) {
		t.Fatalf("recovered %s, want hot wallet %s", sender, wantAddr)
	}
	if resp.Nonce != 4 || tx.Nonce() != 4 {
		t.Fatalf("nonce resp=%d tx=%d, want 4 (seeded from RPC pending nonce)", resp.Nonce, tx.Nonce())
	}
	if tx.To().Hex() != common.HexToAddress(dest).Hex() || tx.Value().String() != "1000000000000000" || len(tx.Data()) != 0 {
		t.Fatalf("wrong tx body: to=%s value=%s data=%x", tx.To(), tx.Value(), tx.Data())
	}
	if tx.ChainId().Int64() != chainID || tx.Gas() != 21000 {
		t.Fatalf("chain=%v gas=%d", tx.ChainId(), tx.Gas())
	}
	if tx.GasTipCap().Int64() != 2e9 || tx.GasFeeCap().Int64() != 22e9 { // 2*base + tip
		t.Fatalf("fees tip=%v cap=%v", tx.GasTipCap(), tx.GasFeeCap())
	}
}

func TestSign_ERC20_EncodesTransferToTheTokenContract(t *testing.T) {
	s, _, _ := setup(t)
	req := nativeReq("w-erc20")
	req.AssetID, req.Amount = tokenID, "5000000"
	resp, err := s.Sign(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	tx := decode(t, resp)
	if tx.To().Hex() != common.HexToAddress(token).Hex() || tx.Value().Sign() != 0 {
		t.Fatalf("erc20 transfer must call the token contract with zero value: to=%s value=%s", tx.To(), tx.Value())
	}
	data := tx.Data()
	if len(data) != 68 || common.Bytes2Hex(data[:4]) != "a9059cbb" {
		t.Fatalf("bad calldata %x", data)
	}
	if common.BytesToAddress(data[4:36]) != common.HexToAddress(dest) || new(big.Int).SetBytes(data[36:]).String() != "5000000" {
		t.Fatalf("calldata recipient/amount wrong: %x", data)
	}
	if tx.Gas() != 21000*12/10 {
		t.Fatalf("erc20 gas %d should carry a 20%% buffer", tx.Gas())
	}
}

func TestSign_RetryReplaysIdenticalResultWithoutNewNonceOrRPC(t *testing.T) {
	s, chain, _ := setup(t)
	ctx := context.Background()
	first, err := s.Sign(ctx, nativeReq("dup"))
	if err != nil {
		t.Fatal(err)
	}
	feeCalls := chain.feeCalls.Load()
	again, err := s.Sign(ctx, nativeReq("dup"))
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatalf("replay differs:\n%+v\n%+v", first, again)
	}
	if chain.feeCalls.Load() != feeCalls {
		t.Fatal("a replay must not need the network")
	}
	next, _ := s.Sign(ctx, nativeReq("another"))
	if next.Nonce != first.Nonce+1 {
		t.Fatalf("next nonce %d, want %d: the retry must not have consumed one", next.Nonce, first.Nonce+1)
	}
}

func TestSign_SameKeyDifferentRequestIsRejected(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	if _, err := s.Sign(ctx, nativeReq("k")); err != nil {
		t.Fatal(err)
	}
	other := nativeReq("k")
	other.Amount = "2"
	if _, err := s.Sign(ctx, other); !errors.Is(err, storage.ErrIdempotencyKeyReused) {
		t.Fatalf("want ErrIdempotencyKeyReused, got %v", err)
	}
}

func TestSign_MaxGasPriceIsACeilingNotAPrice(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()

	tooLow := nativeReq("ceiling-low")
	tooLow.MaxGasPrice = "21999999999" // needed fee cap is 22 gwei
	if _, err := s.Sign(ctx, tooLow); !errors.Is(err, ErrGasCeilingExceeded) {
		t.Fatalf("want ErrGasCeilingExceeded, got %v", err)
	}

	ok := nativeReq("ceiling-ok")
	ok.MaxGasPrice = "22000000000"
	resp, err := s.Sign(ctx, ok)
	if err != nil {
		t.Fatal(err)
	}
	// the refusal above must not have burned a nonce, and the ceiling never sets the price
	if resp.Nonce != 4 || decode(t, resp).GasFeeCap().Int64() != 22e9 {
		t.Fatalf("nonce=%d feecap=%v", resp.Nonce, decode(t, resp).GasFeeCap())
	}
}

func TestSign_Validation(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	mut := func(f func(*Request)) Request { r := nativeReq("v"); f(&r); return r }
	cases := []struct {
		name string
		req  Request
		want error
	}{
		{"no key", mut(func(r *Request) { r.IdempotencyKey = "" }), ErrInvalidRequest},
		{"key too long", mut(func(r *Request) { r.IdempotencyKey = strings.Repeat("a", 129) }), ErrInvalidRequest},
		{"bad address", mut(func(r *Request) { r.ToAddress = "0x123" }), ErrInvalidRequest},
		{"zero amount", mut(func(r *Request) { r.Amount = "0" }), ErrInvalidRequest},
		{"negative amount", mut(func(r *Request) { r.Amount = "-5" }), ErrInvalidRequest},
		{"non-numeric amount", mut(func(r *Request) { r.Amount = "1e18" }), ErrInvalidRequest},
		{"bad max gas", mut(func(r *Request) { r.MaxGasPrice = "abc" }), ErrInvalidRequest},
		{"other chain", mut(func(r *Request) { r.ChainID = 1 }), ErrUnsupportedChain},
		{"unknown asset", mut(func(r *Request) { r.AssetID = "33333333-3333-3333-3333-333333333333" }), ErrUnknownAsset},
	}
	for _, tc := range cases {
		if _, err := s.Sign(ctx, tc.req); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
}
