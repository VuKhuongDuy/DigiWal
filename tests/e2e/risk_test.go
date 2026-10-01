package e2e

import (
	"context"
	"math/big"
	"testing"
	"time"
)

func dbNonce(t testing.TB) int {
	return q1[int](t, wdb, `SELECT next_nonce FROM hot_wallet_nonces WHERE chain_id=$1`, chainID)
}
func chainNonce(t testing.TB) int {
	n, err := cl.NonceAt(context.Background(), hotAddr, nil)
	if err != nil {
		t.Fatal(err)
	}
	return int(n)
}

// WDR-19: Release while the signed tx may still be mined. The stand-in orchestrator times out waiting
// for confirmation (automine off), releases the lock, and the tx is then mined: the recipient is paid AND the
// user keeps the money. Ledger cannot refuse the Release — it has no idea a tx exists.
func TestWDR19_ReleaseOfABroadcastWithdrawalIsPossible(t *testing.T) {
	qa(t)
	u := uid("wdr19")
	credit(t, u, ethAsset, 1_000_000_000_000_000)
	w := newWd(u, ethAsset, 1_000_000_000_000_000)
	w.lock()
	w.sign()
	anvil(t, "evm_setAutomine", false)
	defer anvil(t, "evm_setAutomine", true)
	if err := cl.SendTransaction(context.Background(), decodeRaw(t, w.signed.s("signed_raw_tx"))); err != nil {
		t.Fatal(err)
	}
	// "timeout": the orchestrator gives up while the tx sits in the mempool
	rel := ledgerRelease(w.key)
	anvil(t, "evm_setAutomine", true)
	mine(t, 1)
	paid := onchainETH(t, w.to)
	t.Logf("release=%d recipient on-chain=%s user ledger total=%s", rel.Code, paid, balance(t, u, ethAsset).Total)
	if rel.Code == 200 && paid.Sign() > 0 && balance(t, u, ethAsset).Total.Sign() > 0 {
		t.Errorf("FINDING D-2 (CRITICAL, design): Release was accepted for a withdrawal whose signed tx was later mined. Recipient got %s wei AND the user still has %s in the ledger => insolvency / double spend. "+
			"The ledger has no 'broadcast' state; nothing stops withdrawal-service releasing a lock whose tx is still live.", paid, balance(t, u, ethAsset).Total)
	}
	// cleanup the accounting so later solvency checks stay meaningful
	if rel.Code == 200 {
		ledgerLock(uid("fix"), u, ethAsset, "0")
	}
}

// WDR-33: someone else (an operator, a bug, a second instance) moves the Hot Wallet's on-chain nonce.
// wallet-service seeds its counter from the RPC once and never re-reads, so every later tx has a stale nonce.
func TestWDR33_ExternalNonceDriftWedgesWithdrawals(t *testing.T) {
	qa(t)
	base := chainNonce(t)
	dbn := dbNonce(t)
	if base != dbn {
		t.Skipf("precondition: chain nonce %d vs db %d (earlier test left the hot wallet out of sync)", base, dbn)
	}
	anvil(t, "anvil_setNonce", hotAddr.Hex(), hexBig(big.NewInt(int64(base+5)))) // 5 txs "sent by someone else"
	defer anvil(t, "anvil_setNonce", hotAddr.Hex(), hexBig(big.NewInt(int64(base))))
	r := signHot(uid("drift"), randAddr().Hex(), ethAsset, "1000")
	if r.Code != 200 {
		t.Fatalf("sign %d %s", r.Code, r.Body)
	}
	tx := decodeRaw(t, r.s("signed_raw_tx"))
	err := cl.SendTransaction(context.Background(), tx)
	t.Logf("signed nonce=%d, chain nonce=%d, broadcast err=%v", tx.Nonce(), base+5, err)
	if err == nil {
		t.Fatal("expected 'nonce too low'")
	}
	// is there any way back? signing again with a NEW key just burns the next stale nonce
	r2 := signHot(uid("drift"), randAddr().Hex(), ethAsset, "1000")
	tx2 := decodeRaw(t, r2.s("signed_raw_tx"))
	if cl.SendTransaction(context.Background(), tx2) == nil {
		return
	}
	t.Errorf("FINDING W-6 (HIGH, stuck system): after the Hot Wallet's on-chain nonce moved externally, every signature wallet-service hands out (nonces %d, %d) is rejected 'nonce too low' (chain at %d). "+
		"There is no re-sync from the RPC, no freeze/alert endpoint, so ALL withdrawals fail until an operator edits hot_wallet_nonces by hand.", tx.Nonce(), tx2.Nonce(), base+5)
}

