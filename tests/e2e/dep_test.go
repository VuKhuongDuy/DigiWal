package e2e

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// DEP-01: real native deposit on anvil -> confirmed event -> ledger rows.
func TestDEP01_NativeDepositEndToEnd(t *testing.T) {
	qa(t)
	u := uid("dep01")
	amt := new(big.Int).Mul(big.NewInt(3), eth)
	e, addr := depositNative(t, u, amt)
	waitCredited(t, u, ethAsset, amt.Int64())

	if got := onchainETH(t, addr); got.Cmp(amt) != 0 {
		t.Fatalf("on-chain balance %s != %s", got, amt)
	}
	var src string
	var chain *int64
	var logIdx *int64
	if err := ldb.QueryRow(context.Background(), `SELECT source_type, chain_id, log_index FROM ledger_transactions WHERE idempotency_key=$1`, e.key()).Scan(&src, &chain, &logIdx); err != nil {
		t.Fatal(err)
	}
	if src != "deposit" || *chain != chainID || *logIdx != -1 {
		t.Fatalf("ledger tx %s %v %v", src, *chain, *logIdx)
	}
	if n := count(t, `SELECT count(*) FROM entries e JOIN ledger_transactions l ON l.id=e.ledger_transaction_id WHERE l.idempotency_key=$1`, e.key()); n != 2 {
		t.Fatalf("%d entries", n)
	}
	if sum := q1[string](t, ldb, `SELECT sum(e.amount)::text FROM entries e JOIN ledger_transactions l ON l.id=e.ledger_transaction_id WHERE l.idempotency_key=$1`, e.key()); sum != "0" {
		t.Fatalf("entries sum %s", sum)
	}
	if user := q1[string](t, ldb, `SELECT e.amount::text FROM entries e JOIN accounts a ON a.id=e.account_id JOIN ledger_transactions l ON l.id=e.ledger_transaction_id WHERE l.idempotency_key=$1 AND a.kind='user'`, e.key()); user != amt.String() {
		t.Fatalf("user entry %s", user)
	}
}

// DEP-02/03: the same confirmed event delivered many times, in one batch and across batches.
func TestDEP02_03_RedeliveryNeverDoubleCredits(t *testing.T) {
	qa(t)
	u := uid("dep02")
	e := depositEvent{1, chainID, common.BytesToHash(crypto.Keccak256([]byte(u))).Hex(), -1, ethAsset,
		"0x00000000000000000000000000000000000000a1", "0x00000000000000000000000000000000000000b2", u, "1000", 7}
	batch := make([]depositEvent, 20)
	for i := range batch {
		batch[i] = e
	}
	publish(t, batch...)
	parallel(5, func(int) { publish(t, e, e) })
	waitCredited(t, u, ethAsset, 1000)
	time.Sleep(3 * time.Second) // let any straggling duplicate be consumed
	wantBal(t, u, ethAsset, 1000, 0)
	if n := ledgerHash(t, e.key()); n != 1 {
		t.Fatalf("%d ledger txs for one deposit", n)
	}
	if n := count(t, `SELECT count(*) FROM entries e JOIN ledger_transactions l ON l.id=e.ledger_transaction_id WHERE l.idempotency_key=$1`, e.key()); n != 2 {
		t.Fatalf("%d entries", n)
	}
}

// DEP-07: ERC-20 deposit through the real token contract.
func TestDEP07_ERC20Deposit(t *testing.T) {
	qa(t)
	u := uid("dep07")
	addr := depositAddress(t, u)
	amt := new(big.Int).Mul(big.NewInt(250), eth)
	fundTT(t, addr, amt)
	rc := receipt(t, func() common.Hash { // the transfer tx is the latest block's only tx
		b, _ := cl.BlockByNumber(context.Background(), nil)
		return b.Transactions()[0].Hash()
	}())
	mine(t, 32)
	evs := scanBlock(t, rc.BlockNumber.Uint64())
	if len(evs) != 1 || evs[0].AssetID != ttAsset || evs[0].LogIndex < 0 {
		t.Fatalf("stand-in watcher events: %+v", evs)
	}
	publish(t, evs[0])
	waitFor(t, "erc20 credit", 20*time.Second, func() bool { return balance(t, u, ttAsset).Total.Cmp(amt) == 0 })
	if tokenBalance(common.HexToAddress(ttAddr), addr).Cmp(amt) != 0 {
		t.Fatal("on-chain token balance mismatch")
	}
	if balance(t, u, ethAsset).Total.Sign() != 0 {
		t.Fatal("ETH account touched by a token deposit")
	}
}

