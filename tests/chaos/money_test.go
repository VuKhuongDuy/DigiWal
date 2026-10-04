package chaos

import (
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// C2 (was D-1): an ERC-20 deposit forces the sweeper to send a native Gas Top-Up INTO the user's deposit address.
// That ETH must never show up as a user ETH deposit.
func TestC2_GasTopUpIsNotCredited(t *testing.T) {
	qa(t)
	u := newUser(t)
	u.fundAndCredit(t, ttAsset, eth1(500))
	t.Logf("500 TT credited; waiting for the sweeper (Gas Top-Up + token sweep)")
	waitFor(t, 120*time.Second, "token swept to hot wallet", func() (bool, string) {
		mine(t, 1)
		return tokenBal(u.dep).Sign() == 0, "token balance at deposit address: " + tokenBal(u.dep).String()
	})
	for i := 0; i < 6; i++ { // let chain-watcher see the top-up block(s) and the ledger consume anything it emitted
		mine(t, 1)
		time.Sleep(1500 * time.Millisecond)
	}
	t.Logf("on-chain ETH at deposit address after top-up+sweep: %s wei", ethBal(u.dep))
	if e := u.bal(t, ethAsset).Total; e.Sign() != 0 {
		t.Errorf("D-1 REGRESSION: the Gas Top-Up was credited to the user as %s wei of ETH", e)
	}
	if tt := u.bal(t, ttAsset).Total; tt.Cmp(eth1(500)) != 0 {
		t.Errorf("TT balance %s, want 500", tt)
	}
}

func rawTxOf(t *testing.T, key string) (hash string, raw []byte) {
	t.Helper()
	h := psql(t, "withdrawal", fmt.Sprintf(`SELECT coalesce(tx_hash,'') || '|' || coalesce(signed_raw_tx,'') FROM withdrawal_requests WHERE idempotency_key='%s'`, key))
	p := strings.SplitN(h, "|", 2)
	if len(p) == 2 {
		hash = p[0]
		raw = common.FromHex(p[1])
	}
	return
}

func automine(t *testing.T, on bool) { rpcCall(t, nil, "evm_setAutomine", on) }

// C11 (D-2 family): the tx is in the mempool, withdrawal-service dies, the block is mined while it is down.
// On restart it must settle exactly once: destination paid once, ledger debited once, nothing released.
func TestC11_KillAfterBroadcastBeforeSettle(t *testing.T) {
	qa(t)
	b := newBatch(t, "c11", eth1(2), 1, eth1(1))
	automine(t, false)
	defer automine(t, true)
	if c, body := b.u.withdraw(b.keys[0], ethAsset, b.amt, b.dests[0]); c != 202 {
		t.Fatalf("withdraw %d %s", c, body)
	}
	waitFor(t, 60*time.Second, "status broadcast", func() (bool, string) {
		s, _ := b.u.wstatus(b.keys[0])
		return s == "broadcast", s
	})
	t.Logf("withdrawal is 'broadcast' (tx pending in mempool, automine OFF). Killing withdrawal-service now")
	dc(t, "kill", ctr("withdrawal-service"))
	automine(t, true)
	mine(t, 3)
	t.Logf("tx mined while withdrawal-service was dead: destination holds %s", ethBal(b.dests[0]))
	dc(t, "start", ctr("withdrawal-service"))
	st := b.u.settleAll(t, b.keys, 120*time.Second)
	t.Logf("final: %v", st)
	if paid := b.verify(t, eth1(2), st); paid != 1 {
		t.Errorf("paid %d", paid)
	}
}

// C12: the signed tx is dropped from the mempool (eviction / node restart). A correct system either re-broadcasts the
// stored raw tx or fails the withdrawal and releases the lock; it must not leave funds locked in 'broadcast' forever.
// Then the original raw tx is replayed to prove nothing is paid twice either way (the D-2 hazard).
func TestC12_DroppedTxIsNotLeftStuckForever(t *testing.T) {
	qa(t)
	b := newBatch(t, "c12", eth1(2), 1, eth1(1))
	automine(t, false)
	defer automine(t, true)
	if c, body := b.u.withdraw(b.keys[0], ethAsset, b.amt, b.dests[0]); c != 202 {
		t.Fatalf("withdraw %d %s", c, body)
	}
	waitFor(t, 60*time.Second, "status broadcast", func() (bool, string) {
		s, _ := b.u.wstatus(b.keys[0])
		return s == "broadcast", s
	})
	hash, raw := rawTxOf(t, b.keys[0])
	t.Logf("tx %s pending; saved raw bytes (%d). Dropping it from the mempool (anvil_dropTransaction).", hash, len(raw))
	rpcCall(t, nil, "anvil_dropTransaction", hash)
	automine(t, true)
	var final string
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		mine(t, 1)
		final, _ = b.u.wstatus(b.keys[0])
		if terminal(final) {
			break
		}
		time.Sleep(time.Second)
	}
	bl := b.u.bal(t, ethAsset)
	t.Logf("after 150s: status %q, ledger total/locked %s/%s, destination %s", final, bl.Total, bl.Locked, ethBal(b.dests[0]))
	stuck := !terminal(final)
	if stuck {
		t.Errorf("W-9 (MEDIUM, liveness): the withdrawal's tx was dropped from the mempool and 150s later it is still %q with %s wei locked: not re-broadcast automatically (the raw tx is stored); by design only an ALERT after 128 blocks. Fee Bump only fires for stuck AND underpriced txs, and a dropped tx is neither.", final, bl.Locked)
	}
	// replay the original signed tx: whichever way the system went, the destination must be paid exactly once in total
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		t.Fatalf("raw tx: %v", err)
	}
	t.Logf("replaying the original raw tx: %v", ec.SendTransaction(ctxBG(), &tx))
	st := b.u.settleAll(t, b.keys, 120*time.Second)
	t.Logf("final: %v", st)
	b.verify(t, eth1(2), st) // paid <=> confirmed, ledger == chain, nothing locked
	_ = big.NewInt
}
