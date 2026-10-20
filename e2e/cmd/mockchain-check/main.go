// Command mockchain-check is a smoke test of e2e/mockchain with the same go-ethereum client the services use:
//
//	go run ./cmd/mockchain-check [-rpc http://127.0.0.1:8545]
//
// It deploys MockERC20, moves ETH and tokens, and checks the answers the services depend on (header hash chain,
// full blocks with recoverable senders, receipts, logs, eth_call, reverts, replacement rules). Exit code 1 on a failure.
package main

import (
	"context"
	"crypto/ecdsa"
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"digiwal/e2e/contracts"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

var failed bool

func check(ok bool, format string, a ...any) {
	if ok {
		fmt.Printf("  ok    "+format+"\n", a...)
		return
	}
	failed = true
	fmt.Printf("  FAIL  "+format+"\n", a...)
}

func main() {
	url := flag.String("rpc", "http://127.0.0.1:8545", "JSON-RPC URL")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rc, err := rpc.DialContext(ctx, *url)
	must(err)
	c := ethclient.NewClient(rc)
	chainID, err := c.ChainID(ctx)
	must(err)
	check(chainID.Cmp(big.NewInt(31337)) == 0, "chain id %s", chainID)

	key0, _ := crypto.HexToECDSA("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	key1, _ := crypto.HexToECDSA("59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d")
	a0, a1 := crypto.PubkeyToAddress(key0.PublicKey), crypto.PubkeyToAddress(key1.PublicKey)
	signer := types.LatestSignerForChainID(chainID)

	fmt.Println("1. headers")
	head, err := c.BlockNumber(ctx)
	must(err)
	_ = rc.CallContext(ctx, nil, "anvil_mine", "0x3")
	var prev *types.Header
	for n := head; n <= head+3; n++ {
		h, err := c.HeaderByNumber(ctx, new(big.Int).SetUint64(n))
		must(err)
		if prev != nil {
			check(h.ParentHash == prev.Hash(), "block %d's parent hash is block %d's hash as geth computes it", n, n-1)
		}
		prev = h
	}

	fmt.Println("2. funds and a native transfer")
	bal, err := c.BalanceAt(ctx, a0, nil)
	must(err)
	check(bal.Cmp(new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18))) > 0, "account #0 holds %s wei", bal)
	to := common.HexToAddress("0x00000000000000000000000000000000000000a1")
	before, _ := c.BalanceAt(ctx, to, nil)
	rcpt := send(ctx, c, signer, key0, &to, big.NewInt(1e15), nil)
	check(rcpt.Status == 1 && rcpt.GasUsed == 21000, "ETH transfer mined, gas %d", rcpt.GasUsed)
	got, _ := c.BalanceAt(ctx, to, nil)
	check(new(big.Int).Sub(got, before).Cmp(big.NewInt(1e15)) == 0, "recipient got 0.001 ETH")

	fmt.Println("3. MockERC20")
	raw := common.FromHex(strings.TrimSpace(contracts.MockERC20Bin))
	u256, _ := abi.NewType("uint256", "", nil)
	supply := new(big.Int).Mul(big.NewInt(1_000_000_000_000), big.NewInt(1e18))
	arg, _ := abi.Arguments{{Type: u256}}.Pack(supply)
	want := crypto.CreateAddress(a0, mustNonce(ctx, c, a0))
	rcpt = send(ctx, c, signer, key0, nil, big.NewInt(0), append(raw, arg...))
	check(rcpt.Status == 1 && rcpt.ContractAddress == want, "deployed at %s (the CREATE address)", rcpt.ContractAddress)
	token := rcpt.ContractAddress
	code, _ := c.CodeAt(ctx, token, nil)
	check(len(code) > 0, "CodeAt returns code")
	balOf := func(who common.Address) *big.Int {
		out, err := c.CallContract(ctx, ethereum.CallMsg{To: &token, Data: append(common.FromHex("0x70a08231"), common.LeftPadBytes(who.Bytes(), 32)...)}, nil)
		must(err)
		return new(big.Int).SetBytes(out)
	}
	check(balOf(a0).Cmp(supply) == 0, "deployer holds the whole supply (balanceOf via eth_call)")
	xfer := func(to common.Address, amt *big.Int) []byte {
		return append(append(common.FromHex("0xa9059cbb"), common.LeftPadBytes(to.Bytes(), 32)...), common.LeftPadBytes(amt.Bytes(), 32)...)
	}
	rcpt = send(ctx, c, signer, key0, &token, big.NewInt(0), xfer(a1, big.NewInt(5000)))
	check(rcpt.Status == 1 && len(rcpt.Logs) == 1, "token transfer mined with one Transfer log")
	check(balOf(a1).Cmp(big.NewInt(5000)) == 0, "recipient's tokens: 5000")
	logs, err := c.FilterLogs(ctx, ethereum.FilterQuery{FromBlock: big.NewInt(0), Addresses: []common.Address{token},
		Topics: [][]common.Hash{{crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))}, nil, {common.BytesToHash(a1.Bytes())}}})
	must(err)
	check(len(logs) == 1 && logs[0].TxHash == rcpt.TxHash, "FilterLogs by topic finds it (%d log)", len(logs))
	est, err := c.EstimateGas(ctx, ethereum.CallMsg{From: a0, To: &token, Data: xfer(a1, big.NewInt(1))})
	must(err)
	check(est >= 21000, "EstimateGas for a token transfer: %d", est)
	_, err = c.CallContract(ctx, ethereum.CallMsg{From: a1, To: &token, Data: xfer(a0, new(big.Int).Add(supply, big.NewInt(1)))}, nil)
	check(err != nil && strings.Contains(err.Error(), "revert"), "eth_call of an overdraw reverts: %v", err)
	rcpt = send(ctx, c, signer, key1, &token, big.NewInt(0), xfer(a0, big.NewInt(999_999_999)))
	check(rcpt.Status == 0, "an overdraw mined is a failed receipt (status 0)")

	fmt.Println("4. full block")
	blk, err := c.BlockByNumber(ctx, rcpt.BlockNumber)
	must(err)
	okSender := false
	for _, t := range blk.Transactions() {
		if t.Hash() == rcpt.TxHash {
			from, err := types.Sender(signer, t)
			okSender = err == nil && from == a1
		}
	}
	check(okSender, "BlockByNumber decodes and the sender recovers from the signature")
	tx, pending, err := c.TransactionByHash(ctx, rcpt.TxHash)
	check(err == nil && !pending && tx.Hash() == rcpt.TxHash, "TransactionByHash")

	fmt.Println("5. nonces and replacement")
	n, _ := c.NonceAt(ctx, a0, nil)
	tip, _ := c.SuggestGasTipCap(ctx)
	mk := func(nonce uint64, tipc, maxFee *big.Int) *types.Transaction {
		t, _ := types.SignNewTx(key0, signer, &types.DynamicFeeTx{ChainID: chainID, Nonce: nonce, GasTipCap: tipc, GasFeeCap: maxFee, Gas: 21000, To: &to, Value: big.NewInt(1)})
		return t
	}
	fee := big.NewInt(10_000_000_000)
	gap := mk(n+3, tip, fee) // a nonce gap: it must wait in the pool
	check(c.SendTransaction(ctx, gap) == nil, "a transaction with a nonce gap is accepted and waits")
	pn, _ := c.PendingNonceAt(ctx, a0)
	check(pn == n, "PendingNonceAt stays at %d across the gap (got %d)", n, pn)
	same := mk(n+3, tip, fee)
	same2, _ := types.SignNewTx(key0, signer, &types.DynamicFeeTx{ChainID: chainID, Nonce: n + 3, GasTipCap: tip, GasFeeCap: fee, Gas: 21000, To: &to, Value: big.NewInt(2)})
	_ = same
	err = c.SendTransaction(ctx, same2)
	check(err != nil && strings.Contains(err.Error(), "underpriced"), "same-fee replacement is refused: %v", err)
	bump := mk(n+3, new(big.Int).Mul(tip, big.NewInt(2)), new(big.Int).Mul(fee, big.NewInt(2)))
	check(c.SendTransaction(ctx, bump) == nil, "a replacement with a fee bump is accepted")
	err = c.SendTransaction(ctx, mk(n-1, tip, fee))
	check(err != nil && strings.Contains(err.Error(), "nonce too low"), "a used nonce is refused: %v", err)
	for i := uint64(0); i < 3; i++ {
		must(c.SendTransaction(ctx, mk(n+i, tip, fee)))
	}
	waitTx(ctx, c, bump.Hash())
	r, err := c.TransactionReceipt(ctx, bump.Hash())
	check(err == nil && r.Status == 1, "once the gap is filled the queued transaction mines")

	fmt.Println("6. anvil helpers")
	must(rc.CallContext(ctx, nil, "anvil_setBalance", common.HexToAddress("0x00000000000000000000000000000000000000b2"), "0x56bc75e2d63100000"))
	b2, _ := c.BalanceAt(ctx, common.HexToAddress("0x00000000000000000000000000000000000000b2"), nil)
	check(b2.Cmp(big.NewInt(0).Mul(big.NewInt(100), big.NewInt(1e18))) == 0, "anvil_setBalance")

	if failed {
		fmt.Println("RESULT: FAILED")
		os.Exit(1)
	}
	fmt.Println("RESULT: all checks passed")
}