// WDR-21: Nonce gap. Nonce n is signed but never broadcast; n+1 is broadcast and is stuck pending forever.
func TestWDR21_NonceGapHasNoDetectionOrFreezeEndpoint(t *testing.T) {
	qa(t)
	if chainNonce(t) != dbNonce(t) {
		t.Skip("hot wallet out of sync from an earlier test")
	}
	a := signHot(uid("gapA"), randAddr().Hex(), ethAsset, "1000") // signed, "lost" before broadcast
	b := signHot(uid("gapB"), randAddr().Hex(), ethAsset, "1000")
	if a.Code != 200 || b.Code != 200 {
		t.Fatal("sign failed")
	}
	anvil(t, "evm_setAutomine", false)
	if err := cl.SendTransaction(context.Background(), decodeRaw(t, b.s("signed_raw_tx"))); err != nil {
		t.Fatal(err)
	}
	anvil(t, "evm_setAutomine", true)
	mine(t, 2)
	rc, _ := cl.TransactionReceipt(context.Background(), decodeRaw(t, b.s("signed_raw_tx")).Hash())
	if rc != nil {
		t.Fatal("tx at n+1 mined despite gap?")
	}
	// the design says wallet-service refuses new nonces once a gap is reported. Is there any way to report it?
	for _, p := range []string{"/v1/nonce-gap", "/v1/hot-wallet/nonce-gap", "/v1/hot-wallet/freeze", "/v1/admin/nonce-gap"} {
		if r := do(client("withdrawal-service"), "POST", walletTLS+p, map[string]any{"chain_id": chainID, "from_nonce": base(t)}); r.Code != 404 {
			t.Logf("%s -> %d", p, r.Code)
			goto heal
		}
	}
	t.Errorf("FINDING W-4 (HIGH): no HTTP endpoint exists to report/clear a Nonce Gap (storage.FreezeNonceGap / ResolveNonceGap are only called from tests). "+
		"Task 0.4 'Nonce-Gap freeze flag' is unreachable in the running service: a gap cannot be frozen, so withdrawals keep being signed behind a stuck nonce.")
heal:
	// heal the chain so later tests run: broadcast the lost tx
	if err := cl.SendTransaction(context.Background(), decodeRaw(t, a.s("signed_raw_tx"))); err != nil {
		t.Logf("heal: %v", err)
	}
	mine(t, 2)
	waitFor(t, "gap healed", 10*time.Second, func() bool { return chainNonce(t) == dbNonce(t) })
}

func base(t testing.TB) int { return chainNonce(t) }

// WDR-27: chain reorg removes a withdrawal tx AFTER settle.
func TestWDR27_ReorgAfterSettleLeavesLedgerAheadOfChain(t *testing.T) {
	qa(t)
	if chainNonce(t) != dbNonce(t) {
		t.Skip("hot wallet out of sync from an earlier test")
	}
	u := uid("wdr27")
	credit(t, u, ethAsset, 1_000_000_000_000_000)
	var snap string
	if err := rawRPC.CallContext(context.Background(), &snap, "evm_snapshot"); err != nil {
		t.Fatal(err)
	}
	w := newWd(u, ethAsset, 1_000_000_000_000_000)
	w.full(t)
	var ok bool
	if err := rawRPC.CallContext(context.Background(), &ok, "evm_revert", snap); err != nil || !ok {
		t.Fatalf("revert: %v %v", err, ok)
	}
	paid := onchainETH(t, w.to)
	t.Logf("after reorg: recipient=%s ledger total=%s chain nonce=%d db nonce=%d", paid, balance(t, u, ethAsset).Total, chainNonce(t), dbNonce(t))
	if paid.Sign() == 0 && balance(t, u, ethAsset).Total.Sign() == 0 {
		t.Errorf("FINDING W-7 (CRITICAL until M3 proves otherwise): the withdrawal tx was reorged out after Settle: the user was DEBITED but never PAID; "+
			"wallet-service's nonce counter (%d) is now ahead of the chain (%d) so the next withdrawals are stuck behind a gap. Nothing in ledger/wallet detects or repairs this; "+
			"withdrawal-flow.md invariants #7-9 depend on a withdrawal-service re-check loop that does not exist yet.", dbNonce(t), chainNonce(t))
	}
	// restore consistency for the rest of the run: re-broadcast the same stored raw tx (idempotent replay)
	r := w.sign()
	if cl.SendTransaction(context.Background(), decodeRaw(t, r.s("signed_raw_tx"))) != nil {
		t.Log("could not rebroadcast")
	}
	mine(t, 1)
}
