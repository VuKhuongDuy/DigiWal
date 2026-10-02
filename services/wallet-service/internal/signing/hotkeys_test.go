package signing

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
	"github.com/ethereum/go-ethereum/params"

	"digiwal/services/wallet-service/internal/hdwallet"
	"digiwal/services/wallet-service/internal/storage"
	"digiwal/services/wallet-service/internal/testdb"
)

const wdCN, sweeperCN = "withdrawal-service", "sweeper"

func txOf(t *testing.T, raw string) *types.Transaction {
	t.Helper()
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(common.FromHex(raw)); err != nil {
		t.Fatal(err)
	}
	return tx
}

// invariant #21: same nonce, same destination/amount/data, fees at least 12.5% up on both fields.
func TestReplaceIsTheSameTxWithHigherFees(t *testing.T) {
	s, chain, _ := setup(t)
	ctx := context.Background()
	orig, err := s.Sign(ctx, wdCN, nativeReq("withdrawal:a"))
	if err != nil {
		t.Fatal(err)
	}
	o := txOf(t, orig.SignedRawTx)

	r1, err := s.Replace(ctx, wdCN, "withdrawal:a", ReplaceRequest{ReplacementSeq: 1})
	if err != nil {
		t.Fatal(err)
	}
	n1 := txOf(t, r1.SignedRawTx)
	if n1.Nonce() != o.Nonce() || r1.Nonce != orig.Nonce || *n1.To() != *o.To() || n1.Value().Cmp(o.Value()) != 0 || string(n1.Data()) != string(o.Data()) || n1.Gas() != o.Gas() {
		t.Fatalf("replacement is not the same tx: %+v vs %+v", n1, o)
	}
	if r1.TxHash != strings.ToLower(n1.Hash().Hex()) || r1.TxHash == strings.ToLower(o.Hash().Hex()) || r1.ReplacementSeq != 1 {
		t.Fatalf("response = %+v", r1)
	}
	// 12.5% on both fields, rounded up (the network fee has not moved).
	wantCap := new(big.Int).Div(new(big.Int).Add(new(big.Int).Mul(o.GasFeeCap(), big.NewInt(1125)), big.NewInt(999)), big.NewInt(1000))
	wantTip := new(big.Int).Div(new(big.Int).Add(new(big.Int).Mul(o.GasTipCap(), big.NewInt(1125)), big.NewInt(999)), big.NewInt(1000))
	if n1.GasFeeCap().Cmp(wantCap) != 0 || n1.GasTipCap().Cmp(wantTip) != 0 {
		t.Fatalf("fees %s/%s, want %s/%s", n1.GasFeeCap(), n1.GasTipCap(), wantCap, wantTip)
	}
	if r1.MaxFeePerGas != n1.GasFeeCap().String() || r1.MaxPriorityFeePerGas != n1.GasTipCap().String() {
		t.Fatalf("response fees %+v", r1)
	}

	// The network got dearer than a bump: the replacement follows the network.
	chain.base = big.NewInt(100e9)
	chain.tip = big.NewInt(5e9)
	s.WithFeeCeiling(big.NewInt(1000e9))
	r2, err := s.Replace(ctx, wdCN, "withdrawal:a", ReplaceRequest{ReplacementSeq: 2})
	if err != nil {
		t.Fatal(err)
	}
	if n2 := txOf(t, r2.SignedRawTx); n2.GasFeeCap().Cmp(big.NewInt(205e9)) != 0 || n2.GasTipCap().Cmp(big.NewInt(5e9)) != 0 {
		t.Fatalf("fees %s/%s, want the network's 205/5 gwei", n2.GasFeeCap(), n2.GasTipCap())
	}

	// Allocated no new nonce.
	if next, _ := s.Sign(ctx, wdCN, nativeReq("withdrawal:b")); next.Nonce != orig.Nonce+1 {
		t.Fatalf("a replacement consumed a nonce: next %d", next.Nonce)
	}
}

func TestReplaceKeepsAnERC20TransferIntact(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	req := Request{ToAddress: dest, AssetID: tokenID, Amount: "123456", ChainID: chainID, IdempotencyKey: "withdrawal:t"}
	orig, err := s.Sign(ctx, wdCN, req)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Replace(ctx, wdCN, "withdrawal:t", ReplaceRequest{ReplacementSeq: 1})
	if err != nil {
		t.Fatal(err)
	}
	o, n := txOf(t, orig.SignedRawTx), txOf(t, r.SignedRawTx)
	if *n.To() != common.HexToAddress(token) || n.Value().Sign() != 0 || string(n.Data()) != string(o.Data()) {
		t.Fatal("the token transfer changed")
	}
}