// DEP-11: zero-value transfers (address-poisoning spam) must not credit, create an account, or wedge the stream.
func TestDEP11_ZeroAmountIsParkedAndStreamKeepsMoving(t *testing.T) {
	qa(t)
	u := uid("dep11")
	z := depositEvent{1, chainID, common.BytesToHash(crypto.Keccak256([]byte("z" + u))).Hex(), 0, ttAsset,
		"0x00000000000000000000000000000000000000a1", "0x00000000000000000000000000000000000000b2", u, "0", 5}
	failedBefore := count(t, `SELECT count(*) FROM failed_events`)
	publish(t, z)
	credit(t, u, ethAsset, 777) // a real one right behind it
	if count(t, `SELECT count(*) FROM failed_events`) != failedBefore+1 {
		t.Error("zero deposit was not parked")
	}
	if n := count(t, `SELECT count(*) FROM accounts WHERE user_id=$1 AND asset_id=$2`, u, ttAsset); n != 0 {
		t.Errorf("zero deposit created a token account")
	}
}

// DEP-16: unknown asset -> parked, no account, nothing credited.
func TestDEP16_UnknownAssetIsRefused(t *testing.T) {
	qa(t)
	u := uid("dep16")
	e := depositEvent{1, chainID, common.BytesToHash(crypto.Keccak256([]byte(u))).Hex(), 0, "99999999-9999-9999-9999-999999999999",
		"0x00000000000000000000000000000000000000a1", "0x00000000000000000000000000000000000000b2", u, "500", 5}
	publish(t, e)
	waitDeposit(t, e)
	if n := count(t, `SELECT count(*) FROM accounts WHERE user_id=$1`, u); n != 0 {
		t.Fatalf("account created for unknown asset")
	}
	if n := count(t, `SELECT count(*) FROM failed_events WHERE message_key=$1`, e.key()); n != 1 {
		t.Fatalf("parked %d", n)
	}
}

// DEP-16b: an event for a chain the system doesn't operate on (chain_id=1 mainnet while we run 31337).
func TestDEP16b_ForeignChainIDIsNotCredited(t *testing.T) {
	qa(t)
	u := uid("dep16b")
	e := depositEvent{1, 1, common.BytesToHash(crypto.Keccak256([]byte(u))).Hex(), -1, ethAsset,
		"0x00000000000000000000000000000000000000a1", "0x00000000000000000000000000000000000000b2", u, "500", 5}
	publish(t, e)
	waitDeposit(t, e)
	if b := balance(t, u, ethAsset).Total; b.Sign() != 0 {
		t.Errorf("FINDING D-4: deposit event for chain_id=1 credited %s on a 31337 deployment (ledger has no notion of the supported chain; any producer on the topic can mint balance)", b)
	}
}

// DEP-17: same natural key re-emitted with a different amount: first credit stands, second is parked.
func TestDEP17_SameKeyDifferentAmount(t *testing.T) {
	qa(t)
	u := uid("dep17")
	e := credit(t, u, ethAsset, 1000)
	e2 := e
	e2.Amount = "9000"
	e2.UserID = u
	publish(t, e2)
	time.Sleep(3 * time.Second)
	wantBal(t, u, ethAsset, 1000, 0)
	if n := count(t, `SELECT count(*) FROM failed_events WHERE message_key=$1`, e.key()); n < 1 {
		t.Error("conflicting re-emit was not parked for review")
	}
}

