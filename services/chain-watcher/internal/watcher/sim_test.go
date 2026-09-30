package watcher

import (
	"context"
	"math/big"
	"testing"

	"digiwal/pkg/events"
	"digiwal/services/chain-watcher/internal/chain"
	"digiwal/services/chain-watcher/internal/registry"
	"digiwal/services/chain-watcher/internal/store"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
)

// emitterRuntime is a hand-assembled contract that, when called with (to, amount) as two
// 32-byte words, emits ERC-20's Transfer(caller, to, amount) — a real log from a real EVM,
// without needing a compiler. //
//
//	PUSH1 0x20 CALLDATALOAD PUSH1 0 MSTORE      mem[0:32] = amount
//	PUSH1 0 CALLDATALOAD CALLER PUSH32 topic0   topics: to, from, signature
//	PUSH1 0x20 PUSH1 0 LOG3 STOP
func emitterRuntime() []byte {
	code := []byte{0x60, 0x20, 0x35, 0x60, 0x00, 0x52, 0x60, 0x00, 0x35, 0x33, 0x7f}
	code = append(code, transferTopic.Bytes()...)
	return append(code, 0x60, 0x20, 0x60, 0x00, 0xa3, 0x00)
}

// initcode copies the runtime into memory and returns it.
func emitterInit() []byte {
	rt := emitterRuntime()
	init := []byte{0x60, byte(len(rt)), 0x80, 0x60, 0x0b, 0x60, 0x00, 0x39, 0x60, 0x00, 0xf3}
	return append(init, rt...)
}

func TestAgainstARealEVM(t *testing.T) {
	ctx := context.Background()
	key, _ := crypto.GenerateKey()
	from := crypto.PubkeyToAddress(key.PublicKey)
	backend := simulated.NewBackend(types.GenesisAlloc{from: {Balance: new(big.Int).Mul(big.NewInt(1000), oneEther)}})
	defer backend.Close()
	cl := backend.Client()
	id, _ := cl.ChainID(ctx)
	signer := types.LatestSignerForChainID(id)
	nonce := uint64(0)
	send := func(to *common.Address, value *big.Int, data []byte, gas uint64) common.Hash {
		t.Helper()
		tx := types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
			ChainID: id, Nonce: nonce, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(50e9), Gas: gas, To: to, Value: value, Data: data,
		})
		nonce++
		if err := cl.SendTransaction(ctx, tx); err != nil {
			t.Fatal(err)
		}
		return tx.Hash()
	}

	// Deploy the emitter (block 1), then use it.
	deployTx := send(nil, big.NewInt(0), emitterInit(), 200_000)
	backend.Commit()
	rcpt, err := cl.TransactionReceipt(ctx, deployTx)
	if err != nil || rcpt.ContractAddress == (common.Address{}) {
		t.Fatalf("deploy failed: %v %+v", err, rcpt)
	}
	tokenAddr := rcpt.ContractAddress

	eth, err := chain.NewEth(ctx, cl)
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t, nil)
	w := r.newWatcher(zero())
	w.Chain = eth
	w.Config.ConfirmationDepth = 2
	w.LoadRegistry = func() (*registry.Registry, error) {
		return &registry.Registry{Native: nativeAsset, ERC20: map[common.Address]string{tokenAddr: tokenAsset}}, nil
	}
	r.w = w

	word := func(a common.Address) []byte { return common.LeftPadBytes(a.Bytes(), 32) }
	call := func(to common.Address, amount *big.Int) []byte {
		return append(word(to), common.LeftPadBytes(amount.Bytes(), 32)...)
	}

	tokenAmt := new(big.Int).Mul(oneEther, big.NewInt(5))
	nativeTx := send(&alice, bigAmount, nil, 21_000)                       // native deposit to alice
	strangerTx := send(&stranger, oneEther, nil, 21_000)                   // not ours
	tokTx := send(&tokenAddr, big.NewInt(0), call(bob, tokenAmt), 100_000) // ERC-20 Transfer log to bob
	backend.Commit()
	backend.Commit()
	backend.Commit() // head deep enough

	if err := w.Step(ctx); err != nil {
		t.Fatal(err)
	}
	got := func(h common.Hash, li int64) *store.Transfer {
		tr, err := r.st.Get(ctx, id.Int64(), lowerHash(h), li)
		if err != nil {
			t.Fatal(err)
		}
		return tr
	}
	if tr := got(nativeTx, -1); tr == nil || tr.Status != store.StatusConfirmed || tr.UserID != "alice" || tr.Amount.Cmp(bigAmount) != 0 || tr.From != from.Hex() {
		t.Fatalf("native deposit: %+v (from %s)", tr, from)
	}
	if got(strangerTx, -1) != nil {
		t.Fatal("recorded a transfer to a stranger")
	}
	det := r.pub.on(events.TopicDepositsConfirmed)
	var tokEv *events.DepositConfirmed
	for _, s := range det {
		ev := decodeConfirmed(t, s)
		if ev.TxHash == lowerHash(tokTx) {
			tokEv = &ev
		}
	}
	if tokEv == nil {
		t.Fatalf("no confirmed event for the ERC-20 transfer among %d events", len(det))
	}
	if tokEv.UserID != "bob" || tokEv.Amount.Int().Cmp(tokenAmt) != 0 || tokEv.AssetID != tokenAsset ||
		tokEv.FromAddress != from.Hex() || tokEv.ToAddress != bob.Hex() || tokEv.LogIndex < 0 {
		t.Fatalf("ERC-20 event = %+v", tokEv)
	}
}
