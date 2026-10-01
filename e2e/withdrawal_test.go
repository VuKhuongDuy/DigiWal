// Task 3.8: withdrawals end to end — api-gateway → withdrawal-service → ledger-service Lock →
// wallet-service signing → a real Anvil chain → confirmation → Settle — plus the failure paths the
// design exists for: destination checks, a Fee Bump, and a forced Nonce Gap (a reorg simulated with
// Anvil's evm_snapshot / evm_revert) resolved with a Nonce Filler, a refund and the Reaper. Same stack
// as the M2 test (`make up`); skipped unless E2E=1.
package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

func eth1(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), oneETH) }

// finney is 0.001 ETH.
func finney(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e15)) }

func TestWithdrawals(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 (make e2e) to run against the docker-compose stack")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	eth, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		t.Fatal(err)
	}
	rpcc, _ := rpc.DialContext(ctx, rpcURL)
	key0, key1 := mustKey(t, anvilKey0), mustKey(t, anvilKey1)
	token := ensureToken(ctx, t, eth, key0)
	hot := hotWalletAddress(ctx, t)
	// The Hot Wallet pays out: float it generously (the sweeper tops it up with deposits as well).
	send(ctx, t, eth, key1, &hot, eth1(50), nil)
	send(ctx, t, eth, key0, &token, big.NewInt(0), transferCalldata(t, hot, eth1(5000)))

	alice, bob, carol := newUser(t), newUser(t), newUser(t)
	aliceDep := common.HexToAddress(alice.depositAddress(t))
	bobDep := common.HexToAddress(bob.depositAddress(t))
	carolDep := common.HexToAddress(carol.depositAddress(t))

	// Credit them through the real deposit flow. Native deposits for alice and carol; bob's is the
	// token (its sweep needs a Gas Top-Up from the Hot Wallet, so it is awaited before anything
	// reorgs the chain: a reverted top-up would be a second nonce gap).
	send(ctx, t, eth, key1, &aliceDep, eth1(2), nil)
	send(ctx, t, eth, key1, &carolDep, eth1(3), nil)
	send(ctx, t, eth, key0, &token, big.NewInt(0), transferCalldata(t, bobDep, eth1(500)))
	mine(ctx, t, rpcc, 5) // Confirmation Depth is 2 in this stack
	waitFor(t, 120*time.Second, "deposits credited", func() (bool, string) {
		a, b, c := alice.ledger(t), bob.ledger(t), carol.ledger(t)
		return a[ethAssetID].total == eth1(2).String() && b[tokenAssetID].total == eth1(500).String() && c[ethAssetID].total == eth1(3).String(),
			fmt.Sprint(a, b, c)
	})
	waitFor(t, 150*time.Second, "deposit addresses swept", func() (bool, string) {
		mine(ctx, t, rpcc, 1)
		tt := tokenBalance(ctx, t, eth, token, bobDep)
		a, c := balance(ctx, t, eth, aliceDep), balance(ctx, t, eth, carolDep)
		return tt.Sign() == 0 && a.Cmp(finney(1)) < 0 && c.Cmp(finney(1)) < 0, fmt.Sprintf("bob TT %s, alice %s, carol %s", tt, a, c)
	})

	destA, destB, destC := freshAddress(t), freshAddress(t), freshAddress(t)

	t.Run("whitelist: an own deposit address is refused, others wait out the cooldown", func(t *testing.T) {
		code, body := alice.do(t, "POST", "/v1/whitelist", map[string]string{"address": aliceDep.Hex()})
		if code != http.StatusUnprocessableEntity || !strings.Contains(string(body), "destination_is_deposit_address") {
			t.Fatalf("whitelisting alice's own deposit address: %d %s", code, body)
		}
		code, body = alice.do(t, "POST", "/v1/whitelist", map[string]string{"address": bobDep.Hex()})
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("whitelisting another user's deposit address: %d %s", code, body)
		}
		if code, body := alice.do(t, "POST", "/v1/whitelist", map[string]string{"address": destA.Hex()}); code != http.StatusCreated {
			t.Fatalf("whitelisting %s: %d %s", destA.Hex(), code, body)
		}
		// Not usable yet: the Whitelist Cooldown (2s here, 24h in production) has not passed.
		if code, body := alice.withdraw(t, "too-early", ethAssetID, finney(1), destA); code != http.StatusUnprocessableEntity || !strings.Contains(string(body), "destination_not_whitelisted") {
			t.Errorf("withdrawing to a pending entry: %d %s, want 422 destination_not_whitelisted", code, body)
		}
		if st := alice.whitelistStatus(t, destA); st != "pending" && st != "active" {
			t.Fatalf("a new entry is %q", st)
		}
		for u, dest := range map[*user]common.Address{bob: destB, carol: destC} {
			if code, body := u.do(t, "POST", "/v1/whitelist", map[string]string{"address": dest.Hex()}); code != http.StatusCreated {
				t.Fatalf("whitelisting %s: %d %s", dest.Hex(), code, body)
			}
		}
		for _, u := range []struct {
			u    *user
			dest common.Address
		}{{alice, destA}, {bob, destB}, {carol, destC}} {
			waitFor(t, 30*time.Second, "whitelist entry active", func() (bool, string) {
				st := u.u.whitelistStatus(t, u.dest)
				return st == "active", st
			})
		}
	})

	t.Run("rejections leave the balance untouched", func(t *testing.T) {
		// A request that raced the cooldown above may legitimately have been accepted: reset to a known state.
		before := alice.ledger(t)[ethAssetID]
		if before.locked != "0" {
			t.Fatalf("locked funds before the test: %+v", before)
		}
		cases := []struct {
			name   string
			amount *big.Int
			to     common.Address
			reason string
		}{
			{"not whitelisted", eth1(1), freshAddress(t), "destination_not_whitelisted"},
			{"below the minimum", big.NewInt(1), destA, "below_minimum_withdrawal_amount"},
			{"insufficient funds", eth1(5), destA, "insufficient_funds"},
		}
		for i, c := range cases {
			code, body := alice.withdraw(t, fmt.Sprintf("reject-%d", i), ethAssetID, c.amount, c.to)
			if code != http.StatusUnprocessableEntity || !strings.Contains(string(body), c.reason) {
				t.Errorf("%s: %d %s, want 422 %s", c.name, code, body, c.reason)
			}
		}
		// Invariant #18: a user's own deposit address can't be whitelisted through the API, but one that was
		// whitelisted before the rule (here: inserted straight into the table, long past its cooldown) is
		// still refused when the withdrawal is requested.
		psql(t, "withdrawal", fmt.Sprintf(`INSERT INTO whitelist_entries (user_id, address, added_at) VALUES ('%s', '%s', now() - interval '1 day')`, alice.id, aliceDep.Hex()))
		code, body := alice.withdraw(t, "to-own-deposit", ethAssetID, eth1(1), aliceDep)
		if code != http.StatusUnprocessableEntity || !strings.Contains(string(body), "destination_is_deposit_address") {
			t.Errorf("withdrawal to alice's own deposit address: %d %s", code, body)
		}
		time.Sleep(2 * time.Second)
		if after := alice.ledger(t)[ethAssetID]; after != before {
			t.Errorf("a rejection moved the balance: %+v -> %+v", before, after)
		}
		if st, _ := alice.status(t, "to-own-deposit"); st != "rejected" {
			t.Errorf("status = %q, want rejected", st)
		}
	})

	var nativeTx string
	t.Run("a native withdrawal confirms and settles", func(t *testing.T) {
		amount := finney(500)
		code, body := alice.withdraw(t, "native-1", ethAssetID, amount, destA)
		if code != http.StatusAccepted {
			t.Fatalf("%d %s", code, body)
		}
		// Idempotent: the same key again is the same request; a different body under it is a 409.
		if code, _ := alice.withdraw(t, "native-1", ethAssetID, amount, destA); code != http.StatusOK && code != http.StatusAccepted {
			t.Errorf("duplicate: %d", code)
		}
		if code, _ := alice.withdraw(t, "native-1", ethAssetID, finney(1), destA); code != http.StatusConflict {
			t.Errorf("same key, different amount: %d, want 409", code)
		}
		nativeTx = waitConfirmed(ctx, t, rpcc, alice, "native-1")

		if got := balance(ctx, t, eth, destA); got.Cmp(amount) != 0 {
			t.Errorf("destination received %s, want %s", got, amount)
		}
		assertPaid(ctx, t, eth, nativeTx, hot, destA, amount)
		b := alice.ledger(t)[ethAssetID]
		if want := new(big.Int).Sub(eth1(2), amount).String(); b.total != want || b.locked != "0" || b.available != want {
			t.Errorf("alice's ledger balance = %+v, want total/available %s and nothing locked", b, want)
		}
		// The gas of the mined tx is in the ledger, once: gas_used × effective_gas_price from its receipt,
		// {gas_expense −fee, chain +fee}, and not charged to alice (checked above: only `amount` left her balance).
		rc, err := eth.TransactionReceipt(ctx, common.HexToHash(nativeTx))
		if err != nil {
			t.Fatal(err)
		}
		fee := new(big.Int).Mul(new(big.Int).SetUint64(rc.GasUsed), rc.EffectiveGasPrice)
		waitFor(t, 60*time.Second, "the withdrawal's Gas Fee in the ledger", func() (bool, string) {
			row := psql(t, "ledger", fmt.Sprintf(`SELECT count(*) || '|' || coalesce(sum(e.amount) FILTER (WHERE a.kind = 'gas_expense'), 0) || '|' || coalesce(max(l.purpose), '')
				FROM ledger_transactions l JOIN entries e ON e.ledger_transaction_id = l.id JOIN accounts a ON a.id = e.account_id
				WHERE l.idempotency_key = 'gas:31337:%s'`, strings.ToLower(nativeTx)))
			return row == "2|-"+fee.String()+"|withdrawal", row
		})
		// Only one payout, ever.
		time.Sleep(3 * time.Second)
		if got := balance(ctx, t, eth, destA); got.Cmp(amount) != 0 {
			t.Errorf("destination balance moved again: %s", got)
		}
	})

	t.Run("an ERC-20 withdrawal confirms and settles", func(t *testing.T) {
		amount := eth1(200)
		if code, body := bob.withdraw(t, "token-1", tokenAssetID, amount, destB); code != http.StatusAccepted {
			t.Fatalf("%d %s", code, body)
		}
		waitConfirmed(ctx, t, rpcc, bob, "token-1")
		if got := tokenBalance(ctx, t, eth, token, destB); got.Cmp(amount) != 0 {
			t.Errorf("destination received %s TT, want %s", got, amount)
		}
		b := bob.ledger(t)[tokenAssetID]
		if want := eth1(300).String(); b.total != want || b.locked != "0" {
			t.Errorf("bob's TT ledger balance = %+v, want %s", b, want)
		}
	})

	// A tx the node will not mine at its price is re-signed at the same nonce with higher fees. Anvil's
	// automine is switched off so the tx waits in the pool while the base fee is pushed above its cap.
	t.Run("a stuck underpriced withdrawal is Fee Bumped", func(t *testing.T) {
		setAutomine(ctx, t, rpcc, false)
		defer setAutomine(ctx, t, rpcc, true)
		amount := finney(100)
		if code, body := alice.withdraw(t, "bump-1", ethAssetID, amount, destA); code != http.StatusAccepted {
			t.Fatalf("%d %s", code, body)
		}
		var first string
		waitFor(t, 60*time.Second, "bump-1 broadcast", func() (bool, string) {
			st, h := alice.status(t, "bump-1")
			first = h
			return st == "broadcast" && h != "", st
		})
		if rc, _ := eth.TransactionReceipt(ctx, common.HexToHash(first)); rc != nil {
			t.Fatal("the tx was mined although automine is off")
		}
		id := withdrawalID(t, alice, "bump-1")
		// 22 blocks at a base fee far above the tx's cap (~2 gwei), well under the 50 gwei ceiling.
		for i := 0; i < 22; i++ {
			setNextBaseFee(ctx, t, rpcc, big.NewInt(12e9))
			mine(ctx, t, rpcc, 1)
		}
		waitFor(t, 60*time.Second, "a Fee Bump to be stored", func() (bool, string) {
			n := psql(t, "withdrawal", fmt.Sprintf(`SELECT count(*) FROM withdrawal_txs WHERE withdrawal_id = '%s'`, id))
			return n != "0", "bumps stored: " + n
		})
		// The bumped tx is priced for the new base fee and is mined; the original never is.
		waitFor(t, 120*time.Second, "bump-1 confirmed", func() (bool, string) {
			setNextBaseFee(ctx, t, rpcc, big.NewInt(12e9))
			mine(ctx, t, rpcc, 1)
			st, h := alice.status(t, "bump-1")
			return st == "confirmed", fmt.Sprint(st, " ", h)
		})
		_, final := alice.status(t, "bump-1")
		if final == first {
			t.Errorf("confirmed with the original tx %s, want a Fee Bump version", first)
		}
		if rc, _ := eth.TransactionReceipt(ctx, common.HexToHash(first)); rc != nil {
			t.Errorf("the original tx %s was mined too", first)
		}
		assertPaid(ctx, t, eth, final, hot, destA, amount)
		if got, want := balance(ctx, t, eth, destA), new(big.Int).Add(finney(500), amount); got.Cmp(want) != 0 {
			t.Errorf("destination holds %s, want %s: exactly one payout", got, want)
		}
		want := new(big.Int).Sub(eth1(2), new(big.Int).Add(finney(500), amount)).String()
		if b := alice.ledger(t)[ethAssetID]; b.total != want || b.locked != "0" {
			t.Errorf("alice's ledger balance = %+v, want %s", b, want)
		}
	})

	// The reorg: carol's withdrawal confirms (and Settles), then the chain is rolled back to before it.
	// That is a Nonce Gap: the confirmed tx vanished while wallet-service's next nonce is past it.
	t.Run("a reorg disputes the withdrawal and freezes the Hot Wallet; an operator resolves it", func(t *testing.T) {
		snap := snapshot(ctx, t, rpcc)
		nonceBefore, _ := eth.NonceAt(ctx, hot, nil)
		amount := eth1(1)
		if code, body := carol.withdraw(t, "gap-1", ethAssetID, amount, destC); code != http.StatusAccepted {
			t.Fatalf("%d %s", code, body)
		}
		tx := waitConfirmed(ctx, t, rpcc, carol, "gap-1")
		if got := balance(ctx, t, eth, destC); got.Cmp(amount) != 0 {
			t.Fatalf("destination holds %s before the reorg", got)
		}
		if b := carol.ledger(t)[ethAssetID]; b.total != eth1(2).String() {
			t.Fatalf("carol's balance after Settle = %+v, want 2 ETH", b)
		}
		id := withdrawalID(t, carol, "gap-1")

		revert(ctx, t, rpcc, snap)
		mine(ctx, t, rpcc, 3)
		if rc, _ := eth.TransactionReceipt(ctx, common.HexToHash(tx)); rc != nil {
			t.Fatal("the withdrawal tx survived the revert")
		}
		if got := balance(ctx, t, eth, destC); got.Sign() != 0 {
			t.Fatalf("the destination still holds %s after the revert", got)
		}
		if n, _ := eth.NonceAt(ctx, hot, nil); n != nonceBefore {
			t.Fatalf("Hot Wallet mined nonce %d after the revert, want %d", n, nonceBefore)
		}

		// 1. Detected and disputed; Hot Wallet signing frozen at that nonce.
		waitFor(t, 90*time.Second, "gap-1 disputed", func() (bool, string) {
			mine(ctx, t, rpcc, 1)
			st, _ := carol.status(t, "gap-1")
			return st == "disputed", st
		})
		waitFor(t, 60*time.Second, "wallet-service freeze", func() (bool, string) {
			f := psql(t, "custody", `SELECT coalesce(frozen_from_nonce::text, '') FROM hot_wallet_nonces`)
			return f == fmt.Sprint(nonceBefore), "frozen_from_nonce=" + f
		})

		// 2. While frozen nothing new is signed: a withdrawal waits in `reserved`, and the Reaper
		// reclaims it on its own (void → Release), so the user's funds are not stuck behind the freeze.
		if code, body := carol.withdraw(t, "gap-2", ethAssetID, finney(100), destC); code != http.StatusAccepted {
			t.Fatalf("%d %s", code, body)
		}
		time.Sleep(4 * time.Second)
		if st, _ := carol.status(t, "gap-2"); st != "reserved" {
			t.Fatalf("gap-2 is %q during the freeze, want reserved", st)
		}
		waitFor(t, 90*time.Second, "the Reaper to reclaim gap-2", func() (bool, string) {
			st, _ := carol.status(t, "gap-2")
			return st == "failed", st
		})
		id2 := withdrawalID(t, carol, "gap-2")
		if r := psql(t, "withdrawal", fmt.Sprintf(`SELECT reject_reason FROM withdrawal_requests WHERE withdrawal_id = '%s'`, id2)); r != "stale_reservation" {
			t.Errorf("gap-2 failed with %q, want stale_reservation", r)
		}
		waitFor(t, 30*time.Second, "gap-2's Lock released", func() (bool, string) {
			b := carol.ledger(t)[ethAssetID]
			return b.locked == "0" && b.total == eth1(2).String(), fmt.Sprint(b)
		})

		// 3. The operator cancels the vanished tx's nonce with a Nonce Filler (wallet-service CLI), then
		// abandons the disputed request: it had Settled, so a Compensating Entry refunds it.
		out := compose(t, "exec", "-T", "wallet-service", "service", "hot-wallet", "cancel", "--key", "withdrawal:"+id)
		if !strings.Contains(out, "Nonce Filler broadcast") {
			t.Fatalf("hot-wallet cancel: %s", out)
		}
		mine(ctx, t, rpcc, 1)
		if n, _ := eth.NonceAt(ctx, hot, nil); n != nonceBefore+1 {
			t.Fatalf("Hot Wallet mined nonce %d after the filler, want %d", n, nonceBefore+1)
		}
		out = compose(t, "exec", "-T", "withdrawal-service", "service", "-abandon", id)
		if !strings.Contains(out, "failed (abandoned_refunded)") {
			t.Fatalf("abandon: %s", out)
		}
		if st, _ := carol.status(t, "gap-1"); st != "failed" {
			t.Errorf("gap-1 is %q after abandon", st)
		}
		// withdrawals.failed reached history-service for both failures (invariant #24), and the one that
		// had Settled went confirmed → failed there.
		for id, reason := range map[string]string{id: "abandoned_refunded", id2: "stale_reservation"} {
			waitFor(t, 60*time.Second, "history to record the failure of "+id, func() (bool, string) {
				d := historyDoc(t, id)
				return strings.Contains(d, `"status":"failed"`) && strings.Contains(d, `"fail_reason":"`+reason+`"`), d
			})
		}
		if b := carol.ledger(t)[ethAssetID]; b.total != eth1(3).String() || b.locked != "0" || b.available != eth1(3).String() {
			t.Errorf("carol's balance after the refund = %+v, want her full 3 ETH back", b)
		}

		// 4. Lift the freeze (a database action, by design) and withdrawals flow again.
		psql(t, "custody", `UPDATE hot_wallet_nonces SET frozen_from_nonce = NULL, frozen_at = NULL`)
		amount3 := finney(200)
		if code, body := carol.withdraw(t, "gap-3", ethAssetID, amount3, destC); code != http.StatusAccepted {
			t.Fatalf("%d %s", code, body)
		}
		tx3 := waitConfirmed(ctx, t, rpcc, carol, "gap-3")
		assertPaid(ctx, t, eth, tx3, hot, destC, amount3)
		if b := carol.ledger(t)[ethAssetID]; b.total != new(big.Int).Sub(eth1(3), amount3).String() || b.locked != "0" {
			t.Errorf("carol's final balance = %+v", b)
		}
	})
}