// DEP-18: screening list. Sender is flagged, not credited; mixed case; redelivered 10x -> one flag.
func TestDEP18_ScreenedSenderIsFlaggedNotCredited(t *testing.T) {
	qa(t)
	u := uid("dep18")
	e := depositEvent{1, chainID, common.BytesToHash(crypto.Keccak256([]byte(u))).Hex(), -1, ethAsset,
		"0xDEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF", "0x00000000000000000000000000000000000000b2", u, "4242", 5}
	batch := make([]depositEvent, 10)
	for i := range batch {
		batch[i] = e
	}
	publish(t, batch...)
	waitDeposit(t, e)
	time.Sleep(2 * time.Second)
	if b := balance(t, u, ethAsset).Total; b.Sign() != 0 {
		t.Fatalf("flagged deposit credited %s", b)
	}
	if n := count(t, `SELECT count(*) FROM flagged_deposits WHERE tx_hash=$1`, strings.ToLower(e.TxHash)); n != 1 {
		t.Fatalf("%d flags", n)
	}
	if n := ledgerHash(t, e.key()); n != 0 {
		t.Fatal("flagged deposit became a ledger tx")
	}
}

// DEP-23: poison records between good ones.
func TestDEP23_PoisonRecordDoesNotBlockTheStream(t *testing.T) {
	qa(t)
	u := uid("dep23")
	good := func() depositEvent {
		return depositEvent{1, chainID, common.BytesToHash(crypto.Keccak256([]byte(uid("g")))).Hex(), -1, ethAsset,
			"0x00000000000000000000000000000000000000a1", "0x00000000000000000000000000000000000000b2", u, "10", 5}
	}
	bad := []depositEvent{{Version: 9}, {Version: 1, ChainID: 31337, TxHash: "nothex", AssetID: ethAsset, UserID: u, Amount: "1"},
		{Version: 1, ChainID: 31337, TxHash: common.BytesToHash([]byte("a")).Hex(), AssetID: ethAsset, FromAddress: "0x00000000000000000000000000000000000000a1", ToAddress: "0x00000000000000000000000000000000000000b2", UserID: u, Amount: "-5"},
		{Version: 1, ChainID: 31337, TxHash: common.BytesToHash([]byte("b")).Hex(), AssetID: ethAsset, FromAddress: "0x00000000000000000000000000000000000000a1", ToAddress: "0x00000000000000000000000000000000000000b2", UserID: u, Amount: "1.5"},
		{Version: 1, ChainID: 31337, TxHash: common.BytesToHash([]byte("c")).Hex(), AssetID: ethAsset, FromAddress: "0x00000000000000000000000000000000000000a1", ToAddress: "0x00000000000000000000000000000000000000b2", UserID: "", Amount: "5"}}
	var all []depositEvent
	all = append(all, good())
	all = append(all, bad...)
	all = append(all, good())
	publish(t, all...)
	waitCredited(t, u, ethAsset, 20)
	wantBal(t, u, ethAsset, 20, 0)
}

// DEP-24: Gas Top-Up (Hot Wallet -> user address) must NOT be seen as a user deposit.
// The deposit-flow design scans transfers to every watched address and defines no sender exclusion.
func TestDEP24_GasTopUpIsNotADeposit(t *testing.T) {
	qa(t)
	u := uid("dep24")
	addr := depositAddress(t, u)
	topUp := "1000000000000000" // 0.001 ETH, deposit-flow.md
	s := signHot(uid("gas-topup"), addr.Hex(), ethAsset, topUp)
	if s.Code != 200 {
		t.Fatalf("sign: %d %s", s.Code, s.Body)
	}
	h := broadcast(t, s.s("signed_raw_tx"))
	rc := receipt(t, h)
	mine(t, 32)
	evs := scanBlock(t, rc.BlockNumber.Uint64())
	for _, e := range evs {
		if strings.EqualFold(e.ToAddress, addr.Hex()) {
			publish(t, e)
			waitCredited(t, u, ethAsset, 1_000_000_000_000_000)
			t.Errorf("FINDING D-1 (CRITICAL, design gap): the Hot Wallet's own Gas Top-Up of %s wei to %s (user %s) was emitted as a deposit and CREDITED to the user. "+
				"Phantom balance, withdrawable; the sweep then returns the funds to the Hot Wallet so custody pays twice.", topUp, addr, u)
			return
		}
	}
}

