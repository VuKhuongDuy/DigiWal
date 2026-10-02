package chain

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
)

// balanceOfRuntime is a hand-assembled contract that answers any call with calldata word 1 —
// the ABI-encoded address argument — as a uint256. So TokenBalance(holder) must come back equal
// to the holder's address as a number, which only happens if the selector and argument are
// encoded and decoded right against a real EVM.
//
//	PUSH1 4 CALLDATALOAD PUSH1 0 MSTORE PUSH1 0x20 PUSH1 0 RETURN
func deployCode() []byte {
	rt := []byte{0x60, 0x04, 0x35, 0x60, 0x00, 0x52, 0x60, 0x20, 0x60, 0x00, 0xf3}
	init := []byte{0x60, byte(len(rt)), 0x80, 0x60, 0x0b, 0x60, 0x00, 0x39, 0x60, 0x00, 0xf3}
	return append(init, rt...)
}

func TestEthAgainstARealEVM(t *testing.T) {
	ctx := context.Background()
	key, _ := crypto.GenerateKey()
	from := crypto.PubkeyToAddress(key.PublicKey)
	ether := new(big.Int).Mul(big.NewInt(1e9), big.NewInt(1e9))
	backend := simulated.NewBackend(types.GenesisAlloc{from: {Balance: new(big.Int).Mul(big.NewInt(100), ether)}})
	defer backend.Close()
	cl := backend.Client()
	id, _ := cl.ChainID(ctx)
	signer := types.LatestSignerForChainID(id)
	e := NewEth(cl)
	backend.Commit() // let the node finish indexing: before that it answers receipt queries with an error, not "not found"

	if got, err := e.ChainID(ctx); err != nil || got != id.Int64() {
		t.Fatalf("ChainID = %d %v", got, err)
	}

	sign := func(nonce uint64, to *common.Address, value *big.Int, data []byte, gas uint64) []byte {
		tx := types.MustSignNewTx(key, signer, &types.DynamicFeeTx{ChainID: id, Nonce: nonce, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(50e9), Gas: gas, To: to, Value: value, Data: data})
		raw, _ := tx.MarshalBinary()
		return raw
	}

	// Nonces before anything is sent.
	if p, _ := e.PendingNonce(ctx, from); p != 0 {
		t.Fatalf("pending nonce = %d", p)
	}

	// Deploy the balanceOf contract.
	deployHash, err := e.Broadcast(ctx, sign(0, nil, big.NewInt(0), deployCode(), 200_000))
	if err != nil {
		t.Fatal(err)
	}
	// In the mempool, not mined: no receipt, and pending nonce is ahead of latest.
	if r, err := e.Receipt(ctx, deployHash); err != nil || r != nil {
		t.Fatalf("receipt before mining = %+v %v", r, err)
	}
	p, _ := e.PendingNonce(ctx, from)
	l, _ := e.LatestNonce(ctx, from)
	if p != 1 || l != 0 {
		t.Fatalf("pending %d latest %d: an in-flight tx must show as pending > latest (waitNoPending relies on it)", p, l)
	}
	// Broadcasting the identical signed bytes again is harmless.
	if h2, err := e.Broadcast(ctx, sign(0, nil, big.NewInt(0), deployCode(), 200_000)); err != nil || h2 == (common.Hash{}) {
		// a re-signed identical tx has the same hash (deterministic ECDSA), so the node says "already known"
		t.Fatalf("re-broadcast of a known tx = %v %v", h2, err)
	}
	backend.Commit()
	r, err := e.Receipt(ctx, deployHash)
	if err != nil || r == nil || !r.Success {
		t.Fatalf("receipt after mining = %+v %v", r, err)
	}
	p, _ = e.PendingNonce(ctx, from)
	l, _ = e.LatestNonce(ctx, from)
	if p != 1 || l != 1 {
		t.Fatalf("after mining pending %d latest %d", p, l)
	}
	rc, _ := cl.TransactionReceipt(ctx, deployHash)
	token := rc.ContractAddress

	// Token balance: the contract echoes the holder address back as a number.
	holder := common.HexToAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")
	got, err := e.TokenBalance(ctx, token, holder)
	if err != nil || got.Cmp(new(big.Int).SetBytes(holder.Bytes())) != 0 {
		t.Fatalf("TokenBalance = %v %v", got, err)
	}
	// An address with no code is not an ERC-20: an error, never a zero balance.
	if _, err := e.TokenBalance(ctx, common.HexToAddress("0x000000000000000000000000000000000000dEaD"), holder); err == nil {
		t.Fatal("balanceOf on a non-contract was accepted as a balance")
	}

	// Native balance moves with a real transfer.
	to := common.HexToAddress("0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC")
	if _, err := e.Broadcast(ctx, sign(1, &to, ether, nil, 21_000)); err != nil {
		t.Fatal(err)
	}
	backend.Commit()
	if b, err := e.BalanceAt(ctx, to); err != nil || b.Cmp(ether) != 0 {
		t.Fatalf("BalanceAt = %v %v", b, err)
	}

	// Head grows with each block; the receipt carries what the tx cost (the Gas Fee's two numbers).
	h0, err := e.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	backend.Commit()
	if h1, _ := e.Head(ctx); h1 != h0+1 {
		t.Fatalf("head %d then %d", h0, h1)
	}
	if r, _ := e.Receipt(ctx, deployHash); r.GasUsed == 0 || r.EffectiveGasPrice == nil || r.EffectiveGasPrice.Sign() <= 0 {
		t.Fatalf("receipt carries no gas: %+v", r)
	}

	// Underpriced: a tx whose max fee is below the latest base fee cannot be mined at that price.
	hdr, _ := cl.HeaderByNumber(ctx, nil)
	cheap := types.MustSignNewTx(key, signer, &types.DynamicFeeTx{ChainID: id, Nonce: 2, GasTipCap: big.NewInt(0), GasFeeCap: new(big.Int).Sub(hdr.BaseFee, big.NewInt(1)), Gas: 21_000, To: &to, Value: big.NewInt(1)})
	cheapRaw, _ := cheap.MarshalBinary()
	if under, err := e.Underpriced(ctx, cheapRaw); err != nil || !under {
		t.Fatalf("a tx paying below the base fee: underpriced=%v %v", under, err)
	}
	if under, err := e.Underpriced(ctx, sign(2, &to, big.NewInt(1), nil, 21_000)); err != nil || under {
		t.Fatalf("a tx paying 50 gwei: underpriced=%v %v", under, err)
	}
	if _, err := e.Underpriced(ctx, []byte{1, 2, 3}); err == nil {
		t.Fatal("garbage was judged")
	}

	// Garbage is rejected before it reaches the node.
	if _, err := e.Broadcast(ctx, []byte{1, 2, 3}); err == nil {
		t.Fatal("garbage raw tx accepted")
	}
}