func must(err error) {
	if err != nil {
		fmt.Println("fatal:", err)
		os.Exit(1)
	}
}

func mustNonce(ctx context.Context, c *ethclient.Client, a common.Address) uint64 {
	n, err := c.PendingNonceAt(ctx, a)
	must(err)
	return n
}

func send(ctx context.Context, c *ethclient.Client, s types.Signer, k *ecdsa.PrivateKey, to *common.Address, value *big.Int, data []byte) *types.Receipt {
	from := crypto.PubkeyToAddress(k.PublicKey)
	tip, err := c.SuggestGasTipCap(ctx)
	must(err)
	gas := uint64(900_000)
	if to != nil {
		gas, err = c.EstimateGas(ctx, ethereum.CallMsg{From: from, To: to, Value: value, Data: data})
		if err != nil {
			gas = 100_000 // a call that would revert is still sent, to see the failed receipt
		}
	}
	tx, err := types.SignNewTx(k, s, &types.DynamicFeeTx{ChainID: s.ChainID(), Nonce: mustNonce(ctx, c, from), GasTipCap: tip,
		GasFeeCap: big.NewInt(10_000_000_000), Gas: gas, To: to, Value: value, Data: data})
	must(err)
	must(c.SendTransaction(ctx, tx))
	return waitTx(ctx, c, tx.Hash())
}

func waitTx(ctx context.Context, c *ethclient.Client, h common.Hash) *types.Receipt {
	for {
		r, err := c.TransactionReceipt(ctx, h)
		if err == nil {
			return r
		}
		select {
		case <-ctx.Done():
			must(ctx.Err())
		case <-time.After(300 * time.Millisecond):
		}
	}
}