func TestReplaceRefusesAboveTheFeeCeilingAndSignsNothing(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	if _, err := s.Sign(ctx, wdCN, nativeReq("withdrawal:a")); err != nil {
		t.Fatal(err)
	}
	// Original cap is 2*10+2 = 22 gwei; a bump is 24.75. The hard ceiling:
	s.WithFeeCeiling(big.NewInt(24e9))
	if _, err := s.Replace(ctx, wdCN, "withdrawal:a", ReplaceRequest{ReplacementSeq: 1}); !errors.Is(err, ErrGasCeilingExceeded) {
		t.Fatalf("above the hard ceiling: %v", err)
	}
	// The caller's own, tighter, cap.
	s.WithFeeCeiling(DefaultFeeCeiling)
	if _, err := s.Replace(ctx, wdCN, "withdrawal:a", ReplaceRequest{ReplacementSeq: 1, MaxGasPrice: "24000000000"}); !errors.Is(err, ErrGasCeilingExceeded) {
		t.Fatalf("above max_gas_price: %v", err)
	}
	k, _ := s.Lookup(ctx, wdCN, "withdrawal:a")
	if len(k.Replacements) != 0 {
		t.Fatal("a refused replacement was stored")
	}
	// The same seq now succeeds: nothing was consumed by the refusals.
	if _, err := s.Replace(ctx, wdCN, "withdrawal:a", ReplaceRequest{ReplacementSeq: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceValidation(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	if _, err := s.Sign(ctx, wdCN, nativeReq("withdrawal:a")); err != nil {
		t.Fatal(err)
	}
	for _, r := range []ReplaceRequest{{ReplacementSeq: 0}, {ReplacementSeq: -1}, {ReplacementSeq: 1, MaxGasPrice: "-5"}, {ReplacementSeq: 1, MaxGasPrice: "0"}} {
		if _, err := s.Replace(ctx, wdCN, "withdrawal:a", r); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%+v: %v, want ErrInvalidRequest", r, err)
		}
	}
	if _, err := s.Replace(ctx, wdCN, "withdrawal:zzz", ReplaceRequest{ReplacementSeq: 1}); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("unsigned key: %v", err)
	}
	if _, err := s.Replace(ctx, sweeperCN, "withdrawal:a", ReplaceRequest{ReplacementSeq: 1}); !errors.Is(err, storage.ErrNotCallers) {
		t.Fatalf("another caller: %v", err)
	}
}

func TestLookupShowsTheSignatureAndEveryReplacement(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	orig, _ := s.Sign(ctx, wdCN, nativeReq("withdrawal:a"))
	r1, _ := s.Replace(ctx, wdCN, "withdrawal:a", ReplaceRequest{ReplacementSeq: 1})
	v, err := s.Lookup(ctx, wdCN, "withdrawal:a")
	if err != nil || v.SignedRawTx != orig.SignedRawTx || v.Nonce != orig.Nonce || len(v.Replacements) != 1 ||
		v.Replacements[0].TxHash != r1.TxHash || v.Replacements[0].Kind != storage.KindFeeBump ||
		v.TxHash != strings.ToLower(txOf(t, orig.SignedRawTx).Hash().Hex()) {
		t.Fatalf("%+v %v", v, err)
	}
	if _, err := s.Lookup(ctx, sweeperCN, "withdrawal:a"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("another caller's lookup: %v", err)
	}
}

func TestVoidThroughTheSigner(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	v, err := s.Void(ctx, wdCN, "withdrawal:never")
	if err != nil || v.Status != "voided" {
		t.Fatalf("%+v %v", v, err)
	}
	if _, err := s.Sign(ctx, wdCN, nativeReq("withdrawal:never")); !errors.Is(err, ErrKeyVoided) {
		t.Fatalf("sign after void: %v", err)
	}
	orig, _ := s.Sign(ctx, wdCN, nativeReq("withdrawal:signed"))
	v, err = s.Void(ctx, wdCN, "withdrawal:signed")
	if err != nil || v.Status != "already_signed" || v.SignedRawTx != orig.SignedRawTx || v.Nonce != orig.Nonce || v.TxHash == "" {
		t.Fatalf("%+v %v", v, err)
	}
}

// invariant #23: the Nonce Filler is a 0-value Hot -> Hot self-transfer at the key's nonce.
func TestCancelBuildsAZeroValueSelfTransferAtTheSameNonce(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	orig, _ := s.Sign(ctx, wdCN, nativeReq("withdrawal:a"))
	c, err := s.Cancel(ctx, "withdrawal:a")
	if err != nil {
		t.Fatal(err)
	}
	tx := txOf(t, hexTx(c.SignedRawTx))
	if c.Kind != storage.KindCancel || tx.Nonce() != orig.Nonce || *tx.To() != s.HotWalletAddress() || tx.Value().Sign() != 0 || len(tx.Data()) != 0 || tx.Gas() != 21000 {
		t.Fatalf("filler = %+v (to %v value %s)", c, tx.To(), tx.Value())
	}
	if o := txOf(t, orig.SignedRawTx); tx.GasFeeCap().Cmp(o.GasFeeCap()) <= 0 || tx.GasTipCap().Cmp(o.GasTipCap()) <= 0 {
		t.Fatal("the filler does not outbid the original")
	}
	again, err := s.Cancel(ctx, "withdrawal:a")
	if err != nil || again.TxHash != c.TxHash {
		t.Fatalf("running the command twice made a second filler: %+v %v", again, err)
	}
	// A Fee Bump cannot undo it.
	if _, err := s.Replace(ctx, wdCN, "withdrawal:a", ReplaceRequest{ReplacementSeq: 2}); !errors.Is(err, storage.ErrKeyCancelled) {
		t.Fatalf("bump after cancel: %v", err)
	}
	if _, err := s.Cancel(ctx, "withdrawal:unknown"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cancel of an unsigned key: %v", err)
	}
}

// A real EVM must accept the replacement at the same nonce (its 10% rule) and mine only that one.
func TestReplacementsAndFillerAreAcceptedByARealEVM(t *testing.T) {
	hot, _ := hdwallet.FromMnemonic(testMnemonic)
	hotAddr, _ := hot.AddressAt(hdwallet.HotWalletIndex)
	from := common.HexToAddress(hotAddr)
	backend := simulated.NewBackend(types.GenesisAlloc{from: {Balance: new(big.Int).Mul(big.NewInt(10), big.NewInt(params.Ether))}})
	t.Cleanup(func() { _ = backend.Close() })
	client := backend.Client()
	id, _ := client.ChainID(context.Background())
	store, err := storage.Connect(context.Background(), testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	s, err := New(context.Background(), store, hot, simChain{client}, Assets{nativeID: {Type: AssetNative}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	req := Request{ToAddress: dest, AssetID: nativeID, Amount: "1000000000000000", ChainID: id.Int64(), IdempotencyKey: "withdrawal:a"}
	orig, err := s.Sign(ctx, wdCN, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SendTransaction(ctx, decode(t, orig)); err != nil {
		t.Fatal(err)
	}
	bump, err := s.Replace(ctx, wdCN, "withdrawal:a", ReplaceRequest{ReplacementSeq: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SendTransaction(ctx, txOf(t, bump.SignedRawTx)); err != nil {
		t.Fatalf("the EVM rejected the Fee Bump: %v", err)
	}
	filler, err := s.Cancel(ctx, "withdrawal:a")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SendTransaction(ctx, txOf(t, hexTx(filler.SignedRawTx))); err != nil {
		t.Fatalf("the EVM rejected the Nonce Filler: %v", err)
	}
	backend.Commit()

	if n, _ := client.NonceAt(ctx, from, nil); n != 1 {
		t.Fatalf("on-chain nonce %d, want 1 (one tx mined for nonce 0)", n)
	}
	if bal, _ := client.BalanceAt(ctx, common.HexToAddress(dest), nil); bal.Sign() != 0 {
		t.Fatalf("the withdrawal still paid out %s: the filler did not win nonce 0", bal)
	}
	if r, _ := client.TransactionReceipt(ctx, common.HexToHash(filler.TxHash)); r == nil {
		t.Fatal("the filler was not mined")
	}
}