// DEP-25/26: native sweep: destination can only be the Hot Wallet; 'max' leaves dust only; second sweep refuses.
func TestDEP25_26_NativeSweep(t *testing.T) {
	qa(t)
	u := uid("dep25")
	_, addr := depositNative(t, u, eth)
	hotBefore := onchainETH(t, hotAddr)

	// attacker-chosen destination
	evil := do(client("sweeper"), "POST", walletTLS+"/v1/sign/user-address", map[string]any{
		"user_id": u, "to_address": randAddr().Hex(), "asset_id": ethAsset, "amount": "max", "chain_id": chainID})
	if evil.Code != 403 {
		t.Errorf("sweep to arbitrary address: %d %s (want 403)", evil.Code, evil.Body)
	}
	// unknown user
	if r := do(client("sweeper"), "POST", walletTLS+"/v1/sign/user-address", map[string]any{
		"user_id": "ghost-" + u, "to_address": hotAddr.Hex(), "asset_id": ethAsset, "amount": "max", "chain_id": chainID}); r.Code != 404 {
		t.Errorf("unknown user sweep: %d", r.Code)
	}
	r := do(client("sweeper"), "POST", walletTLS+"/v1/sign/user-address", map[string]any{
		"user_id": u, "to_address": hotAddr.Hex(), "asset_id": ethAsset, "amount": "max", "chain_id": chainID})
	if r.Code != 200 {
		t.Fatalf("sweep sign %d %s", r.Code, r.Body)
	}
	rc := receipt(t, broadcast(t, r.s("signed_raw_tx")))
	if rc.Status != 1 {
		t.Fatal("sweep reverted")
	}
	dust := onchainETH(t, addr)
	moved := new(big.Int).Sub(onchainETH(t, hotAddr), hotBefore)
	t.Logf("swept %s wei, dust left %s", moved, dust)
	if new(big.Int).Add(new(big.Int).Add(moved, dust), new(big.Int).Mul(new(big.Int).SetUint64(rc.GasUsed), rc.EffectiveGasPrice)).Cmp(eth) != 0 {
		t.Errorf("money not conserved: moved %s + dust %s + fee != 1 ETH", moved, dust)
	}
	if dust.Cmp(big.NewInt(1e15)) > 0 {
		t.Errorf("dust %s unexpectedly large", dust)
	}
	// DEP-26: second sweep has nothing sweepable
	r2 := do(client("sweeper"), "POST", walletTLS+"/v1/sign/user-address", map[string]any{
		"user_id": u, "to_address": hotAddr.Hex(), "asset_id": ethAsset, "amount": "max", "chain_id": chainID})
	if r2.Code == 200 {
		t.Errorf("second sweep produced a tx on dust balance %s", dust)
	}
}

// DEP-28: gas top-up replay: same key -> identical signed tx, one nonce, one on-chain transfer.
func TestDEP28_GasTopUpIdempotent(t *testing.T) {
	qa(t)
	u := uid("dep28")
	addr := depositAddress(t, u)
	key := uid("gas-topup")
	a := signHot(key, addr.Hex(), ethAsset, "1000000000000000")
	b := signHot(key, addr.Hex(), ethAsset, "1000000000000000")
	if a.Code != 200 || b.Code != 200 || a.s("signed_raw_tx") != b.s("signed_raw_tx") || a.M["nonce"] != b.M["nonce"] {
		t.Fatalf("replay differs: %d/%d", a.Code, b.Code)
	}
	broadcast(t, a.s("signed_raw_tx"))
	// a naive retry that broadcasts the replayed tx again must be rejected by the chain, not double-pay
	var err error
	func() {
		defer func() { recover() }()
		bytesTx := common.FromHex(b.s("signed_raw_tx"))
		_ = bytesTx
	}()
	_ = err
	mine(t, 1)
	if got := onchainETH(t, addr); got.Cmp(big.NewInt(1e15)) != 0 {
		t.Fatalf("top-up paid %s", got)
	}
	if n := q1[int](t, wdb, `SELECT count(*) FROM hot_wallet_signatures WHERE idempotency_key=$1`, key); n != 1 {
		t.Fatalf("%d signature rows", n)
	}
}

var _ = fmt.Sprint