// ---- gateway helpers ------------------------------------------------------------------

// do calls the gateway as the user and returns the status and body, whatever they are.
func (u *user) do(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, gatewayURL+path, rd)
	req.Header.Set("Authorization", "Bearer "+u.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (u *user) withdraw(t *testing.T, key, assetID string, amount *big.Int, to common.Address) (int, []byte) {
	t.Helper()
	return u.do(t, "POST", "/v1/withdrawals", map[string]string{
		"idempotency_key": key, "asset_id": assetID, "amount": amount.String(), "to_address": to.Hex()})
}

// status is GET /v1/withdrawals/{key}: the state and the tx hash ("" while unknown).
func (u *user) status(t *testing.T, key string) (status, txHash string) {
	t.Helper()
	code, body := u.do(t, "GET", "/v1/withdrawals/"+key, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/withdrawals/%s: %d %s", key, code, body)
	}
	var r struct {
		Status string  `json:"status"`
		TxHash *string `json:"tx_hash"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	if r.TxHash != nil {
		txHash = *r.TxHash
	}
	return r.Status, txHash
}

func (u *user) whitelistStatus(t *testing.T, addr common.Address) string {
	t.Helper()
	code, body := u.do(t, "GET", "/v1/whitelist", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/whitelist: %d %s", code, body)
	}
	var rows []struct{ Address, Status string }
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	for _, r := range rows {
		if strings.EqualFold(r.Address, addr.Hex()) {
			return r.Status
		}
	}
	return "absent"
}

type ledgerBalance struct{ total, locked, available string }

func (u *user) ledger(t *testing.T) map[string]ledgerBalance {
	t.Helper()
	code, body := u.do(t, "GET", "/v1/balances", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/balances: %d %s", code, body)
	}
	var rows []struct {
		AssetID          string `json:"asset_id"`
		TotalBalance     string `json:"total_balance"`
		LockedBalance    string `json:"locked_balance"`
		AvailableBalance string `json:"available_balance"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	m := map[string]ledgerBalance{}
	for _, r := range rows {
		m[r.AssetID] = ledgerBalance{r.TotalBalance, r.LockedBalance, r.AvailableBalance}
	}
	return m
}

// waitConfirmed mines a block per poll (Withdrawal Confirmation Depth is 2 here) until the request
// is `confirmed`, and returns its tx hash.
func waitConfirmed(ctx context.Context, t *testing.T, rpcc *rpc.Client, u *user, key string) string {
	t.Helper()
	var hash string
	waitFor(t, 120*time.Second, key+" confirmed", func() (bool, string) {
		mine(ctx, t, rpcc, 1)
		st, h := u.status(t, key)
		hash = h
		if st == "failed" || st == "rejected" || st == "disputed" {
			t.Fatalf("%s ended %s", key, st)
		}
		return st == "confirmed" && h != "", st + " " + h
	})
	return hash
}

// assertPaid checks the chain itself: tx hash is a tx from the Hot Wallet that pays amount to dest
// (native value, or an ERC-20 transfer's calldata for the registered token).
func assertPaid(ctx context.Context, t *testing.T, eth *ethclient.Client, hash string, from, to common.Address, amount *big.Int) {
	t.Helper()
	tx, _, err := eth.TransactionByHash(ctx, common.HexToHash(hash))
	if err != nil {
		t.Fatalf("tx %s: %v", hash, err)
	}
	rc, err := eth.TransactionReceipt(ctx, tx.Hash())
	if err != nil || rc.Status != 1 {
		t.Fatalf("tx %s receipt: %v %+v", hash, err, rc)
	}
	sender, err := eth.TransactionSender(ctx, tx, rc.BlockHash, rc.TransactionIndex)
	if err != nil || sender != from {
		t.Errorf("tx %s sent by %s (%v), want the Hot Wallet %s", hash, sender, err, from)
	}
	if tx.To() == nil || *tx.To() != to || tx.Value().Cmp(amount) != 0 {
		t.Errorf("tx %s: to %v value %s, want %s %s", hash, tx.To(), tx.Value(), to, amount)
	}
}

// withdrawalID reads the internal id (never exposed by the API) for operator commands.
func withdrawalID(t *testing.T, u *user, key string) string {
	t.Helper()
	id := psql(t, "withdrawal", fmt.Sprintf(`SELECT withdrawal_id FROM withdrawal_requests WHERE user_id = '%s' AND idempotency_key = '%s'`, u.id, key))
	if id == "" {
		t.Fatalf("no withdrawal row for %s/%s", u.id, key)
	}
	return id
}

// ---- stack / chain helpers ------------------------------------------------------------

// compose runs `docker compose ... <args>` against the e2e project (same layering as the Makefile).
func compose(t *testing.T, args ...string) string {
	t.Helper()
	full := append([]string{"compose", "-p", "digiwal-e2e", "--project-directory", ".", "-f", "../infra/docker-compose.yml", "-f", "docker-compose.yml"}, args...)
	cmd := exec.Command("docker", full...)
	cmd.Env = os.Environ()
	if os.Getenv("MASTER_KEY_HEX") == "" { // only parsed by compose; `make test` exports the same throwaway value
		cmd.Env = append(cmd.Env, "MASTER_KEY_HEX=0000000000000000000000000000000000000000000000000000000000000001")
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// psql runs one statement in the shared Postgres and returns its (unaligned, tuples-only) output.
func psql(t *testing.T, db, sql string) string {
	t.Helper()
	return compose(t, "exec", "-T", "postgres", "psql", "-U", "custody", "-d", db, "-tA", "-c", sql)
}

// ensureToken returns the test ERC-20, deploying it if the M2 test has not already.
func ensureToken(ctx context.Context, t *testing.T, eth *ethclient.Client, key *ecdsa.PrivateKey) common.Address {
	t.Helper()
	addr := common.HexToAddress(tokenAddress)
	if code, err := eth.CodeAt(ctx, addr, nil); err == nil && len(code) > 0 {
		return addr
	}
	return deployToken(ctx, t, eth, key)
}

func freshAddress(t *testing.T) common.Address {
	t.Helper()
	k, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return crypto.PubkeyToAddress(k.PublicKey)
}

func setAutomine(ctx context.Context, t *testing.T, c *rpc.Client, on bool) {
	t.Helper()
	if err := c.CallContext(ctx, nil, "evm_setAutomine", on); err != nil {
		t.Fatalf("evm_setAutomine(%v): %v", on, err)
	}
}

func setNextBaseFee(ctx context.Context, t *testing.T, c *rpc.Client, wei *big.Int) {
	t.Helper()
	if err := c.CallContext(ctx, nil, "anvil_setNextBlockBaseFeePerGas", fmt.Sprintf("0x%x", wei)); err != nil {
		t.Fatalf("anvil_setNextBlockBaseFeePerGas: %v", err)
	}
}

func snapshot(ctx context.Context, t *testing.T, c *rpc.Client) string {
	t.Helper()
	var id string
	if err := c.CallContext(ctx, &id, "evm_snapshot"); err != nil {
		t.Fatalf("evm_snapshot: %v", err)
	}
	return id
}

func revert(ctx context.Context, t *testing.T, c *rpc.Client, id string) {
	t.Helper()
	var ok bool
	if err := c.CallContext(ctx, &ok, "evm_revert", id); err != nil || !ok {
		t.Fatalf("evm_revert(%s): %v %v", id, ok, err)
	}
}

// historyDoc is history-service's MongoDB document for a withdrawal (the read model has no query API yet).
func historyDoc(t *testing.T, withdrawalID string) string {
	t.Helper()
	return compose(t, "exec", "-T", "mongo", "mongosh", "--quiet", "-u", "custody", "-p", "custody", "--authenticationDatabase", "admin", "history",
		"--eval", fmt.Sprintf(`JSON.stringify(db.history.findOne({_id: "withdrawal:%s"}) || {})`, withdrawalID))
}
