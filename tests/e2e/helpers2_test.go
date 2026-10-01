package e2e

import (
	"time"
	"net"
	osexec "os/exec"
	"context"
	"fmt"
	"math/big"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// credit gives a user a balance through the real deposit path with a synthetic (not on-chain) event.
// Use for ledger-level tests where the chain isn't the point.
func credit(t testing.TB, user, asset string, amount int64) depositEvent {
	t.Helper()
	e := depositEvent{1, chainID, common.BytesToHash(crypto.Keccak256([]byte(uid("tx")))).Hex(), -1, asset,
		"0x00000000000000000000000000000000000000a1", "0x00000000000000000000000000000000000000b2", user, fmt.Sprint(amount), 1}
	if asset == ttAsset {
		e.LogIndex = 0
	}
	want := balance(t, user, asset).Total.Int64() + amount
	publish(t, e)
	waitCredited(t, user, asset, want)
	return e
}

func fundTT(t testing.TB, to common.Address, v *big.Int) {
	t.Helper()
	if tokenBalance(common.HexToAddress(ttAddr), crypto.PubkeyToAddress(faucet.PublicKey)).Cmp(v) < 0 {
		mintTT(crypto.PubkeyToAddress(faucet.PublicKey), new(big.Int).Mul(v, big.NewInt(100)))
	}
	data := append(selector("transfer(address,uint256)"), append(common.LeftPadBytes(to.Bytes(), 32), word(v)...)...)
	rc := receipt(t, sendFrom(t, faucet, common.HexToAddress(ttAddr), new(big.Int), data).Hash())
	if rc.Status != 1 {
		t.Fatal("TT transfer reverted")
	}
}

func parallel(n int, f func(i int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); <-start; f(i) }(i)
	}
	close(start)
	wg.Wait()
}

func compose(t testing.TB, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", append([]string{"compose", "-p", "digiwal-qa"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func count(t testing.TB, sql string, args ...any) int {
	t.Helper()
	return q1[int](t, ldb, sql, args...)
}

func codeCount(rs []resp) map[int]int {
	m := map[int]int{}
	for _, r := range rs {
		m[r.Code]++
	}
	return m
}

func ledgerHash(t testing.TB, key string) int {
	return count(t, `SELECT count(*) FROM ledger_transactions WHERE idempotency_key=$1`, key)
}

func onchainETH(t testing.TB, a common.Address) *big.Int {
	t.Helper()
	b, err := cl.BalanceAt(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func randAddr() common.Address {
	k, _ := crypto.GenerateKey()
	return crypto.PubkeyToAddress(k.PublicKey)
}

var _ = strings.TrimSpace

func decodeRaw(t testing.TB, hexTx string) *types.Transaction {
	t.Helper()
	var tx types.Transaction
	if err := tx.UnmarshalBinary(common.FromHex(hexTx)); err != nil {
		t.Fatal(err)
	}
	return &tx
}

func ctxBG() context.Context { return context.Background() }

func execCmd(args ...string) (string, error) {
	out, err := osexec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

var net_Dialer = net.Dialer{Timeout: 5 * time.Second}
